package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/multi2api/internal/auth"
	"github.com/nonlog/multi2api/internal/upstream"
)

func alphaSearchFixture(t *testing.T) (*Handler, *int) {
	t.Helper()
	calls := new(int)
	client := &upstream.Client{
		ChatBaseCN:    "https://upstream.example",
		BillingBaseCN: "https://upstream.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/agenttool/v1/search") {
				t.Errorf("wrong upstream path: %s", r.URL.Path)
			}
			*calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(searchOKBody)),
			}, nil
		})},
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		APIKey:   "test-search-secret",
	})
	return h, calls
}

func TestCodexAlphaSearchRoutesAndResults(t *testing.T) {
	for _, path := range []string{"/alpha/search", "/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		h, calls := alphaSearchFixture(t)
		input := map[string]any{
			"id": "test-codex-session", "model": "global:default-model",
			"prompt_cache_key": "valid-responses-only-extra",
			"commands":         map[string]any{"search_query": []any{map[string]any{"q": "golang documentation"}}},
		}
		body, _ := json.Marshal(input)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer test-search-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path=%s code=%d body=%s", path, rec.Code, rec.Body)
		}
		var resp struct {
			Output          string
			EncryptedOutput *string
			Results         []map[string]any
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if *calls != 1 || len(resp.Results) != 1 || resp.Results[0]["type"] != "text_result" ||
			resp.Results[0]["url"] != "https://go.dev" ||
			!strings.Contains(resp.Output, "https://go.dev") {
			t.Fatalf("%s response=%+v calls=%d", path, resp, *calls)
		}
	}
}

func TestCodexSearchAuthAndValidation(t *testing.T) {
	h, _ := alphaSearchFixture(t)
	cases := []struct {
		body, auth string
		want       int
	}{
		{"{", "Bearer test-search-secret", 400},
		{"{}", "Bearer test-search-secret", 400},
		{"{}", "", 401},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/alpha/search", strings.NewReader(tc.body))
		req.Header.Set("Authorization", tc.auth)
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("body=%q code=%d want=%d", tc.body, rec.Code, tc.want)
		}
	}
}

func TestCodexSearchDomainsAndSSRF(t *testing.T) {
	if !isPermittedSearchDomain("https://docs.go.dev/a", []string{"go.dev"}, nil) ||
		isPermittedSearchDomain("https://fakego.dev", []string{"go.dev"}, nil) ||
		isPermittedSearchDomain("https://docs.go.dev", nil, []string{"go.dev"}) {
		t.Fatal("incorrect domain filtering")
	}
	for _, u := range []string{"http://localhost", "http://127.0.0.1", "http://10.0.0.1",
		"http://169.254.169.254", "file:///etc/passwd", "http://example.com:8080",
		"https://[::1]", "http://a.internal"} {
		parsed, err := safeSearchURL(u)
		if err == nil && parsed != nil {
			// literal private IPs are rejected at dial time, not by the initial URL parser.
			if ip := net.ParseIP(parsed.Hostname()); ip == nil || publicSearchIP(ip) {
				t.Errorf("unsafe navigation URL accepted %s", u)
			}
		}
	}
	if publicSearchIP(net.ParseIP("127.0.0.1")) || publicSearchIP(net.ParseIP("192.168.1.5")) ||
		publicSearchIP(net.ParseIP("169.254.169.254")) || publicSearchIP(net.ParseIP("100.64.0.1")) {
		t.Fatal("private network bypass")
	}
	p := parseCodexHTML("<html><head><title>Example</title></head><body><h1>Title</h1><a href='/docs'>Docs</a></body></html>", "https://example.com")
	if p.Title != "Example" || len(p.Links) != 1 || p.Links[0] != "https://example.com/docs" {
		t.Fatalf("invalid HTML parsing: %+v", p)
	}
}

