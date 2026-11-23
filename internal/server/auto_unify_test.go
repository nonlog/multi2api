package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nonlog/multi2api/internal/auth"
	"github.com/nonlog/multi2api/internal/upstream"
)

func TestAutoUnifyWhitelist(t *testing.T) {
	if err := ValidateAutoUnifyModels(AutoUnifyChoices); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAutoUnifyModels([]string{}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"glm-5.3-Flash-Trial"},
		{"deepseek-v4.1"},
		{"glm-5.3-flash", "glm-5.3-flash"},
		{"zcode:GLM-5.3-Flash"},
		{"glm-5.3", "anything-else"},
	} {
		if err := ValidateAutoUnifyModels(bad); err == nil {
			t.Errorf("accepted model list %v", bad)
		}
	}
}

func TestAutoUnifyExactVendorModelMatch(t *testing.T) {
	valid := []string{
		"cn:glm-5.3", "global:glm-5.3", "codearts:glm-5.3",
		"zcode:GLM-5.3-Flash", "codearts:glm-5.3-flash",
		"codearts:deepseek-v4.1-flash",
	}
	for _, id := range valid {
		canonical := strings.ToLower(strings.SplitN(id, ":", 2)[1])
		if !exactAutoSource(id, canonical) {
			t.Errorf("not a matching source: %s", id)
		}
	}
	for _, id := range []string{
		"zcode:GLM-5.3-Flash-Trial", "codearts:glm-5.3-flash-1017",
		"codearts:deepseek-v4.1-flash-1017", "codearts:glm-5.3-pro",
		"random:glm-5.3", "glm-5.3", "unknown:deepseek-v4.1-flash",
	} {
		canonical := "glm-5.3"
		if strings.Contains(id, "deepseek") {
			canonical = "deepseek-v4.1-flash"
		}
		if strings.Contains(id, "Flash") || strings.Contains(id, "flash-") {
			canonical = "glm-5.3-flash"
		}
		if exactAutoSource(id, canonical) {
			t.Errorf("false match: %s", id)
		}
	}
}

func TestAutoUnifyListingAndVendorCandidates(t *testing.T) {
	discovered := []map[string]any{
		{"id": "cn:glm-5.3-flash", "context_length": 131072},
		{"id": "global:glm-5.3-flash", "context_length": 65536},
		{"id": "zcode:GLM-5.3-Flash", "context_length": 200000},
		{"id": "codearts:glm-5.3-flash", "context_length": 100000},
		{"id": "zcode:GLM-5.3-Flash-Trial", "context_length": 1000},
		{"id": "codearts:glm-5.3"},
	}
	enabled := []string{"glm-5.3-flash", "glm-5.3", "deepseek-v4.1-flash"}
	aliases := autoWorkBuddyAliases(map[string][]string{"deepseek-v4.1-flash": {"cn:deepseek-v4.1-flash"}}, enabled, discovered)
	if len(aliases["deepseek-v4.1-flash"]) != 1 {
		t.Fatal("explicit map overwritten")
	}
	if !slices.Equal(aliases["glm-5.3-flash"], []string{"cn:glm-5.3-flash", "global:glm-5.3-flash"}) {
		t.Fatalf("wrong WorkBuddy aliases: %v", aliases)
	}
	merged := mapModelListing(discovered[:2], aliases)
	if len(merged) != 1 || merged[0]["id"] != "glm-5.3-flash" {
		t.Fatalf("CN/Global aliases not collapsed: %v", merged)
	}
	exposed := attachAutoUnifyListing(merged, discovered, enabled)
	if len(exposed) != 2 {
		t.Fatalf("expected one available WorkBuddy and one vendor-only auto alias, got %d", len(exposed))
	}
	sources := exposed[0]["routing_sources"].([]string)
	expected := []string{"cn:glm-5.3-flash", "global:glm-5.3-flash", "zcode:GLM-5.3-Flash", "codearts:glm-5.3-flash"}
	if !slices.Equal(sources, expected) {
		t.Fatalf("invalid source list (trial should be excluded): %v", sources)
	}
	if got := autoVendors("glm-5.3-flash", discovered); !slices.Equal(got, []string{"codearts:glm-5.3-flash", "zcode:GLM-5.3-Flash"}) {
		t.Errorf("wrong vendor candidates: %v", got)
	}
	if got := autoVendors("glm-5.3", discovered); !slices.Equal(got, []string{"codearts:glm-5.3"}) {
		t.Errorf("glm vendor missing: %v", got)
	}
	if got := autoVendors("deepseek-v4.1-flash", discovered); len(got) != 0 {
		t.Errorf("should not route absent model: %v", got)
	}
}

type autoProviderStub struct {
	calls []string
}

func (*autoProviderStub) Models(context.Context) []map[string]any {
	return []map[string]any{{"id": "zcode:GLM-5.3-Flash"}, {"id": "zcode:GLM-5.3-Flash-Trial"}}
}

func (p *autoProviderStub) ProxyChat(w http.ResponseWriter, _ *http.Request, _ []byte, provider, model string, _ func()) {
	p.calls = append(p.calls, provider+":"+model)
	_, _ = io.WriteString(w, "vendor response")
}

func TestAutoRouteWorkBuddyTimeoutAndRotation(t *testing.T) {
	const model = "glm-5.3-flash"
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	for _, testcase := range []struct {
		name      string
		timeout   bool
		wantFirst string
	}{
		{"timeout falls back without retrying same upstream", true, "vendor response"},
		{"healthy WorkBuddy shares traffic", false, "workbuddy response"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			p := testPoolWith(
				&auth.Auth{UID: "first", AccessToken: "at", ExpiresAt: 9999999999},
				&auth.Auth{UID: "second", AccessToken: "at", ExpiresAt: 9999999999},
			)
			u := upstream.New()
			wbCalls := 0
			u.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				wbCalls++
				if testcase.timeout {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
					"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"workbuddy response\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
				))}, nil
			})}
			h := NewHandler(Config{Pool: p, Upstream: u})
			vendors := &autoProviderStub{}
			send := func(want string) {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				w := httptest.NewRecorder()
				h.routeAutoChat(w, r, body, model, vendors, true, func(fallback func()) {
					h.chatWorkBuddy(w, r, body, chatRequestHeader{Model: model}, "cn", model, "", fallback)
				})
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
					t.Fatalf("status=%d body=%s, want %q", w.Code, w.Body.String(), want)
				}
			}
			send(testcase.wantFirst)
			send("vendor response")
			wantVendorCalls := 1
			if testcase.timeout {
				wantVendorCalls++
			}
			if wbCalls != 1 || len(vendors.calls) != wantVendorCalls || vendors.calls[0] != "zcode:GLM-5.3-Flash" {
				t.Fatalf("WorkBuddy calls=%d provider calls=%v", wbCalls, vendors.calls)
			}
			if got := p.AvailableUIDsForModelRealm(model, "cn"); len(got) != 2 {
				t.Errorf("WorkBuddy accounts remain eligible after request: %v", got)
			}
		})
	}
}
