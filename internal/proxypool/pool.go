package proxypool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Entry is a named egress route. The built-in direct entry intentionally has
// an empty URL; it is a selectable no-proxy route, not a network endpoint.
type Entry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type Manager struct {
	mu      sync.RWMutex
	path    string
	entries []Entry
	byID    map[string]Entry
}

func New(path string) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("proxy pool path is empty")
	}
	m := &Manager{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read proxy pool: %w", err)
		}
		if err := m.replace([]Entry{{ID: "direct", Name: "DIRECT"}}); err != nil {
			return nil, err
		}
		return m, nil
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse proxy pool: %w", err)
	}
	if err := validateEntries(entries); err != nil {
		return nil, err
	}
	m.entries = cloneEntries(entries)
	m.reindexLocked()
	return m, nil
}

func (m *Manager) List() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneEntries(m.entries)
}

func (m *Manager) Get(id string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byID[strings.TrimSpace(id)]
	return e, ok
}

func (m *Manager) Resolve(id string) (*url.URL, bool) {
	e, ok := m.Get(id)
	if !ok {
		return nil, false
	}
	if e.URL == "" {
		return nil, true
	}
	u, err := url.Parse(e.URL)
	return u, err == nil
}

// ParseText accepts the user's bulk-import format: NAME | socks5h://host:port.
// A line containing only DIRECT creates the built-in direct route.
func ParseText(text string) ([]Entry, error) {
	if len(text) > 64<<10 {
		return nil, fmt.Errorf("proxy pool import exceeds 64 KiB")
	}
	var entries []Entry
	for lineNo, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawURL, hasURL := strings.Cut(line, "|")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("line %d: proxy name is empty", lineNo+1)
		}
		id := slug(name)
		entry := Entry{ID: id, Name: name}
		if hasURL {
			entry.URL = strings.TrimSpace(rawURL)
		}
		if id == "direct" {
			if entry.URL != "" {
				return nil, fmt.Errorf("line %d: DIRECT must not have a URL", lineNo+1)
			}
			entry.Name = "DIRECT"
		}
		if err := validateEntry(entry); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("proxy pool is empty")
	}
	if err := validateEntries(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (m *Manager) ImportText(text string) ([]Entry, error) {
	entries, err := ParseText(text)
	if err != nil {
		return nil, err
	}
	if err := m.replace(entries); err != nil {
		return nil, err
	}
	return m.List(), nil
}

func (m *Manager) replace(entries []Entry) error {
	if err := validateEntries(entries); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return fmt.Errorf("create proxy pool directory: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write proxy pool: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit proxy pool: %w", err)
	}
	m.mu.Lock()
	m.entries = cloneEntries(entries)
	m.reindexLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) reindexLocked() {
	m.byID = make(map[string]Entry, len(m.entries))
	for _, e := range m.entries {
		m.byID[e.ID] = e
	}
}

func (m *Manager) Test(ctx context.Context, id string) (ip string, elapsed time.Duration, err error) {
	e, ok := m.Get(id)
	if !ok {
		return "", 0, fmt.Errorf("unknown proxy %q", id)
	}
	transport := &http.Transport{}
	if e.URL != "" {
		u, parseErr := url.Parse(e.URL)
		if parseErr != nil {
			return "", 0, parseErr
		}
		transport.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	elapsed = time.Since(start)
	if err != nil {
		return "", elapsed, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", elapsed, fmt.Errorf("egress probe returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return "", elapsed, err
	}
	var result struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &result) == nil && result.IP != "" {
		return result.IP, elapsed, nil
	}
	ip = strings.TrimSpace(string(body))
	if ip == "" || len(ip) > 64 {
		return "", elapsed, fmt.Errorf("egress probe returned an invalid address")
	}
	return ip, elapsed, nil
}

func validateEntries(entries []Entry) error {
	if len(entries) == 0 || len(entries) > 64 {
		return fmt.Errorf("proxy pool must contain 1 to 64 entries")
	}
	seen := make(map[string]bool, len(entries))
	direct := false
	for _, e := range entries {
		if err := validateEntry(e); err != nil {
			return fmt.Errorf("proxy %q: %w", e.Name, err)
		}
		if seen[e.ID] {
			return fmt.Errorf("duplicate proxy ID %q", e.ID)
		}
		seen[e.ID] = true
		if e.ID == "direct" {
			direct = true
		}
	}
	if !direct {
		return fmt.Errorf("proxy pool must include DIRECT")
	}
	return nil
}

func validateEntry(e Entry) error {
	if e.ID == "" || e.Name == "" {
		return fmt.Errorf("ID and name are required")
	}
	if e.ID != slug(e.Name) {
		return fmt.Errorf("ID %q does not match its name", e.ID)
	}
	if e.ID == "direct" {
		if e.URL != "" {
			return fmt.Errorf("DIRECT URL must be empty")
		}
		return nil
	}
	u, err := url.Parse(e.URL)
	if err != nil || u == nil || u.Hostname() == "" {
		return fmt.Errorf("invalid proxy URL")
	}
	if u.Scheme != "socks5h" && u.Scheme != "socks5" {
		return fmt.Errorf("unsupported proxy scheme %q (use socks5h or socks5)", u.Scheme)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("proxy URL must include a valid port")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("proxy URL must not contain a path, query or fragment")
	}
	return nil
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r) && r < 128 {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func cloneEntries(in []Entry) []Entry { return append([]Entry(nil), in...) }
