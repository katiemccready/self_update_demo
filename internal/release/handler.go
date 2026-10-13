package release

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"selfupdate/internal/update/manifest"
)

type fileKind int

const (
	notReleaseFile fileKind = iota
	manifestFile
	packageFile
)

type target struct{ channel, goos, goarch string }

var clientIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func classify(p string) (fileKind, target) {
	rel, ok := strings.CutPrefix(p, "/")
	if !ok {
		return notReleaseFile, target{}
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 3 && len(parts) != 4 {
		return notReleaseFile, target{}
	}
	goos, goarch, ok := strings.Cut(parts[1], "_")
	if !ok || manifest.ValidateChannel(parts[0]) != nil || manifest.ValidatePlatform(goos, goarch) != nil {
		return notReleaseFile, target{}
	}
	t := target{parts[0], goos, goarch}
	if len(parts) == 3 {
		if parts[2] == manifest.FileName {
			return manifestFile, t
		}
		return notReleaseFile, target{}
	}

	if v, err := manifest.ParseVersion(parts[2]); err != nil || v.String() != parts[2] ||
		!strings.HasSuffix(parts[3], PackageExt) || !nameRe.MatchString(parts[3]) {
		return notReleaseFile, target{}
	}
	return packageFile, t
}

func Handler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		kind, t := classify(r.URL.Path)
		switch kind {
		case manifestFile:
			serveManifest(w, r, dir, t)
			return
		case packageFile:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

func serveManifest(w http.ResponseWriter, r *http.Request, dir string, t target) {
	id := r.Header.Get(manifest.ClientIDHeader)
	if !clientIDRe.MatchString(id) {
		id = ""
	}
	body, served, err := manifestFor(dir, t.channel, t.goos, t.goarch, id)
	switch {
	case errors.Is(err, errHalted):
		slog.Info("manifest withheld", "path", r.URL.Path, "reason", "rollout halted")
		http.NotFound(w, r)
		return
	case errors.Is(err, errNoManifest):
		http.NotFound(w, r)
		return
	case err != nil:
		slog.Error("cannot choose a manifest", "path", r.URL.Path, "err", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if served == servedPrevious {
		slog.Info("serving previous release: client is outside the rollout", "path", r.URL.Path)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", manifest.ClientIDHeader)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "application/json")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}
