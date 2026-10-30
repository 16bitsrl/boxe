package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// boxed -front holds the users' connections, so that restarting boxed (every
// deploy) does not drop them: it is the SSH server users talk to and the web
// server behind the TLS proxy. It decides nothing: boxed says who a key
// belongs to, runs the commands, checks every request, and opens every
// connection to a VM; then it hands the open connection over and the front
// pipes it from then on, through any restart of boxed. The front is small
// and changes rarely: changes to what follows need it restarted too
// (scripts/deploy.sh --front), so keep them compatible.
//
// They talk over unix sockets in -sockets, a directory only root can enter:
const (
	frontSock   = "front.sock"   // the front: GET /conns, POST /revalidate
	backendSock = "backend.sock" // boxed: POST /key, POST /jump
	sshSock     = "ssh.sock"     // boxed: SSH sessions, as the user the front logged in
	webSock     = "web.sock"     // boxed: the web, as the TLS proxy sends it
)

// Headers between the two. Only the front reaches web.sock and sets the
// first; only boxed answers the second, with a connection that comes along.
const (
	frontAddrHeader = "X-Box-Front-Addr" // the client's address, as the front sees it
	frontVMHeader   = "X-Box-Front-Vm"   // the VM a connection handed over goes to
)

// jumpRequest asks boxed for a connection to port of user's VM host.
type jumpRequest struct {
	User string `json:"user"`
	Host string `json:"host"`
	Port uint32 `json:"port"`
}

// Front is boxed -front.
type Front struct {
	dir   string
	wait  time.Duration // how long requests wait for boxed while it restarts
	proxy http.Handler

	mu    sync.Mutex
	conns map[ssh.Context]net.Conn // open SSH connections, to drop revoked users
	open  map[string]int           // VM -> connections piped to it
}

func newFront(dir string) *Front {
	f := &Front{dir: dir, wait: 15 * time.Second}
	f.proxy = &httputil.ReverseProxy{
		// One connection per request: none is left half-used by a restart.
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return f.dial(ctx, webSock)
			},
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "boxed"
			for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
			pr.Out.Header.Set(frontAddrHeader, pr.In.RemoteAddr)
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				log.Printf("front: web: %v", err)
			}
			http.Error(rw, "boxe is restarting, try again in a moment.", http.StatusBadGateway)
		},
	}
	return f
}

// runFront is main for boxed -front.
func runFront(dir, stateDir, tsName, listen, httpAddr string) {
	if dir == "" {
		log.Fatal("-front needs -sockets")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	f := newFront(dir)
	ctl, err := listenUnix(filepath.Join(dir, frontSock))
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(http.Serve(ctl, f.control())) }()
	if httpAddr != "" {
		go func() { log.Fatal(http.ListenAndServe(httpAddr, f)) }()
	}
	signer, err := loadHostKey(stateDir + "/boxed_host_ed25519")
	if err != nil {
		log.Fatal(err)
	}
	srv := f.sshServer(signer)
	lns := sshListeners(stateDir, tsName, listen)
	go f.RevalidateLoop(10 * time.Minute)
	for _, ln := range lns[1:] {
		go func() { log.Fatal(srv.Serve(ln)) }()
	}
	log.Printf("boxed -front %s: listening on %d addresses", version, len(lns))
	log.Fatal(srv.Serve(lns[0]))
}

// dial connects to one of boxed's sockets, waiting for it while it restarts.
func (f *Front) dial(ctx context.Context, sock string) (net.Conn, error) {
	end := time.Now().Add(f.wait)
	for {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", filepath.Join(f.dir, sock))
		if err == nil || ctx.Err() != nil || time.Now().After(end) {
			return c, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ask sends req to boxed on sock and reads its answer, with the connection
// boxed handed over if it did. The caller closes c (and vm).
func (f *Front) ask(ctx context.Context, sock string, req *http.Request) (res *http.Response, br *bufio.Reader, c, vm net.Conn, err error) {
	c, err = f.dial(ctx, sock)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, nil, nil, nil, err
	}
	fc := &fdConn{UnixConn: c.(*net.UnixConn)}
	br = bufio.NewReader(fc)
	res, err = http.ReadResponse(br, req)
	if err != nil {
		fc.closeFDs()
		c.Close()
		return nil, nil, nil, nil, err
	}
	if len(fc.fds) > 0 {
		file := os.NewFile(uintptr(fc.fds[0]), "vm")
		fc.fds = fc.fds[1:]
		vm, err = net.FileConn(file)
		file.Close()
	}
	fc.closeFDs()
	if err != nil {
		c.Close()
		return nil, nil, nil, nil, err
	}
	return res, br, c, vm, nil
}

// fdConn reads from boxed and keeps the file descriptors that come along.
type fdConn struct {
	*net.UnixConn
	fds []int
}

func (c *fdConn) Read(b []byte) (int, error) {
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(b, oob)
	if msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn]); perr == nil {
		for _, m := range msgs {
			if fds, err := syscall.ParseUnixRights(&m); err == nil {
				c.fds = append(c.fds, fds...)
			}
		}
	}
	return n, err
}

