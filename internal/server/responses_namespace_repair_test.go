package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/multi2api/internal/auth"
)

const codexNamespaceName = "mcp__codex_apps"
const codexBareToolName = "chat_on_steroids_core"
const codexPrefixedToolName = codexNamespaceName + "__" + codexBareToolName

// Mirrors the actual Codex Desktop incident of 2026-10-18: the model
// emitted a fully qualified tool name in DSML while the translated Chat
// request declared the namespace's child tool by its bare name.
func codexIncidentMarkup(tool string) string {
	pipe := "\uFF5C\uFF5C"
	open := func(s string) string { return "<" + pipe + "DSML" + pipe + " " + s + ">" }
	close := func(s string) string { return "</" + pipe + "DSML" + pipe + " " + s + ">" }
	return open("calls") + "\n" +
		open(`invoke name="`+tool+`"`) + "\n" +
		open(`parameter name="name" string="true"`) + "Get-Content" + close("parameter") + "\n" +
		open(`parameter name="parameters" string="false"`) +
		`{"path":"C:/Users/test/.codex/skills/tidy-skill/SKILL.md","raw":true}` +
		close("parameter") + "\n" +
		close("invoke") + "\n" + close("calls")
}

func codexNamespaceRequest(t *testing.T, stream bool) string {
	t.Helper()
	in := map[string]any{
		"model":  "deepseek-v4.1-flash",
		"input":  "Inspect the skill documentation and report a safe cleanup plan.",
		"stream": stream,
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": codexNamespaceName,
				"tools": []any{map[string]any{
					"type": "function", "name": codexBareToolName,
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":       map[string]any{"type": "string"},
							"parameters": map[string]any{"type": "object"},
						},
					},
				}},
			},
			map[string]any{"type": "function", "name": "exec_command",
				"parameters": map[string]any{"type": "object"}},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newCodexNamespaceHandler(t *testing.T, tool string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, markupUpstreamSSE("I will check the skill.\n" + codexIncidentMarkup(tool)), true
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

func TestResponsesNamespaceDSMLRepairStream(t *testing.T) {
	h := newCodexNamespaceHandler(t, codexPrefixedToolName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(codexNamespaceRequest(t, true))))
	if rec.Code != 200 {
		t.Fatalf("http=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("native markup must not leak into Codex output: %s", rec.Body)
	}
	var added, done map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt) != nil {
			continue
		}
		item, _ := evt["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		switch evt["type"] {
		case "response.output_item.added":
			added = item
		case "response.output_item.done":
			done = item
		}
	}
	for event, item := range map[string]map[string]any{"added": added, "done": done} {
		if item == nil || item["name"] != codexBareToolName || item["namespace"] != codexNamespaceName {
			t.Fatalf("missing/incorrect %s function_call item: %v (SSE=%s)", event, item, rec.Body)
		}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(done["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if args["name"] != "Get-Content" {
		t.Fatalf("unexpected tool name argument: %v", args)
	}
	params, _ := args["parameters"].(map[string]any)
	if params["raw"] != true || !strings.Contains(params["path"].(string), "tidy-skill") {
		t.Fatalf("tool parameters were changed: %v", args)
	}
	if !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("stream didn't complete: %s", rec.Body)
	}
}

func TestResponsesNamespaceDSMLRepairNonStream(t *testing.T) {
	h := newCodexNamespaceHandler(t, codexPrefixedToolName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(codexNamespaceRequest(t, false))))
	if rec.Code != 200 {
		t.Fatalf("http=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("native markup must not appear in nonstream response: %s", rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	items, _ := obj["output"].([]any)
	found := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" {
			found = true
			if item["name"] != codexBareToolName || item["namespace"] != codexNamespaceName {
				t.Fatalf("function_call mismatch: %v", item)
			}
		}
	}
	if !found {
		t.Fatalf("Codex did not receive a tool call: %s", rec.Body)
	}
}

