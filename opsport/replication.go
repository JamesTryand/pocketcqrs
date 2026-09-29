package opsport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// EnvURL sets the ops port's base URL as readers reach it; --cqrsOpsURL
// defaults to it. The master writes it into its heartbeat (writer_ops_url) so
// a stale secondary can ask whether the master is up. Unset means
// http://<host>:<ops port>; set it wherever that hostname is not what readers
// can reach (NAT, container networks).
const EnvURL = "CQRS_OPS_URL"

// Defaults for --cqrsHeartbeatInterval and --cqrsStaleThreshold.
const (
	DefaultHeartbeatInterval = time.Second
	DefaultStaleThreshold    = 5 * time.Second
)

// AdvertisedURL is configured (CQRS_OPS_URL) if set, else
// http://<bind>:<port> when bind (CQRS_OPS_BIND) names a specific address,
// since nothing else reaches it, else http://<host>:<port>. Anything but an
// absolute http or https URL is an error, which fails the boot like any other
// invalid setting.
func AdvertisedURL(configured, bind, host string, port int) (string, error) {
	if configured == "" {
		if bind != "" && bind != "0.0.0.0" && bind != "::" {
			host = bind
		}
		return "http://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	u, err := url.Parse(configured)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid ops URL %q (%s or --cqrsOpsURL): use the ops port's base URL as readers reach it, e.g. http://node-3:10056", configured, EnvURL)
	}
	return strings.TrimRight(configured, "/"), nil
}

// ReplicationState is a reader's replication freshness, as the contract
// names it (STATE-MACHINES.md, machine 4, ReplicationFreshness). A writer has
// none.
type ReplicationState int

const (
	// ReplicationUnknown: no heartbeat row visible yet, or it could not be
	// read; nothing trustworthy to serve.
	ReplicationUnknown ReplicationState = iota
	// ReplicationFresh: the heartbeat is within the stale threshold.
	ReplicationFresh
	// StaleWriterUp: stale while the writer answers; this reader's own
	// replication is behind.
	StaleWriterUp
	// StaleWriterDown: stale and the writer does not answer; a shared cause,
	// so the reader stays in the pool.
	StaleWriterDown
)

// ReplicationStatus is a reader's latest measurement: its state and the
// heartbeat's age in seconds (0 when unknown).
type ReplicationStatus struct {
	State           ReplicationState
	WriteLagSeconds float64
}

// HeartbeatStore is where the heartbeat row lives; *events.Store satisfies
// it.
type HeartbeatStore interface {
	WriteHeartbeat(ctx context.Context, writerNodeID, writerOpsURL string, at time.Time) error
	ReadHeartbeat(ctx context.Context) (*events.Heartbeat, error)
}

// RunWriterHeartbeat is the writer's side (contract section 5): it upserts
// the heartbeat row every interval until ctx is done. A failed write is logged
// once per run of failures and retried on the next beat; it never stops the
// loop.
func RunWriterHeartbeat(ctx context.Context, store HeartbeatStore, writerNodeID, writerOpsURL string, interval time.Duration, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		if err := store.WriteHeartbeat(ctx, writerNodeID, writerOpsURL, time.Now()); err != nil && ctx.Err() == nil {
			if !failing {
				logf("writer heartbeat: write failed, readers will see this master as stale: %v", err)
			}
			failing = true
		} else if err == nil {
			if failing {
				logf("writer heartbeat: writing again")
			}
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReplicationMonitor is a reader's side (machine 4): it reads the row the
// writer upserts and, when it is older than the stale threshold, asks the
// writer's /healthz (at the row's writer_ops_url) whether the writer is up,
// which splits stale into a local problem (StaleWriterUp) and a shared one
// (StaleWriterDown). Clocks are assumed to agree (NTP); a heartbeat from the
// future counts as age 0. Give probe a short timeout.
type ReplicationMonitor struct {
	store   HeartbeatStore
	probe   *http.Client
	stale   time.Duration
	now     func() time.Time
	current atomic.Pointer[ReplicationStatus]
}

// NewReplicationMonitor starts Unknown until the first measurement.
func NewReplicationMonitor(store HeartbeatStore, probe *http.Client, staleThreshold time.Duration) *ReplicationMonitor {
	m := &ReplicationMonitor{store: store, probe: probe, stale: staleThreshold, now: time.Now}
	m.current.Store(&ReplicationStatus{State: ReplicationUnknown})
	return m
}

// Current is the latest measurement.
func (m *ReplicationMonitor) Current() ReplicationStatus { return *m.current.Load() }

// Measure measures once and records the result.
func (m *ReplicationMonitor) Measure(ctx context.Context) ReplicationStatus {
	s := m.measure(ctx)
	m.current.Store(&s)
	return s
}

func (m *ReplicationMonitor) measure(ctx context.Context) ReplicationStatus {
	row, err := m.store.ReadHeartbeat(ctx)
	if err != nil || row == nil {
		return ReplicationStatus{State: ReplicationUnknown}
	}
	written, err := time.Parse(events.HeartbeatTimeLayout, row.WrittenAt)
	if err != nil {
		return ReplicationStatus{State: ReplicationUnknown}
	}
	lag := max(0, m.now().Sub(written).Seconds())
	if lag <= m.stale.Seconds() {
		return ReplicationStatus{State: ReplicationFresh, WriteLagSeconds: lag}
	}
	if m.writerAnswers(ctx, row.WriterOpsURL) {
		return ReplicationStatus{State: StaleWriterUp, WriteLagSeconds: lag}
	}
	return ReplicationStatus{State: StaleWriterDown, WriteLagSeconds: lag}
}

// CheckWriter is the writer dependency's check (machine 3, on a secondary):
// an error unless the heartbeat names a master and its /healthz answers.
func (m *ReplicationMonitor) CheckWriter(ctx context.Context) error {
	row, err := m.store.ReadHeartbeat(ctx)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("no master heartbeat visible, so no master to check")
	}
	if !m.writerAnswers(ctx, row.WriterOpsURL) {
		return fmt.Errorf("the master at %s did not answer /healthz", row.WriterOpsURL)
	}
	return nil
}

// Run measures every interval until ctx is done.
func (m *ReplicationMonitor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.Measure(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *ReplicationMonitor) writerAnswers(ctx context.Context, opsURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(opsURL, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := m.probe.Do(req)
	if err != nil {
		// unreachable, refused or timed out: down, as far as this reader can tell
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
