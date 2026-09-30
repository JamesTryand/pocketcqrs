// Package opsport is the ops port of the cross-stack health/telemetry
// contract (platform/cqrs-runtime-contract/contracts/health-telemetry.md,
// 1.0, sections 2-7), identical to dotnetcqrs's OpsServer: /healthz, /readyz
// and /metrics, on a port of their own, never the traffic port.
//
// It binds first, before configuration is validated or PocketBase
// bootstraps, so a booting node answers instead of refusing connections.
// The port is CQRS_OPS_PORT (or --cqrsOpsPort), default DefaultPort,
// provisional until it is registered on the Prometheus wiki. Several nodes on
// one machine must each set it: a node that cannot bind its ops port does
// not start. The endpoints are unauthenticated; the network path is the
// security boundary, so the ops port must never be an ingress target.
//
// /readyz follows the lifecycle: starting while booting; BeginCatchUp once the
// traffic port listens and the consumers run; serving once every read model is
// within threshold, or, on a writer, once the catch-up deadline passes. The
// reporting tables in STATE-MACHINES.md are the spec for every status and
// reason.
package opsport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/nodeidentity"
)

const (
	// EnvPort sets the ops port; --cqrsOpsPort defaults to it.
	EnvPort = "CQRS_OPS_PORT"

	// EnvBind sets the address the ops port binds; --cqrsOpsBind defaults to
	// it. Empty means every interface, which a real node needs so an
	// orchestrator can reach it; tests bind 127.0.0.1.
	EnvBind = "CQRS_OPS_BIND"

	// DefaultPort is the contract's provisional default.
	DefaultPort = 10056

	// ContractVersion is the health/telemetry contract version implemented.
	ContractVersion = "1.0"

	// DefaultCatchUpDeadline is --cqrsCatchUpDeadline's default.
	DefaultCatchUpDeadline = 60 * time.Second

	// catchUpCheckInterval is how often a catching-up node re-checks its
	// read models, so it opens readiness (and logs a passed deadline) without
	// waiting for a probe to ask.
	catchUpCheckInterval = 250 * time.Millisecond
)

// Lifecycle is where the node is in its life, as the contract names it
// (STATE-MACHINES.md, machine 1, NodeLifecycle). Stopped and failed need no
// value: a process that has exited answers nothing.
type Lifecycle int32

const (
	// Booting: from process start to the main traffic port listening.
	Booting Lifecycle = iota
	// CatchingUp: listening, consumers started, read models not yet within
	// threshold.
	CatchingUp
	Serving
	// Draining: shutting down; readiness closed first, in-flight work
	// finishing.
	Draining
)

func (l Lifecycle) String() string {
	switch l {
	case Booting:
		return "booting"
	case CatchingUp:
		return "catching up"
	case Serving:
		return "serving"
	default:
		return "draining"
	}
}

// Health is what the node reports on its ops port. host, stack and
// started_at are known at process start; the identity is nil until
// SetIdentity. Safe for concurrent use: the ops server reads it while boot
// writes it.
type Health struct {
	host      string
	startedAt time.Time
	identity  atomic.Pointer[nodeidentity.Identity]
	lifecycle atomic.Int32

	// now is the clock the catch-up deadline is measured against (tests
	// replace it).
	now func() time.Time

	// mu guards the catch-up state below and serialises lifecycle moves out
	// of CatchingUp.
	mu              sync.Mutex
	consumers       func() []consumers.Status
	catchUpDeadline time.Time
	deadlineLogged  bool
	logf            func(string, ...any)
	stopCatchUp     chan struct{}

	metrics          *Metrics
	replication      atomic.Pointer[func() ReplicationStatus]
	dependencies     atomic.Pointer[Dependencies]
	mode             atomic.Pointer[func() string]
	functionsPartial atomic.Bool
}

// New starts in Booting with no identity.
func New(host string, startedAt time.Time) *Health {
	return &Health{host: host, startedAt: startedAt.UTC(), now: time.Now, logf: func(string, ...any) {}, metrics: newMetrics()}
}

// Host is the hostname reported in /healthz (the default ops URL's host).
func (h *Health) Host() string { return h.host }

// SetReplication sets where a reader's replication freshness comes from
// (machine 4), usually a ReplicationMonitor's Current. A reader without one
// reports replication_unknown; a writer ignores it.
func (h *Health) SetReplication(status func() ReplicationStatus) { h.replication.Store(&status) }

