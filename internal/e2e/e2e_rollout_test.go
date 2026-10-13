package e2e

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"selfupdate/internal/release"
	"selfupdate/internal/update/install"

	"selfupdate/internal/testutil"
)

func clientIDInBucket(t *testing.T, lo, hi int, versions ...string) string {
	t.Helper()
	for i := 0; i < 100000; i++ {
		id := fmt.Sprintf("%032x", i)
		ok := true
		for _, v := range versions {
			if b := release.Bucket(id, v); b < lo || b >= hi {
				ok = false
				break
			}
		}
		if ok {
			return id
		}
	}
	t.Fatalf("no client ID with bucket in [%d,%d) for %v", lo, hi, versions)
	return ""
}

func TestStagedRolloutWithRealClients(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	h.publish("1.1.0", h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0")))
	ten := 10
	h.publishStaged("1.2.0", h.programDir("1.2.0", h.build("./cmd/agent", "1.2.0")), &ten)

	insideID := clientIDInBucket(t, 0, 10, "1.2.0", "1.3.0")
	outsideID := clientIDInBucket(t, 10, 100, "1.2.0")
	inside, outside := h.bootstrap("1.0.0", v1Dir, "inside"), h.bootstrap("1.0.0", v1Dir, "outside")
	seedClientID(t, inside, insideID)
	seedClientID(t, outside, outsideID)

	run := func(root install.Root) {
		t.Helper()
		out, code := runToExit(t, root, "-update-interval", "300ms", "-step", "50ms",
			"-confirm-after", "1s", "-state", filepath.Join(h.tmp, filepath.Base(root.Dir)+"-app.json"), "-run-for", "5s")
		if code != 0 {
			t.Fatalf("%s exited %d\n%s", root.Dir, code, out)
		}
	}
	want := func(root install.Root, version string) {
		t.Helper()
		if got := currentVersion(root); got != version {
			t.Errorf("%s is on %q, want %q", filepath.Base(root.Dir), got, version)
		}
	}

	run(inside)
	run(outside)
	want(inside, "1.2.0")
	want(outside, "1.1.0")

	if _, err := release.SetRollout(release.RolloutOptions{
		Root: h.releases, Channel: "stable", OS: runtime.GOOS, Arch: runtime.GOARCH, Percent: testutil.Ptr(100),
	}); err != nil {
		t.Fatal(err)
	}
	run(outside)
	want(outside, "1.2.0")

	h.publishStaged("1.3.0", h.programDir("1.3.0", h.build("./cmd/agent", "1.3.0")), &ten)
	if _, err := release.SetRollout(release.RolloutOptions{
		Root: h.releases, Channel: "stable", OS: runtime.GOOS, Arch: runtime.GOARCH, Halt: true,
	}); err != nil {
		t.Fatal(err)
	}
	run(inside)
	want(inside, "1.2.0")
}
