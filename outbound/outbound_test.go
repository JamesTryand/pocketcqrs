package outbound

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig is a permissive baseline; each test tightens the one bound it
// is about. AllowPrivate is on because httptest serves on loopback — the
// address policy itself is tested directly in TestCheckDialAddr, where it
// can be exercised against addresses no test server could bind.
func testConfig(hosts ...string) Config {
	return Config{
		AllowedHosts: hosts,
		AllowPrivate: true,
		Timeout:      2 * time.Second,
		MaxInFlight:  4,
		MaxBodyBytes: 1 << 20,
	}
}

func mustNew(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestAllowedHostReachesTheServer(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("X-Probe", "yes")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := mustNew(t, testConfig("127.0.0.1"))
	resp, err := c.Do(context.Background(), Request{Method: "POST", URL: srv.URL, Body: "x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 201 || resp.Body != "hello" {
		t.Fatalf("got %d %q", resp.Status, resp.Body)
	}
	if resp.Headers["X-Probe"] != "yes" {
		t.Errorf("headers not carried: %v", resp.Headers)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}

// A refused host must leave NO trace on the network. Asserting only that an
// error came back would pass even if the request had been sent and then
// discarded, which is not the same guarantee.
func TestDisallowedHostIsRefusedBeforeAnyIO(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	c := mustNew(t, testConfig("example.com")) // NOT 127.0.0.1
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("server was contacted %d time(s); the refusal must precede all I/O", got)
	}
}

// An empty allow-list means "allow nothing", not "unset, so allow anything".
// v0.4.0 fixed a writeguard bug that was precisely the other reading.
func TestEmptyAllowListRefusesEverything(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	c := mustNew(t, testConfig())
	if _, err := c.Do(context.Background(), Request{URL: srv.URL}); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("an empty allow-list permitted a call")
	}
}

// The address policy, exercised against addresses no test server can bind.
// This is the SSRF guard: an allow-listed hostname pointed at loopback or the
// cloud metadata endpoint by a hostile resolver.
func TestCheckDialAddr(t *testing.T) {
	cases := []struct {
		addr          string
		blockedStrict bool // with AllowPrivate = false
		blockedLoose  bool // with AllowPrivate = true
	}{
		{"93.184.216.34:443", false, false}, // ordinary public v4
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", false, false},

		// Always blocked. 169.254.169.254 is the cloud metadata endpoint.
		{"169.254.169.254:80", true, true},
		{"169.254.1.1:80", true, true},
		{"[fe80::1]:80", true, true},
		{"0.0.0.0:80", true, true},
		{"[::]:80", true, true},
		{"224.0.0.1:80", true, true},

		// Blocked unless AllowPrivate.
		{"127.0.0.1:80", true, false},
		{"[::1]:80", true, false},
		{"10.0.0.5:80", true, false},
		{"172.16.3.4:80", true, false},
		{"192.168.1.10:80", true, false},
		{"[fc00::1]:80", true, false},
	}

	for _, tc := range cases {
		strict := mustNew(t, Config{AllowPrivate: false, Timeout: time.Second, MaxInFlight: 1, MaxBodyBytes: 1024})
		loose := mustNew(t, Config{AllowPrivate: true, Timeout: time.Second, MaxInFlight: 1, MaxBodyBytes: 1024})

		if got := strict.checkDialAddr(tc.addr) != nil; got != tc.blockedStrict {
			t.Errorf("strict %s: blocked = %v, want %v", tc.addr, got, tc.blockedStrict)
		}
		if got := loose.checkDialAddr(tc.addr) != nil; got != tc.blockedLoose {
			t.Errorf("AllowPrivate %s: blocked = %v, want %v", tc.addr, got, tc.blockedLoose)
		}
	}
}

// The dial-time check must actually be wired into the client, not merely
// exist as a function. Loopback with AllowPrivate off is the reachable case.
func TestDialCheckIsWiredIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should never arrive"))
	}))
	defer srv.Close()

	cfg := testConfig("127.0.0.1")
	cfg.AllowPrivate = false // host allow-listed, address still refused
	c := mustNew(t, cfg)

	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

// A 302 must come back as a 302, not be followed. Otherwise an allow-listed
// host can walk a call anywhere it likes.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var secondHits int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		_, _ = w.Write([]byte("followed"))
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	c := mustNew(t, testConfig("127.0.0.1"))
	resp, err := c.Do(context.Background(), Request{URL: first.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302 handed back unfollowed", resp.Status)
	}
	if got := atomic.LoadInt32(&secondHits); got != 0 {
		t.Fatalf("redirect target was contacted %d time(s)", got)
	}
	if resp.Headers["Location"] == "" {
		t.Error("Location header not returned, so the function cannot act on the redirect")
	}
}

