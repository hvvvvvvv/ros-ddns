package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ros-ddns/internal/config"
	"ros-ddns/internal/ros"
)

// configureTolerant preserves every config position, but admits only usable
// items to the worker and trigger collections. Configuration errors stay fixed
// until a new service generation is built after a successful config change.
func (s *Service) configureTolerant(cfg *config.Config, deps Deps) {
	providerErrors := make(map[string]string)
	seenNames := make(map[string]bool)
	for i, original := range cfg.Items {
		name := original.Name
		if name == "" || seenNames[name] {
			name = fmt.Sprintf("item[%d]", i)
		}
		seenNames[original.Name] = true
		if original.Disabled {
			c := config.CloneItem(original)
			c.Name = name
			s.ordered = append(s.ordered, statusSlot{static: initialStatus(c)})
			continue
		}

		problems := cfg.CheckItem(i)
		checked := make(map[string]bool)
		for _, d := range cfg.Items[i].Domains {
			if !checked[d.Provider] {
				problems = append(problems, cfg.CheckProvider(d.Provider)...)
				checked[d.Provider] = true
			}
		}
		// CheckItem refreshes parsed prefix/MAC fields before the runtime copy.
		c := config.CloneItem(cfg.Items[i])
		c.Name = name
		if len(problems) == 0 {
			clear(checked)
			for _, d := range c.Domains {
				if checked[d.Provider] {
					continue
				}
				checked[d.Provider] = true
				if problem := s.tolerantProvider(cfg, deps, d.Provider, providerErrors); problem != "" {
					problems = append(problems, problem)
				}
			}
		}
		if len(problems) != 0 {
			status := initialStatus(c)
			status.Phase = "error"
			status.LastError = s.redactor.Replace("配置错误：" + strings.Join(problems, "; "))
			s.ordered = append(s.ordered, statusSlot{static: status})
			s.invalid++
			continue
		}
		s.ordered = append(s.ordered, statusSlot{item: newItem(s, c)})
	}

	for _, conflict := range cfg.TargetConflicts() {
		slot := &s.ordered[conflict.ItemIndex]
		if slot.item == nil {
			continue
		}
		slot.static = slot.item.snapshot()
		slot.static.Phase = "error"
		slot.static.LastError = s.redactor.Replace(conflict.Message)
		slot.item = nil
		s.invalid++
	}
	for _, slot := range s.ordered {
		if slot.item != nil {
			s.items = append(s.items, slot.item)
			s.byName[slot.item.cfg.Name] = slot.item
		}
	}
	if len(s.items) == 0 {
		return
	}
	if problems := cfg.CheckRouterOS(); len(problems) != 0 {
		s.client = failingROS{err: errors.New(s.redactor.Replace("配置错误：" + strings.Join(problems, "; ")))}
		return
	}
	client, err := deps.NewROS(cfg.RouterOS)
	switch {
	case err != nil:
		s.client = failingROS{err: errors.New(s.redactor.Replace(fmt.Sprintf("create RouterOS client: %s", err)))}
	case client == nil:
		s.client = failingROS{err: errors.New("create RouterOS client: factory returned nil")}
	default:
		s.client = client
	}
}

func (s *Service) tolerantProvider(cfg *config.Config, deps Deps, name string, failures map[string]string) string {
	if _, ok := s.providers[name]; ok {
		return ""
	}
	if problem, ok := failures[name]; ok {
		return problem
	}
	var problem string
	if deps.NewProvider == nil {
		problem = fmt.Sprintf("provider %s: Deps.NewProvider is required", name)
	} else {
		provider, err := deps.NewProvider(name, cfg.DNS.Providers[name])
		switch {
		case err != nil:
			problem = fmt.Sprintf("create provider %s: %s", name, err)
		case provider == nil:
			problem = fmt.Sprintf("create provider %s: factory returned nil", name)
		default:
			s.providers[name] = provider
			return ""
		}
	}
	failures[name] = s.redactor.Replace(problem)
	return failures[name]
}

// failingROS lets the existing polling/backoff path report construction and
// configuration failures without terminating the daemon at startup.
type failingROS struct {
	err error
}

func (f failingROS) Addresses(context.Context, ros.Family) ([]ros.Address, error) {
	return nil, f.err
}

func (f failingROS) ListEntries(context.Context, ros.Family, string) ([]ros.ListEntry, error) {
	return nil, f.err
}

func (f failingROS) AddEntry(context.Context, ros.Family, ros.ListEntry) error {
	return f.err
}

func (f failingROS) RemoveEntry(context.Context, ros.Family, string) error {
	return f.err
}
