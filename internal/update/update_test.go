package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfupdate/internal/pack"
	"selfupdate/internal/testutil"
	"selfupdate/internal/update/manifest"
	"selfupdate/internal/update/transfer"
)

func TestApplySwitchesAllFilesTogether(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	if err := u.Apply(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := f.current(); got != "1.1.0" {
		t.Fatalf("current = %s", got)
	}
	f.assertWholeVersion("1.1.0")
	f.assertWholeVersion("1.0.0")
	if left := f.stagingLeftovers(); len(left) != 0 {
		t.Errorf("staging not cleaned: %v", left)
	}
	st := u.store.Load()
	if st.Pending != "1.1.0" || st.Previous != "1.0.0" || st.Attempts != 0 {
		t.Errorf("state = %+v, want a trial of 1.1.0 with 1.0.0 as the rollback target", st)
	}
}

func TestInstalledProgramIsExecutable(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("no permission bits on Windows")
	}
	f, u := newFixture(t, "1.0.0", "1.1.0")
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(f.root.VersionDir(testutil.Ver(t, "1.1.0")), entryName))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("entry mode = %v, %v; want owner-executable", fi.Mode(), err)
	}
}

func TestNoUpdateWhenCurrentOrOlder(t *testing.T) {
	for _, newV := range []string{"1.0.0", "0.9.0"} {
		_, u := newFixture(t, "1.0.0", newV)
		if m, err := u.Check(context.Background()); m != nil || err != nil {
			t.Errorf("manifest %s: Check = %v, %v; want nil, nil", newV, m, err)
		}
	}
}

func TestRejectsWrongTarget(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	f.manifest.OS = "windows"
	f.sign()
	if _, err := u.Check(context.Background()); err == nil {
		t.Error("accepted manifest for another OS")
	}
}

func TestRejectsBadSignature(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.env, _ = manifest.Sign(otherPriv, f.manifest)
	if _, err := u.Check(context.Background()); err == nil {
		t.Error("accepted manifest signed by wrong key")
	}
}

func TestApplyRefusesAManifestCheckDidNotReturn(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	m.Size++
	if err := u.Apply(context.Background(), m); err == nil || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("Apply(tampered) = %v", err)
	}
	if f.current() != "1.0.0" {
		t.Error("pointer moved")
	}
}

func TestRejectsExpiredAndReplayedManifests(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	f.manifest.Expires = time.Now().Add(-time.Minute)
	f.sign()
	if _, err := u.Check(context.Background()); !errors.Is(err, manifest.ErrStaleManifest) {
		t.Errorf("expired: err = %v", err)
	}

	f.manifest.Expires = time.Now().Add(time.Hour)
	f.sign()
	f.check(u)
	f.manifest.IssuedAt = f.manifest.IssuedAt.Add(-time.Hour)
	f.sign()
	if _, err := u.Check(context.Background()); !errors.Is(err, manifest.ErrReplayedManifest) {
		t.Errorf("replayed: err = %v", err)
	}
}

func TestRejectsCorruptDownload(t *testing.T) {
	good := buildPackage(t, versionFiles("1.1.0"))
	cases := map[string]func() []byte{
		"same size, different bytes": func() []byte {
			b := append([]byte(nil), good...)
			b[len(b)/2] ^= 0xff
			return b
		},
		"truncated": func() []byte { return good[:len(good)/2] },
		"oversized": func() []byte { return append(append([]byte(nil), good...), "extra"...) },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f, u := newFixture(t, "1.0.0", "1.1.0")
			m := f.check(u)
			f.mu.Lock()
			f.pkg = mk()
			f.mu.Unlock()
			if err := u.Apply(context.Background(), m); err == nil {
				t.Fatal("corrupt package accepted")
			}
			if f.current() != "1.0.0" {
				t.Errorf("pointer moved to %s", f.current())
			}
			if dirs := f.versionDirs(); len(dirs) != 1 {
				t.Errorf("versions/ = %v, want only 1.0.0", dirs)
			}

			if left := f.stagingLeftovers(); len(left) != 0 {
				t.Errorf("leftovers: %v", left)
			}

			_, parked := u.store.Load().RejectedUntil("1.1.0", u.now())
			if want := name != "truncated"; parked != want {
				t.Errorf("rejected = %v, want %v", parked, want)
			}
		})
	}
}

