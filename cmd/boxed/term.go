package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"
	gossh "golang.org/x/crypto/ssh"
)

// The web terminal on https://<vm>--term.<domain>: a page served by boxed
// and a WebSocket to boxed, which opens an SSH session with a pty in the VM,
// as boxedev, and attaches to a scroll session (main by default, the one
// `scroll` opens over SSH too). The shell outlives the page and boxed:
// the page reconnects to the same session. Who may use it is who may use
// the agent: the owner and the members with access (see ServeHTTP).

// termWebPort stands for the terminal among a VM's ports: boxed serves it,
// not the VM. Below minVMPort, so not reachable as <vm>--<port>.
const termWebPort = 2997

//go:embed all:term
var termFiles embed.FS

var termStatic = func() http.Handler {
	sub, _ := fs.Sub(termFiles, "term")
	return http.FileServerFS(sub)
}()

// vmTerm is a shell in a VM with a pty.
type vmTerm interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

var termSession = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// terminal serves the terminal of v to the member logged in as email, who
// may use it (checked by ServeHTTP).
func (w *Web) terminal(rw http.ResponseWriter, r *http.Request, v VM, email string) {
	csp := "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; style-src 'self' 'unsafe-inline'; script-src 'self' 'wasm-unsafe-eval'"
	rw.Header().Set("Content-Security-Policy", csp)
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Referrer-Policy", "same-origin")
	if r.URL.Path == "/favicon.ico" {
		rw.Header().Set("Cache-Control", "public, max-age=3600")
		r.URL.Path = "/static/favicon-32.png"
		panelStatic.ServeHTTP(rw, r)
		return
	}
	if r.URL.Path != "/ws" {
		if r.URL.Path == "/" {
			rw.Header().Set("Cache-Control", "no-store")
		} else {
			rw.Header().Set("Cache-Control", "public, max-age=300") // short: no versioned addresses
		}
		termStatic.ServeHTTP(rw, r)
		return
	}
	q := r.URL.Query()
	session := q.Get("s")
	if session == "" {
		session = "main"
	}
	if !termSession.MatchString(session) {
		http.Error(rw, "Invalid session name: letters, digits, - _ and . only.", http.StatusBadRequest)
		return
	}
	cols, rows := termSize(q.Get("cols"), 80), termSize(q.Get("rows"), 24)
	if w.shell == nil || !isUpgrade(r) {
		http.Error(rw, "Not a WebSocket.", http.StatusBadRequest)
		return
	}
	sh, err := w.shell(r.Context(), v, session, cols, rows)
	if err != nil {
		log.Printf("web: %s: terminal in %s: %v", email, v.Name, err)
		http.Error(rw, "Could not open a shell in "+v.Name+", try again.", http.StatusBadGateway)
		return
	}
	defer sh.Close()
	c, err := w.acceptWS(rw, r, v.Name)
	if err != nil {
		log.Printf("web: %s: terminal in %s: %v", email, v.Name, err)
		return
	}
	log.Printf("web: %s: terminal in %s (%s)", email, v.Name, session)
	bridge(c, sh)
}

// termSize is a window size from the page, within what a screen can be.
func termSize(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 2 || n > 1000 {
		return def
	}
	return n
}

// acceptWS opens the WebSocket. Under boxed -front, the front keeps the
// browser's connection and pipes it to a socket boxed answers on: boxed
// restarting closes the WebSocket (the page reconnects), not the front.
func (w *Web) acceptWS(rw http.ResponseWriter, r *http.Request, vm string) (*websocket.Conn, error) {
	if !w.handOff {
		return websocket.Accept(rw, r, nil)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	ours, theirs, err := socketConns(fds)
	if err != nil {
		return nil, err
	}
	defer theirs.Close()
	fake := &handshake{header: http.Header{}, conn: ours}
	fake.header.Set(frontVMHeader, vm) // the front counts it as a connection to vm
	c, err := websocket.Accept(fake, r, nil)
	if err != nil {
		ours.Close()
		// The refusal as websocket wrote it.
		for k, v := range fake.header {
			rw.Header()[k] = v
		}
		rw.WriteHeader(fake.status)
		rw.Write(fake.body.Bytes())
		return nil, err
	}
	if err := handOff(rw, fake.head.Bytes(), theirs); err != nil {
		c.CloseNow()
		return nil, err
	}
	return c, nil
}

func socketConns(fds [2]int) (a, b net.Conn, err error) {
	var conns [2]net.Conn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "ws")
		conns[i], err = net.FileConn(f)
		f.Close()
		if err != nil {
			if i == 1 {
				conns[0].Close()
			} else {
				syscall.Close(fds[1])
			}
			return nil, nil, err
		}
	}
	return conns[0], conns[1], nil
}