func (c *fdConn) closeFDs() {
	for _, fd := range c.fds {
		syscall.Close(fd)
	}
	c.fds = nil
}

// count records a connection to the VM name opening (+1) or closing (-1).
func (f *Front) count(name string, delta int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		f.open = map[string]int{}
	}
	if f.open[name] += delta; f.open[name] <= 0 {
		delete(f.open, name)
	}
}

// openConns is how many connections the front pipes to each VM.
func (f *Front) openConns() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	open := map[string]int{}
	for name, n := range f.open {
		open[name] = n
	}
	return open
}

// control is what boxed asks the front: which VMs are in use (for sleep),
// and to drop the connections of keys no longer valid after a refresh.
func (f *Front) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /conns", func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(f.openConns())
	})
	mux.HandleFunc("POST /revalidate", func(rw http.ResponseWriter, r *http.Request) {
		f.revalidate()
	})
	return mux
}

// userOf asks boxed whose key this is; known is false for nobody's.
func (f *Front) userOf(ctx context.Context, key []byte) (user string, known bool, err error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://boxed/key", bytes.NewReader(key))
	res, _, c, _, err := f.ask(ctx, backendSock, req)
	if err != nil {
		return "", false, err
	}
	defer c.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<10))
	switch {
	case err != nil:
		return "", false, err
	case res.StatusCode == http.StatusOK:
		return string(b), true, nil
	case res.StatusCode == http.StatusForbidden:
		return "", false, nil
	}
	return "", false, fmt.Errorf("key: %s", res.Status)
}

// revalidate drops the connections whose user or key is no longer valid,
// as boxed alone does.
func (f *Front) revalidate() {
	f.mu.Lock()
	conns := map[ssh.Context]net.Conn{}
	for ctx, c := range f.conns {
		conns[ctx] = c
	}
	f.mu.Unlock()
	for ctx, conn := range conns {
		user, _ := ctx.Value(userKey).(string)
		key, _ := ctx.Value(keyKey).(string)
		u, ok, err := f.userOf(context.Background(), []byte(key))
		if err != nil {
			log.Printf("front: revalidate: %v", err)
			return // boxed does not answer: next time
		}
		if !ok || u != user {
			log.Printf("%s: key revoked, disconnecting", user)
			conn.Close()
		}
	}
}

// RevalidateLoop also checks by itself, in case boxed could not tell it.
func (f *Front) RevalidateLoop(every time.Duration) {
	for {
		time.Sleep(every)
		f.revalidate()
	}
}

func (f *Front) sshServer(hostKey ssh.Signer) *ssh.Server {
	srv := &ssh.Server{
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			u, ok, err := f.userOf(ctx, key.Marshal())
			if err != nil {
				log.Printf("front: key: %v", err)
			}
			if ok {
				ctx.SetValue(userKey, u)
				ctx.SetValue(keyKey, string(key.Marshal()))
			}
			return ok
		},
		ConnCallback: func(ctx ssh.Context, conn net.Conn) net.Conn {
			f.mu.Lock()
			if f.conns == nil {
				f.conns = map[ssh.Context]net.Conn{}
			}
			f.conns[ctx] = conn
			f.mu.Unlock()
			go func() {
				<-ctx.Done()
				f.mu.Lock()
				delete(f.conns, ctx)
				f.mu.Unlock()
			}()
			return conn
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      f.session,
			"direct-tcpip": f.directTCPIP,
		},
	}
	srv.AddHostKey(hostKey)
	return srv
}

const restarting = "boxe is restarting, try again in a moment"

