package main

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLockdownIgnoresRuntimeOverridesInProductionBuilds(t *testing.T) {
	const built = "https://updates.example.com/releases"
	url, insecure, warns := lockdown("https://evil.example/releases", true, built, false)
	if url != built || insecure {
		t.Errorf("url=%q insecure=%v; the compiled-in policy must win", url, insecure)
	}
	if len(warns) != 2 {
		t.Errorf("warnings = %q, want one per ignored override", warns)
	}

	if _, _, warns := lockdown(built, false, built, false); len(warns) != 0 {
		t.Errorf("spurious warnings: %q", warns)
	}

	if url, _, _ := lockdown("https://x", false, "", false); url != "" {
		t.Errorf("url = %q", url)
	}
}

func TestLockdownAllowsOverridesInDevBuilds(t *testing.T) {
	url, insecure, warns := lockdown("http://dev.local/r", true, "https://prod/r", true)
	if url != "http://dev.local/r" || !insecure || len(warns) != 0 {
		t.Errorf("dev build: %q %v %q", url, insecure, warns)
	}
}

func TestSanitizeNotesStripsTerminalControlSequences(t *testing.T) {
	in := "Fixes bug\x1b[2J\x1b]0;pwned\x07\nLine two\t(ok)‮evil​\r"
	got := sanitizeNotes(in)
	for _, bad := range []string{"\x1b", "\x07", "\r", "‮", "​"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q survived in %q", bad, got)
		}
	}
	if !strings.Contains(got, "Fixes bug") || !strings.Contains(got, "\nLine two\t(ok)") {
		t.Errorf("legitimate text damaged: %q", got)
	}
	if long := sanitizeNotes(strings.Repeat("a", 10_000)); len(long) > 4100 {
		t.Errorf("notes not truncated: %d bytes", len(long))
	}
}

func TestSanitizeNotesTruncatesOnRuneBoundary(t *testing.T) {
	got := sanitizeNotes(strings.Repeat("€", 5000))
	if !utf8.ValidString(got) {
		t.Errorf("truncated notes are not valid UTF-8: %q", got[len(got)-8:])
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation not marked: %q", got[len(got)-8:])
	}
}

func TestParseFlagsRejectsUnknownUpdateMode(t *testing.T) {
	if _, err := parseFlags([]string{"-update-mode", "sometimes"}); err == nil {
		t.Error("bad -update-mode accepted")
	}
	if o, err := parseFlags([]string{"-update-mode", "prompt", "-state", "/x"}); err != nil || o.mode != "prompt" {
		t.Errorf("valid flags rejected: %v", err)
	}
}

func TestNoTempDirFallbackForStatePath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses the Linux XDG/HOME lookup")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got := defaultStatePath(); got != "" {
		t.Errorf("defaultStatePath = %q with no config dir; want empty so the user must pass -state", got)
	}
	t.Setenv("HOME", "/home/someone")
	if got := defaultStatePath(); !strings.HasPrefix(got, "/home/someone/") {
		t.Errorf("defaultStatePath = %q", got)
	}
}

func TestParseBuildTime(t *testing.T) {
	if got, err := parseBuildTime(""); err != nil || !got.IsZero() {
		t.Fatalf("empty: %v, %v", got, err)
	}
	got, err := parseBuildTime("2026-10-12T17:00:00Z")
	if err != nil || got.Year() != 2026 {
		t.Fatalf("valid: %v, %v", got, err)
	}
	if _, err := parseBuildTime("yesterday"); err == nil {
		t.Fatal("malformed stamp accepted")
	}
}
