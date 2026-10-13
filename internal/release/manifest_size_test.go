package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishRefusesManifestClientsCannotRead(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	o.Notes = strings.Repeat("x", 2<<20)
	if _, err := Publish(o); err == nil {
		t.Fatal("Publish accepted a manifest larger than clients will read")
	}

	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != ".publish.lock" {
			t.Errorf("file written for a release that was refused: %s", p)
		}
		return nil
	})
}
