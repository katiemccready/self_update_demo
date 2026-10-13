package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/pack"
	"selfupdate/internal/update/install"
	"selfupdate/internal/update/manifest"
	"selfupdate/internal/update/transfer"
)

const unpackPrefix = "unpack-"

func (u *Updater) Apply(ctx context.Context, m manifest.Manifest) error {
	u.applyMu.Lock()
	defer u.applyMu.Unlock()

	if err := u.checkApplicable(m); err != nil {
		return err
	}
	switch err := u.applyLock.TryLock(); {
	case err == nil:
		defer u.applyLock.Unlock()
	case errors.Is(err, fsutil.ErrLockHeld):
		return ErrBusy
	default:
		u.warnLockUnavailable(err)
	}
	nv, _ := manifest.ParseVersion(m.Version)
	if err := u.preflight(); err != nil {
		return err
	}
	if err := u.store.RequireNoTrial(); err != nil {
		return err
	}
	if err := u.store.RequireNewerThanInstalled(nv); err != nil {
		return err
	}
	if err := u.checkDiskSpace(m); err != nil {
		return err
	}

	partial, _ := u.stager().Paths(releaseOf(m))
	u.sweepStaging(partial)
	pkgPath, err := u.stager().Download(ctx, releaseOf(m))
	if err != nil {
		if errors.Is(err, transfer.ErrCorrupt) {
			u.store.Reject(nv, u.now())
		}
		return err
	}
	defer os.Remove(pkgPath)

	dir, err := os.MkdirTemp(u.store.Root.Staging(), unpackPrefix)
	if err != nil {
		return fmt.Errorf("create unpack directory: %w", err)
	}
	defer os.RemoveAll(dir)

	tree, err := u.unpack(pkgPath, dir, m)
	if err != nil {
		if ctx.Err() == nil && (errors.Is(err, pack.ErrUnsafeArchive) || errors.Is(err, transfer.ErrCorrupt)) {
			u.store.Reject(nv, u.now())
		}
		return err
	}

	program := filepath.Join(dir, filepath.FromSlash(u.entry))
	if err := u.cfg.Validate(ctx, dir, program, m); err != nil {
		// Deterministic failures (wrong build, wrong platform) would otherwise
		// re-download the package on every poll. Transient ones (shutdown,
		// ETXTBSY) say nothing about the release and must not park it.
		if ctx.Err() == nil && !isTextBusy(err) {
			u.store.Reject(nv, u.now())
		}
		return fmt.Errorf("validate new version: %w", err)
	}

	if err := tree.Prune(dir); err != nil {
		return fmt.Errorf("clean the unpacked version: %w", err)
	}
	if err := tree.Verify(dir); err != nil {
		u.store.Reject(nv, u.now())
		return err
	}
	return u.store.Install(dir, nv, u.cfg.AllowUnsafeInstallDir)
}

// unpack extracts the staged package into dir, hashing the bytes again as they
// are read. pack.Extract reads to the end of the file and refuses trailing
// data, so the hash covers every byte: what is unpacked is what the signed
// manifest described, even if the staged file was swapped after the download
// was verified.
func (u *Updater) unpack(pkgPath, dir string, m manifest.Manifest) (pack.Tree, error) {
	if err := transfer.CheckStagingFile(pkgPath); err != nil {
		return pack.Tree{}, err
	}
	f, err := os.Open(pkgPath)
	if err != nil {
		return pack.Tree{}, err
	}
	defer f.Close()
	h := &countingHash{Hash: sha256.New()}
	tree, err := pack.Extract(io.TeeReader(f, h), dir, u.entry, u.cfg.Limits)
	if err != nil {
		return pack.Tree{}, fmt.Errorf("unpack release: %w", err)
	}
	if sum, n := hex.EncodeToString(h.Sum(nil)), h.n; sum != m.SHA256 || n != m.Size {
		return pack.Tree{}, fmt.Errorf("%w: package changed after verification (sha256 %s, size %d)", transfer.ErrCorrupt, sum, n)
	}
	return tree, nil
}

type countingHash struct {
	hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.Hash.Write(p)
}

// preflight runs before anything is downloaded, so a root that can never work
// (or is unsafe) fails in milliseconds with an actionable message.
//
// The updater unpacks the new version inside the root, executes it (smoke test)
// and installs it. Anyone who can write the root can swap files between the hash
// check and those steps, which is a local privilege escalation if the updater
// runs as root. The supported deployment is a root writable only by the user
// the updater runs as (or by root, when root runs it).
func (u *Updater) preflight() error {
	if err := install.CheckContext(u.store.Root.Dir, u.cfg.AllowUnsafeInstallDir); err != nil {
		return err
	}
	notWritable := func(err error) error {
		return fmt.Errorf("install root %s is not writable by this user, so this process cannot update itself "+
			"(install somewhere the user owns, or use a privileged helper): %w", u.store.Root.Dir, err)
	}
	if err := os.MkdirAll(u.store.Root.Staging(), 0o755); err != nil {
		return notWritable(err)
	}
	probe, err := os.CreateTemp(u.store.Root.Staging(), ".probe-*")
	if err != nil {
		return notWritable(err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

func (u *Updater) sweepStaging(keep string) {
	staging := u.store.Root.Staging()
	fsutil.SweepTemp(u.store.Root.Dir, time.Hour)
	u.stager().Sweep(keep)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), unpackPrefix) {
			_ = os.RemoveAll(filepath.Join(staging, e.Name()))
		}
	}
}

const (
	spaceMargin = 1 << 20

	unpackedFactor = 3
)

var freeBytesFn = freeBytes

func (u *Updater) checkDiskSpace(m manifest.Manifest) error {
	need := uint64(max(m.Size-u.stager().StagedBytes(releaseOf(m)), 0)) +
		uint64(m.Size)*unpackedFactor + spaceMargin
	dir := u.store.Root.Dir
	free, err := freeBytesFn(dir)
	if err != nil {
		u.log.Debug("cannot measure free disk space; skipping the check", "err", err)
		return nil
	}
	if free < need {
		return fmt.Errorf("%w: need about %d bytes in %s, %d available", ErrInsufficientSpace, need, dir, free)
	}
	return nil
}
