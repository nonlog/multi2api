package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProxyIDSurvivesAuthParseAndSave(t *testing.T) {
	input := []byte(`{"auth":{"accessToken":"access","refreshToken":"refresh","expiresAt":1893456000,"realm":"cn"},"account":{"uid":"proxy-test"},"proxy_id":"jp-warp"}`)
	a, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ProxyIDValue(); got != "jp-warp" {
		t.Fatalf("parsed proxy ID=%q", got)
	}
	a.FilePath = filepath.Join(t.TempDir(), "auth.json")
	a.SetProxyID("wa-direct")
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["proxy_id"] != "wa-direct" {
		t.Fatalf("saved proxy_id=%v", saved["proxy_id"])
	}
	reloaded, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ProxyIDValue(); got != "wa-direct" {
		t.Fatalf("reloaded proxy ID=%q", got)
	}
}
