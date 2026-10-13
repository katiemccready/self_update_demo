package transfer

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

func RequireSecureURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("refusing insecure URL %q: updates require https", raw)
	default:
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func baseTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func NewHTTPClient(allowInsecure bool) *http.Client {
	tr := baseTransport()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.TLSHandshakeTimeout = 15 * time.Second
	return HardenClient(&http.Client{Transport: tr}, allowInsecure)
}

// HardenClient returns a copy of c whose redirects are subject to our policy and
// then to c's own CheckRedirect, so a custom client keeps the downgrade protection.
func HardenClient(c *http.Client, allowInsecure bool) *http.Client {
	cp := *c
	next := c.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := checkRedirect(req, via, allowInsecure); err != nil {
			return err
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
	return &cp
}

func checkRedirect(req *http.Request, via []*http.Request, allowInsecure bool) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %s", req.URL.Scheme)
	}

	return RequireSecureURL(req.URL.String(), allowInsecure)
}
