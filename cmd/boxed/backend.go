package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// backend is what boxed serves to boxed -front (front.go).
type backend struct {
	servers []server
}

type server interface {
	Serve(net.Listener) error
	Close() error
}

// serveBackend serves the front on the sockets in dir; web nil: no web.
func serveBackend(b *Box, web *Web, dir string) (*backend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The front connects as root, which may be anyone: any host key will do.
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := gossh.NewSignerFromKey(sk)
	if err != nil {
		return nil, err
	}
	sessions := &ssh.Server{
		// Only root reaches the socket: the user is who the front says.
		Handler: func(s ssh.Session) {
			s.Context().SetValue(userKey, s.User())
			b.session(s)
		},
	}
	sessions.AddHostKey(signer)
	ctl := http.NewServeMux()
	ctl.HandleFunc("POST /key", b.frontKey)
	ctl.HandleFunc("POST /jump", b.frontJump)
	socks := map[string]server{backendSock: &http.Server{Handler: ctl}, sshSock: sessions}
	if web != nil {
		web.handOff = true
		socks[webSock] = &http.Server{Handler: fromFront(web)}
	}
	be := &backend{}
	for sock, srv := range socks {
		ln, err := listenUnix(filepath.Join(dir, sock))
		if err != nil {
			be.Close()
			return nil, err
		}
		be.servers = append(be.servers, srv)
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, ssh.ErrServerClosed) {
				log.Fatalf("%s: %v", sock, err)
			}
		}()
	}
	return be, nil
}

func (be *backend) Close() {
	for _, s := range be.servers {
		s.Close()
	}
}

// listenUnix listens on a socket only root can connect to.
func listenUnix(path string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// fromFront is the web as the front passes it on, with the client's
// address in a header only the front sets.
func fromFront(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get(frontAddrHeader)
		r.Header.Del(frontAddrHeader)
		h.ServeHTTP(rw, r)
	})
}

// frontKey says whose SSH key the body is: 403 for nobody's.
func (b *Box) frontKey(rw http.ResponseWriter, r *http.Request) {
	key, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	u, ok := b.keys.userOf(string(key))
	if !ok {
		http.Error(rw, "unknown key", http.StatusForbidden)
		return
	}
	io.WriteString(rw, u)
}

// frontJump is directTCPIP for the front: it connects as directTCPIP does,
// then hands the connection over.
func (b *Box) frontJump(rw http.ResponseWriter, r *http.Request) {
	var j jumpRequest
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		http.Error(rw, "invalid request", http.StatusBadRequest)
		return
	}
	v, dst, reason, err := b.jump(j.User, j.Host, j.Port)
	if err != nil {
		status := http.StatusBadGateway
		if reason == gossh.Prohibited {
			status = http.StatusForbidden
		}
		http.Error(rw, err.Error(), status)
		return
	}
	defer dst.Close()
	b.busy(v.Name, 0) // in use from now; the front counts it from then on
	head := "HTTP/1.1 200 OK\r\n" + frontVMHeader + ": " + v.Name + "\r\nContent-Length: 0\r\n\r\n"
	if err := handOff(rw, []byte(head), dst); err != nil {
		log.Printf("%s: jump to %s: %v", j.User, v.Name, err)
	}
}

// handOff answers the front with head and the open connection c, passed
// along as a file descriptor: the front owns it from then on, and c can
// close here.
func handOff(rw http.ResponseWriter, head []byte, c net.Conn) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return errors.New("cannot hand over this connection")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	conn, _, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		return err
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not the front")
	}
	var n int
	var werr error
	if err := raw.Control(func(fd uintptr) {
		n, _, werr = uc.WriteMsgUnix(head, syscall.UnixRights(int(fd)), nil)
	}); err != nil {
		return err
	}
	if werr == nil && n < len(head) {
		_, werr = uc.Write(head[n:])
	}
	return werr
}

// Hop-by-hop headers, which are for one connection, not passed on.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// upgrade opens a WebSocket (or another upgrade) to the VM as proxy would,
// then hands it over to the front, which pipes it: it outlives a restart of
// boxed. Everything else in the answer is as proxy makes it.
func (w *Web) upgrade(rw http.ResponseWriter, r *http.Request, v VM, port int, email string) {
	fail := func() {
		http.Error(rw, fmt.Sprintf("%s is not answering on port %d.", v.Name, port), http.StatusBadGateway)
	}
	addr := w.vmAddr(v, port)
	out := r.Clone(r.Context())
	up := r.Header.Get("Upgrade")
	for _, f := range r.Header.Values("Connection") {
		for _, h := range strings.Split(f, ",") {
			out.Header.Del(strings.TrimSpace(h))
		}
	}
	for _, h := range append(hopHeaders, "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto") {
		out.Header.Del(h)
	}
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header["User-Agent"] = []string{""} // not Go's
	}
	w.rewrite(&httputil.ProxyRequest{In: r, Out: out}, &url.URL{Scheme: "http", Host: addr}, email)
	out.Header.Set("Connection", "Upgrade")
	out.Header.Set("Upgrade", up)
	vm, err := w.dialVM(r.Context(), v, "tcp", addr)
	if err != nil {
		fail()
		return
	}
	defer vm.Close()
	vm.SetDeadline(time.Now().Add(30 * time.Second))
	if err := out.Write(vm); err != nil {
		fail()
		return
	}
	br := bufio.NewReader(vm)
	res, err := http.ReadResponse(br, out)
	if err != nil {
		fail()
		return
	}
	defer res.Body.Close()
	dropBoxCookies(res.Header)
	if res.StatusCode != http.StatusSwitchingProtocols {
		for _, h := range hopHeaders {
			res.Header.Del(h)
		}
		for k, vs := range res.Header {
			rw.Header()[k] = vs
		}
		rw.WriteHeader(res.StatusCode)
		io.Copy(rw, res.Body)
		return
	}
	vm.SetDeadline(time.Time{})
	res.Header.Set(frontVMHeader, v.Name) // whatever the VM said
	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 %s\r\n", res.Status)
	res.Header.Write(&head)
	head.WriteString("\r\n")
	// What the VM sent after its answer goes along.
	rest, _ := br.Peek(br.Buffered())
	head.Write(rest)
	if err := handOff(rw, head.Bytes(), vm); err != nil {
		log.Printf("web: %s: %v", v.Name, err)
	}
}
