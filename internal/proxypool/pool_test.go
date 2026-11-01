package proxypool

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testProxyList = `DIRECT
US WARP | socks5h://172.18.0.1:56356
WA Direct | socks5h://wa.414222.xyz:56355
WA WARP | socks5h://wa.414222.xyz:56356
JP Direct | socks5h://jp.414222.xyz:56355
JP WARP | socks5h://jp.414222.xyz:56356
TYO Direct | socks5h://tyo.414222.xyz:56355
TYO WARP | socks5h://tyo.414222.xyz:56356
`

func TestParseTextEightNamedRoutes(t *testing.T) {
	entries, err := ParseText(testProxyList)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 {
		t.Fatalf("got %d routes, want 8", len(entries))
	}
	want := []string{"direct", "us-warp", "wa-direct", "wa-warp", "jp-direct", "jp-warp", "tyo-direct", "tyo-warp"}
	for i, id := range want {
		if entries[i].ID != id {
			t.Fatalf("route[%d].ID=%q, want %q", i, entries[i].ID, id)
		}
	}
	if entries[0].URL != "" {
		t.Fatalf("DIRECT URL must be empty, got %q", entries[0].URL)
	}
}

func TestManagerPersistsImportedPoolAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "proxy-pool.json")
	m, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 1 || m.List()[0].ID != "direct" {
		t.Fatalf("initial pool = %#v", m.List())
	}
	if _, err := m.ImportText(testProxyList); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pool file mode=%o, want 600", info.Mode().Perm())
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 8 {
		t.Fatalf("reloaded %d routes", len(reloaded.List()))
	}
	if entry, ok := reloaded.Get("jp-warp"); !ok || entry.URL != "socks5h://jp.414222.xyz:56356" {
		t.Fatalf("JP WARP route not persisted: %#v %v", entry, ok)
	}
}

func TestParseTextRejectsInvalidOrDuplicateRoutes(t *testing.T) {
	for _, input := range []string{
		"WA | socks5h://example.com:0\nDIRECT",
		"DIRECT\nWA Direct | http://example.com:8080",
		"DIRECT\nWA Direct | socks5h://example.com:1080\nWA Direct | socks5h://other.example:1080",
		"WA Direct | socks5h://example.com:1080",
	} {
		if _, err := ParseText(input); err == nil {
			t.Fatalf("ParseText(%q) unexpectedly succeeded", input)
		}
	}
}

func TestLocalProxyAuthenticatesAndUsesRemoteDNS(t *testing.T) {
	socks, targetHost := startMockSOCKS5(t)
	defer socks.Close()
	manager, err := New(filepath.Join(t.TempDir(), "proxy-pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := "DIRECT\nTest Node | socks5h://" + socks.Addr().String()
	if _, err := manager.ImportText(text); err != nil {
		t.Fatal(err)
	}
	local, err := StartLocalProxy(manager)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(local.Endpoint(), "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("test-node:" + local.Token()))
	_, _ = fmt.Fprintf(conn, "CONNECT example.invalid:443 HTTP/1.1\r\nHost: example.invalid:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "200 Connection Established") {
		t.Fatalf("CONNECT response = %q", line)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	select {
	case host := <-targetHost:
		if host != "example.invalid" {
			t.Fatalf("SOCKS5 target host=%q, want remote hostname", host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SOCKS5 server did not receive CONNECT")
	}
	_ = conn.Close()
}

func TestLocalProxyRejectsMissingAuthentication(t *testing.T) {
	socks, _ := startMockSOCKS5(t)
	defer socks.Close()
	manager, err := New(filepath.Join(t.TempDir(), "proxy-pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ImportText("DIRECT\nTest Node | socks5h://" + socks.Addr().String()); err != nil {
		t.Fatal(err)
	}
	local, err := StartLocalProxy(manager)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(local.Endpoint(), "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "CONNECT example.invalid:443 HTTP/1.1\r\nHost: example.invalid:443\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "407 Proxy Authentication Required") {
		t.Fatalf("unauthenticated CONNECT response=%q", line)
	}
}

func startMockSOCKS5(t *testing.T) (net.Listener, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
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
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		host := ""
		switch header[3] {
		case 3:
			var size [1]byte
			if _, err := io.ReadFull(conn, size[:]); err != nil {
				return
			}
			name := make([]byte, int(size[0]))
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
		default:
			return
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		got <- host
		_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		_, _ = io.Copy(io.Discard, conn)
	}()
	return listener, got
}
