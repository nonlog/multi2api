package responses

import (
	"errors"
	"strings"

	"github.com/nonlog/multi2api/internal/upstream"
)

// ErrReasoningToolMarkup is returned when an otherwise successful Chat
// completion contains an undeclared or malformed native tool invocation in
// reasoning_content. Without this guard, Codex treats the turn as completed
// despite the model attempting a tool call it never received.
var ErrReasoningToolMarkup = errors.New("responses: malformed or undeclared tool invocation in reasoning")

// reasoningMarkupRepair uses only callable tools declared by the *current*
// Responses request. Never recover arbitrary tool names from model thoughts,
// earlier turns, or user-provided text.
func reasoningMarkupRepair(meta *Meta) *upstream.MarkupRepair {
	if meta == nil || len(meta.Tools) == 0 {
		return nil
	}
	if choice, ok := meta.ToolChoice.(string); ok && choice == "none" {
		return nil
	}
	if choice, ok := meta.ToolChoice.(map[string]any); ok && choice["type"] == "none" {
		return nil
	}
	flat, _ := ExpandNamespaceTools(meta.Tools)
	chatTools, _ := toolsForChat(flat)
	allowed := make(map[string]bool, len(chatTools))
	for _, raw := range chatTools {
		tool, ok := raw.(map[string]any)
		if !ok || !strings.EqualFold(stringOf(tool["type"]), "function") {
			continue
		}
		name := stringOf(tool["name"])
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name = stringOf(fn["name"])
			}
		}
		if name != "" {
			allowed[name] = true
		}
	}
	for bare, ns := range meta.NamespaceMap {
		if allowed[bare] && ns != "" {
			allowed[ns+"__"+bare] = true
			allowed[ns+"::"+bare] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	// Strict mode: a model cannot gain permission by inventing tool names.
	return upstream.NewMarkupRepair(allowed, false)
}

func reasonMarkupTailInvalid(repair *upstream.MarkupRepair, tail string) bool {
	if repair == nil {
		return false
	}
	// An incomplete calls block at EOF must not silently complete the turn.
	return repair.Rejected() > 0 ||
		strings.Contains(tail, "<\uFF5C\uFF5CDSML\uFF5C\uFF5C calls>")
}

// RepairReasoningCallsInChat recovers native DSML invocations placed in the
// reasoning channel on non-streaming Responses. Structured Chat tool_calls
// take precedence; plain reasoning remains untouched unless a fully formed,
// strictly declared invocation is present.
func RepairReasoningCallsInChat(chat map[string]any, meta *Meta) (int, error) {
	repair := reasoningMarkupRepair(meta)
	if repair == nil {
		return 0, nil
	}
	choice := firstChoice(chat)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return 0, nil
	}
	if old, ok := message["tool_calls"].([]any); ok && len(old) > 0 {
		return 0, nil
	}
	full := stringOf(message["reasoning_content"])
	if full == "" {
		return 0, nil
	}
	text, calls := repair.Feed(full)
	tail := repair.Flush()
	if reasonMarkupTailInvalid(repair, tail) {
		return 0, ErrReasoningToolMarkup
	}
	if len(calls) == 0 {
		return 0, nil
	}
	message["reasoning_content"] = text + tail
	toolCalls := make([]any, 0, len(calls))
	for _, call := range calls {
		name, _ := ResolveNamespacedName(call.Name, meta.NamespaceMap)
		toolCalls = append(toolCalls, map[string]any{
			"id":   newID("call_"),
			"type": "function",
			"function": map[string]any{
				"name": name, "arguments": call.Arguments,
			},
		})
	}
	message["tool_calls"] = toolCalls
	choice["finish_reason"] = "tool_calls"
	return len(calls), nil
}
