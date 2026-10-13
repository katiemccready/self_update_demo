package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"selfupdate/internal/appstate"
	"selfupdate/internal/update"
	"selfupdate/internal/update/manifest"
)

var (
	version          = "dev"
	publicKey        = ""
	defaultUpdateURL = ""
	devOverrides     = ""
	buildTime        = ""
)

type options struct {
	showVersion  bool
	updateURL    string
	channel      string
	interval     time.Duration
	confirmAfter time.Duration
	maxDeferral  time.Duration
	statePath    string
	stepDur      time.Duration
	mode         string
	runFor       time.Duration
	insecure     bool
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.BoolVar(&o.showVersion, "version", false, "print version and exit")
	fs.StringVar(&o.updateURL, "update-url", defaultUpdateURL, "DEV BUILDS ONLY: base URL of the release tree (production builds use the one compiled in)")
	fs.StringVar(&o.channel, "channel", "stable", "release channel")
	fs.DurationVar(&o.interval, "update-interval", time.Hour, "how often to check for updates")
	fs.DurationVar(&o.confirmAfter, "confirm-after", time.Minute, "uptime after which a new version is considered healthy")
	fs.DurationVar(&o.maxDeferral, "max-restart-deferral", 4*time.Hour, "longest an installed update may wait for the app to go idle before in-flight work is interrupted (state is saved first); 0 = wait forever")
	fs.StringVar(&o.statePath, "state", defaultStatePath(), "where application state is saved across restarts (required if the OS has no per-user config directory)")
	fs.DurationVar(&o.stepDur, "step", 2*time.Second, "duration of one step of the demo workload")
	fs.StringVar(&o.mode, "update-mode", "silent", `"silent": restart into updates automatically when idle; "prompt": ask first (mandatory updates are always silent)`)
	fs.DurationVar(&o.runFor, "run-for", 0, "exit gracefully after this long (demos and tests); 0 = run until signalled")
	fs.BoolVar(&o.insecure, "allow-insecure-updates", false, "DEV BUILDS ONLY: permit plain-http update URLs to non-loopback hosts")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.mode != "silent" && o.mode != "prompt" {
		return o, fmt.Errorf(`-update-mode must be "silent" or "prompt", got %q`, o.mode)
	}
	return o, nil
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	os.Exit(run(opts))
}

func run(opts options) int {
	if opts.showVersion {
		fmt.Println(version)
		return 0
	}
	log := slog.With("version", version)
	if opts.statePath == "" {
		log.Error("no per-user config directory on this system; pass -state PATH")
		return 2
	}
	url, insecure, warnings := lockdown(opts.updateURL, opts.insecure, defaultUpdateURL, devOverrides == "1")
	for _, w := range warnings {
		log.Warn(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.runFor)
		defer cancel()
	}

	up := newUpdater(log, url, opts.channel, insecure)

	if version != "dev" {
		dir, err := programDir()
		if err == nil {
			var greeting string
			if greeting, err = loadAssets(dir, version); err == nil {
				log.Info("program files loaded", "greeting", greeting)
			}
		}
		if err != nil {
			log.Error("this installation is damaged or incomplete", "err", err)
			return 1
		}
	}

	log.Info("agent starting")
	a, err := newApp(log, opts.statePath, opts.stepDur)
	if err != nil {
		log.Error("cannot load application state; exiting without touching it", "err", err)
		return failedStateLoad(log, up, err)
	}
	a.start(ctx)

	var updated <-chan manifest.Manifest
	if up != nil {
		updated = startUpdateLoop(ctx, log, up, opts)
	}
	select {
	case m := <-updated:
		restartIntoUpdate(ctx, log, up, a, opts, m)
	case <-ctx.Done():
	}
	return shutdown(log, a)
}

func failedStateLoad(log *slog.Logger, up *update.Updater, err error) int {
	if up == nil {
		return 1
	}
	if !errors.Is(err, appstate.ErrMigration) {
		return update.EnvironmentExitCode
	}
	rolledBack, rerr := up.ReportBadRelease(err)
	switch {
	case rerr != nil:
		log.Error("could not roll back after failed migration", "err", rerr)
	case rolledBack:
		log.Warn("rolled back to previous version after failed state migration; restarting")
		up.Restart()
	}
	return 1
}