// Over the cap is an ERROR, never a quietly truncated body. A short body the
// caller cannot detect is the same shape of silent wrong-doing this project
// has already fixed in projections, cron triggers and the write-guard.
func TestBodyCapErrorsRatherThanTruncating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 5000)))
	}))
	defer srv.Close()

	cfg := testConfig("127.0.0.1")
	cfg.MaxBodyBytes = 1000
	c := mustNew(t, cfg)

	resp, err := c.Do(context.Background(), Request{URL: srv.URL})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if resp != nil {
		t.Fatalf("a truncated response was returned alongside the error: %d bytes", len(resp.Body))
	}
}

func TestBodyExactlyAtCapIsFine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 1000)))
	}))
	defer srv.Close()

	cfg := testConfig("127.0.0.1")
	cfg.MaxBodyBytes = 1000
	c := mustNew(t, cfg)

	resp, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("a body exactly at the cap was rejected: %v", err)
	}
	if len(resp.Body) != 1000 {
		t.Fatalf("body = %d bytes, want 1000", len(resp.Body))
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	cfg := testConfig("127.0.0.1")
	cfg.Timeout = 250 * time.Millisecond
	c := mustNew(t, cfg)

	start := time.Now()
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hanging server did not produce an error")
	}
	if errors.Is(err, ErrAtCapacity) {
		t.Fatalf("failed on the cap, not the timeout: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v; the timeout was not enforced", elapsed)
	}
}

// Saturated means "wait for the rest of your own deadline, then fail" —
// neither an instant failure nor an unbounded wait. The slot is taken
// directly so the assertion does not race a real server.
func TestAtCapacityWaitsThenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	cfg := testConfig("127.0.0.1")
	cfg.MaxInFlight = 1
	cfg.Timeout = 300 * time.Millisecond
	c := mustNew(t, cfg)

	c.sem <- struct{}{} // occupy the only slot

	start := time.Now()
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("err = %v, want ErrAtCapacity", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("failed after %v; it should wait out its deadline, not fail instantly", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("waited %v; the wait is not bounded by the deadline", elapsed)
	}
}

func TestCapacityIsReleasedAfterEachCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	cfg := testConfig("127.0.0.1")
	cfg.MaxInFlight = 1
	c := mustNew(t, cfg)

	for i := range 3 {
		if _, err := c.Do(context.Background(), Request{URL: srv.URL}); err != nil {
			t.Fatalf("call %d: %v (a leaked slot would wedge this)", i, err)
		}
	}
}

func TestRejectedRequests(t *testing.T) {
	c := mustNew(t, testConfig("example.com"))
	cases := []struct {
		name string
		req  Request
	}{
		{"file scheme", Request{URL: "file:///etc/passwd"}},
		{"no scheme", Request{URL: "example.com/x"}},
		{"unparseable", Request{URL: "http://[::1"}},
		{"bad method", Request{Method: "CONNECT", URL: "https://example.com"}},
	}
	for _, tc := range cases {
		if _, err := c.Do(context.Background(), tc.req); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestNewRejectsUnboundedConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no timeout", Config{MaxInFlight: 1, MaxBodyBytes: 1}},
		{"no cap", Config{Timeout: time.Second, MaxBodyBytes: 1}},
		{"no body cap", Config{Timeout: time.Second, MaxInFlight: 1}},
		{"host with port", Config{Timeout: time.Second, MaxInFlight: 1, MaxBodyBytes: 1, AllowedHosts: []string{"example.com:443"}}},
		{"host with path", Config{Timeout: time.Second, MaxInFlight: 1, MaxBodyBytes: 1, AllowedHosts: []string{"example.com/x"}}},
	}
	for _, tc := range cases {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestAllowListIsCaseInsensitive(t *testing.T) {
	c := mustNew(t, testConfig("API.Example.COM"))
	if !c.AllowsHost("api.example.com") {
		t.Error("allow-list matching is case-sensitive")
	}
	if c.AllowsHost("evil.com") {
		t.Error("an unlisted host was allowed")
	}
}

// No wildcards: "*.example.com" is a much larger grant than it appears, and
// a subdomain takeover would inherit it.
func TestNoWildcardMatching(t *testing.T) {
	c := mustNew(t, testConfig("example.com"))
	if c.AllowsHost("evil.example.com") {
		t.Error("a subdomain matched a bare host entry")
	}
}
