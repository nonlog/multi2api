package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const reasonPipe = "\uFF5C\uFF5C"

func dsmlReasoningCall(name string) string {
	tag := func(s string) string { return "<" + reasonPipe + "DSML" + reasonPipe + " " + s + ">" }
	end := func(s string) string { return "</" + reasonPipe + "DSML" + reasonPipe + " " + s + ">" }
	return tag("calls") + "\n" + tag(`invoke name="`+name+`"`) + "\n" +
		tag(`parameter name="session_id" string="false"`) + "76905" + end("parameter") + "\n" +
		tag(`parameter name="chars" string="true"`) + "" + end("parameter") + "\n" +
		tag(`parameter name="yield_time_ms" string="false"`) + "1000" + end("parameter") + "\n" +
		end("invoke") + "\n" + end("calls")
}

func reasoningChatSSE(t *testing.T, parts []string, content string) string {
	t.Helper()
	var out strings.Builder
	for _, part := range parts {
		frame, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"reasoning_content": part},
			}},
		})
		fmt.Fprintf(&out, "data: %s\n\n", frame)
	}
	if content != "" {
		frame, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": content},
			}},
		})
		fmt.Fprintf(&out, "data: %s\n\n", frame)
	}
	out.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	out.WriteString("data: [DONE]\n\n")
	return out.String()
}

func reasoningEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) == nil {
			events = append(events, e)
		}
	}
	return events
}

func TestReasoningDSMLRealCodexWriteStdinRecovery(t *testing.T) {
	// In a production Codex Desktop turn (2026-10-20 20:24), the model
	// emitted this DSML block in its *reasoning summary*, followed by a
	// progress-only assistant message and an erroneous completed turn.
	meta := &Meta{Model: "deepseek-v4.1-flash", Tools: []any{
		map[string]any{"type": "function", "name": "write_stdin",
			"parameters": map[string]any{"type": "object"}},
	}}
	markup := dsmlReasoningCall("write_stdin")
	reason := "Checking the extracted installer and polling the process.\n\n" + markup
	// Fragment the sentinel and invoke name across SSE frames.
	parts := []string{reason[:15], reason[15:64], reason[64:90], reason[90:]}
	var out bytes.Buffer
	if err := Stream(&out, strings.NewReader(reasoningChatSSE(t, parts,
		"I will continue inspecting the extracted package.")), meta, meta.Model); err != nil {
		t.Fatal(err)
	}
	events := reasoningEvents(t, out.String())
	var added, done map[string]any
	completed := 0
	for _, e := range events {
		if e["type"] == "response.completed" {
			completed++
		}
		if e["type"] != "response.output_item.added" && e["type"] != "response.output_item.done" {
			continue
		}
		item, _ := e["item"].(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		if e["type"] == "response.output_item.added" {
			added = item
		} else {
			done = item
		}
	}
	if completed != 1 || added == nil || done == nil ||
		added["name"] != "write_stdin" || done["name"] != "write_stdin" {
		t.Fatalf("native Codex function_call missing or incomplete: added=%v done=%v completed=%d", added, done, completed)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(done["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if args["session_id"] != float64(76905) || args["chars"] != "" ||
		args["yield_time_ms"] != float64(1000) {
		t.Fatalf("recovered tool parameters changed: %v", args)
	}
	if strings.Contains(out.String(), "DSML") {
		t.Fatalf("raw DSML leaked into Codex reasoning instead of being recovered")
	}
	if !strings.Contains(out.String(), "Checking the extracted installer") {
		t.Fatalf("legitimate reasoning narration was dropped")
	}
}

func TestReasoningDSMLNamespacedCustomToolAndRejection(t *testing.T) {
	meta := &Meta{
		Model: "deepseek-v4.1-flash",
		Tools: []any{map[string]any{
			"type": "namespace", "name": "mcp__codex_apps", "tools": []any{
				map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}},
			},
		}},
		CustomTools:  map[string]bool{"exec": true},
		NamespaceMap: map[string]string{"exec": "mcp__codex_apps"},
	}
	markup := dsmlReasoningCall("mcp__codex_apps__exec")
	var out bytes.Buffer
	if err := Stream(&out, strings.NewReader(reasoningChatSSE(t, []string{markup}, "")), meta, meta.Model); err != nil {
		t.Fatal(err)
	}
	events := reasoningEvents(t, out.String())
	found := false
	for _, e := range events {
		item, _ := e["item"].(map[string]any)
		if e["type"] == "response.output_item.done" && item["type"] == "custom_tool_call" {
			found = item["name"] == "exec" && item["namespace"] == "mcp__codex_apps"
		}
	}
	if !found {
		t.Fatalf("namespace or custom tool type was lost")
	}
	for _, unknown := range []string{"delete_everything", "wrong_namespace__exec"} {
		out.Reset()
		err := Stream(&out, strings.NewReader(reasoningChatSSE(t, []string{dsmlReasoningCall(unknown)}, "")), meta, meta.Model)
		if err == nil || !strings.Contains(out.String(), "response.failed") ||
			strings.Contains(out.String(), "response.completed") {
			t.Fatalf("reasoning tool %q bypassed allowlist or completed silently: %v", unknown, err)
		}
	}
}

func TestRepairReasoningCallsNonStreamingAndNoDeclaredTools(t *testing.T) {
	meta := &Meta{Model: "deepseek-v4.1-flash", Tools: []any{
		map[string]any{"type": "function", "name": "write_stdin"},
	}}
	chat := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop",
		"message": map[string]any{
			"role": "assistant", "content": "I will inspect the installer",
			"reasoning_content": "Checking.\n" + dsmlReasoningCall("write_stdin"),
		},
	}}}
	got, err := RepairReasoningCallsInChat(chat, meta)
	if err != nil || got != 1 {
		t.Fatalf("nonstream recovered %d err=%v", got, err)
	}
	response := FromChat(chat, meta, meta.Model)
	items, _ := response["output"].([]any)
	seen := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" && item["name"] == "write_stdin" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("missing nonstream native tool call: %v", items)
	}
	if _, err := RepairReasoningCallsInChat(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"reasoning_content": dsmlReasoningCall("fake_tool")},
	}}}, meta); !errors.Is(err, ErrReasoningToolMarkup) {
		t.Fatalf("undeclared nonstream reasoning tool should fail: %v", err)
	}
	var out bytes.Buffer
	if err := Stream(&out, strings.NewReader(reasoningChatSSE(t,
		[]string{dsmlReasoningCall("write_stdin")}, "Response only")), nil, meta.Model); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "response.completed") {
		t.Fatal("no-tools requests must remain unaffected")
	}
}
