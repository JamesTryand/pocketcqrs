// Package opsport is the ops port of the cross-stack health/telemetry
// contract (platform/cqrs-runtime-contract/contracts/health-telemetry.md,
// 1.0, sections 2 and 3), identical to dotnetcqrs's OpsServer: /healthz, and
// later /readyz and /metrics, on a port of their own, never the traffic port.
//
// It binds first, before configuration is validated or PocketBase
// bootstraps, so a booting node answers instead of refusing connections.
// The port is CQRS_OPS_PORT (or --cqrsOpsPort), default DefaultPort,
// provisional until it is registered on the Prometheus wiki. Several nodes on
// one machine must each set it: a node that cannot bind its ops port does
// not start. The endpoints are unauthenticated; the network path is the
// security boundary, so the ops port must never be an ingress target.
package opsport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

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

// Health is what the node reports on its ops port. host, stack and
// started_at are known at process start; the identity is nil until
// SetIdentity. Safe for concurrent use: the ops server reads it while boot
// writes it.
type Health struct {
	host      string
	startedAt time.Time
	identity  atomic.Pointer[nodeidentity.Identity]
	lifecycle atomic.Int32
}

// New starts in Booting with no identity.
func New(host string, startedAt time.Time) *Health {
	return &Health{host: host, startedAt: startedAt.UTC()}
}

// SetIdentity records the resolved identity; /healthz reports it from then on.
func (h *Health) SetIdentity(id nodeidentity.Identity) { h.identity.Store(&id) }

// Identity is the resolved identity, or nil while booting.
func (h *Health) Identity() *nodeidentity.Identity { return h.identity.Load() }

// SetLifecycle records where the node is in its life.
func (h *Health) SetLifecycle(l Lifecycle) { h.lifecycle.Store(int32(l)) }

// Lifecycle is where the node is in its life.
func (h *Health) Lifecycle() Lifecycle { return Lifecycle(h.lifecycle.Load()) }

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
