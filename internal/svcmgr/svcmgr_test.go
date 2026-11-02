package svcmgr

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	kservice "github.com/kardianos/service"
)

// fakeService records control calls without touching a real service manager.
type fakeService struct {
	installed bool
	running   bool
	calls     []string
}

func (f *fakeService) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeService) Run() error { f.record("Run"); return nil }

func (f *fakeService) Start() error {
	f.record("Start")
	f.running = true
	return nil
}

func (f *fakeService) Stop() error {
	f.record("Stop")
	f.running = false
	return nil
}

func (f *fakeService) Restart() error {
	f.record("Restart")
	f.running = true
	return nil
}

func (f *fakeService) Install() error {
	f.record("Install")
	f.installed = true
	return nil
}

func (f *fakeService) Uninstall() error {
	f.record("Uninstall")
	f.installed = false
	f.running = false
	return nil
}

func (f *fakeService) Logger(chan<- error) (kservice.Logger, error) { return nil, nil }

func (f *fakeService) SystemLogger(chan<- error) (kservice.Logger, error) { return nil, nil }

func (f *fakeService) String() string { return Name }

func (f *fakeService) Platform() string { return "test" }

func (f *fakeService) Status() (kservice.Status, error) {
	if !f.installed {
		return kservice.StatusUnknown, kservice.ErrNotInstalled
	}
	if f.running {
		return kservice.StatusRunning, nil
	}
	return kservice.StatusStopped, nil
}

func TestBuildConfig(t *testing.T) {
	opts := Options{
		ConfigPath:       "/etc/ros-ddns/config.yaml",
		LockPath:         "/var/lib/ros-ddns/state.json.lock",
		User:             "ros-ddns",
		WorkingDirectory: "/var/lib/ros-ddns",
	}

	systemd := BuildConfig(opts, "linux-systemd")
	if systemd.Name != Name || systemd.DisplayName != DisplayName || systemd.UserName != "ros-ddns" {
		t.Fatalf("unexpected systemd service identity: %+v", systemd)
	}
	wantArgs := []string{"-config", opts.ConfigPath, "-lock-file", opts.LockPath}
	if !reflect.DeepEqual(systemd.Arguments, wantArgs) {
		t.Fatalf("arguments = %v, want %v", systemd.Arguments, wantArgs)
	}
	wantDeps := []string{"Wants=network-online.target", "After=network-online.target"}
	if !reflect.DeepEqual(systemd.Dependencies, wantDeps) {
		t.Fatalf("dependencies = %v, want %v", systemd.Dependencies, wantDeps)
	}
	if got, _ := systemd.Option["Restart"].(string); got != "on-failure" {
		t.Fatalf("systemd Restart option = %q", got)
	}

	windows := BuildConfig(opts, "windows-service")
	if windows.Dependencies != nil {
		t.Fatalf("windows dependencies = %v, want none", windows.Dependencies)
	}
	if delayed, _ := windows.Option["DelayedAutoStart"].(bool); !delayed {
		t.Fatal("windows service should be registered as delayed automatic start")
	}

	other := BuildConfig(opts, "linux-upstart")
	if other.Dependencies != nil || other.Option != nil {
		t.Fatalf("unexpected platform options: deps=%v options=%v", other.Dependencies, other.Option)
	}
}

func TestArguments(t *testing.T) {
	if got := Arguments(Options{}); got != nil {
		t.Fatalf("empty options arguments = %v, want nil", got)
	}
	got := Arguments(Options{ConfigPath: "config.yaml"})
	if !reflect.DeepEqual(got, []string{"-config", "config.yaml"}) {
		t.Fatalf("arguments = %v", got)
	}
}

func TestControl(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service fakeService
		action  string
		want    []string
		err     bool
	}{
		{name: "install", action: ActionInstall, want: []string{"Install"}},
		{name: "start stopped", service: fakeService{installed: true}, action: ActionStart, want: []string{"Start"}},
		{name: "start running", service: fakeService{installed: true, running: true}, action: ActionStart, want: nil},
		{name: "start missing", action: ActionStart, err: true},
		{name: "stop running", service: fakeService{installed: true, running: true}, action: ActionStop, want: []string{"Stop"}},
		{name: "stop stopped", service: fakeService{installed: true}, action: ActionStop, want: nil},
		{name: "restart running", service: fakeService{installed: true, running: true}, action: ActionRestart, want: []string{"Restart"}},
		{name: "restart stopped", service: fakeService{installed: true}, action: ActionRestart, want: []string{"Start"}},
		{name: "uninstall running", service: fakeService{installed: true, running: true}, action: ActionUninstall, want: []string{"Stop", "Uninstall"}},
		{name: "uninstall stopped", service: fakeService{installed: true}, action: ActionUninstall, want: []string{"Uninstall"}},
		{name: "unknown", action: "bogus", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := tc.service
			err := Control(&service, tc.action)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error %v", err, tc.err)
			}
			if !reflect.DeepEqual(service.calls, tc.want) {
				t.Fatalf("calls = %v, want %v", service.calls, tc.want)
			}
		})
	}
}

func TestStatusText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service fakeService
		want    string
	}{
		{"missing", fakeService{}, "not-installed"},
		{"stopped", fakeService{installed: true}, "stopped"},
		{"running", fakeService{installed: true, running: true}, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := tc.service
			got, err := StatusText(&service)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestControlNotInstalledMessage(t *testing.T) {
	service := &fakeService{}
	err := Control(service, ActionStart)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error = %v, want not installed", err)
	}
	if !errors.Is(err, kservice.ErrNotInstalled) {
		t.Fatalf("error = %v, want wrapped ErrNotInstalled", err)
	}
}