// handshake is what websocket.Accept answers on: the head of its answer is
// kept for the front, and the connection it takes over is boxed's end.
type handshake struct {
	header http.Header
	status int
	head   bytes.Buffer // the answer to switch protocols, for the front
	body   bytes.Buffer // a refusal
	conn   net.Conn
}

func (h *handshake) Header() http.Header { return h.header }

func (h *handshake) WriteHeader(status int) {
	h.status = status
	if status == http.StatusSwitchingProtocols {
		fmt.Fprintf(&h.head, "HTTP/1.1 101 %s\r\n", http.StatusText(status))
		h.header.Write(&h.head)
		h.head.WriteString("\r\n")
	}
}

func (h *handshake) Write(b []byte) (int, error) {
	if h.status == 0 {
		h.WriteHeader(http.StatusOK)
	}
	return h.body.Write(b)
}

func (h *handshake) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// bridge passes the pty's output to the page and what the page types to
// the pty, until either ends. Binary messages are the terminal's bytes,
// text messages are commands: {"type":"resize","cols":..,"rows":..}.
func bridge(c *websocket.Conn, sh vmTerm) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetReadLimit(1 << 20)
	go func() {
		defer cancel()
		for {
			typ, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := sh.Write(b); err != nil {
					return
				}
				continue
			}
			var m struct {
				Type       string
				Cols, Rows int
			}
			if json.Unmarshal(b, &m) == nil && m.Type == "resize" {
				sh.Resize(termSize(strconv.Itoa(m.Cols), 80), termSize(strconv.Itoa(m.Rows), 24))
			}
		}
	}()
	// Pings keep proxies from closing a quiet terminal.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	out := make(chan error, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := sh.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					out <- werr
					return
				}
			}
			if err != nil {
				out <- err
				return
			}
		}
	}()
	select {
	case err := <-out:
		if errors.Is(err, io.EOF) {
			c.Close(websocket.StatusNormalClosure, "exited")
			return
		}
		c.CloseNow()
	case <-ctx.Done():
		c.CloseNow()
	}
}

// sshTerm is a session with a pty in a VM.
type sshTerm struct {
	client *gossh.Client
	s      *gossh.Session
	in     io.WriteCloser
	out    io.Reader
}

func (t *sshTerm) Read(b []byte) (int, error)  { return t.out.Read(b) }
func (t *sshTerm) Write(b []byte) (int, error) { return t.in.Write(b) }
func (t *sshTerm) Resize(cols, rows int) error { return t.s.WindowChange(rows, cols) }
func (t *sshTerm) Close() error {
	t.s.Close()
	return t.client.Close()
}

// termCommand attaches to the scroll session, or starts a login shell in
// VMs made before scroll was in the image.
func termCommand(session string) string {
	return "if command -v scroll >/dev/null; then exec scroll " + session + "; else exec bash -l; fi"
}

// openShell is Web.shell: a pty in v, trusting only v's own host key (a VM
// that takes its name and address after it is deleted has another).
func (b *Box) openShell(ctx context.Context, w *Web, v VM, session string, cols, rows int) (vmTerm, error) {
	b.mu.RLock()
	hostKey, err := b.hv.hostKey(v.Name)
	b.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(b.hv.HostKeyPath)
	if err != nil {
		return nil, err
	}
	signer, err := gossh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(v.IP(), "22")
	conn, err := w.dialVM(ctx, v, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	cc, chans, reqs, err := gossh.NewClientConn(conn, addr, &gossh.ClientConfig{
		User: "boxedev", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(pub), HostKeyAlgorithms: []string{pub.Type()},
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	client := gossh.NewClient(cc, chans, reqs)
	s, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	t := &sshTerm{client: client, s: s}
	if t.in, err = s.StdinPipe(); err == nil {
		t.out, err = s.StdoutPipe()
	}
	if err == nil {
		s.Stderr = io.Discard // with a pty, everything comes on stdout
		err = s.RequestPty("xterm-256color", rows, cols, gossh.TerminalModes{gossh.ECHO: 1})
	}
	if err == nil {
		err = s.Start(termCommand(session))
	}
	if err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}
