package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"selfupdate/internal/update/manifest"
	"selfupdate/internal/update/transfer"
)

func (u *Updater) Check(ctx context.Context) (*manifest.Manifest, error) {
	murl := strings.TrimRight(u.cfg.BaseURL, "/") + "/" + manifest.Path(u.cfg.Channel, u.cfg.GOOS, u.cfg.GOARCH)
	body, err := u.fetchManifest(ctx, murl)
	if errors.Is(err, errNoRelease) {
		if u.noReleaseLogged.CompareAndSwap(false, true) {
			u.log.Warn("no manifest at update URL (HTTP 404); nothing published yet, or wrong -update-url/channel?", "url", murl)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var env manifest.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	m, err := manifest.Open(u.keys, env)
	if err != nil {
		return nil, err
	}
	if err := u.checkManifest(m); err != nil {
		return nil, err
	}
	nv, _ := manifest.ParseVersion(m.Version)

	skip, err := u.store.Screen(m.IssuedAt, u.cfg.BuildTime, nv, u.now())
	if err != nil {
		return nil, err
	}
	if skip || nv.Compare(u.cur) <= 0 {
		return nil, nil
	}

	base, _ := url.Parse(murl)
	ref, err := url.Parse(m.URL)
	if err != nil {
		return nil, fmt.Errorf("bad package url: %w", err)
	}
	abs := base.ResolveReference(ref)
	if err := transfer.RequireSecureURL(abs.String(), u.cfg.AllowInsecure); err != nil {
		return nil, fmt.Errorf("package url: %w", err)
	}
	m.URL = abs.String()
	u.verifiedMu.Lock()
	u.verified = &m
	u.verifiedMu.Unlock()
	return &m, nil
}

func (u *Updater) checkManifest(m manifest.Manifest) error {
	if m.Channel != u.cfg.Channel || m.OS != u.cfg.GOOS || m.Arch != u.cfg.GOARCH {
		return fmt.Errorf("manifest is for %s/%s_%s, want %s/%s_%s",
			m.Channel, m.OS, m.Arch, u.cfg.Channel, u.cfg.GOOS, u.cfg.GOARCH)
	}
	if m.Size > u.cfg.MaxSize {
		return fmt.Errorf("manifest announces %d bytes, over the %d limit", m.Size, u.cfg.MaxSize)
	}
	return m.CheckFresh(u.now())
}

func (u *Updater) checkApplicable(m manifest.Manifest) error {
	u.verifiedMu.Lock()
	verified := u.verified != nil && *u.verified == m
	u.verifiedMu.Unlock()
	if !verified {
		return errors.New("manifest was not returned by Check (unverified); refusing to install")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := u.checkManifest(m); err != nil {
		return err
	}
	nv, _ := manifest.ParseVersion(m.Version)
	if nv.Compare(u.cur) <= 0 {
		return fmt.Errorf("version %s is not newer than running %s", nv, u.cur)
	}
	if pu, err := url.Parse(m.URL); err != nil || !pu.IsAbs() {
		return fmt.Errorf("package url %q is not absolute", m.URL)
	}
	return transfer.RequireSecureURL(m.URL, u.cfg.AllowInsecure)
}

func (u *Updater) fetchManifest(ctx context.Context, rawURL string) ([]byte, error) {
	var clientID string
	if !u.cfg.NoClientID {
		clientID = u.store.ClientID()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", u.userAgent())
	if clientID != "" {
		req.Header.Set(manifest.ClientIDHeader, clientID)
	}
	resp, err := u.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNoRelease
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, manifest.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > manifest.MaxBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", rawURL, manifest.MaxBytes)
	}
	return b, nil
}
