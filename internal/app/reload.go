package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"ros-ddns/internal/config"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/pathguard"
	"ros-ddns/internal/redact"
	"ros-ddns/internal/service"
)

// ApplyConfig accepts a complete JSON configuration and a hash from
// ConfigDocument. Before Run, it replaces the dormant service without starting
// goroutines. After Run stops it returns an error. Once a live service is swapped,
// its lifetime belongs to Run's context, not this request's context.
//
// Files are replaced with a same-directory rename; external editors do not share
// the manager's lock, so they must also respect hashes to avoid concurrent edits.
func (m *Manager) ApplyConfig(ctx context.Context, baseHash string, data []byte) (snapshot service.Snapshot, warnings []string, err error) {
	defer func() {
		if err != nil {
			// Validation errors can echo submitted literal values. Do not put
			// request contents or raw errors into logs/notification history.
			slog.Warn("configuration apply failed")
			m.addEvent(notify.Event{Kind: "config_error", Severity: "error", Message: "配置应用失败，请检查校验结果或重载错误。"})
		}
	}()
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	if m.finished {
		m.mu.Unlock()
		return m.Snapshot(), nil, errors.New("manager has stopped; create a new manager")
	}
	m.reloading = true
	oldCfg, oldSvc, oldRun, oldDeps, runCtx := m.cfg, m.svc, m.run, m.deps, m.runCtx
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.reloading = false
		m.mu.Unlock()
		if err != nil {
			snapshot = m.Snapshot()
		}
	}()
	checkContext := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if runCtx != nil {
			return runCtx.Err()
		}
		return nil
	}
	if err := checkContext(); err != nil {
		return service.Snapshot{}, nil, err
	}
	if baseHash == "" {
		return service.Snapshot{}, nil, ErrConfigConflict
	}
	document, err := config.ReadDocument(oldCfg.Path)
	if err != nil {
		return service.Snapshot{}, nil, fmt.Errorf("read current configuration: %w", err)
	}
	if document.Hash != baseHash {
		return service.Snapshot{}, nil, ErrConfigConflict
	}
	newCfg, yamlBytes, err := config.ValidateStructured(data, oldCfg.Path)
	if err != nil {
		return service.Snapshot{}, nil, err
	}
	if err := pathguard.Validate(oldCfg.Path, newCfg.State.Path, m.lockPath); err != nil {
		return service.Snapshot{}, nil, err
	}
	var restartFields []string
	if oldCfg.API.Listen != newCfg.API.Listen {
		restartFields = append(restartFields, "api.listen")
	}
	if oldCfg.API.Token != newCfg.API.Token {
		restartFields = append(restartFields, "api.token")
	}
	if oldCfg.Log.Format != newCfg.Log.Format {
		restartFields = append(restartFields, "log.format")
	}
	if len(restartFields) > 0 {
		return service.Snapshot{}, nil, fmt.Errorf("%w: %s", ErrRestartRequired, strings.Join(restartFields, ", "))
	}
	newDeps := oldDeps
	// Startup tolerance must never weaken a configuration transaction.
	newDeps.Tolerant = false
	if newCfg.State.Path != oldCfg.State.Path {
		newDeps.State = nil
	}
	newDeps, err = withState(newCfg, newDeps)
	if err != nil {
		return service.Snapshot{}, nil, err
	}
	candidate, err := m.newService(newCfg, newDeps)
	if err != nil {
		return service.Snapshot{}, nil, fmt.Errorf("construct candidate service: %s", redact.FromConfig(newCfg).Replace(err.Error()))
	}
	// Until publication the transaction owns this candidate, including failed
	// startup and the dormant validation generation discarded after oldRun joins.
	published := false
	defer func() {
		if !published {
			closeIdleConnections(candidate)
		}
	}()
	if err := checkContext(); err != nil {
		return service.Snapshot{}, nil, err
	}
	oldBytes, mode, err := readOriginal(oldCfg.Path, document.Hash)
	if err != nil {
		return service.Snapshot{}, nil, fmt.Errorf("prepare configuration backup: %w", err)
	}
	// Factories may take time; recheck identities immediately before writing.
	if err := pathguard.Validate(oldCfg.Path, newCfg.State.Path, m.lockPath); err != nil {
		return service.Snapshot{}, nil, err
	}
	if err := m.writeFile(oldCfg.Path+".bak", oldBytes, mode); err != nil {
		return service.Snapshot{}, nil, fmt.Errorf("back up configuration: %w", err)
	}
	if err := checkContext(); err != nil {
		return service.Snapshot{}, nil, err
	}
	if err := m.writeFile(oldCfg.Path, yamlBytes, mode); err != nil {
		return service.Snapshot{}, nil, fmt.Errorf("write configuration: %w", err)
	}
	// From this point every failure restores the exact original file bytes.
	rollback := func(cause error, stopped bool) error {
		var restoreErr, rebuildErr, restartErr error
		if err := m.writeFile(oldCfg.Path, oldBytes, mode); err != nil {
			restoreErr = fmt.Errorf("restore configuration file: %w", err)
		}
		m.updateLogging(oldCfg)
		if stopped {
			restored, err := m.newService(oldCfg, oldDeps)
			if err != nil {
				rebuildErr = fmt.Errorf("rebuild previous service: %s", redact.FromConfig(oldCfg).Replace(err.Error()))
				// Service supports reuse after cancellation, so even a failed
				// factory need not leave an otherwise usable old service down.
				restored = oldSvc
			} else {
				// Only a successful rebuild makes the old object permanently
				// retired. The fallback above must remain reusable.
				closeIdleConnections(oldSvc)
			}
			var restoredRun *serviceRun
			if runCtx.Err() == nil {
				restoredRun, err = m.startService(runCtx, restored)
				if err != nil {
					restartErr = fmt.Errorf("restart previous service: %s", redact.FromConfig(oldCfg).Replace(err.Error()))
				}
			}
			m.mu.Lock()
			m.svc = restored
			// Retain the completed run on a failed restart so Run observes the
			// failure instead of waiting forever for a nonexistent generation.
			if restoredRun != nil {
				m.run = restoredRun
			}
			m.mu.Unlock()
		}
		return errors.Join(cause, restoreErr, rebuildErr, restartErr)
	}
	if err := checkContext(); err != nil {
		return service.Snapshot{}, nil, rollback(err, false)
	}
	var candidateRun *serviceRun
	if oldRun != nil {
		oldRun.stop()
		if err := checkContext(); err != nil {
			return service.Snapshot{}, nil, rollback(err, true)
		}
		// Old workers can invalidate shared cache entries while the preliminary
		// candidate is being built. Only a construction after all of them join
		// may supply the cache used by the next running generation.
		closeIdleConnections(candidate)
		candidate = nil
		if newCfg.State.Path != oldCfg.State.Path {
			// A different spelling may still alias the old state file. Reopen
			// after its last writer joins, preserving state.path change semantics.
			newDeps.State = nil
			newDeps, err = withState(newCfg, newDeps)
			if err != nil {
				return service.Snapshot{}, nil, rollback(err, true)
			}
		}
		fresh, err := m.newService(newCfg, newDeps)
		if err != nil {
			message := redact.FromConfig(oldCfg).Replace(redact.FromConfig(newCfg).Replace(err.Error()))
			return service.Snapshot{}, nil, rollback(fmt.Errorf("rebuild candidate service: %s", message), true)
		}
		candidate = fresh
		if err := checkContext(); err != nil {
			return service.Snapshot{}, nil, rollback(err, true)
		}
		// Protect logs emitted by the new workers before publishing the swap.
		if m.opts.LogHub != nil {
			m.opts.LogHub.SetRedactor(redact.FromConfig(newCfg))
		}
		candidateRun, err = m.startService(runCtx, candidate)
		if err != nil {
			message := redact.FromConfig(oldCfg).Replace(redact.FromConfig(newCfg).Replace(err.Error()))
			return service.Snapshot{}, nil, rollback(fmt.Errorf("start candidate service: %s", message), true)
		}
		if err := checkContext(); err != nil {
			candidateRun.stop()
			return service.Snapshot{}, nil, rollback(err, true)
		}
	}
	m.mu.Lock()
	m.svc, m.cfg, m.deps, m.run = candidate, newCfg, newDeps, candidateRun
	m.updateLogging(newCfg)
	m.mu.Unlock()
	published = true
	closeIdleConnections(oldSvc)
	slog.Info("configuration applied")
	m.addEvent(notify.Event{Kind: "config_applied", Severity: "info", Message: "配置已保存并应用。"})
	return candidate.Snapshot(), nil, nil
}
