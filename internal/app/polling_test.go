package app

import (
	"context"
	"testing"
	"time"
)

// TestApplyConfigDisablingAllItemsStopsPolling covers the configuration-change
// half of the zero-runnable-item rule: the replacement service must not create a
// RouterOS client or start polling once every item is disabled.
func TestApplyConfigDisablingAllItemsStopsPolling(t *testing.T) {
	f := newFixture(t, Options{})
	startManager(t, f.m)
	await(t, func() bool { return f.m.Snapshot().Items[0].Phase == "synced" })

	hash, data := editDocument(t, f.m, func(d map[string]any) {
		runtime := d["runtime"].(map[string]any)
		runtime["pollInterval"] = "20ms"
		runtime["maxBackoff"] = "20ms"
	})
	if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	router := f.router
	router.mu.Lock()
	before := router.polls
	router.mu.Unlock()
	await(t, func() bool {
		router.mu.Lock()
		defer router.mu.Unlock()
		return router.polls > before
	})

	hash, data = editDocument(t, f.m, func(d map[string]any) {
		d["items"].([]any)[0].(map[string]any)["disabled"] = true
	})
	if _, _, err := f.m.ApplyConfig(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	router.mu.Lock()
	settled := router.polls
	router.mu.Unlock()
	// The old generation's next poll would have happened well within this wait.
	time.Sleep(150 * time.Millisecond)
	router.mu.Lock()
	after := router.polls
	router.mu.Unlock()
	if after != settled {
		t.Fatalf("polling continued after all items were disabled: %d -> %d", settled, after)
	}

	snapshot := f.m.Snapshot()
	if len(snapshot.Items) != 1 || !snapshot.Items[0].Disabled || snapshot.Items[0].Phase != "disabled" {
		t.Fatalf("snapshot after disabling = %+v", snapshot.Items)
	}
	if !f.m.Ready() {
		t.Fatal("all-disabled configuration should be ready without polling")
	}
}
