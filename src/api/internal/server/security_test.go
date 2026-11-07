package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestTLSBoundary(t *testing.T) {
	for _, test := range []struct {
		name, peer, host, forwarded string
		trusted                     bool
		want                        bool
	}{
		{"local development", "127.0.0.1:9000", "localhost:8080", "", false, true},
		{"public cleartext", "203.0.113.5:9000", "ting.example", "", false, false},
		{"spoofed proxy", "203.0.113.5:9000", "ting.example", "https", false, false},
		{"trusted proxy", "127.0.0.1:9000", "ting.example", "https", true, true},
		{"trusted cleartext proxy", "127.0.0.1:9000", "localhost", "http", true, false},
		{"unconfigured proxy", "127.0.0.1:9000", "ting.example", "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+test.host+"/", nil)
			r.RemoteAddr = test.peer
			r.Header.Set("X-Forwarded-Proto", test.forwarded)
			var proxies []netip.Prefix
			if test.trusted {
				proxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			}
			if got := secureRequest(r, proxies, false); got != test.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}
func TestRateLimit(t *testing.T) {
	l := newLimiter(DefaultLimits())
	now := time.Now()
	for i := 0; i < 120; i++ {
		if !l.allow("a", now) {
			t.Fatal("early limit")
		}
	}
	if l.allow("a", now) {
		t.Fatal("rate limit not enforced")
	}
	if !l.allow("b", now) || !l.allow("a", now.Add(time.Minute)) {
		t.Fatal("other peers or next window blocked")
	}
}

func TestConfiguredRateAndPeerBounds(t *testing.T) {
	c := DefaultLimits()
	c.RequestsPerMinute = 2
	c.MaxPeers = 1
	l := newLimiter(c)
	now := time.Now()
	if !l.allow("one", now) || !l.allow("one", now) || l.allow("one", now) {
		t.Fatal("configured request count ignored")
	}
	if l.allow("two", now) {
		t.Fatal("peer memory limit ignored")
	}
	if !l.allow("two", now.Add(time.Minute)) {
		t.Fatal("expired peer did not free its bucket")
	}
	if len(l.entries) != 1 {
		t.Fatal("peer map exceeded bound")
	}
}

func TestDockerLocalHTTPOptIn(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost:8080/v1/notifications", nil)
	r.RemoteAddr = "172.18.0.1:5000"
	if secureRequest(r, nil, false) {
		t.Fatal("private peer allowed without opt-in")
	}
	if !secureRequest(r, nil, true) {
		t.Fatal("development Docker bridge rejected")
	}
	r.Host = "ting.example"
	if secureRequest(r, nil, true) {
		t.Fatal("non-local hostname accepted over HTTP")
	}
	r.Host = "localhost"
	r.RemoteAddr = "203.0.113.5:5000"
	if secureRequest(r, nil, true) {
		t.Fatal("public peer accepted over HTTP")
	}
}
