package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseWriter sends Server-Sent Events to an http.ResponseWriter.
// It rate-limits "progress" events to avoid flooding the client during
// fast downloads where events fire on every io.Copy chunk.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu      sync.Mutex
	pending any // latest buffered progress event (nil = nothing pending)
	ticker  *time.Ticker
	done    chan struct{}
}

// newSSEWriter creates an sseWriter that flushes buffered progress events
// at most once per interval. Call Close when done.
func newSSEWriter(w http.ResponseWriter, interval time.Duration) *sseWriter {
	flusher, _ := w.(http.Flusher)

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if flusher != nil {
		flusher.Flush()
	}

	s := &sseWriter{
		w:       w,
		flusher: flusher,
		ticker:  time.NewTicker(interval),
		done:    make(chan struct{}),
	}

	go s.loop()

	return s
}

// SendProgress buffers a progress event. Only the latest is kept;
// the ticker flushes it periodically.
func (s *sseWriter) SendProgress(data any) {
	s.mu.Lock()
	s.pending = data
	s.mu.Unlock()
}

// SendEvent sends an event immediately (not rate-limited).
// Use for terminal events like "done" or "error".
func (s *sseWriter) SendEvent(event string, data any) {
	payload, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, payload) //nolint:errcheck

	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Close stops the ticker, flushes any remaining buffered event.
func (s *sseWriter) Close() {
	s.ticker.Stop()
	close(s.done)

	// Flush any remaining buffered event.
	s.mu.Lock()
	p := s.pending
	s.pending = nil
	s.mu.Unlock()

	if p != nil {
		s.SendEvent("progress", p)
	}
}

func (s *sseWriter) loop() {
	for {
		select {
		case <-s.ticker.C:
			s.mu.Lock()
			p := s.pending
			s.pending = nil
			s.mu.Unlock()

			if p != nil {
				s.SendEvent("progress", p)
			}

		case <-s.done:
			return
		}
	}
}
