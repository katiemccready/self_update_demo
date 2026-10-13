package manifest

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Manifest struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`

	URL   string `json:"url"`
	Notes string `json:"notes,omitempty"`

	Mandatory bool      `json:"mandatory,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	Expires   time.Time `json:"expires"`
}

const maxClockSkew = 24 * time.Hour

var (
	ErrStaleManifest    = errors.New("manifest expired")
	ErrReplayedManifest = errors.New("manifest is older than one already seen")
)

func (m Manifest) CheckFresh(now time.Time) error {
	if now.After(m.Expires) {
		return fmt.Errorf("%w at %s (client clock %s); the publisher must run `updatectl refresh`",
			ErrStaleManifest, m.Expires.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if m.IssuedAt.After(now.Add(maxClockSkew)) {
		return fmt.Errorf("manifest issued in the future (%s) relative to client clock %s; check system time",
			m.IssuedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

type Envelope struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

const signingContext = "selfupdate-manifest-v1\n"

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Sign(priv ed25519.PrivateKey, m Manifest) (Envelope, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Envelope{}, errors.New("invalid private key")
	}
	if err := m.Validate(); err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Payload:   payload,
		Signature: ed25519.Sign(priv, append([]byte(signingContext), payload...)),
	}, nil
}

func Open(keys []ed25519.PublicKey, env Envelope) (Manifest, error) {
	if len(keys) == 0 {
		return Manifest{}, errors.New("no public keys")
	}
	msg := append([]byte(signingContext), env.Payload...)
	verified := false
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return Manifest{}, errors.New("invalid public key")
		}
		if ed25519.Verify(k, msg, env.Signature) {
			verified = true
		}
	}
	if !verified {
		return Manifest{}, errors.New("manifest signature verification failed")
	}
	var m Manifest
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if _, err := ParseVersion(m.Version); err != nil {
		return err
	}
	switch {
	case m.Channel == "" || m.OS == "" || m.Arch == "":
		return errors.New("manifest missing channel/os/arch")
	case !sha256Hex.MatchString(m.SHA256):
		return errors.New("manifest has malformed sha256")
	case m.Size <= 0:
		return errors.New("manifest has non-positive size")
	case m.URL == "":
		return errors.New("manifest missing url")
	case m.IssuedAt.IsZero() || m.Expires.IsZero():
		return errors.New("manifest missing issued_at/expires")
	case !m.Expires.After(m.IssuedAt):
		return errors.New("manifest expires before it was issued")
	}
	return nil
}