// SetDependencies sets the node's required dependencies (machine 3). Without
// them, dependencies is empty and none contributes.
func (h *Health) SetDependencies(d *Dependencies) { h.dependencies.Store(d) }

// SetMode sets where the system mode comes from (running or maintenance, the
// events.db meta key); maintenance reports degraded. Unset means running.
func (h *Health) SetMode(mode func() string) { h.mode.Store(&mode) }

// SetFunctionsPartial records that some JS function failed validation at boot
// and was skipped while the node serves anyway (without --cqrsStrictBoot);
// that reports degraded, functions_skipped. Fixed at boot.
func (h *Health) SetFunctionsPartial(partial bool) { h.functionsPartial.Store(partial) }

// Metrics is the /metrics series (contract section 6); they exist from
// process start.
func (h *Health) Metrics() *Metrics { return h.metrics }

// Consumers is the consumer engine's status once boot has completed; empty
// while booting.
func (h *Health) Consumers() []consumers.Status {
	h.mu.Lock()
	status := h.consumers
	h.mu.Unlock()
	if status == nil {
		return nil
	}
	return status()
}

// SetIdentity records the resolved identity; /healthz reports it from then on.
func (h *Health) SetIdentity(id nodeidentity.Identity) { h.identity.Store(&id) }

// Identity is the resolved identity, or nil while booting.
func (h *Health) Identity() *nodeidentity.Identity { return h.identity.Load() }

// SetLifecycle records where the node is in its life.
func (h *Health) SetLifecycle(l Lifecycle) { h.lifecycle.Store(int32(l)) }

// Lifecycle is where the node is in its life.
func (h *Health) Lifecycle() Lifecycle { return Lifecycle(h.lifecycle.Load()) }

// BeginCatchUp is boot completing (machine 1, BootCompleted): the traffic
// port listens and the consumers have started, so the node moves to catching
// up. status is the consumer engine's Status; only read models count. The
// node starts serving when every read model is current; if it is a writer and
// catchUpDeadline passes first, it starts serving anyway and reports what is
// still behind as degraded. A reader keeps catching up. logf (may be nil)
// hears when readiness opens and when the deadline passes. An error means
// boot had already completed.
func (h *Health) BeginCatchUp(status func() []consumers.Status, catchUpDeadline time.Duration, logf func(string, ...any)) error {
	h.mu.Lock()
	if l := h.Lifecycle(); l != Booting {
		h.mu.Unlock()
		return fmt.Errorf("boot already completed: the node is %s", l)
	}
	h.consumers = status
	h.catchUpDeadline = h.now().Add(catchUpDeadline)
	if logf != nil {
		h.logf = logf
	}
	h.stopCatchUp = make(chan struct{})
	h.SetLifecycle(CatchingUp)
	stop := h.stopCatchUp
	h.mu.Unlock()

	go func() {
		ticker := time.NewTicker(catchUpCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				h.Refresh()
			}
		}
	}()
	h.Refresh()
	return nil
}

// BeginDraining is shutdown being requested (machine 1, ShutdownRequested):
// /readyz closes (not_ready, draining) before anything else stops, so the
// pool stops routing here while in-flight work finishes. Idempotent, and the
// node stays draining until the process ends. A node still booting has no
// traffic to drain and just exits, so this does nothing. logf (may be nil)
// hears that draining began; nil uses the logger BeginCatchUp was given.
func (h *Health) BeginDraining(logf func(string, ...any)) {
	h.mu.Lock()
	if l := h.Lifecycle(); l == Booting || l == Draining {
		h.mu.Unlock()
		return
	}
	h.SetLifecycle(Draining)
	if h.stopCatchUp != nil {
		close(h.stopCatchUp)
		h.stopCatchUp = nil
	}
	if logf == nil {
		logf = h.logf
	}
	h.mu.Unlock()
	logf("draining: /readyz is not_ready; finishing in-flight work")
}

