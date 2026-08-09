// Package outbound is a hard-bounded HTTP client for the effect and reactor
// tiers: the one sanctioned way for pocketcqrs to call a third party.
//
// It exists because "call an external service" is common enough that
// requiring a separate component for it is the wrong default, and because an
// unbounded call from inside the process is a blast radius nobody chose. A
// hanging or flapping third party must not be able to starve the core, and a
// failure must not turn into a retry storm.
//
// Every guarantee here is enforced, not documented:
//
//   - the destination host must be on a GLOBAL allow-list, checked before any
//     network I/O and before DNS;
//   - the resolved IP is checked again at dial time, so a hostile resolver
//     cannot aim an allow-listed name at loopback or the metadata endpoint;
//   - redirects are never followed automatically, so a 302 cannot walk a call
//     off the allow-list;
//   - one attempt, no retry — the caller dead-letters, which is the failure
//     model the consumer engine already has;
//   - a process-wide cap on in-flight calls, so one chatty domain cannot
//     exhaust the process;
//   - a response body cap, because an unbounded body is the same class of
//     hazard as an unbounded call.
//
// NOTHING HERE IS REACHABLE FROM A DECIDER OR PROJECTION. That is enforced by
// the caller (the binding is installed only on effect/reactor VMs), not by
// this package, which has no idea who is calling it.
//
// For Go callers this package is a convention rather than an enforcement —
// Go code can always reach net/http directly. It is exported anyway so that
// Go reactors and out-of-process components share one implementation of the
// guardrails instead of each growing their own.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Failure modes callers (and tests) need to tell apart. A refusal is not a
// transport error: it means the call never happened.
var (
	// ErrHostNotAllowed means the destination is not on the allow-list. No
	// DNS lookup and no connection were attempted.
	ErrHostNotAllowed = errors.New("outbound: host is not on the allow-list")

	// ErrBlockedAddress means the host resolved to an address class that is
	// refused: link-local (the cloud metadata endpoint) always, and private
	// or loopback ranges unless AllowPrivate is set.
	ErrBlockedAddress = errors.New("outbound: destination address is blocked")

	// ErrAtCapacity means the process-wide in-flight cap was saturated for
	// the whole of this call's deadline.
	ErrAtCapacity = errors.New("outbound: too many calls in flight")

	// ErrBodyTooLarge means the response exceeded MaxBodyBytes. The body is
	// NOT returned truncated — a silently short body is worse than an error,
	// because the caller cannot tell it happened.
	ErrBodyTooLarge = errors.New("outbound: response body exceeds the cap")

	// ErrBadRequest means the request was malformed before any I/O: an
	// unsupported scheme, an unparseable URL, a rejected method.
	ErrBadRequest = errors.New("outbound: bad request")
)

// Config describes one deployment's outbound policy. It comes from boot
// flags; nothing here is per-function, deliberately — function code is
// hot-reloadable without a code-review gate, so a per-function allow-list
// would be authored by whoever authored the call.
type Config struct {
	// AllowedHosts is the global allow-list, as hostnames without ports.
	// Matching is exact and case-insensitive; there are no wildcards, because
	// "*.example.com" is a larger grant than it looks.
	//
	// An EMPTY list allows NOTHING. It is not "unset, so permit everything" —
	// v0.4.0 fixed a writeguard bug that was exactly that misreading.
	AllowedHosts []string

	// AllowPrivate permits loopback, RFC1918 and unique-local destinations.
	// Dev-only in spirit, but an internal API on 10.x is an ordinary
	// deployment shape too. Link-local stays blocked regardless.
	AllowPrivate bool

	// Timeout bounds one call end to end. It must sit under the JS VM's
	// execution budget so a slow call surfaces as a catchable error rather
	// than as a VM interrupt, which tells the author nothing.
	Timeout time.Duration

	// MaxInFlight caps concurrent outbound calls process-wide.
	MaxInFlight int

	// MaxBodyBytes caps the response body.
	MaxBodyBytes int64
}

// Client is safe for concurrent use and is shared process-wide: the in-flight
// cap only means anything if every caller goes through the same one.
type Client struct {
	cfg     Config
	allowed map[string]struct{}
	sem     chan struct{}
	http    *http.Client
}

// Request is one outbound call. Body is a string because it crosses into a JS
// VM, where bytes have no natural representation.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

// Response is what came back. A non-2xx is a Response, not an error — only a
// call that did not complete is an error.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// methods is the permitted verb set. POST matters as much as GET: firing a
// webhook is the primary effect-tier use.
var methods = map[string]struct{}{
	http.MethodGet: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodHead: {},
}

