package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// A 11-128 channel rejection is an invalid request channel, not an account
// restriction. The old gateway both disabled valid accounts and retried the
// same rejected request through the entire pool, needlessly amplifying it.
func TestChatChannelRefusedStopsAtOneAccountAndDoesNotDisable(t *testing.T) {
	const body = `{"code":"11-128","msg":"request illegal","displayTips":{"en":"channel verification failed"}}`
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			var calls int
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return 400, body, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
				&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
			)
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`
			if path == "/v1/responses" {
				payload = `{"model":"glm-5.2","input":"hello"}`
			} else if path == "/v1/messages" {
				payload = `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload)))
			wantType := "channel_refused"
			if path == "/v1/messages" {
				// Native Anthropic Messages uses its standard
				// invalid_request_error envelope; preserve the original
				// 11-128 explanation in the message instead of inventing
				// an incompatible Anthropic error type.
				wantType = "invalid_request_error"
			}
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), wantType) ||
				!strings.Contains(rec.Body.String(), "11-128") {
				t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body)
			}
			if calls != 1 {
				t.Fatalf("channel rejection must not rotate all accounts: calls=%d", calls)
			}
			for _, uid := range []string{"a1", "a2"} {
				st, _ := p.Status(uid)
				if st.Disabled || st.Cooling || st.ErrTotal != 0 {
					t.Fatalf("channel error incorrectly penalized %s: %+v", uid, st)
				}
			}
		})
	}
}

func TestSessionDeadPolicyCountsTransientFailures(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a1"})
	h := NewHandler(Config{Pool: p})
	for i := 1; i <= 2; i++ {
		h.applyErrorPolicy("a1", upstream.ErrSessionDead, `{"code":12153}`, "glm-5.2", nil)
		st, _ := p.Status("a1")
		if st.Disabled {
			t.Fatalf("12153 must not permanently disable on occurrence %d", i)
		}
	}
	p.ClearSessionDead("a1") // a successful refresh or chat breaks the streak
	for i := 1; i <= 2; i++ {
		h.applyErrorPolicy("a1", upstream.ErrSessionDead, `{"code":12153}`, "glm-5.2", nil)
		st, _ := p.Status("a1")
		if st.Disabled {
			t.Fatalf("successful refresh must reset session-dead streak: i=%d", i)
		}
	}
	h.applyErrorPolicy("a1", upstream.ErrSessionDead, `{"code":12153}`, "glm-5.2", nil)
	st, _ := p.Status("a1")
	if !st.Disabled {
		t.Fatal("three consecutive 12153 failures must disable until verified recovery")
	}
}
