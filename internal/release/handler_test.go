package release

import (
	"os"
	"path/filepath"
	"testing"

	"selfupdate/internal/update/manifest"
)

func TestClassify(t *testing.T) {
	cases := map[string]fileKind{
		"/stable/linux_amd64/manifest.json":         manifestFile,
		"/beta-1.x/darwin_arm64/manifest.json":      manifestFile,
		"/stable/linux_amd64/1.2.3/agent.tar.gz":    packageFile,
		"/stable/windows_amd64/10.0.0/agent.tar.gz": packageFile,
		"/stable/linux_amd64/1.2.3/agent":           notReleaseFile,
		"/stable/windows_amd64/10.0.0/agent.exe":    notReleaseFile,

		"":                                        notReleaseFile,
		"/":                                       notReleaseFile,
		"stable/linux_amd64/manifest.json":        notReleaseFile,
		"//linux_amd64/manifest.json":             notReleaseFile,
		"/Stable/linux_amd64/manifest.json":       notReleaseFile,
		"/.hidden/linux_amd64/manifest.json":      notReleaseFile,
		"/stable/linux/manifest.json":             notReleaseFile,
		"/stable/linux_amd64_x/manifest.json":     notReleaseFile,
		"/stable/Linux_amd64/manifest.json":       notReleaseFile,
		"/stable/linux_amd64/manifest.json.bak":   notReleaseFile,
		"/stable/linux_amd64/notes.txt":           notReleaseFile,
		"/stable/linux_amd64/1.2/agent.tar.gz":    notReleaseFile,
		"/stable/linux_amd64/v1.2.3/agent.tar.gz": notReleaseFile,
		"/stable/linux_amd64/01.2.3/agent":        notReleaseFile,
		"/stable/linux_amd64/1.2.3/.hidden":       notReleaseFile,
		"/stable/linux_amd64/1.2.3/..":            notReleaseFile,
		"/stable/linux_amd64/1.2.3/sub/agent":     notReleaseFile,
		"/stable/linux_amd64/1.2.3/":              notReleaseFile,
		"/stable/linux_amd64/../1.2.3/agent":      notReleaseFile,
		"/stable/linux_amd64/":                    notReleaseFile,
	}
	for p, want := range cases {
		if got, _ := classify(p); got != want {
			t.Errorf("classify(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestClassifyAcceptsEverythingPublishWrites(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.2.3")
	o.OS, o.Name = "windows", "agent"
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fileKind{
		manifest.Path("stable", "windows", "amd64"):                               manifestFile,
		manifest.TargetPath("stable", "windows", "amd64") + "/1.2.3/agent.tar.gz": packageFile,
	} {
		if got, _ := classify("/" + rel); got != want {
			t.Errorf("classify(/%s) = %v, want %v", rel, got, want)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("Publish did not write %s: %v", rel, err)
		}
	}
}