func fakeNativeSearchChatChunk(useTool bool) string {
	delta := map[string]any{"role": "assistant"}
	finish := "stop"
	if useTool {
		finish = "tool_calls"
		delta["tool_calls"] = []any{map[string]any{
			"index": 0, "id": "call_search", "type": "function",
			"function": map[string]any{"name": nativeSearchFunction, "arguments": mustJSON(map[string]any{"query": "golang docs"})},
		}}
	} else {
		delta["content"] = "Summary: Go documentation is at https://go.dev."
	}
	one, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "glm-5.2",
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
	last, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "glm-5.2",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 6, "completion_tokens": 7, "total_tokens": 13},
	})
	return "data: " + string(one) + "\n\n" + "data: " + string(last) + "\n\n" + "data: [DONE]\n\n"
}

func nativeSearchFixture(t *testing.T) (*Handler, *int, *int) {
	t.Helper()
	searchCalls, chatCalls := new(int), new(int)
	client := &upstream.Client{
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var content string
			contentType := "text/event-stream"
			if strings.HasSuffix(r.URL.Path, "/agenttool/v1/search") {
				*searchCalls++
				content = searchOKBody
				contentType = "application/json"
			} else {
				*chatCalls++
				data, _ := io.ReadAll(r.Body)
				var body map[string]any
				_ = json.Unmarshal(data, &body)
				toolReply := false
				for _, raw := range body["messages"].([]any) {
					m, _ := raw.(map[string]any)
					if m["role"] == "tool" {
						toolReply = true
					}
				}
				content = fakeNativeSearchChatChunk(!toolReply)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{contentType}},
				Body:       io.NopCloser(strings.NewReader(content)),
			}, nil
		})},
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client, APIKey: "test-search-secret"})
	return h, searchCalls, chatCalls
}

func nativeSearchRequestJSON(stream bool) string {
	return mustJSON(map[string]any{
		"model":      "glm-5.2",
		"max_tokens": 200,
		"stream":     stream,
		"tools":      []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 2}},
		"messages":   []any{map[string]any{"role": "user", "content": "Find Go official documentation"}},
	})
}

func TestClaudeNativeWebSearchNonStream(t *testing.T) {
	h, searchCalls, chatCalls := nativeSearchFixture(t)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(nativeSearchRequestJSON(false)))
	req.Header.Set("x-api-key", "test-search-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d: %s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	blocks, _ := obj["content"].([]any)
	types := []string{}
	for _, raw := range blocks {
		if b, ok := raw.(map[string]any); ok {
			types = append(types, asString(b["type"]))
		}
	}
	if !containsStr(types, "server_tool_use") || !containsStr(types, "web_search_tool_result") ||
		!containsStr(types, "text") || obj["stop_reason"] != "end_turn" || *searchCalls != 1 || *chatCalls != 2 {
		t.Fatalf("types=%v searches=%d chats=%d response=%s", types, *searchCalls, *chatCalls, rec.Body)
	}
	usage := obj["usage"].(map[string]any)
	count := usage["server_tool_use"].(map[string]any)["web_search_requests"]
	if count != float64(1) {
		t.Fatalf("wrong search usage: %v", usage)
	}
}

func TestClaudeNativeWebSearchSSE(t *testing.T) {
	h, _, _ := nativeSearchFixture(t)
	req := httptest.NewRequest("POST", "/messages", strings.NewReader(nativeSearchRequestJSON(true)))
	req.Header.Set("x-api-key", "test-search-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("code=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
	events := sseTypes(rec.Body.String())
	for _, ev := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !containsStr(events, ev) {
			t.Errorf("missing SSE event %s in %v", ev, events)
		}
	}
	if !strings.Contains(rec.Body.String(), "web_search_tool_result") || !strings.Contains(rec.Body.String(), "server_tool_use") {
		t.Fatalf("missing tool event types: %s", rec.Body)
	}
}

func TestClaudeNativeSearchRejectInvalidMaxUses(t *testing.T) {
	h, searchCalls, chatCalls := nativeSearchFixture(t)
	body := mustJSON(map[string]any{
		"model":    "glm-5.2",
		"tools":    []any{map[string]any{"type": "web_search_20250305", "max_uses": 0}},
		"messages": []any{},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "test-search-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || *searchCalls != 0 || *chatCalls != 0 {
		t.Fatalf("code=%d search=%d chat=%d", rec.Code, *searchCalls, *chatCalls)
	}
}