func startUpdateLoop(ctx context.Context, log *slog.Logger, up *update.Updater, opts options) <-chan manifest.Manifest {
	updated := make(chan manifest.Manifest, 1)
	time.AfterFunc(opts.confirmAfter, func() {
		if err := up.Confirm(); err != nil {
			log.Warn("confirm", "err", err)
		}
	})
	go func() {
		m, err := up.Run(ctx, opts.interval)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("update loop stopped", "err", err)
			}
			return
		}
		updated <- *m
	}()
	return updated
}

func restartIntoUpdate(ctx context.Context, log *slog.Logger, up *update.Updater, a *app, opts options, m manifest.Manifest) {
	notes := sanitizeNotes(m.Notes)
	log.Info("update installed", "new", m.Version, "mandatory", m.Mandatory, "notes", notes)
	policy := update.RestartPolicy{
		Quiesce:     a.Quiesce,
		SaveState:   a.Save,
		Resume:      a.Resume,
		Poll:        500 * time.Millisecond,
		MaxDeferral: opts.maxDeferral,
	}
	if opts.mode == "prompt" && !m.Mandatory {
		approved := askUser(log, m.Version, notes)
		policy.MaxDeferral = 0
		policy.Quiesce = func(force bool) bool { return approved.Load() && a.Quiesce(force) }
	}
	if err := up.RestartWhenIdle(ctx, policy); err != nil && ctx.Err() == nil {
		log.Error("restart failed", "err", err)
	}
}

func shutdown(log *slog.Logger, a *app) int {
	log.Info("shutting down")
	a.stop()
	if err := a.Save(); err != nil {
		log.Error("final save failed", "err", err)
		return 1
	}
	return 0
}

func lockdown(url string, insecure bool, builtURL string, dev bool) (string, bool, []string) {
	if dev {
		return url, insecure, nil
	}
	var warnings []string
	if url != builtURL {
		warnings = append(warnings, "ignoring -update-url: only honoured in dev builds (build with -X main.devOverrides=1)")
		url = builtURL
	}
	if insecure {
		warnings = append(warnings, "ignoring -allow-insecure-updates: only honoured in dev builds")
		insecure = false
	}
	return url, insecure, warnings
}

func sanitizeNotes(s string) string {
	const maxNotes = 4000
	clean := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	if len(clean) > maxNotes {
		cut := maxNotes
		for cut > 0 && !utf8.RuneStart(clean[cut]) {
			cut--
		}
		clean = clean[:cut] + "…"
	}
	return clean
}

func askUser(log *slog.Logger, ver, notes string) *atomic.Bool {
	var approved atomic.Bool
	fmt.Fprintf(os.Stderr, "\nUpdate %s is installed and ready.\n%s\nType 'restart' + Enter to switch now (or just keep working; it applies at next launch).\n", ver, notes)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if strings.EqualFold(strings.TrimSpace(sc.Text()), "restart") {
				log.Info("user approved restart")
				approved.Store(true)
				return
			}
		}
	}()
	return &approved
}

func defaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "selfupdate-agent", "state.json")
}

func newUpdater(log *slog.Logger, url, channel string, insecure bool) *update.Updater {
	if url == "" {
		log.Info("updates disabled (no update URL / public key)")
		return nil
	}
	if publicKey == "" {
		log.Warn("updates disabled: an update URL is set but no public key was compiled in (build with -X main.publicKey=...)")
		return nil
	}
	keys, err := parsePublicKeys(publicKey)
	if err != nil {
		log.Error("bad embedded public key", "err", err)
		return nil
	}
	built, err := parseBuildTime(buildTime)
	if err != nil {
		log.Error("ignoring malformed build time; fresh installs get no replay floor", "err", err)
	}
	up, err := update.New(update.Config{
		BaseURL: url, Channel: channel, Version: version, BuildTime: built,
		PublicKey: keys[0], PublicKeys: keys[1:], Logger: slog.With("component", "updater"),
		AllowInsecure: insecure,
	})
	if errors.Is(err, update.ErrNotManaged) {
		log.Info("updates disabled: not started by the launcher, so there is no install root to update")
		return nil
	}
	if err != nil {
		log.Warn("updates disabled", "reason", err)
		return nil
	}
	return up
}

func parsePublicKeys(s string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, part := range strings.Split(s, ",") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

func parseBuildTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("build time %q: %w", s, err)
	}
	return t, nil
}
