package update

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

func TestApplyRefusesBeforeDownloadWhenDiskIsFull(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	hits := 0
	f.pkgHandler = func(http.ResponseWriter, *http.Request) { hits++ }
	old := freeBytesFn
	freeBytesFn = func(string) (uint64, error) { return 10, nil }
	defer func() { freeBytesFn = old }()

	if err := u.Apply(context.Background(), m); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if hits != 0 {
		t.Errorf("package was requested %d times despite the preflight", hits)
	}
	if f.current() != "1.0.0" {
		t.Error("pointer moved")
	}

	if _, parked := u.store.Load().Rejected["1.1.0"]; parked {
		t.Error("full disk wrongly recorded as a rejected release")
	}
}

func TestUnmeasurableDiskSpaceDoesNotBlockUpdates(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	old := freeBytesFn
	freeBytesFn = func(string) (uint64, error) { return 0, errors.New("statfs unsupported") }
	defer func() { freeBytesFn = old }()
	if err := u.Apply(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if f.current() != "1.1.0" {
		t.Errorf("current = %s", f.current())
	}
}

func TestDiskSpaceCountsAnExistingPartial(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	m := f.check(u)
	partial, _ := u.stager().Paths(releaseOf(m))
	os.MkdirAll(f.root.Staging(), 0o755)
	if err := os.WriteFile(partial, f.pkg[:len(f.pkg)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	old := freeBytesFn
	defer func() { freeBytesFn = old }()

	need := uint64(m.Size)*unpackedFactor + spaceMargin + 1
	freeBytesFn = func(string) (uint64, error) { return need, nil }
	if err := u.checkDiskSpace(m); err != nil {
		t.Errorf("a nearly finished download was refused: %v", err)
	}
	freeBytesFn = func(string) (uint64, error) { return need - 2, nil }
	if err := u.checkDiskSpace(m); !errors.Is(err, ErrInsufficientSpace) {
		t.Errorf("err = %v, want ErrInsufficientSpace", err)
	}
}
