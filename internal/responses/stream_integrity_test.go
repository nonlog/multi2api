package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesFragmentedContentDSMLIsNativeToolCall(t *testing.T) {
	markup := dsmlReasoningCall("write_stdin")
	text := "Polling the running task.\n" + markup
	chars := []rune(text)
	var sse strings.Builder
	for i := 0; i < len(chars); i += 8 {
		end := i + 8
		if end > len(chars) { end = len(chars) }
		frame, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": string(chars[i:end])},
		}}})
		if err != nil { t.Fatal(err) }
		sse.WriteString("data: " + string(frame) + "\n\n")
	}
	sse.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	sse.WriteString("data: [DONE]\n\n")
	var out bytes.Buffer
	meta := &Meta{Tools: []any{map[string]any{"type":"function", "name":"write_stdin", "parameters":map[string]any{"type":"object"}}}}
	if err := Stream(&out, strings.NewReader(sse.String()), meta, "deepseek-v4.1-flash"); err != nil { t.Fatal(err) }
	got := out.String()
	for _, want := range []string{`"type":"function_call"`, `"name":"write_stdin"`, "event: response.function_call_arguments.done", "event: response.completed"} {
		if !strings.Contains(got, want) { t.Fatalf("missing %q from fragmented content conversion", want) }
	}
	if strings.Contains(got,"DSML") { t.Fatal("raw native DSML leaked into client message text") }
	if !strings.Contains(got,"Polling the running task") { t.Fatal("non-tool text was dropped") }
}

func TestResponsesUndeclaredFragmentedContentFails(t *testing.T) {
	markup := dsmlReasoningCall("delete_everything")
	chars := []rune(markup)
	var sse strings.Builder
	for i := 0; i < len(chars); i += 9 {
		end := i+9; if end > len(chars) { end=len(chars) }
		b,_ := json.Marshal(map[string]any{"choices":[]any{map[string]any{"delta":map[string]any{"content":string(chars[i:end])}}}})
		sse.WriteString("data: " + string(b) + "\n\n")
	}
	sse.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	var out bytes.Buffer
	meta := &Meta{Tools: []any{map[string]any{"type":"function", "name":"write_stdin"}}}
	if err := Stream(&out, strings.NewReader(sse.String()), meta, "deepseek-v4.1-flash"); err == nil { t.Fatal("undeclared tool incorrectly succeeded") }
	if !strings.Contains(out.String(),"response.failed") || strings.Contains(out.String(),"response.completed") { t.Fatal("undeclared tool falsely completed") }
}

func TestResponsesTruncatedAfterContentNeverCompletes(t *testing.T) {
	// A provider may end HTTP 200 with a TCP EOF or even [DONE] but no
	// finish_reason. Neither is proof that a tool call finished.
	for _, suffix := range []string{"", "data: [DONE]\n\n"} {
		chat := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+suffix
		var out bytes.Buffer
		if err := Stream(&out, strings.NewReader(chat), &Meta{}, "glm-5.3"); err == nil { t.Fatal("truncated generation succeeded") }
		if !strings.Contains(out.String(),`"code":"upstream_truncated"`) ||
			strings.Contains(out.String(),"response.completed") || !strings.Contains(out.String(),"response.failed") {
			t.Fatalf("invalid truncated stream terminal event: %s",out.String())
		}
	}
}

func TestResponsesLengthFinishEmitsIncomplete(t *testing.T) {
	chat := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	var out bytes.Buffer
	if err := Stream(&out,strings.NewReader(chat),&Meta{},"glm-5.3"); err != nil { t.Fatal(err) }
	if !strings.Contains(out.String(),"event: response.incomplete") || strings.Contains(out.String(),"event: response.completed") {
		t.Fatalf("length limit emitted wrong terminal event: %s",out.String())
	}
}
