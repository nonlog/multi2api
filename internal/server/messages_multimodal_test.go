package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Real Pi 2026-10-18 incident: read returned a text block followed by
// image/png with 694,976 Base64 characters. The previous adapter flattened
// the PNG into a TEXT tool result, inflating the next input to 525K tokens.
// The image must remain a multimodal part, regardless of data length.
func TestMessagesToolResultPNGNotFlattenedToText(t *testing.T) {
	base64Data := strings.Repeat("QUJD", 173744) // same encoded length as incident
	input, err := json.Marshal(map[string]any{
		"model":      "deepseek-v4.1-flash",
		"max_tokens": 2048,
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_read", "name": "read",
					"input": map[string]any{"path": "image.png"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_read",
					"content": []any{
						map[string]any{"type": "text", "text": "Read image file, size 690 KB"},
						map[string]any{"type": "image", "source": map[string]any{
							"type": "base64", "media_type": "image/png", "data": base64Data,
						}},
					},
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	converted, err := messagesToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("wanted assistant + tool, got %d messages", len(msgs))
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_read" {
		t.Fatalf("tool identity changed: role=%v call_id=%v", tool["role"], tool["tool_call_id"])
	}
	parts, ok := tool["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("tool content must be multimodal text/image, got type %T", tool["content"])
	}
	first := parts[0].(map[string]any)
	image := parts[1].(map[string]any)
	if first["type"] != "text" || first["text"] != "Read image file, size 690 KB" ||
		image["type"] != "image_url" {
		t.Fatalf("tool parts differ: first_type=%v image_type=%v", first["type"], image["type"])
	}
	url := image["image_url"].(map[string]any)["url"].(string)
	if url != "data:image/png;base64,"+base64Data {
		t.Fatalf("image URL changed: bytes=%d", len(url))
	}
	// The outbound WorkBuddy sanitizer and image normalization must not
	// collapse image_url back into a text data URI.
	prepared := upstream.PrepareBodyOptWithEfforts(converted, true, nil)
	if err := json.Unmarshal(prepared, &chat); err != nil {
		t.Fatal(err)
	}
	tool = chat["messages"].([]any)[1].(map[string]any)
	parts = tool["content"].([]any)
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("outbound payload lost image type")
	}
	for _, raw := range parts {
		part := raw.(map[string]any)
		if text, ok := part["text"].(string); ok && strings.Contains(text, base64Data[:1024]) {
			t.Fatal("image Base64 leaked into model text")
		}
	}
}

func TestMessagesToolResultMixedImageErrorAndTextCompatibility(t *testing.T) {
	data := []byte(`{"model":"glm-5.2","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[
				{"type":"text","text":"partial result"},
				{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}
			]}
		]}
	]}`)
	out, err := messagesToChat(data)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal(out, &obj) != nil {
		t.Fatal("invalid JSON")
	}
	blocks := obj["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["text"] != "error: " ||
		blocks[1].(map[string]any)["text"] != "partial result" ||
		blocks[2].(map[string]any)["image_url"].(map[string]any)["url"] != "https://example.test/a.png" {
		t.Fatalf("mixed result lost ordering or error marker: count=%d", len(blocks))
	}
	if got := toolResultChatContent([]any{map[string]any{"type": "text", "text": "plain text"}}, false); got != "plain text" {
		t.Fatalf("text-only tool results should remain strings, got %T %v", got, got)
	}
	if got := toolResultContent([]any{map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": strings.Repeat("A", 100000),
	}}}, false); got != "[image attachment]" {
		t.Fatalf("text fallback must never expose image Base64: %d bytes", len(got))
	}
}

func TestMessagesMultimodalToolResultFullHandler(t *testing.T) {
	seen := false
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upstream body: %v", err)
			}
			var req map[string]any
			if json.Unmarshal(body, &req) != nil {
				t.Error("upstream request is not JSON")
			} else if messages, ok := req["messages"].([]any); ok && len(messages) >= 2 {
				tool := messages[len(messages)-1].(map[string]any)
				parts, ok := tool["content"].([]any)
				if ok && tool["role"] == "tool" && len(parts) == 2 &&
					parts[1].(map[string]any)["type"] == "image_url" {
					seen = true
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	input := `{"model":"glm-5.2","stream":true,"max_tokens":100,
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
		{"type":"text","text":"image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(input)))
	if rec.Code != 200 || !seen {
		t.Fatalf("server did not forward multimodal tool image: status=%d seen=%t", rec.Code, seen)
	}
	if !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatal("Anthropic streaming end event missing")
	}
}
