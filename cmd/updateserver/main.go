package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"selfupdate/internal/release"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dir := flag.String("dir", "releases", "release tree root (must not contain signing keys)")
	certFile := flag.String("tls-cert", "", "TLS certificate (PEM); with -tls-key serves https directly")
	keyFile := flag.String("tls-key", "", "TLS private key (PEM)")
	flag.Parse()
	if (*certFile == "") != (*keyFile == "") {
		slog.Error("-tls-cert and -tls-key must be given together")
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/releases/", http.StripPrefix("/releases", release.Handler(*dir)))

	srv := &http.Server{
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	tls := *certFile != ""
	if !tls {
		slog.Warn("serving plain http: clients require https for non-loopback hosts, so terminate TLS in front of this (CDN/proxy) or pass -tls-cert/-tls-key")
	}
	slog.Info("serving releases", "addr", ln.Addr().String(), "dir", *dir, "tls", tls)
	if err := serve(ctx, srv, ln, *certFile, *keyFile, 30*time.Second); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener, certFile, keyFile string, grace time.Duration) error {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			slog.Warn("shutdown incomplete; closing remaining connections", "err", err)
			_ = srv.Close()
		}
	}()
	var err error
	if certFile != "" {
		err = srv.ServeTLS(ln, certFile, keyFile)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-drained
		return nil
	}
	return err
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "ua", r.UserAgent(), "dur", time.Since(start).Round(time.Millisecond))
	})
}
