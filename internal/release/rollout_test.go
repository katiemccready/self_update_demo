package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfupdate/internal/testutil"
	"selfupdate/internal/update/manifest"
)

func stagedTree(t *testing.T, staged *int) (root string, priv ed25519.PrivateKey) {
	t.Helper()
	root = t.TempDir()
	_, priv, _ = ed25519.GenerateKey(rand.Reader)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		o := opts(t, root, v)
		o.Key = priv
		if v == "1.1.0" {
			o.StagedPercent = staged
		}
		if _, err := Publish(o); err != nil {
			t.Fatal(err)
		}
	}
	return root, priv
}

func targetPath(root string, parts ...string) string {
	return filepath.Join(append([]string{root, "stable", "linux_amd64"}, parts...)...)
}

func idN(i int) string { return fmt.Sprintf("%032x", i) }

func versionOf(t *testing.T, body []byte) string {
	t.Helper()
	m, err := decodeUnverified(body)
	if err != nil {
		t.Fatal(err)
	}
	return m.Version
}

func TestBucketIsStableAndRoughlyUniform(t *testing.T) {
	if Bucket(idN(1), "1.2.0") != Bucket(idN(1), "1.2.0") {
		t.Fatal("bucket is not deterministic")
	}
	inside := 0
	const n = 20000
	for i := 0; i < n; i++ {
		if Bucket(idN(i), "1.2.0") < 10 {
			inside++
		}
	}
	if inside < n*8/100 || inside > n*12/100 {
		t.Errorf("%d of %d clients in a 10%% rollout", inside, n)
	}
}

func TestWideningARolloutOnlyAddsClients(t *testing.T) {
	for i := 0; i < 2000; i++ {
		id := idN(i)
		if (Rollout{Version: "1.2.0", Percent: 10}).includes(id) && !(Rollout{Version: "1.2.0", Percent: 30}).includes(id) {
			t.Fatalf("client %s was in at 10%% and out at 30%%", id)
		}
	}
}

func TestCanariesRotateBetweenReleases(t *testing.T) {
	same := 0
	for i := 0; i < 1000; i++ {
		if Bucket(idN(i), "1.2.0") == Bucket(idN(i), "1.3.0") {
			same++
		}
	}
	if same > 50 {
		t.Errorf("%d of 1000 clients have the same bucket for two releases", same)
	}
}

func TestPublishKeepsThePreviousManifest(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath(root, previousFile)); !os.IsNotExist(err) {
		t.Fatalf("first release has a previous manifest: %v", err)
	}
	first := mustRead(t, targetPath(root, "manifest.json"))
	o2 := opts(t, root, "1.1.0")
	o2.Key = o.Key
	if _, err := Publish(o2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, targetPath(root, previousFile)), first) {
		t.Error("previous.json is not the manifest that was replaced")
	}
}

func TestForceRepublishDoesNotOverwritePreviousWithLatest(t *testing.T) {
	root, priv := stagedTree(t, testutil.Ptr(10))
	prev := mustRead(t, targetPath(root, previousFile))
	o := opts(t, root, "1.1.0")
	o.Key, o.Force = priv, true
	writePackage(t, o.PackagePath, "binary-1.1.0")
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, targetPath(root, previousFile)), prev) {
		t.Error("previous.json changed on a same-version republish")
	}
	if r, ok, _ := readRollout(targetPath(root)); !ok || r.Percent != 10 {
		t.Errorf("a same-version republish lifted the rollout: %+v ok=%v", r, ok)
	}
}

func TestPublishingTheNextVersionDropsAStaleRolloutFile(t *testing.T) {
	root, priv := stagedTree(t, testutil.Ptr(10))
	o := opts(t, root, "1.2.0")
	o.Key = priv
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath(root, rolloutFile)); !os.IsNotExist(err) {
		t.Errorf("rollout.json for 1.1.0 survived the publish of 1.2.0: %v", err)
	}
}

