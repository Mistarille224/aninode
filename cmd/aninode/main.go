package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aninode/internal/application"
	"aninode/internal/configstore"
	"aninode/internal/filesystem"
	"aninode/internal/server"
)

var version = "dev"
var commit = "unknown"
var buildDate = "unknown"

const (
	defaultConfigDir = "/config"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("aninode failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("a command is required: init, preflight, serve, setup-code, check, run, migrate, or version")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "preflight":
		return runPreflight(args[1:])
	case "serve":
		return runServe(args[1:])
	case "setup-code":
		return runSetupCode(args[1:])
	case "check":
		return runCheck(args[1:])
	case "run":
		return runCycle(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "version":
		return runVersion(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runPreflight(args []string) error {
	set := flag.NewFlagSet("preflight", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("preflight: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	cfg, err := configstore.LoadOrganizer(*configDir)
	if err != nil {
		return err
	}
	ready, err := verifyMediaPaths(cfg.Source, cfg.Target)
	if err != nil {
		return err
	}
	if !ready {
		slog.Warn("media preflight deferred until configured paths exist", "source", cfg.Source, "target", cfg.Target)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":        true,
		"ready":     ready,
		"source":    cfg.Source,
		"target":    cfg.Target,
		"hardlinks": ready,
	})
}

// verifyMediaPaths validates existing media roots without creating them. Missing
// roots are a normal first-run state: the setup UI must remain reachable so the
// user can choose the paths that are actually mounted into the container.
func verifyMediaPaths(source, target string) (bool, error) {
	paths := []struct {
		name string
		path string
	}{{"source", source}, {"target", target}}
	for _, item := range paths {
		if strings.TrimSpace(item.path) == "" || !filepath.IsAbs(item.path) {
			return false, fmt.Errorf("media %s path must be absolute: %q", item.name, item.path)
		}
		info, err := os.Lstat(item.path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect media %s path %s: %w", item.name, item.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("media %s path %s is not a real directory", item.name, item.path)
		}
	}
	if err := filesystem.ValidateHardlinkDomain(source, target); err != nil {
		return false, fmt.Errorf("media paths are not hardlink-compatible: %w", err)
	}
	for _, item := range paths {
		if _, err := os.ReadDir(item.path); err != nil {
			return false, fmt.Errorf("media %s path %s is not readable: %w", item.name, item.path, err)
		}
	}

	probe, err := os.CreateTemp(source, ".aninode-preflight-*")
	if err != nil {
		return false, fmt.Errorf("media source path %s is not writable: %w", source, err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		os.Remove(probePath)
		return false, fmt.Errorf("close media preflight probe %s: %w", probePath, err)
	}
	defer os.Remove(probePath)

	linkPath := filepath.Join(target, filepath.Base(probePath)+".link")
	defer os.Remove(linkPath)
	if err := os.Link(probePath, linkPath); err != nil {
		return false, fmt.Errorf("media source %s and target %s cannot create hardlinks; ensure both are writable and on the same filesystem: %w", source, target, err)
	}
	return true, nil
}

func runServe(args []string) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	listen := set.String("listen", "127.0.0.1:7391", "HTTP listen address")
	interval := set.Duration("interval", 15*time.Minute, "remote RSS/full reconciliation interval (0 disables periodic remote observation)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("serve: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	app, err := application.Open(application.Options{ConfigRoot: *configDir})
	if err != nil {
		return fmt.Errorf("open application: %w", err)
	}
	auth, err := server.OpenWebAuth(*configDir)
	if err != nil {
		return fmt.Errorf("open login settings: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service := server.Service{App: app, Auth: auth, Logger: slog.Default()}
	return service.Serve(ctx, *listen, *interval)
}

func runSetupCode(args []string) error {
	set := flag.NewFlagSet("setup-code", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("setup-code: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	code, expires, err := server.SetupCode(*configDir)
	if err != nil {
		return fmt.Errorf("get setup code: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"setup_code": code, "expires_at": expires})
}

func runInit(args []string) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	source := set.String("source", "", "initial media source root; used only when organizer.json is first created")
	target := set.String("target", "", "initial media library root; used only when organizer.json is first created")
	extensions := set.String("extensions", "", "comma-separated initial media extensions; used only when organizer.json is first created")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("init: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	var extensionList []string
	if strings.TrimSpace(*extensions) != "" {
		extensionList = strings.Split(*extensions, ",")
	}
	created, err := configstore.Bootstrap(*configDir, configstore.BootstrapOptions{Source: *source, Target: *target, Extensions: extensionList})
	if err != nil {
		return fmt.Errorf("initialize configuration directory: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"created": created, "config_dir": *configDir})
}

func runCheck(args []string) error {
	set := flag.NewFlagSet("check", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("check: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	bundle, err := configstore.Load(*configDir)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	summary := map[string]int{"clients": len(bundle.Clients), "sources": len(bundle.Sources), "entries": len(bundle.Entries)}
	return json.NewEncoder(os.Stdout).Encode(summary)
}

func runCycle(args []string) error {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	acquire := set.Bool("acquire", true, "submit selected releases")
	reconcile := set.Bool("reconcile", true, "publish completed owned downloads")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("run: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	app, err := application.Open(application.Options{ConfigRoot: *configDir})
	if err != nil {
		return fmt.Errorf("open application: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, cycleErr := app.Cycle(ctx, application.CycleOptions{Acquire: *acquire, Reconcile: *reconcile})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("encode cycle result: %w", err)
	}
	return cycleErr
}

func runMigrate(args []string) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	configDir := set.String("config-dir", defaultConfigDir, "configuration directory")
	apply := set.Bool("apply", false, "persist and apply unambiguous migration/adoption plans")
	var groups repeatedFlag
	set.Var(&groups, "group", "exact confirmation key from a prior migration plan (repeatable; required with --apply)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("migrate: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	app, err := application.Open(application.Options{ConfigRoot: *configDir})
	if err != nil {
		return fmt.Errorf("open application: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result application.MigrationResult
	var migrationErr error
	if *apply {
		if len(groups) == 0 {
			return errors.New("migrate --apply requires at least one --group confirmation key from a current plan")
		}
		result, migrationErr = app.MigrationApplyGroups(ctx, groups)
	} else {
		result, migrationErr = app.MigrationPlan(ctx)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("encode migration result: %w", err)
	}
	return migrationErr
}

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty value")
	}
	*values = append(*values, value)
	return nil
}

func runVersion(args []string) error {
	set := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("version: unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	fmt.Printf("aninode %s commit=%s built=%s\n", version, commit, buildDate)
	return nil
}