// Refresh, while catching up, moves to serving if every read model is
// current or, on a writer, the catch-up deadline has passed (machine 1's
// InitialCatchUpCompleted and CatchUpDeadlineReached). It runs on a ticker
// and before every /readyz; in any other state it does nothing.
func (h *Health) Refresh() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Lifecycle() != CatchingUp || h.consumers == nil {
		return
	}
	var notCurrent []consumers.Status
	for _, s := range h.consumers() {
		if s.ReadModel && s.State != consumers.Current {
			notCurrent = append(notCurrent, s)
		}
	}
	if len(notCurrent) == 0 {
		h.openReadiness("readiness opened: every read model is within its lag threshold")
		return
	}
	if h.now().Before(h.catchUpDeadline) {
		return
	}
	id := h.identity.Load()
	isWriter := id != nil && id.Role == "writer"
	if !h.deadlineLogged {
		h.deadlineLogged = true
		still := make([]string, len(notCurrent))
		for i, s := range notCurrent {
			still[i] = fmt.Sprintf("%s (%s)", s.Name, s.State)
		}
		if isWriter {
			h.logf("catch-up deadline reached; serving anyway as the writer, still catching up: %s", strings.Join(still, ", "))
		} else {
			h.logf("catch-up deadline reached; a reader keeps catching up before it serves: %s", strings.Join(still, ", "))
		}
	}
	if isWriter {
		h.openReadiness("")
	}
}

// openReadiness moves to Serving; h.mu is held.
func (h *Health) openReadiness(message string) {
	h.SetLifecycle(Serving)
	if h.stopCatchUp != nil {
		close(h.stopCatchUp)
		h.stopCatchUp = nil
	}
	if message != "" {
		h.logf("%s", message)
	}
}

// readiness is a /readyz status, in order of severity, so the most severe
// contribution is the maximum.
type readiness int

const (
	ready readiness = iota
	degraded
	notReady
)

func (r readiness) String() string {
	switch r {
	case ready:
		return "ready"
	case degraded:
		return "degraded"
	default:
		return "not_ready"
	}
}

// ReadyzBody is the GET /readyz body (contract section 4), fields in the
// contract's order. role and node_id are null while booting.
type ReadyzBody struct {
	Status          string       `json:"status"`
	Role            *string      `json:"role"`
	NodeID          *string      `json:"node_id"`
	ContractVersion string       `json:"contract_version"`
	Reasons         []string     `json:"reasons"`
	Checks          ReadyzChecks `json:"checks"`
}

// ReadyzChecks is /readyz's checks object.
type ReadyzChecks struct {
	// WriteLagSeconds is 0 on a writer; on a reader, the heartbeat's age.
	WriteLagSeconds      float64           `json:"write_lag_seconds"`
	ProjectionLagSeconds float64           `json:"projection_lag_seconds"`
	Dependencies         map[string]string `json:"dependencies"`
}

// Readyz is the current /readyz status code and body: 200 when ready or
// degraded, 503 when not_ready. The status is the most severe contribution of
// the lifecycle and the read models (STATE-MACHINES.md, "Readiness:"
// tables); reasons lists every non-ready one.
func (h *Health) Readyz() (int, ReadyzBody) {
	h.Refresh()
	id := h.identity.Load()
	type contribution struct {
		status readiness
		reason string
	}
	var contributions []contribution

	switch h.Lifecycle() {
	case Booting:
		contributions = append(contributions, contribution{notReady, "starting"})
	case CatchingUp:
		contributions = append(contributions, contribution{notReady, "catching_up"})
	case Draining:
		contributions = append(contributions, contribution{notReady, "draining"})
	}

	// read_models: the worst state across the node's read models. Behind or
	// blocked is local to this node, so not_ready on a reader; on the sole
	// writer it is degraded, so that a stuck projection never removes the
	// only write authority from the pool.
	h.mu.Lock()
	status := h.consumers
	h.mu.Unlock()
	worst, lag := consumers.Current, 0.0
	if status != nil {
		for _, s := range status() {
			if !s.ReadModel {
				continue
			}
			worst = max(worst, s.State)
			if s.LagSeconds != nil {
				lag = max(lag, *s.LagSeconds)
			}
		}
	}
	if worst != consumers.Current {
		severity := notReady
		if id != nil && id.Role == "writer" {
			severity = degraded
		}
		reason := "projection_behind"
		if worst == consumers.Blocked {
			reason = "projection_blocked"
		}
		contributions = append(contributions, contribution{severity, reason})
	}

	// replication (readers only): stale because this reader is behind is
	// local, not_ready; stale because the writer is down is shared, degraded;
	// never having seen a heartbeat leaves nothing trustworthy to serve.
	writeLag := 0.0
	if id != nil && id.Role == "reader" {
		r := ReplicationStatus{State: ReplicationUnknown}
		if f := h.replication.Load(); f != nil {
			r = (*f)()
		}
		writeLag = r.WriteLagSeconds
		switch r.State {
		case ReplicationUnknown:
			contributions = append(contributions, contribution{notReady, "replication_unknown"})
		case StaleWriterUp:
			contributions = append(contributions, contribution{notReady, "replication_stale"})
		case StaleWriterDown:
			contributions = append(contributions, contribution{degraded, "replication_stale"})
		}
	}

	// event_store: this node's own store is local, so not_ready on a reader;
	// the sole writer stays in the pool as degraded. shared_dependencies (the
	// master, on a secondary) fail every node at once, so degraded, one reason
	// however many are down.
	dependencies := map[string]string{}
	if d := h.dependencies.Load(); d != nil {
		sharedDown := false
		for _, s := range d.States() {
			state := "up"
			if !s.Up {
				state = "down"
				if s.Name == DepEventStore {
					severity := notReady
					if id != nil && id.Role == "writer" {
						severity = degraded
					}
					contributions = append(contributions, contribution{severity, "event_store_unavailable"})
				} else {
					sharedDown = true
				}
			}
			dependencies[s.Name] = state
		}
		if sharedDown {
			contributions = append(contributions, contribution{degraded, "dependency_unavailable"})
		}
	}
	if f := h.mode.Load(); f != nil && (*f)() == "maintenance" {
		contributions = append(contributions, contribution{degraded, "maintenance"})
	}
	if h.functionsPartial.Load() {
		contributions = append(contributions, contribution{degraded, "functions_skipped"})
	}

	overall, reasons := ready, []string{}
	for _, c := range contributions {
		overall = max(overall, c.status)
		reasons = append(reasons, c.reason)
	}
	body := ReadyzBody{
		Status:          overall.String(),
		ContractVersion: ContractVersion,
		Reasons:         reasons,
		Checks: ReadyzChecks{
			WriteLagSeconds:      float64(int64(writeLag*1000+0.5)) / 1000,
			ProjectionLagSeconds: float64(int64(lag*1000+0.5)) / 1000,
			Dependencies:         dependencies,
		},
	}
	if id != nil {
		role, nodeID := id.Role, id.NodeID
		body.Role, body.NodeID = &role, &nodeID
	}
	if overall == notReady {
		return http.StatusServiceUnavailable, body
	}
	return http.StatusOK, body
}

