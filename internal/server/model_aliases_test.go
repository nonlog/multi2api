package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/multi2api/internal/auth"
)

var deepseekAlias = map[string][]string{
	"deepseek-v4.1-flash": {
		"cn:deepseek-v4.1-flash",
		"global:deepseek-v4.1-flash",
	},
}

func TestModelAliasesValidateAndResolve(t *testing.T) {
	if err := ValidateModelAliases(deepseekAlias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, realm, bare string
	}{
		{"deepseek-v4.1-flash", "", "deepseek-v4.1-flash"},
		{"cn:deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
		{"unmapped-model", "cn", "unmapped-model"},
	} {
		realm, bare := ResolveMappedModel(tc.name, deepseekAlias)
		if realm != tc.realm || bare != tc.bare {
			t.Errorf("%q returned (%q,%q), want (%q,%q)", tc.name, realm, bare, tc.realm, tc.bare)
		}
	}
	for _, bad := range []map[string][]string{
		{"": {"cn:foo"}}, {"cn:foo": {"cn:foo"}},
		{"foo": {}}, {"foo": {"foo"}},
		{"foo": {"cn:foo", "cn:foo"}},
		{"foo": {"cn:foo", "global:other"}},
		{"foo": {"cn:foo", "global:foo", "cn:foo"}},
		{"foo": {"global: foo"}},
	} {
		if err := ValidateModelAliases(bad); err == nil {
			t.Errorf("invalid alias configuration was accepted: %v", bad)
		}
	}
	if realm, bare := ResolveMappedModel("example", map[string][]string{
		"example": {"global:deepseek-v4.1-flash"},
	}); realm != "global" || bare != "deepseek-v4.1-flash" {
		t.Errorf("single-realm alias mismatch: %q, %q", realm, bare)
	}
}

func TestMapModelListingDeduplicatesConservatively(t *testing.T) {
	cn := map[string]any{
		"id": "cn:deepseek-v4.1-flash", "name": "DeepSeek Flash",
		"context_length": int64(1_000_000), "max_output_tokens": int64(100_000),
		"supports_tool_call": true, "supports_images": true,
	}
	global := map[string]any{
		"id": "global:deepseek-v4.1-flash", "name": "DeepSeek Flash",
		"context_length": int64(400_000), "max_output_tokens": int64(32_000),
		"supports_tool_call": true,
	}
	other := map[string]any{"id": "cn:other"}
	out := mapModelListing([]map[string]any{cn, global, other}, deepseekAlias)
	if len(out) != 2 {
		t.Fatalf("models=%d, want alias and other: %+v", len(out), out)
	}
	first := out[0]
	if first["id"] != "deepseek-v4.1-flash" ||
		first["context_length"] != int64(400_000) ||
		first["max_output_tokens"] != int64(32_000) ||
		first["supports_images"] != nil ||
		first["supports_tool_call"] != true {
		t.Errorf("dedup or conservative capabilities broken: %+v", first)
	}
	if v, ok := first["routing_realms"].([]string); !ok || len(v) != 2 || v[0] != "cn" || v[1] != "global" {
		t.Errorf("routing realms not retained: %v", first["routing_realms"])
	}
	if cn["id"] != "cn:deepseek-v4.1-flash" {
		t.Fatal("source model was modified")
	}
	if v := mapModelListing([]map[string]any{global}, deepseekAlias); len(v) != 1 ||
		v[0]["id"] != "deepseek-v4.1-flash" {
		t.Fatalf("global-only catalog must still advertise alias: %v", v)
	}
	if v := mapModelListing([]map[string]any{other}, deepseekAlias); len(v) != 1 || v[0]["id"] != "cn:other" {
		t.Fatalf("must not invent alias if no realm serves it: %v", v)
	}
}

func TestAliasCallsNativeProtocolsWithOnlyGlobalAccount(t *testing.T) {
	up := newFakeUpstream(t, func(_ string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.GlobalEnabled = true
	up.ChatBaseGlobal = "https://fake.example"
	p := testPoolWith(&auth.Auth{
		UID: "global", AccessToken: "at", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai",
	})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true,
		ModelAliases: deepseekAlias})
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"deepseek-v4.1-flash","input":"hi","stream":false}`},
		{"/v1/messages", `{"model":"deepseek-v4.1-flash","max_tokens":50,"stream":false,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s status=%d body=%s", tc.path, w.Code, w.Body)
		}
		if tc.path == "/v1/responses" {
			var response map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			if response["model"] != "deepseek-v4.1-flash" {
				t.Errorf("native Responses must expose unprefixed model: %v", response["model"])
			}
		}
	}
}
