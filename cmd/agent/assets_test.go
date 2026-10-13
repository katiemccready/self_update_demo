package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfupdate/internal/testutil"
)

func writeAssets(t *testing.T, dir, greeting, engine string) {
	t.Helper()
	testutil.WriteTree(t, dir, map[string]string{greetingFile: greeting + "\n", engineFile: engine + "\n"})
}

func TestLoadAssetsAcceptsFilesOfTheSameVersion(t *testing.T) {
	dir := t.TempDir()
	writeAssets(t, dir, "hello 1.2.0", "engine 1.2.0")
	got, err := loadAssets(dir, "1.2.0")
	if err != nil || got != "hello 1.2.0" {
		t.Fatalf("loadAssets = %q, %v", got, err)
	}
}

func TestLoadAssetsRefusesFilesFromAnotherVersion(t *testing.T) {
	dir := t.TempDir()
	writeAssets(t, dir, "hello 1.2.0", "engine 1.1.0")
	if _, err := loadAssets(dir, "1.2.0"); err == nil || !strings.Contains(err.Error(), "do not belong") {
		t.Fatalf("err = %v", err)
	}
	writeAssets(t, dir, "hello 1.1.0", "engine 1.2.0")
	if _, err := loadAssets(dir, "1.2.0"); err == nil {
		t.Fatal("mismatched greeting accepted")
	}
}

func TestLoadAssetsReportsAMissingFile(t *testing.T) {
	dir := t.TempDir()
	writeAssets(t, dir, "hello 1.2.0", "engine 1.2.0")
	os.Remove(filepath.Join(dir, "lib", "engine.dat"))
	if _, err := loadAssets(dir, "1.2.0"); err == nil || !strings.Contains(err.Error(), "engine.dat") {
		t.Fatalf("err = %v", err)
	}
}