func TestManifestForChoosesByRollout(t *testing.T) {
	inside, outside := "", ""
	for i := 0; inside == "" || outside == ""; i++ {
		if Bucket(idN(i), "1.1.0") < 10 {
			inside = idN(i)
		} else {
			outside = idN(i)
		}
	}
	root, _ := stagedTree(t, testutil.Ptr(10))
	get := func(id string) (string, served, error) {
		body, served, err := manifestFor(root, "stable", "linux", "amd64", id)
		if err != nil {
			return "", served, err
		}
		return versionOf(t, body), served, nil
	}
	if v, s, err := get(inside); err != nil || v != "1.1.0" || s != servedLatest {
		t.Errorf("inside: %q %v %v", v, s, err)
	}
	if v, s, err := get(outside); err != nil || v != "1.0.0" || s != servedPrevious {
		t.Errorf("outside: %q %v %v", v, s, err)
	}
	if v, _, err := get(""); err != nil || v != "1.0.0" {
		t.Errorf("a client without an ID must wait for 100%%: %q %v", v, err)
	}

	if _, err := SetRollout(RolloutOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Percent: testutil.Ptr(100)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{inside, outside, ""} {
		if v, _, err := get(id); err != nil || v != "1.1.0" {
			t.Errorf("at 100%%, client %q got %q, %v", id, v, err)
		}
	}

	if _, err := SetRollout(RolloutOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Halt: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{inside, outside, ""} {
		if _, _, err := get(id); !errors.Is(err, errHalted) || !errors.Is(err, errNoManifest) {
			t.Errorf("halted: client %q err = %v", id, err)
		}
	}
}

func TestFirstReleaseOutsideTheRolloutGetsNothing(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	o.StagedPercent = testutil.Ptr(0)
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manifestFor(root, "stable", "linux", "amd64", idN(1)); !errors.Is(err, errNoManifest) {
		t.Fatalf("err = %v, want errNoManifest", err)
	}
}

func TestUnreadableRolloutFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not json":        "{halt: true",
		"percent too big": `{"version":"1.1.0","percent":500}`,
		"negative":        `{"version":"1.1.0","percent":-1}`,
		"bad version":     `{"version":"x","percent":10}`,
	} {
		t.Run(name, func(t *testing.T) {
			root, _ := stagedTree(t, nil)
			if err := os.WriteFile(targetPath(root, rolloutFile), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			body, _, err := manifestFor(root, "stable", "linux", "amd64", idN(1))
			if err == nil || errors.Is(err, errNoManifest) || body != nil {
				t.Fatalf("body=%d bytes err=%v; want a hard error", len(body), err)
			}
		})
	}
}

func TestRolloutForAnotherVersionIsIgnored(t *testing.T) {
	root, _ := stagedTree(t, nil)
	if err := writeRollout(targetPath(root), Rollout{Version: "1.0.0", Percent: 0, Halt: true}); err != nil {
		t.Fatal(err)
	}
	body, _, err := manifestFor(root, "stable", "linux", "amd64", idN(1))
	if err != nil || versionOf(t, body) != "1.1.0" {
		t.Fatalf("a halt of 1.0.0 throttled 1.1.0: %v", err)
	}
}

func TestSetRolloutValidatesAndPreserves(t *testing.T) {
	root, _ := stagedTree(t, testutil.Ptr(5))
	base := RolloutOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64"}
	for name, o := range map[string]RolloutOptions{
		"nothing":         base,
		"too big":         {Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Percent: testutil.Ptr(101)},
		"halt and resume": {Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Halt: true, Resume: true},
	} {
		if _, err := SetRollout(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h := base
	h.Halt = true
	if r, err := SetRollout(h); err != nil || !r.Halt || r.Percent != 5 {
		t.Fatalf("halt: %+v %v", r, err)
	}
	w := base
	w.Percent = testutil.Ptr(50)
	if r, err := SetRollout(w); err != nil || !r.Halt || r.Percent != 50 {
		t.Fatalf("widening must not silently lift a halt: %+v %v", r, err)
	}
	res := base
	res.Resume = true
	if r, err := SetRollout(res); err != nil || r.Halt || r.Percent != 50 {
		t.Fatalf("resume: %+v %v", r, err)
	}
}

func TestRefreshAlsoRenewsThePreviousManifest(t *testing.T) {
	root, priv := stagedTree(t, testutil.Ptr(10))
	before, _ := decodeUnverified(mustRead(t, targetPath(root, previousFile)))
	_, err := Refresh(RefreshOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Key: priv,
		Now: fixedClock(before.IssuedAt.AddDate(0, 0, 20))})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := decodeUnverified(mustRead(t, targetPath(root, previousFile)))
	if !after.Expires.After(before.Expires) || after.Version != before.Version || after.SHA256 != before.SHA256 {
		t.Errorf("previous before %+v after %+v", before, after)
	}
	pub := priv.Public().(ed25519.PublicKey)
	var env manifest.Envelope
	if err := json.Unmarshal(mustRead(t, targetPath(root, previousFile)), &env); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Open([]ed25519.PublicKey{pub}, env); err != nil {
		t.Errorf("refreshed previous.json does not verify: %v", err)
	}
}

func TestRefreshReportsAPreviousManifestItCannotRenew(t *testing.T) {
	root, priv := stagedTree(t, testutil.Ptr(10))
	if err := os.WriteFile(targetPath(root, previousFile), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	latestBefore := readManifest(t, root)
	m, err := Refresh(RefreshOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Key: priv,
		Now: fixedClock(latestBefore.IssuedAt.AddDate(0, 0, 10))})
	if err == nil || !strings.Contains(err.Error(), previousFile) {
		t.Fatalf("err = %v, want it to name %s", err, previousFile)
	}
	if !m.Expires.After(latestBefore.Expires) || !readManifest(t, root).Expires.After(latestBefore.Expires) {
		t.Error("the latest manifest was not renewed")
	}
}

func TestHandlerAppliesTheRollout(t *testing.T) {
	root, _ := stagedTree(t, testutil.Ptr(0))
	srv := httptest.NewServer(Handler(root))
	defer srv.Close()
	get := func(path, id string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if id != "" {
			req.Header.Set(manifest.ClientIDHeader, id)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	const mp = "/stable/linux_amd64/manifest.json"

	resp := get(mp, idN(1))
	if resp.StatusCode != 200 || resp.Header.Get("Vary") != manifest.ClientIDHeader || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("status %d vary %q cache %q", resp.StatusCode, resp.Header.Get("Vary"), resp.Header.Get("Cache-Control"))
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if v := versionOf(t, buf.Bytes()); v != "1.0.0" {
		t.Errorf("client outside a 0%% rollout was given %s", v)
	}

	for _, bad := range []string{"../../etc", strings.Repeat("A", 32), "short"} {
		if r := get(mp, bad); r.StatusCode != 200 {
			t.Errorf("bad ID %q: status %d", bad, r.StatusCode)
		}
	}

	for _, p := range []string{"/stable/linux_amd64/previous.json", "/stable/linux_amd64/rollout.json"} {
		if r := get(p, ""); r.StatusCode != 404 {
			t.Errorf("%s served directly: %d", p, r.StatusCode)
		}
	}

	if _, err := SetRollout(RolloutOptions{Root: root, Channel: "stable", OS: "linux", Arch: "amd64", Halt: true}); err != nil {
		t.Fatal(err)
	}
	if r := get(mp, idN(1)); r.StatusCode != 404 {
		t.Errorf("halted: status %d, want 404", r.StatusCode)
	}
	os.WriteFile(targetPath(root, rolloutFile), []byte("garbage"), 0o644)
	if r := get(mp, idN(1)); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("broken rollout.json: status %d, want 503", r.StatusCode)
	}
}

func TestClientWithoutAnIDIsNeverInAPartialRollout(t *testing.T) {
	for patch := 0; patch < 60; patch++ {
		r := Rollout{Version: fmt.Sprintf("1.0.%d", patch), Percent: 99}
		if r.includes("") {
			t.Fatalf("an unplaceable client was let into %s at 99%%", r.Version)
		}
	}
	if !(Rollout{Version: "1.0.0", Percent: 100}).includes("") {
		t.Error("at 100% everyone, including clients without an ID, gets the release")
	}
}
