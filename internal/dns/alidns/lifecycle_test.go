package alidns

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ros-ddns/internal/config"
)

func TestProviderPoolsExpireCloseAndRemainReusable(t *testing.T) {
	idle, closed := make(chan struct{}, 16), make(chan struct{}, 16)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			idle <- struct{}{}
		case http.StateClosed:
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	wait := func(event <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-event:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection did not become %s", name)
		}
	}
	for range 4 {
		result, err := New(config.ProviderConfig{Endpoint: "unused.invalid", AccessKeyID: "test", AccessKeySecret: "test-secret", QPS: 5, Timeout: config.Duration{Duration: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		p := result.(*provider)
		defer p.CloseIdleConnections()
		sdk := p.api.(*sdkAPI)
		p.CloseIdleConnections() // Closing an unused provider must not prevent use.
		original := &http.Transport{}
		for range 2 {
			request, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (contextHTTPClient{ctx: context.Background(), api: sdk}).Call(request, original)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("response: %v %v", readErr, closeErr)
			}
			wait(idle, "idle")
			pool := sdk.idleTransport.Load()
			if pool == nil || pool == original || pool.IdleConnTimeout != 90*time.Second || pool.MaxIdleConns != 16 || pool.MaxIdleConnsPerHost != 4 {
				t.Fatal("SDK pool retention limits or ownership not configured")
			}
			if original.IdleConnTimeout != 0 || original.MaxIdleConns != 0 {
				t.Fatal("mutated SDK-supplied transport")
			}
			p.CloseIdleConnections()
			wait(closed, "closed") // No server Close or 90-second wait hides leaks.
		}
	}
}
