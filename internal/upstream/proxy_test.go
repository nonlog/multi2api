package upstream

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nonlog/multi2api/internal/auth"
)

func TestAccountHTTPClientSelectsAssignedProxyTransport(t *testing.T) {
	client := New()
	client.SetProxyResolver(func(id string) (string, bool) {
		if id == "wa-direct" {
			return "socks5h://127.0.0.1:1080", true
		}
		return "", false
	})

	proxiedAccount := &auth.Auth{}
	proxiedAccount.SetProxyID("wa-direct")
	proxied := client.httpFor(proxiedAccount)
	if proxied == client.HTTP {
		t.Fatal("assigned account unexpectedly uses the shared DIRECT client")
	}
	transport, ok := proxied.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil || transport.Proxy != nil {
		t.Fatalf("account proxy transport is not using the SOCKS dialer: %#v", proxied.Transport)
	}

	directAccount := &auth.Auth{}
	directAccount.SetProxyID("direct")
	if got := client.httpFor(directAccount); got != client.HTTP {
		t.Fatal("DIRECT account did not reuse the shared HTTP client")
	}
}

func TestUnknownAccountProxyFailsClosed(t *testing.T) {
	client := New()
	client.SetProxyResolver(func(string) (string, bool) { return "", false })
	a := &auth.Auth{}
	a.SetProxyID("deleted-proxy")
	if _, err := client.httpFor(a).Get("http://127.0.0.1/"); err == nil || !strings.Contains(err.Error(), "not present in the proxy pool") {
		t.Fatalf("unknown proxy did not fail closed: %v", err)
	}
}

func TestSOCKS5HUsesRemoteDNSForAccountRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	hostCh := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var greeting [2]byte
		if _, err := io.ReadFull(conn, greeting[:]); err != nil {
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		_, _ = conn.Write([]byte{5, 0})
		var head [4]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		host := ""
		switch head[3] {
		case 3:
			var n [1]byte
			if _, err := io.ReadFull(conn, n[:]); err != nil {
				return
			}
			name := make([]byte, int(n[0]))
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			host = string(name)
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = net.IP(ip).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = net.IP(ip).String()
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		hostCh <- host
		// Return a deterministic SOCKS failure after recording the requested host.
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
	}()

	client := New()
	client.SetProxyResolver(func(id string) (string, bool) {
		if id == "remote-dns" {
			return "socks5h://" + listener.Addr().String(), true
		}
		return "", false
	})
	a := &auth.Auth{}
	a.SetProxyID("remote-dns")
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/path", nil)
	_, err = client.httpFor(a).Do(req)
	if err == nil {
		t.Fatal("expected mocked SOCKS failure")
	}
	select {
	case got := <-hostCh:
		if got != "example.invalid" {
			t.Fatalf("SOCKS5H sent %q instead of remote hostname", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SOCKS5 proxy did not receive request")
	}
}

func TestAccountChatTransportPreservesHeaderTimeout(t *testing.T) {
	client := New()
	client.HeaderTimeout = 37 * time.Second
	client.IdleTimeout = 19 * time.Second
	client.SetProxyResolver(func(id string) (string, bool) {
		if id == "wa-warp" {
			return "socks5h://127.0.0.1:56356", true
		}
		return "", false
	})
	a := &auth.Auth{}
	a.SetProxyID("wa-warp")
	pair := client.clientsForAuth(a)
	transport, ok := pair.chat.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("account chat transport has type %T", pair.chat.Transport)
	}
	if transport.ResponseHeaderTimeout != client.HeaderTimeout {
		t.Fatalf("account chat header timeout=%s, want %s", transport.ResponseHeaderTimeout, client.HeaderTimeout)
	}
	if pair.chat.Timeout != 0 {
		t.Fatalf("account chat total timeout=%s, want 0", pair.chat.Timeout)
	}
	if pair.http.Timeout != client.HTTP.Timeout {
		t.Fatalf("account HTTP timeout=%s, want %s", pair.http.Timeout, client.HTTP.Timeout)
	}
}
