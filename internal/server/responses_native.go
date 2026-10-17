package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

// responses is a native Responses API entry point. Inbound requests are converted
// to the Chat shape understood by WorkBuddy, then routed through the CURRENT chat
// pipeline (including account rotation, cooldown, streaming repair and telemetry).
// PR #109 supplies the bidirectional Responses mappings; this wrapper deliberately
// avoids copying or replacing the newer chat pipeline.
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, meta, err := responses.TranslateRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(chat, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))

	model := meta.Model
	if _, bare := resolveModel(model); bare != "" {
		model = bare
	}
	if !peek.Stream {
		// Only non-stream output is buffered. All requests still go through the
		// existing chat handler, including its latest account and error policies.
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, r2)
		if rec.Code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		var chatResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "invalid chat response: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, responses.FromChat(chatResp, meta, model))
		return
	}

	// Convert the live, already normalized Chat SSE into native Responses SSE
	// without dropping the original StreamHint processing (usage, error frames,
	// DeepSeek-style native tool-call repair and successful-account bookkeeping).
	pr, pw := io.Pipe()
	capture := &responsesPipeWriter{
		headers: make(http.Header),
		status:  make(chan int, 1),
		writer:  pw,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer capture.finish()
		h.chatCompletions(capture, r2)
	}()
	status := <-capture.status
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.Copy(w, pr)
		_ = pr.Close()
		<-done
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	streamErr := responses.Stream(w, pr, meta, model)
	// If the client disconnects or translation fails, unblock the upstream
	// producer, allowing the existing handler to record interruption correctly.
	_ = pr.Close()
	<-done
	if streamErr != nil {
		log.Printf("WARN: [server] responses stream conversion failed: %v", streamErr)
	}
}

// responsesPipeWriter is the minimal ResponseWriter/Flusher needed by StreamHint.
// It streams bytes into io.Pipe; the status channel lets the outer handler
// distinguish pre-stream JSON failures from a successful SSE stream.
type responsesPipeWriter struct {
	headers http.Header
	status  chan int
	once    sync.Once
	writer  *io.PipeWriter
}

func (w *responsesPipeWriter) Header() http.Header { return w.headers }

func (w *responsesPipeWriter) WriteHeader(status int) {
	w.once.Do(func() { w.status <- status })
}

func (w *responsesPipeWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}

func (w *responsesPipeWriter) Flush() { w.WriteHeader(http.StatusOK) }

func (w *responsesPipeWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.writer.Close()
}
