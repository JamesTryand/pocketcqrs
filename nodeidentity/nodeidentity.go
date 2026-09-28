// Package nodeidentity resolves this node's identity at boot, per the
// cross-stack node-identity contract (platform/cqrs-runtime-contract,
// contracts/node-identity.md, 1.0): an opaque node_id that survives restarts
// and tells apart several nodes on one machine, plus the descriptive
// attributes reported beside it.
//
// Resolution order: an assigned id (CQRS_NODE_ID, or --cqrsNodeId), then the
// node-id file in the node's own state directory, then a freshly generated
// UUIDv7 written to that file. The state directory must be node-local and
// never replicated -- here it is PocketBase's data dir (pb_data/), which the
// multi-node design already keeps per-node; only events.db is replicated.
//
// The resolver never rewrites or deletes an existing node-id file: a damaged
// one gives an ephemeral id for this process and is left for an operator to
// inspect, rather than being silently replaced by a new identity.
package nodeidentity

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// EnvNodeID assigns the id explicitly; an orchestrator that knows the
	// workload and replica sets it.
	EnvNodeID = "CQRS_NODE_ID"

	// FileName is the stored id's file name inside the state directory.
	FileName = "node-id"

	// Stack is this stack's name as the contract reports it.
	Stack = "pocketcqrs"

	// EnvInstance sets the instance attribute explicitly (contract I6);
	// unset, the caller's own workload name is used.
	EnvInstance = "CQRS_INSTANCE"

	// UnknownHost is reported when the hostname cannot be read (contract I7).
	UnknownHost = "unknown"
)

// Kind says where node_id came from; it is reported as `identity`.
type Kind string

const (
	// Assigned: from CQRS_NODE_ID. Stable without any volume, and
	// intentional, so it wins over a stored id and never touches the file.
	Assigned Kind = "assigned"
	// Persistent: read from, or just written to, the state directory. It
	// says nothing about whether that directory survives a restart.
	Persistent Kind = "persistent"
	// Ephemeral: generated for this process only.
	Ephemeral Kind = "ephemeral"
)

// format is the contract's id format: one literal token on NATS, Kafka,
// RabbitMQ and MQTT, a Prometheus label value and a file's content.
var format = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Valid reports whether id matches the contract's node_id format.
func Valid(id string) bool { return format.MatchString(id) }

// ValidateAssigned checks an assigned id (empty means unassigned). An
// invalid one fails the boot, like any other invalid configuration.
func ValidateAssigned(id string) error {
	if id == "" || Valid(id) {
		return nil
	}
	return fmt.Errorf("invalid node id %q (%s or --cqrsNodeId): want 1-64 characters from A-Z a-z 0-9 _ -", id, EnvNodeID)
}

// ValidateInstance checks an explicit instance name (empty means use the
// workload's own name). It has node_id's format; an invalid one fails the
// boot, like any other invalid configuration.
func ValidateInstance(name string) error {
	if name == "" || Valid(name) {
		return nil
	}
	return fmt.Errorf("invalid instance %q (%s or --cqrsInstance): want 1-64 characters from A-Z a-z 0-9 _ -", name, EnvInstance)
}

// Identity is who this node is. NodeID and Kind identify it; the rest
// describe it and are not part of it.
type Identity struct {
	NodeID    string
	Kind      Kind
	Instance  string    // the workload or model name
	Host      string    // the hostname
	Stack     string    // always "pocketcqrs"
	Role      string    // "writer" or "reader"
	StartedAt time.Time // process start, UTC
}

// StartedAtRFC3339 is StartedAt as the contract reports it: UTC RFC3339 with
// milliseconds.
func (i Identity) StartedAtRFC3339() string {
	return i.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Options are Resolve's inputs.
type Options struct {
	// Assigned is CQRS_NODE_ID (or --cqrsNodeId); empty means unset.
	Assigned string
	// StateDir is the node-local, non-replicated state directory. Empty
	// behaves as a directory that cannot be written.
	StateDir string

	Instance  string
	Role      string
	StartedAt time.Time // zero means now

	// Logf receives the warning and error lines the contract asks for
	// (unwritable directory, unusable file). Nil discards them.
	Logf func(format string, args ...any)

	// fs, hostname and newID are seams for tests; nil means the real ones.
	fs       fileSystem
	hostname func() (string, error)
	newID    func() (string, error)
}

// Resolve determines this node's identity. The only error is an invalid
// assigned id, which must fail the boot; every other fault still starts the
// node, with an ephemeral id.
func Resolve(o Options) (Identity, error) {
	if err := ValidateAssigned(o.Assigned); err != nil {
		return Identity{}, err
	}
	fsys, logf := o.fs, o.Logf
	if fsys == nil {
		fsys = osFS{}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	hostname, newID := o.hostname, o.newID
	if hostname == nil {
		hostname = os.Hostname
	}
	if newID == nil {
		newID = newUUIDv7
	}

	started := o.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	host, err := hostname()
	if err != nil || host == "" {
		// never a boot failure: host describes the node, it does not identify it
		logf("warning: node identity: cannot read the hostname (%v); reporting host=%s", err, UnknownHost)
		host = UnknownHost
	}
	id := Identity{Instance: o.Instance, Host: host, Stack: Stack, Role: o.Role, StartedAt: started.UTC()}

	if o.Assigned != "" {
		id.NodeID, id.Kind = o.Assigned, Assigned
		return id, nil
	}

	fresh, err := newID()
	if err != nil {
		return Identity{}, fmt.Errorf("node identity: generate id: %w", err)
	}
	if o.StateDir == "" {
		logf("warning: node identity: no state directory; node_id %s is ephemeral and changes on every restart (set %s to keep one)", fresh, EnvNodeID)
		id.NodeID, id.Kind = fresh, Ephemeral
		return id, nil
	}
	path := filepath.Join(o.StateDir, FileName)

	data, err := fsys.ReadFile(path)
	switch {
	case err == nil:
		if stored := strings.TrimSpace(string(data)); Valid(stored) {
			id.NodeID, id.Kind = stored, Persistent
			return id, nil
		}
		logf("error: node identity: %s is empty or not a valid node id; left untouched, using ephemeral node_id %s until it is removed or rewritten", path, fresh)
	case errors.Is(err, fs.ErrNotExist):
		if werr := fsys.WriteFileAtomic(o.StateDir, FileName, []byte(fresh+"\n")); werr != nil {
			logf("warning: node identity: cannot write %s (%v); node_id %s is ephemeral and changes on every restart (set %s to keep one)", path, werr, fresh, EnvNodeID)
			break
		}
		id.NodeID, id.Kind = fresh, Persistent
		return id, nil
	default:
		logf("error: node identity: cannot read %s (%v); left untouched, using ephemeral node_id %s until it is fixed", path, err, fresh)
	}
	id.NodeID, id.Kind = fresh, Ephemeral
	return id, nil
}

// newUUIDv7 is a canonical, lowercase, hyphenated UUIDv7 (RFC 9562).
func newUUIDv7() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// fileSystem is the little of the filesystem Resolve touches.
type fileSystem interface {
	// ReadFile returns an error matching fs.ErrNotExist when the file is
	// absent.
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes dir/name so that no reader ever sees a partial
	// file.
	WriteFileAtomic(dir, name string, data []byte) error
}

type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFileAtomic writes a temporary file in dir, syncs it, then renames it
// into place, removing the temporary file on any failure. Resolve calls it
// only after finding no file, so it never replaces an existing id.
func (osFS) WriteFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmpName)
	}
	return err
}
