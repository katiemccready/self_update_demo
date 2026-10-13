package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"selfupdate/internal/pack"
	"selfupdate/internal/release"
	"selfupdate/internal/update/install"
	"selfupdate/internal/update/manifest"
)

type harness struct {
	t        *testing.T
	root     string
	tmp      string
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	releases string
	srv      *httptest.Server
	launcher string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("builds binaries; skipped in -short mode")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: repoRoot(t), tmp: t.TempDir(), pub: pub, priv: priv}
	h.releases = filepath.Join(h.tmp, "releases")
	if err := os.MkdirAll(h.releases, 0o755); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(http.StripPrefix("/releases", release.Handler(h.releases)))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) goBuild(out, pkg string, ldflags ...string) {
	h.t.Helper()
	args := []string{"build"}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	cmd := exec.Command("go", append(args, "-o", out, pkg)...)
	cmd.Dir = h.root
	if b, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
}

func (h *harness) buildLauncher() string {
	h.t.Helper()
	if h.launcher == "" {
		h.launcher = filepath.Join(h.tmp, "build-launcher", exeName("launcher"))
		h.goBuild(h.launcher, "./cmd/launcher")
	}
	return h.launcher
}

func (h *harness) build(pkg, version string) string {
	h.t.Helper()
	out := filepath.Join(h.tmp, "build-"+filepath.Base(pkg)+"-"+version, exeName("agent"))
	h.goBuild(out, pkg,
		"-X main.version="+version,
		"-X main.publicKey="+base64.StdEncoding.EncodeToString(h.pub),
		"-X main.defaultUpdateURL="+h.srv.URL+"/releases")
	return out
}

func (h *harness) programDir(version, binary string) string {
	h.t.Helper()
	dir := filepath.Join(h.tmp, "program-"+version+"-"+filepath.Base(filepath.Dir(binary)))
	files := map[string][]byte{
		"data/greeting":  []byte("hello " + version + "\n"),
		"lib/engine.dat": []byte("engine " + version + "\n"),
	}
	b, err := os.ReadFile(binary)
	if err != nil {
		h.t.Fatal(err)
	}
	files[exeName("agent")] = b
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == exeName("agent") {
			mode = 0o755
		}
		if err := os.WriteFile(p, content, mode); err != nil {
			h.t.Fatal(err)
		}
	}
	return dir
}

func (h *harness) packageOf(dir string) []byte {
	h.t.Helper()
	var buf bytes.Buffer
	if err := pack.Build(dir, exeName("agent"), &buf); err != nil {
		h.t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *harness) publish(version, dir string) string { return h.publishStaged(version, dir, nil) }

func (h *harness) publishStaged(version, dir string, percent *int) string {
	h.t.Helper()
	pkg := filepath.Join(h.tmp, "pkg-"+version+"-"+filepath.Base(dir)+".tar.gz")
	if err := os.WriteFile(pkg, h.packageOf(dir), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if _, err := release.Publish(release.Options{
		Root: h.releases, Channel: "stable", Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		PackagePath: pkg, Entry: exeName("agent"), Name: "agent", Key: h.priv, Notes: "e2e " + version,
		StagedPercent: percent,
	}); err != nil {
		h.t.Fatalf("publish %s: %v", version, err)
	}
	return h.publishedPackage(version)
}

func (h *harness) publishedPackage(version string) string {
	return filepath.Join(h.releases, "stable", runtime.GOOS+"_"+runtime.GOARCH, version, "agent"+release.PackageExt)
}

func (h *harness) publishRaw(version string, pkg []byte) {
	h.t.Helper()
	dir := filepath.Join(h.releases, "stable", runtime.GOOS+"_"+runtime.GOARCH)
	if err := os.MkdirAll(filepath.Join(dir, version), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, version, "agent.tar.gz"), pkg, 0o644); err != nil {
		h.t.Fatal(err)
	}
	sum, size := sha256Hex(pkg), int64(len(pkg))
	m := manifest.Manifest{Version: version, Channel: "stable", OS: runtime.GOOS, Arch: runtime.GOARCH,
		SHA256: sum, Size: size, URL: version + "/agent.tar.gz",
		IssuedAt: time.Now().UTC().Truncate(time.Second), Expires: time.Now().Add(24 * time.Hour)}
	env, err := manifest.Sign(h.priv, m)
	if err != nil {
		h.t.Fatal(err)
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) bootstrap(version, dir, name string) install.Root {
	h.t.Helper()
	root := install.Root{Dir: filepath.Join(h.tmp, name)}
	v, err := manifest.ParseVersion(version)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := install.Bootstrap(root, h.buildLauncher(), bytes.NewReader(h.packageOf(dir)), v, exeName("agent")); err != nil {
		h.t.Fatal(err)
	}
	return root
}

func currentVersion(root install.Root) string {
	v, err := root.ReadPointer()
	if err != nil {
		return "error: " + err.Error()
	}
	return v.String()
}

func seedClientID(t *testing.T, root install.Root, id string) {
	t.Helper()
	b, err := json.Marshal(install.State{ClientID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Dir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runToExit(t *testing.T, root install.Root, args ...string) (output string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, root.Launcher(), args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode()
	default:
		t.Fatalf("run %s: %v\n%s", root.Launcher(), err, out.String())
		return "", -1
	}
}

func (h *harness) startLauncher(root install.Root, logName string, args ...string) (logPath string, cmd *exec.Cmd) {
	h.t.Helper()
	logPath = filepath.Join(h.tmp, logName)
	logFile, err := os.Create(logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd = exec.Command(root.Launcher(), args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	dumpOnFailure(h.t, logName, func() string { return readLog(logPath) })
	h.t.Cleanup(func() {
		killTree(cmd)
		cmd.Wait()
		logFile.Close()
		time.Sleep(200 * time.Millisecond)
	})
	return logPath, cmd
}

func dumpOnFailure(t *testing.T, label string, content func() string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s:\n%s", label, content())
		}
	})
}

func (h *harness) agentArgs(runFor, confirmAfter string) []string {
	return []string{"-update-interval", "300ms", "-step", "50ms", "-confirm-after", confirmAfter,
		"-state", filepath.Join(h.tmp, "app-state.json"), "-run-for", runFor}
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func walkFiles(dir string, fn func(rel string, mode os.FileMode, content []byte) error) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel), fi.Mode(), b)
	})
}

func copyTree(src, dst string) error {
	return walkFiles(src, func(rel string, mode os.FileMode, content []byte) error {
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, content, mode.Perm())
	})
}
