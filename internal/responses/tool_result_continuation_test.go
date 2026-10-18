package responses

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Codex emits lifecycle developer messages between a function_call item
// and its matching function_call_output in its Responses history. End-to-end
// replay must translate the call to Chat tool_calls and place the matching
// role=tool message DIRECTLY behind it before sending it to DeepSeek.
// Otherwise WorkBuddy returns HTTP 400 code 11148 after the model's first
// successful tool invocation. Observed in a real read-only Codex CLI run on
// 2026-10-18 14:15:12 UTC+8.
func TestCodexToolContinuationWithInterleavedDeveloperMessages(t *testing.T) {
	const callID = "call_1792304099757798900_0"
	request := map[string]any{
		"model":  "deepseek-v4.1-flash",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "Read the README heading."},
			map[string]any{"type": "function_call", "call_id": callID,
				"name":      "exec_command",
				"arguments": `{"cmd":"Get-Content README.md -TotalCount 1"}`},
			map[string]any{"type": "message", "role": "developer",
				"content": "Codex has an active readonly sandbox."},
			map[string]any{"type": "message", "role": "developer",
				"content": "Tool execution hook completed."},
			map[string]any{"type": "function_call_output", "call_id": callID,
				"output": "README heading"},
			map[string]any{"type": "message", "role": "user", "content": "Continue."},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
			},
		}},
	}
	data, _ := json.Marshal(request)
	chat, _, err := TranslateRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	prepared := upstream.PrepareBodyOptWithEfforts(chat, false, nil)
	var decoded map[string]any
	if err := json.Unmarshal(prepared, &decoded); err != nil {
		t.Fatal(err)
	}
	messages, _ := decoded["messages"].([]any)
	if len(messages) != 6 {
		t.Fatalf("unexpected messages count %d", len(messages))
	}
	assistant := messages[1].(map[string]any)
	result := messages[2].(map[string]any)
	if assistant["role"] != "assistant" || result["role"] != "tool" {
		t.Fatalf("tool call and result no longer adjacent: %v, %v",
			assistant["role"], result["role"])
	}
	calls := assistant["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if calls[0].(map[string]any)["id"] != callID ||
		result["tool_call_id"] != callID || fn["name"] != "exec_command" {
		t.Fatalf("tool-call identity changed: %+v %+v", calls, result)
	}
	for _, i := range []int{3, 4} {
		if messages[i].(map[string]any)["role"] != "system" {
			t.Fatalf("developer message was lost or moved into tool pair: %v", messages[i])
		}
	}
	if messages[5].(map[string]any)["role"] != "user" {
		t.Fatalf("user continuation changed: %v", messages[5])
	}
}
