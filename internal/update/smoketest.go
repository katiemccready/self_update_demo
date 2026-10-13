package update

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/update/manifest"
)

const (
	versionOutputLimit = 4096
)

type capWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func smokeTestEnv(env []string) []string {
	keep := map[string]bool{"PATH": true, "TMPDIR": true, "TEMP": true, "TMP": true, "SYSTEMROOT": true, "LANG": true}
	out := []string{"SELFUPDATE_SMOKE_TEST=1"}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if keep[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return out
}

func runVersionCheck(ctx context.Context, dir, program string, m manifest.Manifest) error {
	want, err := manifest.ParseVersion(m.Version)
	if err != nil {
		return err
	}
	got, err := candidateVersion(ctx, dir, program)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("binary reports version %s, manifest says %s", got, want)
	}
	return nil
}

func candidateVersion(ctx context.Context, dir, program string) (manifest.Version, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := &capWriter{n: versionOutputLimit}
	err := fsutil.Retry(textBusyAttempts, textBusyDelay, isTextBusy, func() error {
		out.buf.Reset()
		cmd := exec.CommandContext(ctx, program, "--version")

		cmd.Env = smokeTestEnv(os.Environ())
		cmd.Dir = dir
		cmd.Stdout = out
		cmd.WaitDelay = 2 * time.Second
		started, sweep := isolateProcessGroup(cmd)
		defer sweep()
		if err := cmd.Start(); err != nil {
			return err
		}
		started()
		return cmd.Wait()
	})
	if err != nil {
		return manifest.Version{}, fmt.Errorf("run --version: %w", err)
	}
	got, err := manifest.ParseVersion(string(bytes.TrimSpace(out.buf.Bytes())))
	if err != nil {
		return manifest.Version{}, fmt.Errorf("binary printed an unparseable version: %w", err)
	}
	return got, nil
}
