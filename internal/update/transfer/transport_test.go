package transfer

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedirectPolicyHoldsEveryHopToTheTransportRules(t *testing.T) {
	loop, _ := http.NewRequest("GET", "http://127.0.0.1:9/x", nil)
	remote, _ := http.NewRequest("GET", "http://203.0.113.9/x", nil)
	if err := checkRedirect(remote, []*http.Request{loop}, false); err == nil {
		t.Error("loopback http -> remote http redirect allowed")
	}
	if err := checkRedirect(remote, []*http.Request{loop}, true); err != nil {
		t.Errorf("explicit AllowInsecure still refused: %v", err)
	}
	if err := checkRedirect(loop, []*http.Request{loop}, false); err != nil {
		t.Errorf("loopback -> loopback refused: %v", err)
	}
}

func TestCallerSuppliedClientStillGetsRedirectProtection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.9/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	called := false
	custom := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { called = true; return nil }}
	c := HardenClient(custom, false)
	resp, err := c.Get(srv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "refusing insecure URL") {
		t.Fatalf("err = %v, want the insecure redirect refused", err)
	}
	if called {
		t.Error("caller's CheckRedirect ran before ours vetoed the hop")
	}
	if custom.CheckRedirect == nil || HardenClient(custom, false) == custom {
		t.Error("HardenClient must copy, not mutate, the caller's client")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReplacedDefaultTransportDoesNotPanic(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })
	defer func() { http.DefaultTransport = old }()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	c := NewHTTPClient(false)
	if c.Transport == nil || c.CheckRedirect == nil {
		t.Error("client not configured")
	}
}

func TestRedirectFromHTTPSNeverDropsToPlainHTTP(t *testing.T) {
	https, _ := http.NewRequest("GET", "https://updates.example.com/m", nil)
	loopback, _ := http.NewRequest("GET", "http://127.0.0.1:8080/m", nil)
	for _, allowInsecure := range []bool{false, true} {
		if err := checkRedirect(loopback, []*http.Request{https}, allowInsecure); err == nil {
			t.Errorf("https -> http://127.0.0.1 redirect allowed (allowInsecure=%v)", allowInsecure)
		}
	}
}

func TestRedirectNeverDowngradesToHTTP(t *testing.T) {
	https, _ := http.NewRequest("GET", "https://a.example/x", nil)
	plain, _ := http.NewRequest("GET", "http://a.example/x", nil)
	if err := checkRedirect(plain, []*http.Request{https}, false); err == nil {
		t.Error("https->http redirect allowed")
	}
	if err := checkRedirect(https, []*http.Request{https}, false); err != nil {
		t.Errorf("https->https redirect refused: %v", err)
	}
}
