package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
	"webfart/internal/identity"
	"webfart/internal/storage"
)

// SSEManager manages client connections and stream limits.
type SSEManager struct {
	mu           sync.Mutex
	ipStreams    map[string]int
	threadCounts map[string]int

	maxPerIP     int
	maxPerThread int

	quit     chan struct{}
	quitOnce sync.Once
}

// NewSSEManager creates an SSE manager with concurrency caps.
func NewSSEManager(maxPerIP, maxPerThread int) *SSEManager {
	if maxPerIP <= 0 {
		maxPerIP = 10
	}
	if maxPerThread <= 0 {
		maxPerThread = 200
	}
	return &SSEManager{
		ipStreams:    make(map[string]int),
		threadCounts: make(map[string]int),
		maxPerIP:     maxPerIP,
		maxPerThread: maxPerThread,
		quit:         make(chan struct{}),
	}
}

// Close signals all active SSE connections to close immediately.
func (sm *SSEManager) Close() {
	sm.quitOnce.Do(func() {
		close(sm.quit)
	})
}

func (sm *SSEManager) acquire(clientIP, thread string) bool {
	truncIP := identity.TruncateIP(clientIP)
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.ipStreams[truncIP] >= sm.maxPerIP {
		return false
	}
	if sm.threadCounts[thread] >= sm.maxPerThread {
		return false
	}

	sm.ipStreams[truncIP]++
	sm.threadCounts[thread]++
	return true
}

func (sm *SSEManager) release(clientIP, thread string) {
	truncIP := identity.TruncateIP(clientIP)
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ipStreams[truncIP]--
	if sm.ipStreams[truncIP] <= 0 {
		delete(sm.ipStreams, truncIP)
	}

	sm.threadCounts[thread]--
	if sm.threadCounts[thread] <= 0 {
		delete(sm.threadCounts, thread)
	}
}

// HandleSSE serves the Server-Sent Events stream for a thread.
func (sm *SSEManager) HandleSSE(w http.ResponseWriter, r *http.Request, thread string, mgr *storage.Manager) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	clientIP := GetClientIP(r)
	if !sm.acquire(clientIP, thread) {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer sm.release(clientIP, thread)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Determine starting seq for catchup
	var sinceSeq uint64
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if s, err := strconv.ParseUint(lastID, 10, 64); err == nil {
			sinceSeq = s
		}
	} else if qSince := r.URL.Query().Get("since_seq"); qSince != "" {
		if s, err := strconv.ParseUint(qSince, 10, 64); err == nil {
			sinceSeq = s
		}
	}

	// Send initial catchup messages if requested
	if sinceSeq > 0 {
		resp, err := mgr.GetMessages(thread, sinceSeq, 100)
		if err == nil {
			for _, m := range resp.Messages {
				sendSSEMessage(w, flusher, m)
				sinceSeq = m.Seq
			}
		}
	}

	// Subscribe to live messages
	msgChan, cancel, err := mgr.Subscribe(thread)
	if err != nil {
		http.Error(w, "thread subscription failed", http.StatusInternalServerError)
		return
	}
	defer cancel()

	// Flush headers immediately
	flusher.Flush()

	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()

	notify := r.Context().Done()

	for {
		select {
		case <-notify:
			return

		case <-sm.quit:
			return

		case <-heartbeatTicker.C:
			// SSE comment ping
			_, err := fmt.Fprintf(w, ": ping\n\n")
			if err != nil {
				return
			}
			flusher.Flush()

		case msg, ok := <-msgChan:
			if !ok {
				return
			}
			if msg.Seq <= sinceSeq {
				continue
			}
			sinceSeq = msg.Seq
			if err := sendSSEMessage(w, flusher, msg); err != nil {
				return
			}
		}
	}
}

func sendSSEMessage(w http.ResponseWriter, flusher http.Flusher, msg storage.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", msg.Seq, string(data))
	if err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
