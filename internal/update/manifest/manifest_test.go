package manifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{Version: "1.0.0", Channel: "stable", OS: "linux", Arch: "amd64",
		SHA256: strings.Repeat("a", 64), Size: 1, URL: "x",
		IssuedAt: time.Now().Add(-time.Hour), Expires: time.Now().Add(24 * time.Hour)}
}

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "01.2.3", "1.2.-3", "+1.2.3", "1.2.3-rc1"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) should fail", s)
		}
	}
	a, _ := ParseVersion("v1.10.0")
	b, _ := ParseVersion("1.9.9")
	if a.Compare(b) != 1 || b.Compare(a) != -1 || a.Compare(a) != 0 {
		t.Error("numeric (not lexical) comparison broken")
	}
}

func TestSignOpen(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	env, err := Sign(priv, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	keys := func(k ed25519.PublicKey) []ed25519.PublicKey { return []ed25519.PublicKey{k} }
	if _, err := Open(keys(pub), env); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	tampered := env
	tampered.Payload = []byte(strings.Replace(string(env.Payload), "1.0.0", "9.9.9", 1))
	if _, err := Open(keys(pub), tampered); err == nil {
		t.Error("tampered payload accepted")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Open(keys(otherPub), env); err == nil {
		t.Error("wrong key accepted")
	}
	if _, err := Open(keys(pub), Envelope{Payload: env.Payload}); err == nil {
		t.Error("missing signature accepted")
	}
}

func TestValidateChannel(t *testing.T) {
	for _, ok := range []string{"stable", "beta-2", "a.b_c"} {
		if ValidateChannel(ok) != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "A", "-x", "a b"} {
		if ValidateChannel(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateRequiresValidityWindow(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	for name, mutate := range map[string]func(*Manifest){
		"no issued_at":      func(m *Manifest) { m.IssuedAt = time.Time{} },
		"no expires":        func(m *Manifest) { m.Expires = time.Time{} },
		"expires<=issued":   func(m *Manifest) { m.Expires = m.IssuedAt },
		"expires<issued":    func(m *Manifest) { m.Expires = m.IssuedAt.Add(-time.Second) },
		"bad sha":           func(m *Manifest) { m.SHA256 = "zz" },
		"non-positive size": func(m *Manifest) { m.Size = 0 },
	} {
		m := testManifest()
		mutate(&m)
		if _, err := Sign(priv, m); err == nil {
			t.Errorf("%s: Sign accepted an invalid manifest", name)
		}
	}
}

func TestSignRejectsMalformedKeyInsteadOfPanicking(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	for _, k := range []ed25519.PrivateKey{nil, make(ed25519.PrivateKey, 10)} {
		if _, err := Sign(k, testManifest()); err == nil {
			t.Error("malformed key accepted")
		}
	}
}

func TestOpenAcceptsEitherTrustedKey(t *testing.T) {
	oldPub, _, _ := ed25519.GenerateKey(rand.Reader)
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	strangerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	env, err := Sign(newPriv, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open([]ed25519.PublicKey{oldPub, newPub}, env); err != nil {
		t.Errorf("rotated-in key rejected: %v", err)
	}
	if _, err := Open([]ed25519.PublicKey{oldPub, strangerPub}, env); err == nil {
		t.Error("untrusted signer accepted")
	}
	if _, err := Open(nil, env); err == nil {
		t.Error("empty key set accepted")
	}
	if _, err := Open([]ed25519.PublicKey{newPub, []byte("short")}, env); err == nil {
		t.Error("malformed key in the set was ignored")
	}
}
