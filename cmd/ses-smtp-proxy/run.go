package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kardianos/service"

	"github.com/TCANationals/ses-to-smtp-proxy/internal/config"
	"github.com/TCANationals/ses-to-smtp-proxy/internal/logging"
	svcpkg "github.com/TCANationals/ses-to-smtp-proxy/internal/service"
)

const usage = `ses-smtp-proxy %s

Usage: ses-smtp-proxy <command> [flags]

Commands:
  run         Run the service (also invoked by the OS service manager)
  debug       Run in the foreground with logs on stderr
  install     Register the Windows service
  uninstall   Remove the Windows service registration
  start       Tell the OS service manager to start the service
  stop        Tell the OS service manager to stop the service
  restart     Restart the service
  status      Show the service status
  version     Print the build version and exit

Flags:
  --config <path>  Override the configuration file path. When omitted, the
                   service loads %s from the same directory as the
                   executable.
`

func run(args []string) error {
	if len(args) == 0 {
		return runDefault()
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Fprintf(os.Stdout, usage, version, config.DefaultFileName)
		return nil
	}

	cfgPath, rest, err := extractConfigFlag(rest)
	if err != nil {
		return err
	}

	switch cmd {
	case "run":
		return runService(cfgPath, rest)
	case "debug":
		return runDebug(cfgPath, rest)
	case "install":
		return controlService("install", cfgPath, rest)
	case "uninstall":
		return controlService("uninstall", cfgPath, rest)
	case "start":
		return controlService("start", cfgPath, rest)
	case "stop":
		return controlService("stop", cfgPath, rest)
	case "restart":
		return controlService("restart", cfgPath, rest)
	case "status":
		return showStatus(cfgPath, rest)
	default:
		return fmt.Errorf("unknown command %q (run with --help for usage)", cmd)
	}
}

// runDefault handles invocation with no arguments. The OS service manager on
// Windows starts the binary without args when our service registration uses
// Arguments=[]; ours uses Arguments=["run"], so this branch is mostly hit by
// users running the binary by hand.
func runDefault() error {
	if !service.Interactive() {
		return runService("", nil)
	}
	fmt.Fprintf(os.Stderr, usage, version, config.DefaultFileName)
	return errors.New("no command specified")
}

func runService(cfgPath string, extra []string) error {
	if len(extra) > 0 {
		return fmt.Errorf("unexpected arguments: %v", extra)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	prog := &svcpkg.Program{Config: cfg}
	svc, err := service.New(prog, svcpkg.NewServiceConfig())
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	sysLogger, err := svc.SystemLogger(nil)
	if err != nil {
		return fmt.Errorf("system logger: %w", err)
	}

	closer, err := logging.Setup(cfg.Logging, logging.ModeAuto, sysLogger)
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	defer func() { _ = closer.Close() }()

	if err := svc.Run(); err != nil {
		return fmt.Errorf("service run: %w", err)
	}
	return nil
}

func runDebug(cfgPath string, extra []string) error {
	if len(extra) > 0 {
		return fmt.Errorf("unexpected arguments: %v", extra)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	closer, err := logging.Setup(cfg.Logging, logging.ModeStderr, nil)
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	defer func() { _ = closer.Close() }()

	prog := &svcpkg.Program{Config: cfg}
	svc, err := service.New(prog, svcpkg.NewServiceConfig())
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	if err := svc.Run(); err != nil {
		return fmt.Errorf("service run: %w", err)
	}
	return nil
}

// controlService executes an OS-service-manager control verb. The configuration
// is loaded only to validate it is parseable; install/uninstall don't strictly
// need it, but failing early with a config error is friendlier than registering
// a service that won't start.
func controlService(action, cfgPath string, extra []string) error {
	if len(extra) > 0 {
		return fmt.Errorf("unexpected arguments: %v", extra)
	}

	// For install/uninstall we don't require a config to exist; for everything
	// else we don't read config at all - it's the running service that does.
	// We still construct a kardianos service handle so we can talk to SCM.
	prog := &svcpkg.Program{} // never started here
	svc, err := service.New(prog, svcpkg.NewServiceConfig())
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	if action == "install" {
		// Best-effort warning if no config can be resolved yet.
		if _, err := config.Resolve(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			fmt.Fprintln(os.Stderr,
				"the service will fail to start until a config.json is placed next to the executable")
		}
	}

	if err := service.Control(svc, action); err != nil {
		return err
	}
	fmt.Printf("%sed %s\n", action, svcpkg.Name)
	return nil
}

func showStatus(_ string, extra []string) error {
	if len(extra) > 0 {
		return fmt.Errorf("unexpected arguments: %v", extra)
	}
	prog := &svcpkg.Program{}
	svc, err := service.New(prog, svcpkg.NewServiceConfig())
	if err != nil {
		return err
	}
	status, err := svc.Status()
	if err != nil {
		return err
	}
	switch status {
	case service.StatusRunning:
		fmt.Println("running")
	case service.StatusStopped:
		fmt.Println("stopped")
	default:
		fmt.Println("unknown")
	}
	return nil
}

// extractConfigFlag pulls out a --config / --config=PATH (or -config) flag
// from anywhere in args, returning the path and the remaining args.
//
// We do this manually because the flag package can't parse a global flag that
// appears after a subcommand name without subcommand-specific FlagSet plumbing.
func extractConfigFlag(args []string) (string, []string, error) {
	cfg := ""
	out := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 >= len(args) {
				return "", nil, errors.New("--config requires a path")
			}
			cfg = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			cfg = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-config="):
			cfg = strings.TrimPrefix(a, "-config=")
		default:
			out = append(out, a)
		}
	}
	return cfg, out, nil
}

// init silences the default flag package usage output; we render our own.
func init() {
	flag.CommandLine.SetOutput(io.Discard)
}
