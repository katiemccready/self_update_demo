package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/pack"
	"selfupdate/internal/update/install"
	"selfupdate/internal/update/manifest"
	"selfupdate/internal/update/transfer"
)

// defaultMaxSize guards against a publishing mistake, not an attacker:
// manifests are signed.
const defaultMaxSize = 1 << 30

var (
	errNoRelease         = errors.New("no release published")
	ErrBusy              = errors.New("another instance is applying an update")
	ErrTrialPending      = install.ErrTrialPending
	ErrNotNewer          = install.ErrNotNewer
	ErrNotManaged        = install.ErrNotManaged
	ErrInsufficientSpace = errors.New("not enough free disk space to stage the update")
)

type Config struct {
	BaseURL string
	Channel string
	Version string

	PublicKey  ed25519.PublicKey
	PublicKeys []ed25519.PublicKey

	RootDir    string
	GOOS       string
	GOARCH     string
	HTTPClient *http.Client
	Logger     *slog.Logger

	// BuildTime is the starting replay floor for a client with no saved state:
	// a manifest issued before the program existed is refused. Zero disables it.
	BuildTime time.Time

	AllowInsecure bool

	NoClientID bool

	StallTimeout time.Duration

	MaxSize int64

	Limits pack.Limits

	AllowUnsafeInstallDir bool

	Validate func(ctx context.Context, dir, program string, m manifest.Manifest) error
}

type Updater struct {
	cfg   Config
	keys  []ed25519.PublicKey
	cur   manifest.Version
	entry string
	log   *slog.Logger
	store *install.Store

	applyMu   sync.Mutex
	applyLock *fsutil.FileLock

	verifiedMu sync.Mutex
	verified   *manifest.Manifest

	lockWarned      atomic.Bool
	noReleaseLogged atomic.Bool

	now        func() time.Time
	exitFn     func(code int)
	retryDelay time.Duration
}

func New(cfg Config) (*Updater, error) {
	cur, err := manifest.ParseVersion(cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("current version: %w", err)
	}
	if err := manifest.ValidateChannel(cfg.Channel); err != nil {
		return nil, err
	}
	keys, err := cfg.trustedKeys()
	if err != nil {
		return nil, err
	}
	if err := cfg.validateBaseURL(); err != nil {
		return nil, err
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := manifest.ValidatePlatform(cfg.GOOS, cfg.GOARCH); err != nil {
		return nil, err
	}
	root := install.Root{Dir: cfg.RootDir}
	entry, err := root.ReadEntry()
	if err != nil {
		return nil, err
	}
	return &Updater{
		cfg: cfg, keys: keys, cur: cur, entry: entry, log: cfg.Logger,
		store:     install.Open(root, cfg.Logger),
		applyLock: fsutil.NewFileLock(root.ApplyLock()),
		exitFn:    os.Exit, retryDelay: 2 * time.Second, now: time.Now,
	}, nil
}

func (c Config) trustedKeys() ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	if len(c.PublicKey) > 0 {
		keys = append(keys, c.PublicKey)
	}
	keys = append(keys, c.PublicKeys...)
	if len(keys) == 0 {
		return nil, errors.New("missing public key")
	}
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return nil, errors.New("malformed public key")
		}
	}
	return keys, nil
}

func (c Config) validateBaseURL() error {
	if u, err := url.Parse(c.BaseURL); err != nil || u.Host == "" {
		return fmt.Errorf("invalid base URL %q", c.BaseURL)
	}
	return transfer.RequireSecureURL(c.BaseURL, c.AllowInsecure)
}

func (c *Config) applyDefaults() error {
	if c.GOOS == "" {
		c.GOOS = runtime.GOOS
	}
	if c.GOARCH == "" {
		c.GOARCH = runtime.GOARCH
	}
	if c.HTTPClient == nil {
		c.HTTPClient = transfer.NewHTTPClient(c.AllowInsecure)
	} else {
		c.HTTPClient = transfer.HardenClient(c.HTTPClient, c.AllowInsecure)
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = 30 * time.Second
	}
	if c.MaxSize <= 0 {
		c.MaxSize = defaultMaxSize
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Validate == nil {
		c.Validate = runVersionCheck
	}
	if c.Limits == (pack.Limits{}) {
		c.Limits = pack.DefaultLimits
	}
	if c.RootDir == "" {
		r, err := install.RootFromEnv()
		if err != nil {
			return err
		}
		c.RootDir = r.Dir
	}
	return nil
}

func (u *Updater) warnLockUnavailable(err error) {
	if u.lockWarned.CompareAndSwap(false, true) {
		u.log.Warn("cross-process locking unavailable; relying on a single running instance", "err", err)
	}
}

func (u *Updater) stager() *transfer.Stager {
	return &transfer.Stager{
		Dir: u.store.Root.Staging(), Client: u.cfg.HTTPClient, Log: u.log, UserAgent: u.userAgent(),
		StallTimeout: u.cfg.StallTimeout, RetryDelay: u.retryDelay,
	}
}

func releaseOf(m manifest.Manifest) transfer.Release {
	return transfer.Release{URL: m.URL, Size: m.Size, SHA256: m.SHA256}
}

func (u *Updater) userAgent() string { return "selfupdate/" + u.cur.String() }
