package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

// guideCodexDeepSeekToolNames addresses a recurring native-tool issue in
// DeepSeek: the model has tools declared by Codex but invents a shell shortcut
// such as "exec" instead of naming a real tool. No unknown tool may be
// executed by the proxy. A small, request-scoped system hint gives the model
// a chance to call a valid tool before the server-side repair/validation.
//
// Do not modify Chat Completions, Messages, non-Codex clients or other models.
// The declared tool schemas and the user's messages remain unchanged.
func guideCodexDeepSeekToolNames(body []byte, r *http.Request, meta *responses.Meta) []byte {
	if meta == nil || len(meta.Tools) == 0 ||
		!strings.Contains(strings.ToLower(r.UserAgent()), "codex") ||
		!strings.Contains(strings.ToLower(meta.Model), "deepseek") {
		return body
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return body
	}
	msgs, ok := chat["messages"].([]any)
	if !ok {
		return body
	}
	flatTools, ok := chat["tools"].([]any)
	if !ok || len(flatTools) == 0 {
		return body
	}
	if choice, ok := chat["tool_choice"].(string); ok && choice == "none" {
		return body
	}
	if choice, ok := chat["tool_choice"].(map[string]any); ok && choice["type"] == "none" {
		return body
	}
	allowed := map[string]bool{}
	for _, raw := range meta.Tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := tool["name"].(string); ok && name != "" {
			allowed[name] = true
		}
	}
	// Use the flattened Chat tool names; namespaces have already been mapped.
	candidates := []string{}
	for _, raw := range flatTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if name == "" || len(name) > 96 {
			continue
		}
		allowed[name] = true
		low := strings.ToLower(name)
		if low == "functions.exec" || low == "exec_command" ||
			low == "chat_on_steroids_core" || strings.HasSuffix(low, "__exec_command") {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	hint := "Tool routing: call only EXACT tool names supplied in the tools list. " +
		"Do not invent aliases for terminal or file access, and follow the selected tool's exact argument schema. " +
		"Do not describe a tool invocation as finished without executing a declared tool."
	if !allowed["exec"] {
		hint += " In particular, 'exec' is NOT a declared function here."
	}
	if len(candidates) > 0 {
		if len(candidates) > 4 {
			candidates = candidates[:4]
		}
		hint += " Relevant declared tool identifiers include: " + strings.Join(candidates, ", ") + "."
	}
	if allowed["functions.exec"] {
		hint += " The freeform 'functions.exec' tool takes JavaScript as its 'input' string; " +
			"do not pass it shell-command parameters."
	}
	insert := 0
	for insert < len(msgs) {
		msg, ok := msgs[insert].(map[string]any)
		if !ok {
			break
		}
		if msg["role"] != "system" && msg["role"] != "developer" {
			break
		}
		insert++
	}
	updated := make([]any, 0, len(msgs)+1)
	updated = append(updated, msgs[:insert]...)
	updated = append(updated, map[string]any{"role": "system", "content": hint})
	updated = append(updated, msgs[insert:]...)
	chat["messages"] = updated
	result, err := json.Marshal(chat)
	if err != nil {
		return body
	}
	return result
}