// New builds a client from cfg. It returns an error rather than silently
// correcting a nonsensical config, because every field here is a bound and a
// bound that quietly became something else is not a bound.
func New(cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("outbound: Timeout must be positive, got %v", cfg.Timeout)
	}
	if cfg.MaxInFlight <= 0 {
		return nil, fmt.Errorf("outbound: MaxInFlight must be positive, got %d", cfg.MaxInFlight)
	}
	if cfg.MaxBodyBytes <= 0 {
		return nil, fmt.Errorf("outbound: MaxBodyBytes must be positive, got %d", cfg.MaxBodyBytes)
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedHosts))
	for _, h := range cfg.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.Contains(h, "/") || strings.Contains(h, ":") {
			return nil, fmt.Errorf("outbound: allow-list wants a bare hostname, got %q", h)
		}
		allowed[h] = struct{}{}
	}

	c := &Client{
		cfg:     cfg,
		allowed: allowed,
		sem:     make(chan struct{}, cfg.MaxInFlight),
	}

	// Control runs after DNS resolution and before connect, with the address
	// actually about to be dialled. Checking here rather than on the URL is
	// what makes the address policy unbypassable: every connection attempt
	// goes through it, including each hop of anything that retries internally.
	dialer := &net.Dialer{
		Timeout: cfg.Timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			return c.checkDialAddr(address)
		},
	}

	c.http = &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   cfg.Timeout,
			ResponseHeaderTimeout: cfg.Timeout,
			DisableKeepAlives:     true,
		},
		// Never follow a redirect. An allow-listed host answering
		// "302 Location: http://169.254.169.254/..." would otherwise walk the
		// call straight off the allow-list. Handing the 3xx back means a
		// function that wants the next hop must ask for it, and that ask is
		// checked like any other call.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c, nil
}

// checkDialAddr adjudicates a resolved address.
func (c *Client) checkDialAddr(address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable address %q", ErrBlockedAddress, address)
	}
	addr := ap.Addr().Unmap()

	// Always refused, with no override. 169.254.169.254 is the cloud metadata
	// endpoint and hands out instance credentials; no domain-driven outbound
	// call has a reason to reach it, and it is the highest-value SSRF target
	// there is. Unspecified and multicast are refused for the same "nothing
	// legitimate points here" reason.
	switch {
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return fmt.Errorf("%w: %s is link-local", ErrBlockedAddress, addr)
	case addr.IsUnspecified():
		return fmt.Errorf("%w: %s is unspecified", ErrBlockedAddress, addr)
	case addr.IsMulticast():
		return fmt.Errorf("%w: %s is multicast", ErrBlockedAddress, addr)
	case addr.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s is interface-local", ErrBlockedAddress, addr)
	}

	if c.cfg.AllowPrivate {
		return nil
	}
	switch {
	case addr.IsLoopback():
		return fmt.Errorf("%w: %s is loopback (--cqrsAllowPrivateOutbound permits it)", ErrBlockedAddress, addr)
	case addr.IsPrivate():
		return fmt.Errorf("%w: %s is a private range (--cqrsAllowPrivateOutbound permits it)", ErrBlockedAddress, addr)
	}
	return nil
}

// AllowsHost reports whether host is on the allow-list. Exported so callers
// can answer "would this be refused?" without making the call.
func (c *Client) AllowsHost(host string) bool {
	_, ok := c.allowed[strings.ToLower(host)]
	return ok
}

// Do performs one call. One attempt: there is no retry here, deliberately.
// The caller's failure path (dead-letter, checkpoint still advances) is the
// failure model this system already has, and a retry budget hidden inside a
// binding would be a second one nobody asked for.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if _, ok := methods[method]; !ok {
		return nil, fmt.Errorf("%w: method %q is not permitted", ErrBadRequest, method)
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q is not permitted", ErrBadRequest, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: no host in %q", ErrBadRequest, req.URL)
	}

	// Before any I/O, including DNS. A refused call must leave no trace on
	// the network at all — otherwise the allow-list still leaks which hosts
	// were asked for.
	if !c.AllowsHost(host) {
		return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// Wait for a slot, but only for the remainder of this call's own
	// deadline. Blocking indefinitely would hold a JS VM open on a slow third
	// party, which is the starvation the cap exists to prevent; failing
	// instantly would turn a brief burst into a pile of dead letters.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w (cap %d)", ErrAtCapacity, c.cfg.MaxInFlight)
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}

	resp, err := c.http.Do(hreq)
	if err != nil {
		// Unwrap so a blocked address stays recognisable through the
		// url.Error the client wraps it in.
		if errors.Is(err, ErrBlockedAddress) {
			return nil, err
		}
		return nil, fmt.Errorf("outbound: %s %s: %w", method, host, err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap: that is how "exactly at the cap" and "over
	// it" are told apart without trusting Content-Length, which a hostile or
	// broken server controls.
	limited := io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("outbound: reading response from %s: %w", host, err)
	}
	if int64(len(raw)) > c.cfg.MaxBodyBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, c.cfg.MaxBodyBytes)
	}

	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return &Response{Status: resp.StatusCode, Headers: headers, Body: string(raw)}, nil
}
