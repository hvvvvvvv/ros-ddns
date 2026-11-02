package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kservice "github.com/kardianos/service"

	"ros-ddns/internal/config"
	"ros-ddns/internal/svcmgr"
)

// fakeControlService records service manager calls without touching the OS.
type fakeControlService struct {
	installed bool
	running   bool
	calls     []string
}

func (f *fakeControlService) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeControlService) Run() error { f.record("Run"); return nil }

func (f *fakeControlService) Start() error { f.record("Start"); f.running = true; return nil }

func (f *fakeControlService) Stop() error { f.record("Stop"); f.running = false; return nil }

func (f *fakeControlService) Restart() error { f.record("Restart"); f.running = true; return nil }

func (f *fakeControlService) Install() error { f.record("Install"); f.installed = true; return nil }

func (f *fakeControlService) Uninstall() error {
	f.record("Uninstall")
	f.installed = false
	f.running = false
	return nil
}

func (f *fakeControlService) Logger(chan<- error) (kservice.Logger, error) { return nil, nil }

func (f *fakeControlService) SystemLogger(chan<- error) (kservice.Logger, error) {
	return nil, nil
}

func (f *fakeControlService) String() string { return svcmgr.Name }

func (f *fakeControlService) Platform() string { return "test" }

func (f *fakeControlService) Status() (kservice.Status, error) {
	if !f.installed {
		return kservice.StatusUnknown, kservice.ErrNotInstalled
	}
	if f.running {
		return kservice.StatusRunning, nil
	}
	return kservice.StatusStopped, nil
}

func withFakeService(t *testing.T, service *fakeControlService) *svcmgr.Options {
	t.Helper()
	var captured svcmgr.Options
	original := newServiceControl
	newServiceControl = func(opts svcmgr.Options) (kservice.Service, error) {
		captured = opts
		return service, nil
	}
	t.Cleanup(func() { newServiceControl = original })
	return &captured
}

func TestServiceActionStatus(t *testing.T) {
	service := &fakeControlService{installed: true, running: true}
	withFakeService(t, service)

	for _, tc := range []struct {
		name string
		code int
		text string
	}{
		{"running", 0, "status=running"},
		{"stopped", 1, "status=stopped"},
		{"not installed", 1, "status=not-installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.name {
			case "stopped":
				service.running = false
			case "not installed":
				service.installed = false
			}
			var stdout, stderr bytes.Buffer
			code := runServiceAction(serviceActionOptions{
				Action: svcmgr.ActionStatus, ConfigPath: "config.yaml",
				Stdout: &stdout, Stderr: &stderr,
			})
			if code != tc.code || !strings.Contains(stdout.String(), tc.text) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestServiceActionValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runServiceAction(serviceActionOptions{Action: "bogus", Stdout: &stdout, Stderr: &stderr}); code != 2 {
		t.Fatalf("unknown action code = %d", code)
	}
	stderr.Reset()
	if code := runServiceAction(serviceActionOptions{Action: svcmgr.ActionStart, Check: true, Stdout: &stdout, Stderr: &stderr}); code != 2 {
		t.Fatalf("combined flags code = %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot be combined") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServiceActionInstallRecordsAbsolutePaths(t *testing.T) {
	service := &fakeControlService{}
	opts := withFakeService(t, service)
	configPath := filepath.Join(t.TempDir(), "deploy", "config.yaml")
	var stdout, stderr bytes.Buffer
	code := runServiceAction(serviceActionOptions{
		Action:     svcmgr.ActionInstall,
		User:       "ros-ddns",
		ConfigPath: configPath,
		LockPath:   filepath.Join(t.TempDir(), "state.json.lock"),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !filepath.IsAbs(opts.ConfigPath) || !filepath.IsAbs(opts.LockPath) {
		t.Fatalf("recorded paths must be absolute: %+v", opts)
	}
	if opts.User != "ros-ddns" || opts.WorkingDirectory != filepath.Dir(opts.ConfigPath) {
		t.Fatalf("recorded options = %+v", opts)
	}
	if len(service.calls) != 1 || service.calls[0] != "Install" {
		t.Fatalf("calls = %v", service.calls)
	}
	if !strings.Contains(stderr.String(), "generate a default configuration") {
		t.Fatalf("missing configuration warning: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed service ros-ddns") || !strings.Contains(stdout.String(), opts.ConfigPath) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestServiceActionUninstallStopsRunningService(t *testing.T) {
	service := &fakeControlService{installed: true, running: true}
	withFakeService(t, service)
	var stdout, stderr bytes.Buffer
	code := runServiceAction(serviceActionOptions{
		Action: svcmgr.ActionUninstall, ConfigPath: "config.yaml",
		Stdout: &stdout, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.Join(service.calls, ",") != "Stop,Uninstall" {
		t.Fatalf("calls = %v", service.calls)
	}
}

func TestRunServiceFlagDispatch(t *testing.T) {
	service := &fakeControlService{installed: true, running: true}
	withFakeService(t, service)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-service", "status"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "status=running") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"-service", "bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("unknown action code = %d, stderr=%q", code, stderr.String())
	}
}

// fakeRunner drives the service program lifecycle without a daemon.
type fakeRunner struct {
	release   chan struct{}
	waitErr   error
	startErr  error
	started   chan struct{}
	waitCalls int
}

func (f *fakeRunner) start(context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	if f.started != nil {
		close(f.started)
	}
	return nil
}

func (f *fakeRunner) wait() error {
	f.waitCalls++
	<-f.release
	return f.waitErr
}

func TestServiceProgramStartStop(t *testing.T) {
	runner := &fakeRunner{release: make(chan struct{}), started: make(chan struct{})}
	program := &serviceProgram{session: runner}
	program.exit = func(code int) { t.Errorf("unexpected exit(%d)", code) }
	if err := program.Start(nil); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	close(runner.release)
	if err := program.Stop(nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !program.stopping.Load() {
		t.Fatal("program should be marked as stopping")
	}
}

func TestServiceProgramStartFailure(t *testing.T) {
	runner := &fakeRunner{release: make(chan struct{}), startErr: errors.New("listen failed")}
	program := &serviceProgram{session: runner}
	if err := program.Start(nil); err == nil || !strings.Contains(err.Error(), "listen failed") {
		t.Fatalf("start error = %v", err)
	}
}

func TestServiceProgramExitsOnUnexpectedStop(t *testing.T) {
	runner := &fakeRunner{release: make(chan struct{}), started: make(chan struct{}), waitErr: errors.New("daemon stopped")}
	exited := make(chan int, 1)
	program := &serviceProgram{session: runner, exit: func(code int) { exited <- code }}
	if err := program.Start(nil); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	close(runner.release)
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unexpected daemon stop did not exit the process")
	}
}

func TestServiceProgramStopWithoutStart(t *testing.T) {
	program := &serviceProgram{}
	if err := program.Stop(nil); err != nil {
		t.Fatalf("stop without start: %v", err)
	}
}

func TestRecoveryFileIsLoadableByMainConfigLoad(t *testing.T) {
	// A generated default must load from any working directory because its state
	// path is written as an absolute path.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load generated default configuration: %v", err)
	}
	wantState := filepath.ToSlash(filepath.Join(filepath.Dir(path), "state.json"))
	if cfg.State.Path != wantState || cfg.API.Listen != "127.0.0.1:8080" || len(cfg.Items) != 1 {
		t.Fatalf("generated configuration = state.path %q (want %q) api.listen %q items %d", cfg.State.Path, wantState, cfg.API.Listen, len(cfg.Items))
	}
}