// directTCPIP asks boxed for the connection to the VM (boxed checks it is
// the user's, wakes the VM up and connects), then pipes it.
func (f *Front) directTCPIP(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	var d struct {
		DestAddr string
		DestPort uint32
		OrigAddr string
		OrigPort uint32
	}
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, "invalid request")
		return
	}
	user, _ := ctx.Value(userKey).(string)
	body, _ := json.Marshal(jumpRequest{User: user, Host: d.DestAddr, Port: d.DestPort})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://boxed/jump", bytes.NewReader(body))
	res, _, c, vm, err := f.ask(ctx, backendSock, req)
	if err != nil {
		log.Printf("front: jump: %v", err)
		newChan.Reject(gossh.ConnectionFailed, restarting)
		return
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
	c.Close()
	if vm == nil {
		reason := gossh.ConnectionFailed
		if res.StatusCode == http.StatusForbidden {
			reason = gossh.Prohibited
		}
		newChan.Reject(reason, strings.TrimSpace(string(msg)))
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		vm.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	name := res.Header.Get(frontVMHeader)
	f.count(name, 1)
	go func() {
		pipe(ch, vm)
		f.count(name, -1)
	}()
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		defer dst.Close()
		defer src.Close()
		io.Copy(dst, src)
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// session passes a session (a command, the prompt) on to boxed's own SSH
// server, as the user the front logged in, with everything that goes with
// it both ways: stdin, pty and window changes, exit status. It ends when
// boxed restarts.
func (f *Front) session(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	user, _ := ctx.Value(userKey).(string)
	c, err := f.dial(ctx, sshSock)
	if err != nil {
		log.Printf("front: session: %v", err)
		newChan.Reject(gossh.ConnectionFailed, restarting)
		return
	}
	cc, chans, reqs, err := gossh.NewClientConn(c, "boxed", &gossh.ClientConfig{User: user, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
	if err != nil {
		c.Close()
		log.Printf("front: session: %v", err)
		newChan.Reject(gossh.ConnectionFailed, restarting)
		return
	}
	client := gossh.NewClient(cc, chans, reqs)
	defer client.Close()
	bch, breqs, err := client.OpenChannel("session", newChan.ExtraData())
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	ch, creqs, err := newChan.Accept()
	if err != nil {
		bch.Close()
		return
	}
	var out sync.WaitGroup
	out.Add(2)
	go func() {
		defer out.Done()
		io.Copy(ch, bch)
	}()
	go func() {
		defer out.Done()
		io.Copy(ch.Stderr(), bch.Stderr())
	}()
	go func() {
		io.Copy(bch, ch)
		bch.CloseWrite()
	}()
	go func() {
		for r := range creqs {
			ok, err := bch.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
		}
		bch.Close()
	}()
	for r := range breqs {
		// The exit status after everything the command printed.
		if strings.HasPrefix(r.Type, "exit-") {
			out.Wait()
		}
		ok, err := ch.SendRequest(r.Type, r.WantReply, r.Payload)
		if r.WantReply {
			r.Reply(ok && err == nil, nil)
		}
	}
	out.Wait()
	ch.Close()
}

// ServeHTTP passes every request on to boxed. WebSockets (and other
// upgrades) boxed hands over once open: the front pipes them.
func (f *Front) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if isUpgrade(r) {
		f.upgrade(rw, r)
		return
	}
	f.proxy.ServeHTTP(rw, r)
}

func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (f *Front) upgrade(rw http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.Header.Set(frontAddrHeader, r.RemoteAddr)
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header["User-Agent"] = []string{""} // not Go's
	}
	res, br, c, vm, err := f.ask(r.Context(), webSock, out)
	if err != nil {
		log.Printf("front: web: %v", err)
		http.Error(rw, restarting+".", http.StatusBadGateway)
		return
	}
	defer c.Close()
	if vm == nil {
		// Refused, or the VM did not switch: boxed's answer as it is.
		defer res.Body.Close()
		if res.StatusCode == http.StatusSwitchingProtocols {
			http.Error(rw, restarting+".", http.StatusBadGateway)
			return
		}
		for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding"} {
			res.Header.Del(h)
		}
		for k, v := range res.Header {
			rw.Header()[k] = v
		}
		rw.WriteHeader(res.StatusCode)
		io.Copy(rw, res.Body)
		return
	}
	defer vm.Close()
	name := res.Header.Get(frontVMHeader)
	res.Header.Del(frontVMHeader)
	// What the VM sent after its answer, which boxed read already.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest, err := io.ReadAll(br)
	if err != nil {
		http.Error(rw, restarting+".", http.StatusBadGateway)
		return
	}
	client, cbuf, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	fmt.Fprintf(cbuf, "HTTP/1.1 %s\r\n", res.Status)
	res.Header.Write(cbuf)
	cbuf.WriteString("\r\n")
	cbuf.Write(rest)
	if err := cbuf.Flush(); err != nil {
		return
	}
	// What the client sent already.
	if n := cbuf.Reader.Buffered(); n > 0 {
		b, _ := cbuf.Peek(n)
		if _, err := vm.Write(b); err != nil {
			return
		}
	}
	f.count(name, 1)
	defer f.count(name, -1)
	pipe(client, vm)
}

// frontClient is how boxed reaches the front.
type frontClient struct{ sock string }

func (fc *frontClient) do(method, path string) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", fc.sock)
		},
	}}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest(method, "http://front"+path, nil)
	res, err := client.Do(req)
	if err == nil && res.StatusCode != http.StatusOK {
		res.Body.Close()
		err = errors.New(path + ": " + res.Status)
	}
	return res, err
}

// conns is how many connections the front pipes to each VM.
func (fc *frontClient) conns() (map[string]int, error) {
	res, err := fc.do("GET", "/conns")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var open map[string]int
	return open, json.NewDecoder(res.Body).Decode(&open)
}

// revalidate has the front drop the connections of keys no longer valid.
func (fc *frontClient) revalidate() error {
	res, err := fc.do("POST", "/revalidate")
	if err == nil {
		res.Body.Close()
	}
	return err
}