func TestResponsesNamespaceDSMLUnknownToolRejected(t *testing.T) {
	h := newCodexNamespaceHandler(t, codexNamespaceName+"__not_declared")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(codexNamespaceRequest(t, false))))
	// Native Responses no longer treats an undeclared DSML call as a valid
	// completed answer. Plain Chat remains unchanged; Codex receives a
	// structured error and cannot accidentally dispatch this tool.
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "invalid_tool_call") ||
		strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("undeclared tool must fail safely: HTTP %d, body=%s", rec.Code, rec.Body)
	}
}

func TestResponsesNamespaceDSMLSpoofedNamespaceRejected(t *testing.T) {
	h := newCodexNamespaceHandler(t, "another_namespace__"+codexBareToolName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(codexNamespaceRequest(t, false))))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "invalid_tool_call") ||
		strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("spoofed namespace must fail safely: code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestResponsesInvokeStringFlagProductionVariant(t *testing.T) {
	// The 13:52 Codex incident had an invocation attribute that the old
	// parser refused, even though the tool was already declared.
	const tool = codexPrefixedToolName
	oldTag := `invoke name="` + tool + `">`
	newTag := `invoke name="` + tool + `" string="true">`
	content := strings.Replace(codexIncidentMarkup(tool), oldTag, newTag, 1)
	for _, stream := range []bool{true, false} {
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, markupUpstreamSSE("Starting a safe read.\n" + content), true
		})
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			Upstream: up,
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(codexNamespaceRequest(t, stream))))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "DSML") {
			t.Fatalf("stream=%v invalid: HTTP %d, body=%s", stream, rec.Code, rec.Body)
		}
		if stream {
			if !strings.Contains(rec.Body.String(), `"namespace":"`+codexNamespaceName+`"`) ||
				!strings.Contains(rec.Body.String(), `"type":"function_call"`) ||
				!strings.Contains(rec.Body.String(), "response.completed") {
				t.Fatalf("streamed native function_call missing: %s", rec.Body)
			}
		} else {
			var obj map[string]any
			if json.Unmarshal(rec.Body.Bytes(), &obj) != nil {
				t.Fatal("invalid nonstream response")
			}
			items, _ := obj["output"].([]any)
			found := false
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if item["type"] == "function_call" && item["name"] == codexBareToolName &&
					item["namespace"] == codexNamespaceName {
					found = true
				}
			}
			if !found {
				t.Fatalf("no namespaced function call in %v", obj["output"])
			}
		}
	}
}

func TestResponsesUndeclaredExecFailsInsteadOfCompleting(t *testing.T) {
	// The 13:53 Codex Desktop reply invented the name "exec" with command,
	// workdir and timeout_ms, while 185 *other* tools were declared.
	const pipe = "\uFF5C\uFF5C"
	tag := func(value string) string { return "<" + pipe + "DSML" + pipe + " " + value + ">" }
	end := func(value string) string { return "</" + pipe + "DSML" + pipe + " " + value + ">" }
	block := tag("calls") + "\n" + tag(`invoke name="exec"`) + "\n" +
		tag(`parameter name="command" string="true"`) + "echo hello" + end("parameter") + "\n" +
		tag(`parameter name="workdir" string="true"`) + "C:/test" + end("parameter") + "\n" +
		tag(`parameter name="timeout_ms" string="false"`) + "20000" + end("parameter") + "\n" +
		end("invoke") + "\n" + end("calls")
	for _, stream := range []bool{true, false} {
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, markupUpstreamSSE("I will inspect the files.\n" + block), true
		})
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			Upstream: up,
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(codexNamespaceRequest(t, stream))))
		if strings.Contains(rec.Body.String(), "DSML") {
			t.Fatalf("stream=%v leaked rejected tool markup: %s", stream, rec.Body)
		}
		if stream {
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.failed") ||
				strings.Contains(rec.Body.String(), "response.completed") ||
				!strings.Contains(rec.Body.String(), "invalid_tool_call") {
				t.Fatalf("rejected stream must fail instead of completing: HTTP %d, body=%s", rec.Code, rec.Body)
			}
		} else if rec.Code != 502 || !strings.Contains(rec.Body.String(), "invalid_tool_call") {
			t.Fatalf("rejected nonstream must return HTTP 502: code=%d body=%s", rec.Code, rec.Body)
		}
	}
}
