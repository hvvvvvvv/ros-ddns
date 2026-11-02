package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
)

func registerStreams(mux *http.ServeMux, logs LogStore, notifications NotificationStore, epoch string) {
	setEpoch := func(w http.ResponseWriter) {
		if epoch != "" {
			w.Header().Set("X-Ros-DDNS-Started-At", epoch)
		}
	}
	mux.HandleFunc("GET /api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		after, ok := queryAfter(w, query)
		if !ok {
			return
		}
		limit, ok := queryLimit(w, query)
		if !ok {
			return
		}
		// History defaults to info to match the UI; streams include all levels.
		level := slog.LevelInfo
		switch query.Get("level") {
		case "", "info":
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		default:
			writeError(w, http.StatusBadRequest, "level must be debug, info, warn or error")
			return
		}
		if logs == nil {
			writeError(w, http.StatusServiceUnavailable, "logs unavailable")
			return
		}
		entries, next := logs.History(after, limit, level)
		if entries == nil {
			entries = []logging.Entry{}
		}
		writeJSON(w, http.StatusOK, struct {
			Entries []logging.Entry `json:"entries"`
			Next    uint64          `json:"next"`
		}{entries, next})
	})
	mux.HandleFunc("GET /api/v1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		after, ok := queryAfter(w, r.URL.Query())
		if !ok {
			return
		}
		if logs == nil {
			writeError(w, http.StatusServiceUnavailable, "logs unavailable")
			return
		}
		entries, cancel := logs.Subscribe(after)
		defer cancel()
		setEpoch(w)
		streamEvents(w, r, "log", entries, func(e logging.Entry) uint64 { return e.Seq })
	})
	mux.HandleFunc("GET /api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		limit, ok := queryLimit(w, r.URL.Query())
		if !ok {
			return
		}
		if notifications == nil {
			writeError(w, http.StatusServiceUnavailable, "notifications unavailable")
			return
		}
		events := notifications.History(limit)
		if events == nil {
			events = []notify.Event{}
		}
		writeJSON(w, http.StatusOK, struct {
			Events []notify.Event `json:"events"`
		}{events})
	})
	mux.HandleFunc("GET /api/v1/notifications/stream", func(w http.ResponseWriter, r *http.Request) {
		after, ok := queryAfter(w, r.URL.Query())
		if !ok {
			return
		}
		if notifications == nil {
			writeError(w, http.StatusServiceUnavailable, "notifications unavailable")
			return
		}
		events, cancel := notifications.Subscribe(after)
		defer cancel()
		setEpoch(w)
		streamEvents(w, r, "notification", events, func(e notify.Event) uint64 { return e.ID })
	})
}

func queryAfter(w http.ResponseWriter, query url.Values) (uint64, bool) {
	value := query.Get("after")
	if value == "" {
		return 0, true
	}
	after, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "after must be an unsigned 64-bit integer")
		return 0, false
	}
	return after, true
}

// Both histories default to 200 and cap at 1000; nonpositive limits use the
// default, matching logging.Hub. Malformed numbers are rejected with JSON 400.
func queryLimit(w http.ResponseWriter, query url.Values) (int, bool) {
	value := query.Get("limit")
	if value == "" {
		return 200, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be an integer")
		return 0, false
	}
	if limit <= 0 {
		limit = 200
	}
	return min(limit, 1000), true
}

// streamWriteTimeout bounds each SSE write so a connected but non-consuming
// client cannot retain a handler/subscription forever. Heartbeats are twice as
// frequent, so an idle but healthy stream always refreshes the deadline.
const streamWriteTimeout = 30 * time.Second

func streamEvents[T any](w http.ResponseWriter, r *http.Request, event string, events <-chan T, id func(T) uint64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	setDeadline := func() {
		// httptest recorders may not support deadlines; real HTTP connections do.
		_ = controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	}
	setDeadline()
	if err := controller.Flush(); err != nil {
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		if r.Context().Err() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				return
			}
			setDeadline()
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id(entry), event, data); err != nil {
				return
			}
		case <-heartbeat.C:
			setDeadline()
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
		}
		if err := controller.Flush(); err != nil {
			return
		}
	}
}
