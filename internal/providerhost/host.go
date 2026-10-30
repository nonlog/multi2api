// Package providerhost supervises the Magpie-compatible Provider plugins
// INSIDE the existing WorkBuddy2API service. The optional Bun worker speaks
// only over an owner-private Unix socket. No second public gateway, no remote
// Magpie dependency, and no access to WorkBuddy's account credential pool.
package providerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var supported = map[string]bool{
	"zcode": true, "qoder": true, "qoder-cn": true,
	"qwenwork": true, "qwenwork-enterprise": true, "codearts": true,
}

// Recognizes matches a provider-qualified model ID. Bare model names remain
// owned by WorkBuddy's existing CN/Global model-routing logic.
func Recognizes(id string) bool { return supported[id] }

type Host struct {
	client *http.Client
	cmd    *exec.Cmd
	cancel context.CancelFunc
	dir    string
	once   sync.Once
}

func Start(bun, worker, authFile string, autoCheckin ...bool) (*Host, error) {
	if bun == "" {
		bun = "bun"
	}
	if worker == "" {
		worker = "./integrations/provider-host/worker.mjs"
	}
	if authFile == "" {
		return nil, errors.New("providers.auth_file is required")
	}
	if _, err := os.Stat(worker); err != nil {
		return nil, fmt.Errorf("provider worker file: %w", err)
	}
	if st, err := os.Stat(authFile); err == nil && st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("providers.auth_file %s must not be accessible by other users", authFile)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "wb2api-plugins-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	socket := filepath.Join(dir, "provider.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bun, worker)
	cmd.Env = append(os.Environ(),
		"WB2API_PROVIDER_SOCKET="+socket,
		"WB2API_PROVIDER_AUTH_FILE="+authFile,
		"WB2API_PROVIDER_AUTO_CHECKIN="+fmt.Sprint(len(autoCheckin) > 0 && autoCheckin[0]))
	// Plugin diagnostics can contain upstream-issued tokens. Do not expose
	// their stdout/stderr in the gateway logs or to downstream API callers.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	h := &Host{
		cmd: cmd, cancel: cancel, dir: dir,
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		}},
	}
	if err := cmd.Start(); err != nil {
		h.Close()
		return nil, fmt.Errorf("start embedded provider runtime: %w", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		ctx, done := context.WithTimeout(context.Background(), 600*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://provider.local/health", nil)
		resp, reqErr := h.client.Do(req)
		if reqErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done()
		if reqErr == nil && resp.StatusCode == http.StatusOK {
			return h, nil
		}
		time.Sleep(80 * time.Millisecond)
	}
	h.Close()
	return nil, errors.New("embedded provider runtime did not become ready")
}

// Close stops only the worker subprocess created by this Host, then removes
// only its private temporary socket directory.
func (h *Host) Close() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.cancel()
		if h.cmd.Process != nil {
			_ = h.cmd.Wait()
		}
		h.client.CloseIdleConnections()
		_ = os.RemoveAll(h.dir)
	})
}

func ModelProvider(model string) (provider, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx <= 0 || idx == len(model)-1 {
		return "", "", false
	}
	provider, bare = model[:idx], model[idx+1:]
	return provider, bare, Recognizes(provider)
}

func (h *Host) Models(ctx context.Context) []map[string]any {
	if h == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://provider.local/models", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload) != nil {
		return nil
	}
	return payload.Data
}

func (h *Host) Accounts(ctx context.Context) (map[string]any, error) {
	if h == nil {
		return map[string]any{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://provider.local/accounts", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("provider runtime unavailable")
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadMaintenance exposes only the public quota/check-in summaries supplied
// by the bundled plugin's original usage and checkin hooks.
func (h *Host) ReadMaintenance(ctx context.Context, endpoint string, provider ...string) (map[string]any, error) {
	if h == nil || (endpoint != "usage" && endpoint != "checkins") {
		return nil, errors.New("provider runtime unavailable")
	}
	path := "http://provider.local/" + endpoint
	if len(provider) > 0 && provider[0] != "" {
		if endpoint != "usage" || !Recognizes(provider[0]) {
			return nil, errors.New("invalid provider quota filter")
		}
		path += "?provider=" + url.QueryEscape(provider[0])
	}
	ctx, cancel := context.WithTimeout(ctx, 105*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", path, nil)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider maintenance request status %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func (h *Host) Checkin(ctx context.Context, provider, account string, force bool) (map[string]any, error) {
	if h == nil || !Recognizes(provider) {
		return nil, errors.New("invalid provider")
	}
	payload, _ := json.Marshal(map[string]any{
		"provider": provider, "account": account, "force": force,
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://provider.local/checkin", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&data); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider check-in rejected (HTTP %d)", resp.StatusCode)
	}
	return data, nil
}

func (h *Host) ProxyChat(w http.ResponseWriter, r *http.Request, body []byte, provider, bare string) {
	if h == nil || !Recognizes(provider) {
		http.Error(w, "provider runtime not configured", http.StatusServiceUnavailable)
		return
	}
	var src map[string]json.RawMessage
	if err := json.Unmarshal(body, &src); err != nil {
		http.Error(w, "invalid Chat Completions payload", http.StatusBadRequest)
		return
	}
	modelBytes, _ := json.Marshal(bare)
	src["model"] = modelBytes
	encoded, err := json.Marshal(src)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST",
		"http://provider.local/chat/"+provider, bytes.NewReader(encoded))
	if err != nil {
		http.Error(w, "invalid provider request", http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if session := r.Header.Get("X-Deepseek-Harness-Session-Id"); session != "" {
		req.Header.Set("X-Wb2api-Session", session)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, "embedded provider runtime unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	if strings.Contains(ct, "text/event-stream") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	// Flush each received chunk: Codex and Claude Code must see tool calls
	// without waiting for the entire completion or upstream timeout.
	buf := make([]byte, 16*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			_ = http.NewResponseController(w).Flush()
		}
		if readErr != nil {
			return
		}
	}
}
