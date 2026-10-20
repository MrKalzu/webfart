package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"webfart/internal/storage"
)

// HandleTerminalText renders messages in formatted plain text for terminal clients.
func HandleTerminalText(w http.ResponseWriter, r *http.Request, thread string, mgr *storage.Manager) {
	var sinceSeq uint64
	if qSince := r.URL.Query().Get("since_seq"); qSince != "" {
		if s, err := strconv.ParseUint(qSince, 10, 64); err == nil {
			sinceSeq = s
		}
	}

	limit := 50
	if qLimit := r.URL.Query().Get("limit"); qLimit != "" {
		if l, err := strconv.Atoi(qLimit); err == nil {
			limit = l
		}
	}

	resp, err := mgr.GetMessages(thread, sinceSeq, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	for _, msg := range resp.Messages {
		t := time.UnixMilli(msg.CreatedAt).UTC()
		timeStr := t.Format("15:04:05")

		pidStr := "unknown"
		if msg.PID != nil {
			pidStr = *msg.PID
		}

		headerLine := fmt.Sprintf("[%s] <%s>", timeStr, pidStr)
		if msg.Nick != nil && *msg.Nick != "" && *msg.Nick != pidStr {
			headerLine += fmt.Sprintf(" %s", *msg.Nick)
		}
		headerLine += fmt.Sprintf(" (seq: %d)", msg.Seq)

		fmt.Fprintln(w, headerLine)

		if msg.Body == nil {
			fmt.Fprintln(w, "  [message deleted]")
		} else {
			lines := strings.Split(*msg.Body, "\n")
			for _, line := range lines {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
		fmt.Fprintln(w)
	}
}
