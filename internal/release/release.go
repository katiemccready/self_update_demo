package release

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/pack"
	"selfupdate/internal/update/manifest"
)

type Options struct {
	Root     string
	Channel  string
	Version  string
	OS, Arch string

	PackagePath string

	Entry string

	Name      string
	Key       ed25519.PrivateKey
	Notes     string
	Mandatory bool

	Validity time.Duration
	Now      func() time.Time

	Force bool

	StagedPercent *int
}

const DefaultValidity = 30 * 24 * time.Hour

const PackageExt = ".tar.gz"

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateTarget(channel, goos, goarch string) error {
	if err := manifest.ValidateChannel(channel); err != nil {
		return err
	}
	return manifest.ValidatePlatform(goos, goarch)
}

func (o Options) validate() error {
	if err := validateTarget(o.Channel, o.OS, o.Arch); err != nil {
		return err
	}
	if !nameRe.MatchString(o.Name) {
		return fmt.Errorf("invalid package name %q", o.Name)
	}
	if o.PackagePath == "" {
		return errors.New("no package to publish")
	}
	return nil
}

func targetDir(root, channel, goos, goarch string) string {
	return filepath.Join(root, filepath.FromSlash(manifest.TargetPath(channel, goos, goarch)))
}

func Publish(o Options) (manifest.Manifest, error) {
	if err := o.validate(); err != nil {
		return manifest.Manifest{}, err
	}
	nv, err := manifest.ParseVersion(o.Version)
	if err != nil {
		return manifest.Manifest{}, err
	}
	name := o.Name + PackageExt
	dir := targetDir(o.Root, o.Channel, o.OS, o.Arch)
	manifestPath := filepath.Join(dir, manifest.FileName)
	unlock, err := lockTarget(dir, true)
	if err != nil {
		return manifest.Manifest{}, err
	}
	defer unlock()

	if !o.Force {
		if err := requireNewer(manifestPath, nv); err != nil {
			return manifest.Manifest{}, err
		}
	}
	if err := inspectPackage(o.PackagePath, o.Entry); err != nil {
		return manifest.Manifest{}, err
	}
	sum, size, err := fsutil.HashFile(o.PackagePath)
	if err != nil {
		return manifest.Manifest{}, err
	}
	issued, expires := validityWindow(o.Now, o.Validity, previousIssuedAt(manifestPath))
	m := manifest.Manifest{
		Version: nv.String(), Channel: o.Channel, OS: o.OS, Arch: o.Arch,
		SHA256: sum, Size: size, URL: nv.String() + "/" + name,
		Notes: o.Notes, Mandatory: o.Mandatory, IssuedAt: issued, Expires: expires,
	}
	env, err := manifest.Sign(o.Key, m)
	if err != nil {
		return manifest.Manifest{}, err
	}

	encoded, err := encodeEnvelope(env)
	if err != nil {
		return manifest.Manifest{}, err
	}

	pkgDir := filepath.Join(dir, nv.String())
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return m, err
	}
	if err := publishPackage(o.PackagePath, filepath.Join(pkgDir, name), sum, size); err != nil {
		return m, err
	}

	if err := stageRollout(dir, manifestPath, nv, o.StagedPercent); err != nil {
		return m, err
	}
	return m, writeManifest(manifestPath, encoded)
}

func stageRollout(dir, manifestPath string, nv manifest.Version, percent *int) error {
	if old, err := os.ReadFile(manifestPath); err == nil {
		if pv, perr := publishedVersion(old); perr == nil && pv.Compare(nv) != 0 {
			if err := fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(filepath.Join(dir, previousFile), old, 0o644)); err != nil {
				return fmt.Errorf("keep previous manifest: %w", err)
			}
		}
	}
	if percent != nil {
		return writeRollout(dir, Rollout{Version: nv.String(), Percent: *percent})
	}
	if r, ok, err := readRollout(dir); err == nil && ok && r.Version != nv.String() {
		_ = os.Remove(filepath.Join(dir, rolloutFile))
	}
	return nil
}

func encodeEnvelope(env manifest.Envelope) ([]byte, error) {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(b) > manifest.MaxBytes {
		return nil, fmt.Errorf("signed manifest is %d bytes; clients read at most %d (shorten the release notes)",
			len(b), manifest.MaxBytes)
	}
	return b, nil
}

func writeManifest(path string, encoded []byte) error {
	return fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(path, encoded, 0o644))
}

func requireNewer(manifestPath string, nv manifest.Version) error {
	b, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read published manifest: %w (use -force to override)", err)
	}
	pv, err := publishedVersion(b)
	if err != nil {
		return fmt.Errorf("existing %s is unreadable (%v); repair or remove it, or use -force", manifestPath, err)
	}
	if nv.Compare(pv) <= 0 {
		return fmt.Errorf("version %s is not newer than published %s (use -force to override)", nv, pv)
	}
	return nil
}

func inspectPackage(path, entry string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := pack.Inspect(f, entry, pack.DefaultLimits); err != nil {
		return fmt.Errorf("%s is not a package clients would accept: %w", path, err)
	}
	return nil
}

