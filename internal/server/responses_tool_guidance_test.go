package server

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

func TestCodexDeepSeekToolGuidanceOnlyWhenRelevant(t *testing.T) {
	chat := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "Original system instructions"},
			map[string]any{"role": "user", "content": "Read a documentation file"},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "functions.exec",
			"parameters": map[string]any{"type": "object"},
		}},
	}
	original, _ := json.Marshal(chat)
	meta := &responses.Meta{
		Model: "deepseek-v4.1-flash",
		Tools: []any{map[string]any{"type": "custom", "name": "functions.exec"}},
	}
	for _, tc := range []struct {
		userAgent string
		model     string
		wantHint  bool
	}{
		{"Codex Desktop/0.163.0", "deepseek-v4.1-flash", true},
		{"Other Coding Agent", "deepseek-v4.1-flash", false},
		{"Codex Desktop/0.163.0", "gpt-6", false},
	} {
		r := httptest.NewRequest("POST", "/v1/responses", nil)
		r.Header.Set("User-Agent", tc.userAgent)
		m := *meta
		m.Model = tc.model
		out := guideCodexDeepSeekToolNames(original, r, &m)
		if !tc.wantHint && string(out) != string(original) {
			t.Fatalf("unrelated caller must not change: %s", out)
		}
		if !tc.wantHint {
			continue
		}
		var modified map[string]any
		if err := json.Unmarshal(out, &modified); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(chat["tools"], modified["tools"]) {
			t.Fatalf("declared tools changed: %v", modified["tools"])
		}
		msgs, _ := modified["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("expected original system + hint + user: %v", msgs)
		}
		if !reflect.DeepEqual(msgs[0], chat["messages"].([]any)[0]) ||
			!reflect.DeepEqual(msgs[2], chat["messages"].([]any)[1]) {
			t.Fatalf("original instructions or user message modified: %v", msgs)
		}
		hint := msgs[1].(map[string]any)["content"].(string)
		if !strings.Contains(hint, "functions.exec") || !strings.Contains(hint, "'exec' is NOT a declared function") ||
			!strings.Contains(hint, "input") {
			t.Fatalf("guidance should clarify known executor semantics: %s", hint)
		}
	}
}

func TestCodexToolGuidanceNoFlattenedToolsDoesNotPanic(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("User-Agent", "Codex Desktop/0.163.0")
	meta := &responses.Meta{
		Model: "deepseek-v4.1-flash",
		Tools: []any{map[string]any{"type": "web_search_preview"}},
	}
	in := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out := guideCodexDeepSeekToolNames(in, r, meta)
	if !json.Valid(out) {
		t.Fatal("guidance produced invalid JSON")
	}
}
