// Package natstransport is the NATS binding of the telemetry push
// (health/telemetry contract section 8.2): each snapshot is published to the
// subject cqrs.telemetry.metrics.<node_id>. node_id is one literal subject
// token by construction (node-identity 1.2). Kept apart from package
// telemetry so only a binary that registers it carries a bus client.
package natstransport

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/jamestryand/pocketcqrs/telemetry"
)

const (
	// Scheme is the URL scheme that selects this transport.
	Scheme = "nats"
	// SubjectPrefix is the subject prefix; the node id is the last token.
	SubjectPrefix = "cqrs.telemetry.metrics."
)

// Register adds this transport for nats:// URLs.
func Register(t *telemetry.Transports) *telemetry.Transports {
	return t.Register(Scheme, func(u *url.URL, logf func(string, ...any)) (telemetry.Transport, error) {
		return New(u, logf)
	})
}

// Transport publishes to NATS. Best-effort: while the connection is not open a
// publish fails instead of queueing (reconnect buffering is off), so a snapshot
// is never delivered late, and the client reconnects in the background without
// limit. A URL may carry credentials, nats://user:pass@host:4222, and is never
// logged.
type Transport struct {
	conn *nats.Conn
}

// New starts connecting in the background and returns at once, whether or not
// the bus is reachable yet.
func New(u *url.URL, logf func(string, ...any)) (*Transport, error) {
	conn, err := nats.Connect(u.String(),
		nats.Name("pocketcqrs-telemetry"),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
		nats.ReconnectBufSize(-1), // never queue while disconnected
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logf("telemetry: NATS connection lost; snapshots are dropped until it returns")
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) { logf("telemetry: NATS reconnected") }),
	)
	if err != nil {
		return nil, err
	}
	return &Transport{conn: conn}, nil
}

// Publish sends one snapshot, or fails at once if NATS is not connected.
func (t *Transport) Publish(ctx context.Context, key string, payload []byte) error {
	if t.conn.Status() != nats.CONNECTED {
		return errors.New("not connected to NATS")
	}
	if err := t.conn.Publish(SubjectPrefix+key, payload); err != nil {
		return err
	}
	return t.conn.FlushWithContext(ctx)
}

// Close closes the connection.
func (t *Transport) Close() error {
	t.conn.Close()
	return nil
}
