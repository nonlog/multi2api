package proxypool

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LocalProxy exposes an authenticated loopback HTTP CONNECT proxy for the Bun
// provider worker. Bun supports HTTP proxies but not SOCKS5; this bridge routes
// its CONNECT tunnels through the same named SOCKS5 pool used by Go accounts.
type LocalProxy struct {
	manager  *Manager
	server   *http.Server
	listener net.Listener
	token    string
	endpoint string
}

func StartLocalProxy(manager *Manager) (*LocalProxy, error) {
	if manager == nil {
		return nil, fmt.Errorf("proxy pool manager is nil")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, fmt.Errorf("generate local proxy token: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for provider proxy: %w", err)
	}
	p := &LocalProxy{manager: manager, listener: listener, token: base64.RawURLEncoding.EncodeToString(secret[:]), endpoint: "http://" + listener.Addr().String()}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if err := p.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			// The parent process owns the lifecycle; an unavailable proxy causes
			// provider requests to fail closed rather than silently going direct.
			fmt.Printf("provider proxy server stopped: %v\n", err)
		}
	}()
	return p, nil
}

func (p *LocalProxy) Endpoint() string { return p.endpoint }
func (p *LocalProxy) Token() string    { return p.token }

func (p *LocalProxy) Close() error {
	if p == nil || p.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

func (p *LocalProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := p.authorize(r.Header.Get("Proxy-Authorization"))
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="Multi2API proxy pool"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	entry, ok := p.manager.Get(id)
	if !ok || entry.URL == "" {
		http.Error(w, "proxy route not found", http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, entry.URL)
		return
	}
	p.forward(w, r, entry.URL)
}

func (p *LocalProxy) authorize(header string) (string, bool) {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	id, token, ok := strings.Cut(string(decoded), ":")
	if !ok || id == "" || subtle.ConstantTimeCompare([]byte(token), []byte(p.token)) != 1 {
		return "", false
	}
	return id, true
}

func (p *LocalProxy) connect(w http.ResponseWriter, r *http.Request, proxyURL string) {
	target := strings.TrimSpace(r.Host)
	if target == "" {
		target = strings.TrimSpace(r.URL.Host)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "CONNECT target must include a port", http.StatusBadRequest)
		return
	}
	upstream, err := dialSOCKS5(r.Context(), proxyURL, target)
	if err != nil {
		http.Error(w, "upstream proxy connection failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "CONNECT is unavailable", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, rw)
		closeWrite(upstream)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		copyDone <- struct{}{}
	}()
	<-copyDone
	_ = client.Close()
	_ = upstream.Close()
	<-copyDone
}

func (p *LocalProxy) forward(w http.ResponseWriter, r *http.Request, proxyURL string) {
	if r.URL == nil || !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "absolute request URL required", http.StatusBadRequest)
		return
	}
	target := r.URL.Host
	if !strings.Contains(target, ":") {
		if r.URL.Scheme == "https" {
			target = net.JoinHostPort(target, "443")
		} else {
			target = net.JoinHostPort(target, "80")
		}
	}
	conn, err := dialSOCKS5(r.Context(), proxyURL, target)
	if err != nil {
		http.Error(w, "upstream proxy connection failed", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	out := r.Clone(r.Context())
	out.RequestURI = r.URL.RequestURI()
	out.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	if err := out.Write(conn); err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), out)
	if err != nil {
		http.Error(w, "upstream response failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	resp.Header.Del("Proxy-Authenticate")
	resp.Header.Del("Proxy-Authorization")
	_ = resp.Write(w)
}

func closeWrite(conn net.Conn) {
	if c, ok := conn.(*net.TCPConn); ok {
		_ = c.CloseWrite()
	}
}

// DialContext connects target (host:port) through a named SOCKS5/SOCKS5H endpoint.
// For socks5h, the target hostname is sent to the proxy and resolved remotely.
func DialContext(ctx context.Context, rawURL, target string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return dialSOCKS5(ctx, rawURL, target)
}

func dialSOCKS5(ctx context.Context, rawURL, target string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || (u.Scheme != "socks5h" && u.Scheme != "socks5") || u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("invalid SOCKS5 proxy URL")
	}
	proxyAddress := net.JoinHostPort(u.Hostname(), u.Port())
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) { _ = conn.Close(); return nil, err }
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	methods := []byte{0}
	if u.User != nil {
		methods = []byte{0, 2}
	}
	greeting := append([]byte{5, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fail(err)
	}
	var selection [2]byte
	if _, err := io.ReadFull(conn, selection[:]); err != nil {
		return fail(err)
	}
	if selection[0] != 5 || selection[1] == 0xff {
		return fail(fmt.Errorf("SOCKS5 authentication method rejected"))
	}
	if selection[1] == 2 {
		username := u.User.Username()
		password, _ := u.User.Password()
		if len(username) > 255 || len(password) > 255 {
			return fail(fmt.Errorf("SOCKS5 credentials exceed 255 bytes"))
		}
		auth := []byte{1, byte(len(username))}
		auth = append(auth, username...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if _, err := conn.Write(auth); err != nil {
			return fail(err)
		}
		var result [2]byte
		if _, err := io.ReadFull(conn, result[:]); err != nil {
			return fail(err)
		}
		if result[0] != 1 || result[1] != 0 {
			return fail(fmt.Errorf("SOCKS5 username/password rejected"))
		}
	} else if selection[1] != 0 {
		return fail(fmt.Errorf("SOCKS5 server selected unsupported auth method %d", selection[1]))
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return fail(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fail(fmt.Errorf("invalid SOCKS5 target port"))
	}
	request := []byte{5, 1, 0}
	if u.Scheme == "socks5h" {
		if len(host) > 255 {
			return fail(fmt.Errorf("SOCKS5 target hostname is too long"))
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	} else if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 1)
			request = append(request, v4...)
		} else {
			request = append(request, 4)
			request = append(request, ip.To16()...)
		}
	} else {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return fail(fmt.Errorf("resolve SOCKS5 target: %w", err))
		}
		if v4 := ips[0].IP.To4(); v4 != nil {
			request = append(request, 1)
			request = append(request, v4...)
		} else {
			request = append(request, 4)
			request = append(request, ips[0].IP.To16()...)
		}
	}
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fail(err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fail(fmt.Errorf("SOCKS5 CONNECT rejected with code %d", reply[1]))
	}
	var boundLen int
	switch reply[3] {
	case 1:
		boundLen = 4
	case 4:
		boundLen = 16
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return fail(err)
		}
		boundLen = int(n[0])
	default:
		return fail(fmt.Errorf("SOCKS5 reply has invalid address type"))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(boundLen+2)); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
