package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestResponsesDesktopReasoningMarkupEndToEnd(t *testing.T) {
	// Codex Desktop 2026-10-20: the upstream model put a write_stdin call
	// inside reasoning_content. The previous adapter only repaired content,
	// forwarding the call as a reasoning summary and completing the turn.
	pipe := "\uFF5C\uFF5C"
	open := func(tag string) string { return "<" + pipe + "DSML" + pipe + " " + tag + ">" }
	end := func(tag string) string { return "</" + pipe + "DSML" + pipe + " " + tag + ">" }
	markup := open("calls") + "\n" +
		open(`invoke name="write_stdin"`) + "\n" +
		open(`parameter name="session_id" string="false"`) + "76905" + end("parameter") + "\n" +
		open(`parameter name="chars" string="true"`) + "" + end("parameter") + "\n" +
		end("invoke") + "\n" + end("calls")
	firstFrame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"reasoning_content": "I will poll the installer.\n" + markup},
		}},
	})
	secondFrame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": "I will keep inspecting the installer."},
		}},
	})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: " + string(firstFrame) + "\n\ndata: " + string(secondFrame) +
			"\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"deepseek-v4.1-flash","stream":true,
		"input":"Verify the installer","tools":[
			{"type":"function","name":"write_stdin",
			 "parameters":{"type":"object","properties":{
			   "session_id":{"type":"integer"},"chars":{"type":"string"}
			 }}}
		]}`))
	req.Header.Set("User-Agent", "Codex Desktop/0.163.0-alpha.10.2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"type":"function_call"`) ||
		!strings.Contains(rec.Body.String(), `"name":"write_stdin"`) ||
		!strings.Contains(rec.Body.String(), "response.completed") ||
		strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("native tool was not dispatched from reasoning: HTTP %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "response.function_call_arguments.done") ||
		!strings.Contains(rec.Body.String(), "response.output_item.done") {
		t.Fatalf("incomplete function-call protocol: %s", rec.Body)
	}
}
