// Package api exposes daemon status, configuration, event streams and the WebUI.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/service"
)

// Source is the concurrency-safe portion of a service used by HTTP handlers.
type Source interface {
	Snapshot() service.Snapshot
	Ready() bool
	TriggerItem(string) bool
}

type ConfigEditor interface {
	ConfigDocument() (config.Document, error)
	ValidateConfig([]byte) error
	ApplyConfig(context.Context, string, []byte) (service.Snapshot, []string, error)
}

type LogStore interface {
	History(after uint64, limit int, minLevel slog.Level) ([]logging.Entry, uint64)
	Subscribe(after uint64) (<-chan logging.Entry, func())
}

type NotificationStore interface {
	History(limit int) []notify.Event
	Subscribe(after uint64) (<-chan notify.Event, func())
}

type MetaSource interface{ Meta() app.Meta }

type Options struct {
	Source        Source
	Config        ConfigEditor
	Logs          LogStore
	Notifications NotificationStore
	Meta          MetaSource
	Token         string
	Version       string
	// StreamEpoch identifies the daemon process generation in SSE responses.
	// Set it from the manager's StartedAt formatted as RFC3339Nano.
	StreamEpoch string
	// UIFS has index.html and assets at its root; /assets/ is stripped.
	UIFS fs.FS
}

// NewHandler constructs the API without opening a listener. A non-empty token
// protects the entire /api/v1/ namespace, including unknown paths and methods.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := opts.Source != nil && opts.Source.Ready()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"ready": ready})
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if opts.Source == nil {
			writeError(w, http.StatusServiceUnavailable, "service unavailable")
			return
		}
		writeJSON(w, http.StatusOK, opts.Source.Snapshot())
	})
	mux.HandleFunc("POST /api/v1/items/{name}/sync", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		if opts.Source == nil {
			writeError(w, http.StatusServiceUnavailable, "service unavailable")
			return
		}
		name := r.PathValue("name")
		if !opts.Source.TriggerItem(name) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "name": name})
	})
	mux.HandleFunc("GET /api/v1/meta", func(w http.ResponseWriter, r *http.Request) {
		meta := app.Meta{Version: opts.Version, LogLevel: "info"}
		if opts.Meta != nil {
			meta = opts.Meta.Meta()
		} else if opts.Source != nil {
			snapshot := opts.Source.Snapshot()
			meta.ConfigPath, meta.StartedAt = snapshot.ConfigPath, snapshot.StartedAt
			if !meta.StartedAt.IsZero() {
				meta.Uptime = max(0, time.Since(meta.StartedAt))
			}
		}
		writeJSON(w, http.StatusOK, struct {
			app.Meta
			UptimeSeconds float64 `json:"uptimeSeconds"`
			AuthRequired  bool    `json:"authRequired"`
			Ready         bool    `json:"ready"`
		}{meta, meta.Uptime.Seconds(), opts.Token != "", opts.Source != nil && opts.Source.Ready()})
	})
	registerConfig(mux, opts.Config)
	registerStreams(mux, opts.Logs, opts.Notifications, opts.StreamEpoch)
	registerUI(mux, opts.UIFS)

	// Compare fixed-size digests so comparison time does not reveal token length.
	expected := sha256.Sum256([]byte(opts.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.Token != "" && (r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/")) {
			scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			actual := sha256.Sum256([]byte(credential))
			if len(r.Header.Values("Authorization")) != 1 || !ok || !strings.EqualFold(scheme, "Bearer") ||
				subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		if handler, pattern := mux.Handler(r); pattern == "" {
			// Let the mux determine 404/405 and Allow; only change its error body.
			handler.ServeHTTP(&muxErrorWriter{ResponseWriter: w}, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type muxErrorWriter struct {
	http.ResponseWriter
	errorWritten bool
}

func (w *muxErrorWriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		w.errorWritten = true
		writeError(w.ResponseWriter, status, http.StatusText(status))
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *muxErrorWriter) Write(data []byte) (int, error) {
	if w.errorWritten {
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Marshal before writing headers so an encoding failure can still return 500.
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// Run binds cfg.Listen and serves until cancellation or a listener error. It
// waits for in-flight requests to finish, with a ten-second shutdown deadline.
func Run(ctx context.Context, cfg config.APIConfig, opts Options) error {
	listener, err := Listen(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	return Serve(ctx, cfg, opts, listener)
}

// Listen binds cfg.Listen synchronously. The caller must close the listener or
// transfer its ownership to Serve. Cancellation is returned as an error.
func Listen(ctx context.Context, cfg config.APIConfig) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("listen HTTP API: %w", err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen HTTP API: %w", err)
	}
	return listener, nil
}

// Serve owns listener and closes it when serving stops, including cancellation.
// In-flight requests have up to ten seconds to finish during shutdown.
func Serve(ctx context.Context, cfg config.APIConfig, opts Options, listener net.Listener) error {
	if opts.Token == "" {
		opts.Token = cfg.Token
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewHandler(opts),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
	return serve(ctx, server, listener)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Streams must see daemon cancellation (and listener failures) so shutdown
	// does not spend its grace period waiting for an indefinitely live request.
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// The parent is canceled; shutdown needs its own live deadline.
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
			err = fmt.Errorf("shutdown HTTP API: %w", err)
		}
		shutdownDone <- err
	}()

	err := server.Serve(listener)
	cancel() // Also drain requests if Serve exits with a listener failure.
	shutdownErr := <-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		err = fmt.Errorf("serve HTTP API: %w", err)
	}
	return errors.Join(err, shutdownErr)
}