func TestHostilePackagesAreRefusedAndParked(t *testing.T) {
	cases := map[string][]byte{
		"path traversal": testutil.Archive(t,
			testutil.Member{Name: entryName, Body: "x", Mode: 0o755},
			testutil.Member{Name: "../escaped", Body: "pwned"}),
		"absolute path": testutil.Archive(t,
			testutil.Member{Name: entryName, Body: "x", Mode: 0o755},
			testutil.Member{Name: "/tmp/escaped", Body: "pwned"}),
		"symlink": testutil.Archive(t,
			testutil.Member{Name: entryName, Body: "x", Mode: 0o755},
			testutil.Member{Name: "link", Type: 2, Link: "/etc/passwd"}),
		"hard link": testutil.Archive(t,
			testutil.Member{Name: entryName, Body: "x", Mode: 0o755},
			testutil.Member{Name: "link", Type: 1, Link: entryName}),
		"no entry point": testutil.Archive(t,
			testutil.Member{Name: "readme", Body: "x"}),
		"duplicate by case": testutil.Archive(t,
			testutil.Member{Name: entryName, Body: "x", Mode: 0o755},
			testutil.Member{Name: "Data", Body: "1"}, testutil.Member{Name: "data", Body: "2"}),
	}
	for name, pkg := range cases {
		t.Run(name, func(t *testing.T) {
			f, u := newFixture(t, "1.0.0", "1.1.0")
			f.release("1.1.0", pkg)
			m := f.check(u)
			err := u.Apply(context.Background(), m)
			if !errors.Is(err, pack.ErrUnsafeArchive) {
				t.Fatalf("Apply = %v, want ErrUnsafeArchive", err)
			}
			if f.current() != "1.0.0" || len(f.versionDirs()) != 1 {
				t.Errorf("current=%s versions=%v", f.current(), f.versionDirs())
			}
			if left := f.stagingLeftovers(); len(left) != 0 {
				t.Errorf("leftovers: %v", left)
			}
			if _, parked := u.store.Load().RejectedUntil("1.1.0", u.now()); !parked {
				t.Error("a hostile package was not parked; it would be re-downloaded every poll")
			}
			parent := filepath.Dir(f.root.Dir)
			for _, p := range []string{"escaped", filepath.Join(parent, "escaped")} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was created", p)
				}
			}
		})
	}
}

func TestUnpackedSizeLimitIsEnforced(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.Limits = pack.Limits{MaxFiles: 100, MaxBytes: 10, MaxPathLen: 100}
	m := f.check(u)
	if err := u.Apply(context.Background(), m); !errors.Is(err, pack.ErrUnsafeArchive) {
		t.Fatalf("Apply = %v, want ErrUnsafeArchive", err)
	}
	if f.current() != "1.0.0" {
		t.Error("pointer moved")
	}
}

func TestOversizedManifestIsRefused(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.MaxSize = 10
	_ = f
	if _, err := u.Check(context.Background()); err == nil {
		t.Error("manifest announcing a package over MaxSize was accepted")
	}
}

func TestValidateFailureLeavesCurrentUntouchedAndParksTheRelease(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.Validate = func(context.Context, string, string, manifest.Manifest) error { return errors.New("segfault") }
	m := f.check(u)
	if err := u.Apply(context.Background(), m); err == nil || !strings.Contains(err.Error(), "segfault") {
		t.Fatalf("Apply = %v", err)
	}
	if f.current() != "1.0.0" || len(f.versionDirs()) != 1 {
		t.Errorf("current=%s versions=%v", f.current(), f.versionDirs())
	}
	if left := f.stagingLeftovers(); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
	if _, parked := u.store.Load().RejectedUntil("1.1.0", u.now()); !parked {
		t.Error("deterministic validation failure was not parked")
	}
}

func TestCancelledValidationDoesNotParkTheRelease(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	ctx, cancel := context.WithCancel(context.Background())
	u.cfg.Validate = func(context.Context, string, string, manifest.Manifest) error { cancel(); return ctx.Err() }
	m := f.check(u)
	if err := u.Apply(ctx, m); err == nil {
		t.Fatal("expected error")
	}
	if _, parked := u.store.Load().RejectedUntil("1.1.0", u.now()); parked {
		t.Error("a shutdown parked a healthy release")
	}
}

func TestFilesAddedBySmokeTestAreNotInstalled(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.Validate = func(_ context.Context, dir, _ string, _ manifest.Manifest) error {
		if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "cache", "junk"), []byte("x"), 0o644)
	}
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	f.assertWholeVersion("1.1.0")
	if _, err := os.Stat(filepath.Join(f.root.VersionDir(testutil.Ver(t, "1.1.0")), "cache")); err == nil {
		t.Error("a file the smoke test created was installed")
	}
}

func TestPackagedFileChangedBySmokeTestIsNotInstalled(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.Validate = func(_ context.Context, dir, _ string, _ manifest.Manifest) error {
		return os.WriteFile(filepath.Join(dir, "lib", "engine.dat"), []byte("tampered"), 0o644)
	}
	if err := u.Apply(context.Background(), f.check(u)); err == nil {
		t.Fatal("a modified tree was installed")
	}
	if f.current() != "1.0.0" || len(f.versionDirs()) != 1 {
		t.Errorf("current=%s versions=%v", f.current(), f.versionDirs())
	}
}

func TestSmokeTestRunsInTheUnpackedDirectory(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	var gotDir, gotProgram string
	u.cfg.Validate = func(_ context.Context, dir, program string, _ manifest.Manifest) error {
		gotDir, gotProgram = dir, program
		_, err := os.Stat(program)
		return err
	}
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(gotProgram) != gotDir || filepath.Base(gotProgram) != entryName {
		t.Errorf("dir=%s program=%s", gotDir, gotProgram)
	}
	if !strings.HasPrefix(gotDir, f.root.Staging()) {
		t.Errorf("candidate ran outside staging/: %s", gotDir)
	}
}

