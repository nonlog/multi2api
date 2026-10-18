package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func writeNativeSearchSSE(w http.ResponseWriter, result map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	emit := func(name string, value map[string]any) {
		value["type"] = name
		data, err := json.Marshal(value)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	usage, _ := result["usage"].(map[string]any)
	startUsage := map[string]any{"input_tokens": usage["input_tokens"], "output_tokens": 0}
	emit("message_start", map[string]any{"message": map[string]any{
		"id": result["id"], "type": "message", "role": "assistant",
		"model": result["model"], "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage,
	}})
	content, _ := result["content"].([]any)
	for idx, raw := range content {
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var block map[string]any
		if json.Unmarshal(b, &block) != nil {
			continue
		}
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			text, _ := block["text"].(string)
			initial := map[string]any{"type": "text", "text": ""}
			if citations, ok := block["citations"]; ok {
				initial["citations"] = citations
			}
			emit("content_block_start", map[string]any{"index": idx, "content_block": initial})
			if text != "" {
				emit("content_block_delta", map[string]any{"index": idx, "delta": map[string]any{"type": "text_delta", "text": text}})
			}
		case "server_tool_use", "tool_use":
			input := block["input"]
			initial := map[string]any{"type": typ, "id": block["id"], "name": block["name"], "input": map[string]any{}}
			emit("content_block_start", map[string]any{"index": idx, "content_block": initial})
			if input != nil {
				encoded, err := json.Marshal(input)
				if err == nil {
					emit("content_block_delta", map[string]any{"index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}})
				}
			}
		default:
			// web_search_tool_result contains the complete results in its
			// content_block_start, exactly as Anthropic's streaming protocol.
			emit("content_block_start", map[string]any{"index": idx, "content_block": block})
		}
		emit("content_block_stop", map[string]any{"index": idx})
	}
	emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": result["stop_reason"], "stop_sequence": nil},
		"usage": map[string]any{
			"output_tokens":   usage["output_tokens"],
			"server_tool_use": usage["server_tool_use"],
		},
	})
	emit("message_stop", map[string]any{})
}
