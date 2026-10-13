// Package transfer is the download layer of the updater: it brings a release
// package onto the machine as a staged, fully verified file and knows nothing
// about what happens next. It owns the network policy (https only, no downgrade
// on redirect) and the staging area, and depends only on fsutil.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfupdate/internal/fsutil"
)

const (
	maxDownloadAttempts = 4
	stagingStem         = "release"
	stagingExt          = ".tar.gz"
)

type Release struct {
	URL    string
	Size   int64
	SHA256 string
}

type Stager struct {
	Dir          string
	Client       *http.Client
	Log          *slog.Logger
	UserAgent    string
	StallTimeout time.Duration
	RetryDelay   time.Duration
}

var ErrCorrupt = errors.New("downloaded data failed verification")

func (s *Stager) Paths(r Release) (partial, ready string) {
	key := r.SHA256[:12]
	return filepath.Join(s.Dir, "."+stagingStem+".partial-"+key+stagingExt),
		filepath.Join(s.Dir, "."+stagingStem+".new-"+key+stagingExt)
}

func (s *Stager) StagedBytes(r Release) int64 {
	partial, _ := s.Paths(r)
	if fi, err := os.Stat(partial); err == nil {
		return fi.Size()
	}
	return 0
}

func (s *Stager) Download(ctx context.Context, r Release) (string, error) {
	partial, ready := s.Paths(r)
	s.Sweep(partial)

	var err error
	corrupt := 0
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		if err = s.fetchInto(ctx, r, partial); err == nil {
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.Log.Warn("download interrupted", "attempt", attempt, "err", err)

		if errors.Is(err, ErrCorrupt) {
			if corrupt++; corrupt >= 2 {
				break
			}
		}
		if attempt == maxDownloadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * s.RetryDelay):
		}
	}
	if err != nil {
		if fi, serr := os.Stat(partial); serr == nil && fi.Size() == 0 {
			_ = os.Remove(partial)
		}
		return "", fmt.Errorf("download failed: %w", err)
	}

	if err := os.Chmod(partial, 0o600); err != nil {
		return "", err
	}
	if err := fsutil.Rename(partial, ready); err != nil {
		return "", err
	}
	return ready, nil
}

func (s *Stager) fetchInto(ctx context.Context, r Release, path string) error {
	if err := CheckStagingFile(path); err != nil {
		return err
	}
	f, err := openStagingFile(path)
	if err != nil {
		return fmt.Errorf("create staging file (is the install dir writable?): %w", err)
	}
	defer f.Close()

	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if have > r.Size {
		if err := f.Truncate(0); err != nil {
			return err
		}
		have = 0
	}
	if have < r.Size {
		if err := s.transfer(ctx, r, f, have); err != nil {
			return err
		}
	}
	if err := verifyFile(f, r); err != nil {
		return err
	}
	// Verification read the bytes back through the page cache; flush them
	// before the file is renamed into place, or a power loss right after an
	// update could leave a truncated package.
	return f.Sync()
}

func (s *Stager) transfer(ctx context.Context, r Release, f *os.File, have int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(s.StallTimeout, cancel)
	defer stall.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		s.Log.Info("resuming download", "have", have, "of", r.Size)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range")
		if start, end, total, ok := parseContentRange(cr); !ok || start != have || end < start ||
			(total >= 0 && total != r.Size) {
			return discardStaged(f, fmt.Errorf("unexpected Content-Range %q", cr))
		}
	case resp.StatusCode == http.StatusOK:
		if have > 0 {
			if err := f.Truncate(0); err != nil {
				return err
			}
			have = 0
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return discardStaged(f, errors.New("server rejected resume range"))
	default:
		return fmt.Errorf("GET %s: %s", r.URL, resp.Status)
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return err
	}

	body := &progressReader{r: resp.Body, onRead: func() { stall.Reset(s.StallTimeout) }}
	if _, err := io.Copy(f, io.LimitReader(body, r.Size-have+1)); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return nil
}

// openStagingFile opens (creating if needed) a staging file without following a
// symlink at the final path component.
func openStagingFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|openNoFollow, 0o600)
}

func parseContentRange(h string) (start, end, total int64, ok bool) {
	if _, err := fmt.Sscanf(h, "bytes %d-%d/%d", &start, &end, &total); err == nil {
		return start, end, total, true
	}
	if _, err := fmt.Sscanf(h, "bytes %d-%d/*", &start, &end); err == nil {
		return start, end, -1, true
	}
	return 0, 0, 0, false
}

func discardStaged(f *os.File, cause error) error {
	if err := f.Truncate(0); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func verifyFile(f *os.File, r Release) error {
	fail := func(format string, a ...any) error {
		_ = f.Truncate(0)
		return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, a...)...)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	switch {
	case size < r.Size:
		return fmt.Errorf("incomplete download: have %d of %d bytes", size, r.Size)
	case size > r.Size:
		return fail("size %d, manifest says %d", size, r.Size)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got, _, err := fsutil.Hash(f)
	if err != nil {
		return err
	}
	if got != r.SHA256 {
		return fail("sha256 %s, want %s", got, r.SHA256)
	}
	return nil
}

// CheckStagingFile makes sure a staging path we are about to open is absent or a
// regular file we own. A symlink planted at the predictable staging name would
// make us write the download through it; a file owned by someone else could be
// modified by them after we verify it. Either is removed (or, if it cannot be,
// reported) and recreated by us.
func CheckStagingFile(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().IsRegular() && fsutil.OwnedByCurrentUser(fi) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("%w: staging path %s is not a regular file owned by this user and cannot be removed: %v",
			fsutil.ErrUnsafeLocation, path, err)
	}
	return nil
}

func (s *Stager) Sweep(keep string) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	partialPfx, newPfx := "."+stagingStem+".partial-", "."+stagingStem+".new-"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, stagingExt) {
			continue
		}
		p := filepath.Join(s.Dir, name)
		if strings.HasPrefix(name, partialPfx) && p != keep || strings.HasPrefix(name, newPfx) {
			_ = os.Remove(p)
		}
	}
}

type progressReader struct {
	r      io.Reader
	onRead func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead()
	}
	return n, err
}
