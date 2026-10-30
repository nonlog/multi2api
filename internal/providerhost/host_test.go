package providerhost

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestMaintenanceProviderFilter(t *testing.T) {
	var target string
	h := &Host{client: &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		target = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Header:     make(http.Header),
		}, nil
	})}}
	if _, err := h.ReadMaintenance(context.Background(), "usage", "qoder-cn"); err != nil {
		t.Fatalf("filtered quota request failed: %v", err)
	}
	if target != "http://provider.local/usage?provider=qoder-cn" {
		t.Errorf("unexpected quota endpoint: %s", target)
	}
	if _, err := h.ReadMaintenance(context.Background(), "usage", "not-a-provider"); err == nil {
		t.Fatal("unknown provider quota filter was accepted")
	}
	if _, err := h.ReadMaintenance(context.Background(), "checkins", "zcode"); err == nil {
		t.Fatal("unexpected check-in account filter was accepted")
	}
}

func TestRecognizeProviderModel(t *testing.T) {
	for _, model := range []string{
		"zcode:glm-5.3", "qoder:qoder-model", "qoder-cn:qoder-model",
		"qwenwork:qwen-work", "qwenwork-enterprise:qwen-work",
		"codearts:codearts-model",
	} {
		id, bare, ok := ModelProvider(model)
		if !ok || id == "" || bare == "" {
			t.Errorf("unrecognized integrated provider model: %q", model)
		}
	}
	for _, invalid := range []string{"", "glm-5.3", "cn:glm-5.3", "global:glm-5.3",
		"qoder:", ":glm", "unknown:glm", "https://example.com/secret"} {
		if _, _, ok := ModelProvider(invalid); ok {
			t.Errorf("unrecognized model %q was treated as external provider", invalid)
		}
	}
}

func TestStartRejectsMissingCredentialPathAndRuntimeFile(t *testing.T) {
	if _, err := Start("", "", ""); err == nil {
		t.Fatal("empty provider credential file was accepted")
	}
	if _, err := Start("", "/does/not/exist/worker.mjs", "/tmp/never-used.json"); err == nil {
		t.Fatal("missing plugin script was accepted")
	}
}