func TestApplyWaitsWhileAnEarlierUpdateIsOnTrial(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	f.release("1.2.0", buildPackage(t, versionFiles("1.2.0")))
	u2 := f.updater("1.1.0")
	m := f.check(u2)
	hits := 0
	f.pkgHandler = func(http.ResponseWriter, *http.Request) { hits++ }
	if err := u2.Apply(context.Background(), m); !errors.Is(err, ErrTrialPending) {
		t.Fatalf("Apply = %v, want ErrTrialPending", err)
	}
	if hits != 0 {
		t.Error("downloaded although the trial blocks the install")
	}

	if err := u2.Confirm(); err != nil {
		t.Fatal(err)
	}
	f.pkgHandler = nil
	if err := u2.Apply(context.Background(), f.check(u2)); err != nil {
		t.Fatal(err)
	}
	if f.current() != "1.2.0" {
		t.Errorf("current = %s", f.current())
	}
}

func TestSecondApplyIsBusyWhileTheFirstHoldsTheLock(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	if err := u.applyLock.TryLock(); err != nil {
		t.Fatal(err)
	}
	defer u.applyLock.Unlock()
	other := f.updater("1.0.0")
	if err := other.Apply(context.Background(), f.check(other)); !errors.Is(err, ErrBusy) {
		t.Fatalf("Apply = %v, want ErrBusy", err)
	}
}

func TestInterruptedDownloadResumes(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	partial, _ := u.stager().Paths(releaseOf(m))
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	half := len(f.pkg) / 2
	if err := os.WriteFile(partial, f.pkg[:half], 0o600); err != nil {
		t.Fatal(err)
	}
	var sawRange string
	f.pkgHandler = func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		http.ServeContent(w, r, "pkg", time.Time{}, strings.NewReader(string(f.pkg)))
	}
	if err := u.Apply(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if sawRange == "" {
		t.Error("the download restarted from zero instead of resuming")
	}
	f.assertWholeVersion("1.1.0")
}

func TestStaleUnpackDirectoriesAreSwept(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	stale := filepath.Join(f.root.Staging(), unpackPrefix+"crashed")
	if err := os.MkdirAll(filepath.Join(stale, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a crashed run's unpack directory was left in staging/")
	}
}

func TestClientIDIsSentUnlessDisabled(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	f.check(u)
	if len(f.clientIDs) != 1 || len(f.clientIDs[0]) < 16 {
		t.Fatalf("client IDs sent: %q", f.clientIDs)
	}
	u.cfg.NoClientID = true
	u.Check(context.Background())
	if got := f.clientIDs[len(f.clientIDs)-1]; got != "" {
		t.Errorf("NoClientID still sent %q", got)
	}
}

func TestNewWithoutARootIsNotManaged(t *testing.T) {
	t.Setenv("SELFUPDATE_ROOT", "")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err := New(Config{BaseURL: "https://example.com", Channel: "stable", Version: "1.0.0", PublicKey: pub})
	if !errors.Is(err, ErrNotManaged) {
		t.Fatalf("err = %v, want ErrNotManaged", err)
	}
}

func TestRestartExitsWithTheRestartCode(t *testing.T) {
	_, u := newFixture(t, "1.0.0", "1.1.0")
	code := -1
	u.exitFn = func(c int) { code = c }
	u.Restart()
	if code != RestartExitCode || RestartExitCode != 75 {
		t.Errorf("exit code = %d", code)
	}
}

func TestConfirmAndReportBadReleaseActOnTheRunningVersion(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}

	nu := f.updater("1.1.0")
	rolled, err := nu.ReportBadRelease(errors.New("cannot read data"))
	if err != nil || !rolled {
		t.Fatalf("ReportBadRelease = %v, %v", rolled, err)
	}
	if f.current() != "1.0.0" {
		t.Errorf("current = %s, want rolled back", f.current())
	}
	if !nu.store.Load().IsBad("1.1.0") {
		t.Error("the bad release was not blacklisted")
	}
	if rolled, err := f.updater("1.0.0").ReportBadRelease(errors.New("x")); err != nil || rolled {
		t.Errorf("outside a trial: %v, %v", rolled, err)
	}
}

func TestUnpackRefusesAFileThatIsNotTheSignedPackage(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	other := buildPackage(t, versionFiles("6.6.6"))
	p := filepath.Join(t.TempDir(), "swapped.tar.gz")
	if err := os.WriteFile(p, other, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := u.unpack(p, dir, m); !errors.Is(err, transfer.ErrCorrupt) {
		t.Fatalf("unpack = %v, want ErrCorrupt", err)
	}
}

func TestUnpackRefusesBytesAfterTheArchiveEnd(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	pkg := append(buildPackage(t, versionFiles("1.1.0")), make([]byte, 1024)...)
	f.release("1.1.0", pkg)
	m := f.check(u)
	p := filepath.Join(t.TempDir(), "padded.tar.gz")
	if err := os.WriteFile(p, pkg, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.unpack(p, t.TempDir(), m); !errors.Is(err, pack.ErrUnsafeArchive) {
		t.Fatalf("unpack = %v, want ErrUnsafeArchive", err)
	}
}
