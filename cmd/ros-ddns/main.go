package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/dns/alidns"
	"ros-ddns/internal/instancelock"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/pathguard"
	"ros-ddns/internal/redact"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/service"
	"ros-ddns/internal/svcmgr"
	"ros-ddns/internal/version"
)

// isInteractive reports whether the process runs from a console instead of the
// OS service manager. Tests replace it to exercise the service-managed startup.
var isInteractive = svcmgr.Interactive

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ros-ddns", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		configPath    = flags.String("config", "config.yaml", "path to the configuration file")
		lockFile      = flags.String("lock-file", "", "instance lock path (default: state.path + .lock, or a temp path hashed from the canonical config path)")
		check         = flags.Bool("check", false, "validate the configuration and exit")
		showVersion   = flags.Bool("version", false, "print version information and exit")
		serviceAction = flags.String("service", "", "manage the installed OS service: install|uninstall|start|stop|restart|status")
		serviceUser   = flags.String("service-user", "", "account the installed service runs as")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "ros-ddns: unexpected positional arguments")
		return 2
	}

	if *showVersion {
		if _, err := fmt.Fprintln(stdout, version.String()); err != nil {
			fmt.Fprintf(stderr, "ros-ddns: write version: %v\n", err)
			return 1
		}
		return 0
	}

	if *serviceAction != "" {
		return runServiceAction(serviceActionOptions{
			Action:     *serviceAction,
			User:       *serviceUser,
			ConfigPath: *configPath,
			LockPath:   *lockFile,
			Check:      *check,
			Stdout:     stdout,
			Stderr:     stderr,
		})
	}

	// Every daemon start, whether from a console or from the OS service manager,
	// must come up even when the configuration cannot be loaded: a default
	// configuration is generated first. -check stays side-effect free and keeps
	// its strict validation semantics.
	serviceManaged := !isInteractive()
	activeConfigPath := *configPath
	absConfigPath, err := filepath.Abs(activeConfigPath)
	if err != nil {
		fmt.Fprintln(stderr, "ros-ddns: cannot resolve config path")
		return 2
	}

	var (
		program       *serviceProgram
		serviceHandle svcmgr.Service
		serviceLogger svcmgr.Logger
	)
	if serviceManaged {
		program = &serviceProgram{}
		serviceHandle, err = svcmgr.New(program, svcmgr.Options{})
		if err != nil {
			fmt.Fprintf(stderr, "ros-ddns: initialize service manager: %v\n", err)
			return 1
		}
		serviceLogger, _ = serviceHandle.Logger(nil)
		program.logger = serviceLogger
	}

	var (
		cfg      *config.Config
		recovery *configRecovery
	)
	if *check {
		cfg, err = config.Load(activeConfigPath)
		if err != nil {
			fmt.Fprintf(stderr, "ros-ddns: %v\n", err)
			return 2
		}
	} else {
		cfg, recovery, err = prepareDaemonConfig(absConfigPath, serviceManaged)
		if err != nil {
			if serviceManaged {
				return serviceFatal(serviceLogger, stderr, "%v", err)
			}
			fmt.Fprintf(stderr, "ros-ddns: %v\n", err)
			// An existing but unloadable configuration is a configuration
			// error; a failure to create the default is a startup error.
			if _, statErr := os.Stat(absConfigPath); statErr == nil {
				return 2
			}
			return 1
		}
		activeConfigPath = absConfigPath
	}

	absConfigPath, err = filepath.Abs(activeConfigPath)
	if err != nil {
		fmt.Fprintln(stderr, "ros-ddns: cannot resolve config path")
		return 2
	}
	lockPath, err := instanceLockPath(*lockFile, cfg.State.Path, activeConfigPath)
	if err == nil {
		err = pathguard.Validate(activeConfigPath, cfg.State.Path, lockPath)
	}
	if err != nil {
		if serviceManaged {
			return serviceFatal(serviceLogger, stderr, "configuration paths: %v", err)
		}
		fmt.Fprintf(stderr, "ros-ddns: %v\n", err)
		return 2
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(parseLogLevel(cfg.Log.Level))
	hub := logging.NewHub(2000)
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(newBaseHandler(cfg.Log, stderr, levelVar), hub, levelVar)))
	defer slog.SetDefault(previousLogger)
	defer hub.Close()
	hub.SetRedactor(redact.FromConfig(cfg))

	if recovery != nil {
		if recovery.Quarantined != "" {
			slog.Error("configuration could not be loaded; preserved it and wrote the default configuration",
				"preserved", recovery.Quarantined, "error", recovery.LoadErr)
		}
		if recovery.Created {
			slog.Warn("wrote default configuration; edit it or use the WebUI", "path", absConfigPath)
		}
	}

	if strings.HasPrefix(strings.ToLower(cfg.RouterOS.URL), "http://") {
		slog.Warn("routeros.url uses http://; RouterOS credentials and traffic are unencrypted")
	}
	if *check {
		if _, err := fmt.Fprint(stdout, config.Summary(cfg)); err != nil {
			slog.Error("write config summary", "error", err)
			return 1
		}
		return 0
	}

	// The daemon mutates remote targets, so it must hold the single-instance
	// lock for the entire run, even if state.path is reloaded.
	lock, err := instancelock.Acquire(lockPath, instancelock.Info{
		PID: os.Getpid(), StartedAt: time.Now().UTC(), ConfigPath: absConfigPath, Version: version.Version,
	})
	if err != nil {
		slog.Error("acquire single-instance lock", "error", err)
		if serviceLogger != nil {
			_ = serviceLogger.Error("acquire single-instance lock: " + err.Error())
		}
		return 1
	}
	defer func() {
		if err := lock.Close(); err != nil {
			slog.Warn("release single-instance lock", "path", lock.Path(), "error", err)
		}
	}()
	slog.Info("acquired single-instance lock", "path", lock.Path())

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	factory := func(name string, pc config.ProviderConfig) (dns.Provider, error) {
		return alidns.New(pc)
	}
	deps := service.Deps{
		NewROS: ros.NewRESTClient, NewProvider: factory,
	}

	notifications := notify.NewStore(200)
	defer notifications.Close()
	if recovery != nil {
		if recovery.Created {
			notifications.Add(notify.Event{
				Kind: "configuration_default_created", Severity: "warning",
				Message: "配置文件缺失或无法加载，已生成默认配置；请编辑配置文件或使用 WebUI 完成配置。",
			})
		}
		if recovery.Quarantined != "" {
			notifications.Add(notify.Event{
				Kind: "configuration_quarantined", Severity: "error",
				Message: "原配置无法加载，已保留为 " + recovery.Quarantined + " 并生成默认配置。",
			})
		}
	}

	appOptions := app.Options{
		LogHub: hub, LogLevel: levelVar, Notifications: notifications,
		Version: version.Version, LockPath: lockPath,
	}
	session, err := newDaemonSession(cfg, deps, appOptions)
	if err != nil {
		slog.Error("initialize service", "error", err)
		if serviceLogger != nil {
			_ = serviceLogger.Error("initialize service: " + err.Error())
		}
		return 1
	}

	if !serviceManaged {
		if err := session.start(ctx); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return 0
			}
			slog.Error("start HTTP API", "error", err)
			return 1
		}
		if err := session.wait(); err != nil {
			slog.Error("daemon stopped", "error", err)
			return 1
		}
		return 0
	}

	// Service-managed daemon: the service manager owns the process lifecycle,
	// invokes Start/Stop and polls the health endpoint.
	program.session = session
	program.exit = os.Exit
	if err := serviceHandle.Run(); err != nil {
		slog.Error("run as a service", "error", err)
		if serviceLogger != nil {
			_ = serviceLogger.Error("run as a service: " + err.Error())
		}
		return 1
	}
	return 0
}

// instanceLockPath uses the explicit override, then the initial state path.
// In memory-only mode the first eight bytes of SHA-256 of the canonical config
// path form a stable, credential-free name in the process's temporary directory.
func instanceLockPath(explicit, statePath, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if statePath != "" {
		return statePath + ".lock", nil
	}
	name, err := pathguard.Canonical(configPath)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(name))
	return filepath.Join(os.TempDir(), "ros-ddns-"+hex.EncodeToString(hash[:8])+".lock"), nil
}

func newBaseHandler(cfg config.LogConfig, output io.Writer, level *slog.LevelVar) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		return slog.NewJSONHandler(output, opts)
	}
	return slog.NewTextHandler(output, opts)
}

// newLogger keeps the historical testable helper. The daemon uses
// newBaseHandler plus logging.NewHandler so the UI can also receive records.
func newLogger(cfg config.LogConfig, output io.Writer) *slog.Logger {
	level := &slog.LevelVar{}
	level.Set(parseLogLevel(cfg.Level))
	return slog.New(logging.NewHandler(newBaseHandler(cfg, output, level), nil, level))
}

func parseLogLevel(value string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err == nil {
		return level
	}
	return slog.LevelInfo
}
