package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"selfupdate/internal/release"

	"selfupdate/internal/testutil"
)

func tree(t *testing.T) string {
	root := t.TempDir()
	files := map[string]string{
		"stable/linux_amd64/manifest.json":          `{"payload":""}`,
		"stable/linux_amd64/1.2.3/agent.tar.gz":     "BIN",
		"stable/windows_amd64/1.2.3/agent.tar.gz":   "PKG",
		"stable/linux_amd64/1.2.3/agent":            "a bare binary is not a release file",
		"private.key":                               "SECRET",
		"stable/private.key":                        "SECRET",
		"stable/linux_amd64/.tmp-123":               "partial",
		"stable/linux_amd64/notes.txt":              "not part of the layout",
		"stable/linux_amd64/1.2.3/.hidden":          "x",
		"stable/linux_amd64/1.2.3/sub/agent.tar.gz": "too deep",
		"stable/linux_amd64/1.2/agent.tar.gz":       "bad version dir",
		"beta/linux_amd64/manifest.json.bak":        "backup",
		"stable/linux_amd64/manifest.json.old":      "old",
	}
	testutil.WriteTree(t, root, files)
	return root
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestServesOnlyTheReleaseLayout(t *testing.T) {
	h := release.Handler(tree(t))
	ok := map[string]string{
		"/stable/linux_amd64/manifest.json":        "no-cache",
		"/stable/linux_amd64/1.2.3/agent.tar.gz":   "public, max-age=31536000, immutable",
		"/stable/windows_amd64/1.2.3/agent.tar.gz": "public, max-age=31536000, immutable",
	}
	for path, cache := range ok {
		rec := get(h, "GET", path)
		if rec.Code != 200 {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != cache {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, cache)
		}
	}
	notFound := []string{
		"/private.key", "/stable/private.key",
		"/", "/stable/", "/stable/linux_amd64/", "/stable/linux_amd64/1.2.3/",
		"/stable/linux_amd64/.tmp-123", "/stable/linux_amd64/1.2.3/.hidden",
		"/stable/linux_amd64/1.2.3/agent",
		"/stable/linux_amd64/notes.txt", "/beta/linux_amd64/manifest.json.bak",
		"/stable/linux_amd64/manifest.json.old",
		"/stable/linux_amd64/1.2.3/sub/agent.tar.gz", "/stable/linux_amd64/1.2/agent.tar.gz",
		"/../private.key", "/stable/../private.key", "/stable/%2e%2e/private.key",
		"/stable/linux_amd64/%2e%2e/%2e%2e/private.key",
		"/stable/linux_amd64/manifest.json/", "/stable//linux_amd64/manifest.json",
	}
	for _, path := range notFound {
		if rec := get(h, "GET", path); rec.Code != 404 && rec.Code != 301 && rec.Code != 400 {
			t.Errorf("%s: status %d, want 404 (body %q)", path, rec.Code, rec.Body.String())
		} else if rec.Body.String() == "SECRET" {
			t.Errorf("%s: LEAKED the secret", path)
		}
	}
}

func TestOnlyReadMethods(t *testing.T) {
	h := release.Handler(tree(t))
	if rec := get(h, "HEAD", "/stable/linux_amd64/manifest.json"); rec.Code != 200 {
		t.Errorf("HEAD: %d", rec.Code)
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if rec := get(h, m, "/stable/linux_amd64/manifest.json"); rec.Code != 405 {
			t.Errorf("%s: %d, want 405", m, rec.Code)
		}
	}
}

func TestSupportsRangeForResumableDownloads(t *testing.T) {
	h := release.Handler(tree(t))
	req := httptest.NewRequest("GET", "/stable/linux_amd64/1.2.3/agent.tar.gz", nil)
	req.Header.Set("Range", "bytes=1-")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "IN" {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}
}

func TestServeWaitsForInFlightRequestsOnShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Write([]byte("complete"))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, "", "", 5*time.Second) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			body <- "ERR " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	cancel()

	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) while a request was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if got := <-body; got != "complete" {
		t.Errorf("client saw %q, want the response to finish", got)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v", err)
	}
}

func TestServeReturnsListenErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ln.Close()
	err := serve(context.Background(), &http.Server{}, ln, "", "", time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
}
