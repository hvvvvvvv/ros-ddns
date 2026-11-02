package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"ros-ddns/internal/notify"
	"ros-ddns/internal/redact"
	"ros-ddns/internal/service"
)

type notificationState struct {
	signature string
	failed    bool
}

func (m *Manager) monitor(ctx context.Context) {
	previous := make(map[string]notificationState)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		m.pollNotifications(previous)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) pollNotifications(previous map[string]notificationState) {
	if m.opts.Notifications == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Ignore the old service's shutdown and the candidate's startup window.
	if m.reloading {
		return
	}
	r := redact.FromConfig(m.cfg)
	snapshot := m.svc.Snapshot()
	seen := make(map[string]bool, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.Disabled {
			continue
		}
		seen[item.Name] = true
		current, message := itemNotification(item)
		old := previous[item.Name]
		var event notify.Event
		if current.failed && (!old.failed || current.signature != old.signature) {
			event = notify.Event{Kind: "item_error", Severity: "error", Message: message}
		} else if !current.failed && old.failed {
			event = notify.Event{Kind: "item_recovery", Severity: "info", Message: "项目已恢复，当前无错误。"}
		}
		if event.Kind != "" {
			event.Item = r.Replace(item.Name)
			event.Phase = r.Replace(item.Phase)
			event.Message = r.Replace(event.Message)
			m.addEvent(event)
		}
		previous[item.Name] = current
	}
	for name := range previous {
		if !seen[name] {
			delete(previous, name)
		}
	}
}

func itemNotification(item service.ItemStatus) (notificationState, string) {
	signature := struct {
		Phase   string
		Error   string
		Domains []string
		List    string
	}{Phase: item.Phase, Error: item.LastError, Domains: make([]string, len(item.Domains))}
	var messages []string
	seen := make(map[string]bool)
	add := func(message string) {
		if message != "" && !seen[message] {
			seen[message] = true
			messages = append(messages, message)
		}
	}
	add(item.LastError)
	for i, domain := range item.Domains {
		signature.Domains[i] = domain.LastError
		add(domain.LastError)
	}
	if item.AddressList != nil {
		signature.List = item.AddressList.LastError
		add(item.AddressList.LastError)
	}
	failed := len(messages) > 0 || item.Phase == "degraded" || item.Phase == "error"
	if failed && len(messages) == 0 {
		messages = append(messages, "项目进入 "+item.Phase+" 状态。")
	}
	encoded, _ := json.Marshal(signature) // All fields are strings or string slices.
	return notificationState{signature: string(encoded), failed: failed}, strings.Join(messages, "; ")
}