func publishPackage(src, dst, sum string, size int64) error {
	have, _, err := fsutil.HashFile(dst)
	switch {
	case err == nil && have == sum:
		return nil
	case err == nil:
		return fmt.Errorf("%s already exists with different contents; published versions are immutable, bump the version", dst)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := fsutil.IgnoreDirSync(fsutil.CopyFileAtomic(src, dst, 0o644)); err != nil {
		return err
	}

	got, n, err := fsutil.HashFile(dst)
	if err != nil {
		return err
	}
	if got != sum || n != size {
		_ = os.Remove(dst)
		return fmt.Errorf("%s changed while publishing; nothing was released", src)
	}
	return nil
}

func validityWindow(now func() time.Time, validity time.Duration, prev time.Time) (issued, expires time.Time) {
	if now == nil {
		now = time.Now
	}
	if validity <= 0 {
		validity = DefaultValidity
	}
	issued = now().UTC().Truncate(time.Second)
	if !issued.After(prev) {
		issued = prev.Add(time.Second)
	}
	return issued, issued.Add(validity)
}

func previousIssuedAt(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	prev, err := decodeUnverified(b)
	if err != nil {
		return time.Time{}
	}
	return prev.IssuedAt
}

type RefreshOptions struct {
	Root, Channel, OS, Arch string
	Key                     ed25519.PrivateKey
	Validity                time.Duration
	Now                     func() time.Time
}

func Refresh(o RefreshOptions) (manifest.Manifest, error) {
	if err := validateTarget(o.Channel, o.OS, o.Arch); err != nil {
		return manifest.Manifest{}, err
	}
	if len(o.Key) != ed25519.PrivateKeySize {
		return manifest.Manifest{}, errors.New("invalid private key")
	}
	dir := targetDir(o.Root, o.Channel, o.OS, o.Arch)
	path := filepath.Join(dir, manifest.FileName)
	unlock, err := lockTarget(dir, false)
	if err != nil {
		return manifest.Manifest{}, err
	}
	defer unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("read published manifest: %w", err)
	}
	var env manifest.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	m, err := manifest.Open(publicKeys(o.Key), env)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("%s was not signed by this key (%w); use publish, not refresh", path, err)
	}

	if m.Channel != o.Channel || m.OS != o.OS || m.Arch != o.Arch {
		return manifest.Manifest{}, fmt.Errorf("%s describes %s/%s_%s, not %s/%s_%s; refusing to refresh a misplaced manifest",
			path, m.Channel, m.OS, m.Arch, o.Channel, o.OS, o.Arch)
	}
	if err := resign(o, &m, path); err != nil {
		return m, err
	}

	if err := refreshPrevious(o, dir); err != nil {
		return m, fmt.Errorf("latest manifest refreshed, but %s was not: %w", previousFile, err)
	}
	return m, nil
}

func publicKeys(priv ed25519.PrivateKey) []ed25519.PublicKey {
	pub, _ := priv.Public().(ed25519.PublicKey)
	return []ed25519.PublicKey{pub}
}

func resign(o RefreshOptions, m *manifest.Manifest, path string) error {
	m.IssuedAt, m.Expires = validityWindow(o.Now, o.Validity, m.IssuedAt)
	env, err := manifest.Sign(o.Key, *m)
	if err != nil {
		return err
	}
	encoded, err := encodeEnvelope(env)
	if err != nil {
		return err
	}
	return writeManifest(path, encoded)
}

func refreshPrevious(o RefreshOptions, dir string) error {
	path := filepath.Join(dir, previousFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var env manifest.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	m, err := manifest.Open(publicKeys(o.Key), env)
	if err != nil {
		return fmt.Errorf("%s was not signed by this key: %w", path, err)
	}
	if m.Channel != o.Channel || m.OS != o.OS || m.Arch != o.Arch {
		return fmt.Errorf("%s describes another target; refusing to refresh it", path)
	}
	return resign(o, &m, path)
}

func decodeUnverified(b []byte) (manifest.Manifest, error) {
	var env manifest.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return manifest.Manifest{}, err
	}
	var m manifest.Manifest
	err := json.Unmarshal(env.Payload, &m)
	return m, err
}

func publishedVersion(b []byte) (manifest.Version, error) {
	m, err := decodeUnverified(b)
	if err != nil {
		return manifest.Version{}, err
	}
	return manifest.ParseVersion(m.Version)
}

var publishLockTimeout = 30 * time.Second

func lockTarget(dir string, create bool) (unlock func(), err error) {
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(dir); err != nil {
		return func() {}, nil
	}
	l := fsutil.NewFileLock(filepath.Join(dir, ".publish.lock"))
	switch err := l.Lock(publishLockTimeout); {
	case err == nil:
		return l.Unlock, nil
	case errors.Is(err, fsutil.ErrLockHeld):
		return nil, fmt.Errorf("another publish or refresh is running on %s: %w", dir, err)
	default:
		return func() {}, nil
	}
}
