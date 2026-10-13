package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"selfupdate/internal/pack"
	"selfupdate/internal/testutil"
	"selfupdate/internal/update/install"
	"selfupdate/internal/update/manifest"
)

const entryName = "app"

func versionFiles(v string) map[string]string {
	return map[string]string{
		entryName:        "program " + v,
		"data/greeting":  "hello from " + v,
		"lib/engine.dat": "engine " + v,
	}
}

func buildPackage(t testing.TB, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	testutil.WriteTree(t, dir, files)
	var buf bytes.Buffer
	if err := pack.Build(dir, entryName, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fixture struct {
	t        *testing.T
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	root     install.Root
	srv      *httptest.Server
	manifest manifest.Manifest
	env      manifest.Envelope

	mu         sync.Mutex
	pkg        []byte
	pkgHandler http.HandlerFunc
	clientIDs  []string
}

func newFixture(t *testing.T, curVersion, newVersion string) (*fixture, *Updater) {
	t.Helper()
	f := &fixture{t: t, root: install.Root{Dir: t.TempDir()}}
	f.pub, f.priv, _ = ed25519.GenerateKey(rand.Reader)

	launcher := filepath.Join(t.TempDir(), "launcher-src")
	if err := os.WriteFile(launcher, []byte("launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := install.Bootstrap(f.root, launcher, bytes.NewReader(buildPackage(t, versionFiles(curVersion))), testutil.Ver(t, curVersion), entryName); err != nil {
		t.Fatal(err)
	}
	f.release(newVersion, buildPackage(t, versionFiles(newVersion)))

	mux := http.NewServeMux()
	mux.HandleFunc("/stable/linux_amd64/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.clientIDs = append(f.clientIDs, r.Header.Get(manifest.ClientIDHeader))
		env := f.env
		f.mu.Unlock()
		json.NewEncoder(w).Encode(env)
	})
	mux.HandleFunc("/stable/linux_amd64/pkg.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h, body := f.pkgHandler, f.pkg
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		http.ServeContent(w, r, "pkg", time.Time{}, bytes.NewReader(body))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	return f, f.updater(curVersion)
}

func (f *fixture) updater(curVersion string) *Updater {
	f.t.Helper()
	u, err := New(Config{
		BaseURL: f.srv.URL, Channel: "stable", Version: curVersion, PublicKey: f.pub,
		RootDir: f.root.Dir, GOOS: "linux", GOARCH: "amd64",
		Validate:     func(context.Context, string, string, manifest.Manifest) error { return nil },
		StallTimeout: time.Second,
		Logger:       testutil.Quiet(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	u.retryDelay = time.Millisecond
	return u
}

func (f *fixture) release(v string, pkg []byte) {
	f.t.Helper()
	sum := sha256.Sum256(pkg)
	f.mu.Lock()
	f.pkg = pkg
	f.manifest = manifest.Manifest{Version: v, Channel: "stable", OS: "linux", Arch: "amd64",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(pkg)), URL: "pkg.tar.gz",
		IssuedAt: time.Now().Add(-time.Hour), Expires: time.Now().Add(30 * 24 * time.Hour)}
	f.mu.Unlock()
	f.sign()
}

func (f *fixture) sign() {
	env, err := manifest.Sign(f.priv, f.manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.env = env
	f.mu.Unlock()
}

func (f *fixture) check(u *Updater) manifest.Manifest {
	f.t.Helper()
	m, err := u.Check(context.Background())
	if err != nil || m == nil {
		f.t.Fatalf("Check = %v, %v", m, err)
	}
	return *m
}

func (f *fixture) current() string {
	f.t.Helper()
	v, err := f.root.ReadPointer()
	if err != nil {
		f.t.Fatal(err)
	}
	return v.String()
}

func (f *fixture) assertWholeVersion(v string) {
	f.t.Helper()
	dir := f.root.VersionDir(testutil.Ver(f.t, v))
	for name, want := range versionFiles(v) {
		if got := testutil.ReadFile(f.t, filepath.Join(dir, filepath.FromSlash(name))); got != want {
			f.t.Errorf("%s/%s = %q, want %q", v, name, got, want)
		}
	}
}

func (f *fixture) stagingLeftovers() []string {
	entries, _ := os.ReadDir(f.root.Staging())
	var out []string
	for _, e := range entries {
		if !strings.Contains(e.Name(), ".partial-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func (f *fixture) versionDirs() []string {
	entries, _ := os.ReadDir(f.root.Versions())
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
