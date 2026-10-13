package release

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"selfupdate/internal/pack"
	"selfupdate/internal/testutil"
	"selfupdate/internal/update/manifest"
)

func writePackage(t *testing.T, path, content string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "agent"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte("data for "+content), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pack.Build(src, "agent", &buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func opts(t *testing.T, root, version string) Options {
	t.Helper()
	pkg := filepath.Join(t.TempDir(), "pkg.tar.gz")
	writePackage(t, pkg, "binary-"+version)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return Options{Root: root, Channel: "stable", Version: version, OS: "linux", Arch: "amd64",
		PackagePath: pkg, Entry: "agent", Name: "agent", Key: priv}
}

func TestPublishProducesVerifiableManifestAndPackage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root := t.TempDir()
	o := opts(t, root, "1.2.3")
	o.Key, o.Notes, o.Mandatory = priv, "notes", true
	m, err := Publish(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "stable", "linux_amd64", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env manifest.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	got, err := manifest.Open([]ed25519.PublicKey{pub}, env)
	if err != nil || got != m {
		t.Fatalf("Open = %+v, %v; want %+v", got, err, m)
	}
	published, _ := os.ReadFile(filepath.Join(root, "stable", "linux_amd64", "1.2.3", "agent.tar.gz"))
	if want, _ := os.ReadFile(o.PackagePath); !bytes.Equal(published, want) || len(want) == 0 {
		t.Error("the published package is not the one given")
	}
	if m.URL != "1.2.3/agent.tar.gz" {
		t.Errorf("URL = %q", m.URL)
	}
}

func TestPublishRejectsPathTraversalAndBadNames(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	os.MkdirAll(root, 0o755)
	for _, tc := range []struct{ field, val string }{
		{"channel", "../../escaped"}, {"channel", "a/b"}, {"channel", ""},
		{"os", "../x"}, {"os", "li nux"}, {"arch", "amd64/.."}, {"arch", ""},
		{"name", "../../evil"}, {"name", "a/b"}, {"name", ".hidden"}, {"name", ""},
	} {
		o := opts(t, root, "1.0.0")
		switch tc.field {
		case "channel":
			o.Channel = tc.val
		case "os":
			o.OS = tc.val
		case "arch":
			o.Arch = tc.val
		case "name":
			o.Name = tc.val
		}
		if _, err := Publish(o); err == nil {
			t.Errorf("%s=%q accepted", tc.field, tc.val)
		}
	}

	var created []string
	filepath.Walk(base, func(p string, info os.FileInfo, _ error) error {
		if p != base && p != root {
			created = append(created, strings.TrimPrefix(p, base))
		}
		return nil
	})
	if len(created) != 0 {
		t.Errorf("rejected publishes still created: %v", created)
	}
}

func TestPublishIsMonotonicAndImmutable(t *testing.T) {
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "1.1.0")); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.1.0", "1.0.9"} {
		if _, err := Publish(opts(t, root, v)); err == nil {
			t.Errorf("publishing %s after 1.1.0 succeeded", v)
		}
	}
	if _, err := Publish(opts(t, root, "1.2.0")); err != nil {
		t.Errorf("newer version rejected: %v", err)
	}
}

func TestPublishFailsClosedOnUnreadablePreviousManifest(t *testing.T) {
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "2.0.0")); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(root, "stable", "linux_amd64", "manifest.json")
	for name, content := range map[string]string{
		"not json":         "{garbage",
		"empty":            "",
		"bad payload":      `{"payload":"e30=","signature":""}`,
		"payload not json": `{"payload":"Z2FyYmFnZQ==","signature":""}`,
	} {
		os.WriteFile(mp, []byte(content), 0o644)
		if _, err := Publish(opts(t, root, "1.0.0")); err == nil {
			t.Errorf("%s: published 1.0.0 over an unreadable manifest", name)
		}
	}

	o := opts(t, root, "1.0.0")
	o.Force = true
	if _, err := Publish(o); err != nil {
		t.Errorf("force publish failed: %v", err)
	}
}

func TestForceNeverOverwritesDifferentPublishedBinary(t *testing.T) {
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(root, "stable", "linux_amd64", "1.0.0", "agent.tar.gz")
	before, _ := os.ReadFile(binPath)

	o := opts(t, root, "1.0.0")
	writePackage(t, o.PackagePath, "DIFFERENT BYTES")
	o.Force = true
	if _, err := Publish(o); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("err = %v, want immutability error", err)
	}
	if after, _ := os.ReadFile(binPath); string(after) != string(before) {
		t.Error("published binary was modified")
	}
}

func TestPublishRetryAfterInterruptedRunIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	o := opts(t, root, "1.1.0")
	dir := filepath.Join(root, "stable", "linux_amd64", "1.1.0")
	os.MkdirAll(dir, 0o755)
	b, _ := os.ReadFile(o.PackagePath)
	os.WriteFile(filepath.Join(dir, "agent.tar.gz"), b, 0o644)
	if _, err := Publish(o); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	pv, err := publishedVersion(mustRead(t, filepath.Join(root, "stable", "linux_amd64", "manifest.json")))
	if err != nil || pv.String() != "1.1.0" {
		t.Errorf("manifest version = %v, %v", pv, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func readManifest(t *testing.T, root string) manifest.Manifest {
	t.Helper()
	var env manifest.Envelope
	if err := json.Unmarshal(mustRead(t, filepath.Join(root, "stable", "linux_amd64", "manifest.json")), &env); err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPublishStampsIssueAndExpiry(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := opts(t, root, "1.0.0")
	o.Now = fixedClock(now)
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	m := readManifest(t, root)
	if !m.IssuedAt.Equal(now) || !m.Expires.Equal(now.Add(DefaultValidity)) {
		t.Errorf("window = %v .. %v", m.IssuedAt, m.Expires)
	}
	o = opts(t, root, "1.1.0")
	o.Now, o.Validity = fixedClock(now.Add(time.Hour)), 48*time.Hour
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if m := readManifest(t, root); m.Expires.Sub(m.IssuedAt) != 48*time.Hour {
		t.Errorf("custom validity ignored: %v", m.Expires.Sub(m.IssuedAt))
	}
}

func TestIssueTimeNeverGoesBackwards(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	o.Now = fixedClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	Publish(o)
	first := readManifest(t, root).IssuedAt
	o = opts(t, root, "1.1.0")
	o.Now = fixedClock(first.Add(-6 * time.Hour))
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if got := readManifest(t, root).IssuedAt; !got.After(first) {
		t.Errorf("issued_at went backwards: %v then %v", first, got)
	}
}

func TestRefreshRenewsOnlyTheWindow(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.4.0")
	o.Now = fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	before := readManifest(t, root)

	later := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Refresh(RefreshOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Key: o.Key, Now: fixedClock(later)}); err != nil {
		t.Fatal(err)
	}
	after := readManifest(t, root)
	if !after.IssuedAt.Equal(later) || !after.Expires.After(before.Expires) {
		t.Errorf("window not renewed: %v .. %v", after.IssuedAt, after.Expires)
	}
	after.IssuedAt, after.Expires = before.IssuedAt, before.Expires
	if after != before {
		t.Errorf("refresh changed more than the window:\n%+v\n%+v", before, after)
	}
}

func TestRefreshRefusesAManifestSignedByAnotherKey(t *testing.T) {
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	before := mustRead(t, filepath.Join(root, "stable", "linux_amd64", "manifest.json"))
	_, err := Refresh(RefreshOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Key: other})
	if err == nil {
		t.Fatal("refresh re-blessed content signed by a different key")
	}
	if after := mustRead(t, filepath.Join(root, "stable", "linux_amd64", "manifest.json")); string(after) != string(before) {
		t.Error("manifest changed despite the refusal")
	}
	if _, err := Refresh(RefreshOptions{Root: t.TempDir(), Channel: "stable", OS: "linux", Arch: "amd64", Key: other}); err == nil {
		t.Error("refresh of a missing manifest succeeded")
	}
	if _, err := Refresh(RefreshOptions{Root: root, Channel: "../x", OS: "linux", Arch: "amd64", Key: other}); err == nil {
		t.Error("refresh accepted a path-traversing channel")
	}
}

func TestPackageNameIsTheSameOnEveryOS(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "darwin"} {
		o := opts(t, t.TempDir(), "1.0.0")
		o.OS = goos
		m, err := Publish(o)
		if err != nil {
			t.Fatal(err)
		}
		if m.URL != "1.0.0/agent.tar.gz" {
			t.Errorf("%s: url %q", goos, m.URL)
		}
	}
}

func TestPublishRefusesAPackageClientsWouldRefuse(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) []byte{
		"not gzip": func(*testing.T) []byte { return []byte("just some bytes") },
		"no entry": func(t *testing.T) []byte {
			return testutil.Archive(t, testutil.Member{Name: "readme", Body: "x"})
		},
		"traversal": func(t *testing.T) []byte {
			return testutil.Archive(t, testutil.Member{Name: "../evil", Body: "x"})
		},
		"symlink": func(t *testing.T) []byte {
			return testutil.Archive(t, testutil.Member{Name: "agent", Type: tar.TypeSymlink, Link: "/bin/sh"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			o := opts(t, root, "1.0.0")
			if err := os.WriteFile(o.PackagePath, mk(t), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Publish(o); err == nil {
				t.Fatal("published an unacceptable package")
			}
			if _, err := os.Stat(filepath.Join(root, "stable")); err == nil {
				entries, _ := os.ReadDir(filepath.Join(root, "stable", "linux_amd64"))
				for _, e := range entries {
					if e.Name() != ".publish.lock" {
						t.Errorf("left %s behind", e.Name())
					}
				}
			}
		})
	}
}

func TestPublishedFilesAreWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits")
	}
	root := t.TempDir()
	if _, err := Publish(opts(t, root, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"1.0.0/agent.tar.gz", "manifest.json"} {
		fi, err := os.Stat(filepath.Join(root, "stable", "linux_amd64", p))
		if err != nil || fi.Mode().Perm()&0o044 != 0o044 {
			t.Errorf("%s: mode %v, %v; want world-readable", p, fi.Mode(), err)
		}
	}
}