// HealthzBody is the GET /healthz body (contract section 3), fields in the
// contract's order. node_id, identity, instance and role are null while
// booting; once the node has booted every field is set.
type HealthzBody struct {
	Status          string  `json:"status"`
	ContractVersion string  `json:"contract_version"`
	NodeID          *string `json:"node_id"`
	Identity        *string `json:"identity"`
	Instance        *string `json:"instance"`
	Host            string  `json:"host"`
	Stack           string  `json:"stack"`
	Role            *string `json:"role"`
	StartedAt       string  `json:"started_at"`
}

// Healthz is the current /healthz body.
func (h *Health) Healthz() HealthzBody {
	body := HealthzBody{
		Status:          "alive",
		ContractVersion: ContractVersion,
		Host:            h.host,
		Stack:           nodeidentity.Stack,
		StartedAt:       h.startedAt.Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if id := h.identity.Load(); id != nil {
		nodeID, kind, instance, role := id.NodeID, string(id.Kind), id.Instance, id.Role
		body.NodeID, body.Identity, body.Instance, body.Role = &nodeID, &kind, &instance, &role
	}
	return body
}

// ParsePort reads an ops port setting: empty means DefaultPort; anything
// but 0-65535 is an error, which fails the boot like any other invalid
// setting. 0 picks a free port (tests).
func ParsePort(configured string) (int, error) {
	if configured == "" {
		return DefaultPort, nil
	}
	port, err := strconv.Atoi(configured)
	if err != nil || port < 0 || port > 65535 || strconv.Itoa(port) != configured {
		return 0, fmt.Errorf("invalid ops port %q (%s or --cqrsOpsPort): use a number from 0 to 65535", configured, EnvPort)
	}
	return port, nil
}

// Server is a started ops port.
type Server struct {
	listener net.Listener
	server   *http.Server
}

// Start binds the ops port on bind (empty: every interface) and serves h on
// it. The bind is synchronous: an error (the port is taken) means the node
// must not start.
func Start(h *Health, bind string, port int) (*Server, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("ops port: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Healthz())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		body := h.metrics.Render(r.Context(), h)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		code, body := h.Readyz()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
	s := &Server{listener: listener, server: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
	// Serve returns only when the server is shut down or the listener fails;
	// either way the ops port has stopped answering, which is all there is.
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

// Addr is where it is listening (the real port when started on port 0).
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Close stops serving.
func (s *Server) Close(ctx context.Context) error { return s.server.Shutdown(ctx) }
