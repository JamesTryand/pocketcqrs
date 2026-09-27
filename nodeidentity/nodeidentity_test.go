package nodeidentity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeFS stands in for the state directory so every combination of the
// contract's resolution inputs can be built, including "file present in a
// directory that cannot be written", which a real filesystem can't produce
// portably without chmod.
type fakeFS struct {
	file     []byte // nil: absent
	readErr  error  // non-nil: the file is present but unreadable
	writable bool
	writes   int
}

func (f *fakeFS) ReadFile(string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.file == nil {
		return nil, fs.ErrNotExist
	}
	return f.file, nil
}

func (f *fakeFS) WriteFileAtomic(_, _ string, data []byte) error {
	if !f.writable {
		return errors.New("read-only file system")
	}
	f.writes++
	f.file = append([]byte(nil), data...)
	return nil
}

const (
	assignedID = "orders-replica-2"
	storedID   = "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77"
	freshID    = "0192b5c4-0000-7000-8000-000000000001"
)

// want is the contract's resolution table (node-identity.md section 3),
// written out as a function of the three inputs so the test covers every one
// of the 3 x 4 x 2 = 24 combinations rather than one example per row.
func want(env, file, dir string) (nodeID string, kind Kind, bootFails, fileWritten bool) {
	switch {
	case env == "valid":
		return assignedID, Assigned, false, false
	case env == "invalid":
		return "", "", true, false
	case file == "valid":
		return storedID, Persistent, false, false
	case file == "absent" && dir == "writable":
		return freshID, Persistent, false, true
	default: // absent+unwritable, invalid, unreadable
		return freshID, Ephemeral, false, false
	}
}

func TestResolveEveryCombination(t *testing.T) {
	envs := map[string]string{"unset": "", "valid": assignedID, "invalid": "orders.replica 2"}
	for _, env := range []string{"unset", "valid", "invalid"} {
		for _, file := range []string{"absent", "valid", "invalid", "unreadable"} {
			for _, dir := range []string{"writable", "unwritable"} {
				t.Run(env+"/"+file+"/"+dir, func(t *testing.T) {
					f := &fakeFS{writable: dir == "writable"}
					switch file {
					case "valid":
						f.file = []byte(storedID + "\n")
					case "invalid":
						f.file = []byte("not a node id\n")
					case "unreadable":
						f.readErr = errors.New("input/output error")
					}
					before := append([]byte(nil), f.file...)
					var logged []string
					got, err := Resolve(Options{
						Assigned: envs[env], StateDir: "state", Role: "writer", Instance: "timesheets",
						Logf:     func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
						fs:       f,
						hostname: func() (string, error) { return "node-3", nil },
						newID:    func() (string, error) { return freshID, nil },
					})

					wantID, wantKind, wantFail, wantWrite := want(env, file, dir)
					if wantFail {
						if err == nil {
							t.Fatalf("want boot failure, got %+v", got)
						}
						return
					}
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if got.NodeID != wantID || got.Kind != wantKind {
						t.Errorf("got %s/%s, want %s/%s", got.NodeID, got.Kind, wantID, wantKind)
					}
					if wantWrite != (f.writes == 1) || f.writes > 1 {
						t.Errorf("writes = %d, want written=%v", f.writes, wantWrite)
					}
					if !wantWrite && !bytes.Equal(before, f.file) {
						t.Errorf("file changed: %q -> %q", before, f.file)
					}
					if wantKind == Ephemeral && len(logged) == 0 {
						t.Error("an ephemeral id must be logged")
					}
				})
			}
		}
	}
}

func TestAttributes(t *testing.T) {
	started := time.Date(2026, 9, 27, 1, 40, 12, 345_600_000, time.FixedZone("BST", 3600))
	got, err := Resolve(Options{
		Assigned: assignedID, Instance: "timesheets", Role: "reader", StartedAt: started,
		hostname: func() (string, error) { return "node-3", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Instance != "timesheets" || got.Host != "node-3" || got.Stack != "pocketcqrs" || got.Role != "reader" {
		t.Errorf("attributes: %+v", got)
	}
	if s := got.StartedAtRFC3339(); s != "2026-09-27T00:40:12.345Z" {
		t.Errorf("started_at = %s", s)
	}
}

func TestValid(t *testing.T) {
	for id, ok := range map[string]bool{
		storedID: true, "a": true, "A_z-9": true, strings.Repeat("x", 64): true,
		"": false, strings.Repeat("x", 65): false, "a.b": false, "a*": false, "a>": false,
		"a b": false, "a/b": false, "a+b": false, "a#b": false, "é": false,
	} {
		if Valid(id) != ok {
			t.Errorf("Valid(%q) = %v, want %v", id, !ok, ok)
		}
	}
}

// The rest run against the real filesystem.

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRestartKeepsNodeIDAndChangesStartedAt(t *testing.T) {
	dir := t.TempDir()
	first, err := Resolve(Options{StateDir: dir, StartedAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != Persistent || !uuidV7.MatchString(first.NodeID) {
		t.Fatalf("first boot: %s/%s, want a persistent lowercase UUIDv7", first.NodeID, first.Kind)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || string(data) != first.NodeID+"\n" {
		t.Fatalf("node-id file = %q, %v", data, err)
	}
	second, err := Resolve(Options{StateDir: dir, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if second.NodeID != first.NodeID || second.Kind != Persistent {
		t.Errorf("restart: %s/%s, want %s/persistent", second.NodeID, second.Kind, first.NodeID)
	}
	if second.StartedAtRFC3339() == first.StartedAtRFC3339() {
		t.Error("started_at did not change across the restart")
	}
}

func TestStoredIDIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("  "+storedID+"\r\n"), 0o644)
	got, err := Resolve(Options{StateDir: dir})
	if err != nil || got.NodeID != storedID || got.Kind != Persistent {
		t.Errorf("got %s/%s, %v", got.NodeID, got.Kind, err)
	}
}

func TestInvalidFileLeftByteIdentical(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty": {}, "garbage": []byte("orders.replica 2\n"), "binary": {0xff, 0xfe, 0x00, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			os.WriteFile(path, content, 0o644)
			for boot := 0; boot < 2; boot++ {
				got, err := Resolve(Options{StateDir: dir})
				if err != nil || got.Kind != Ephemeral {
					t.Fatalf("boot %d: %s/%s, %v; want ephemeral", boot, got.NodeID, got.Kind, err)
				}
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, content) {
				t.Errorf("file changed: %q -> %q", content, after)
			}
		})
	}
}

func TestAssignedNeverTouchesFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte(storedID+"\n"), 0o644)
	got, err := Resolve(Options{Assigned: assignedID, StateDir: dir})
	if err != nil || got.NodeID != assignedID || got.Kind != Assigned {
		t.Fatalf("got %s/%s, %v", got.NodeID, got.Kind, err)
	}
	// Removing the assignment returns the node to its stored id (I1).
	got, _ = Resolve(Options{StateDir: dir})
	if got.NodeID != storedID || got.Kind != Persistent {
		t.Errorf("after removing the assignment: %s/%s, want %s/persistent", got.NodeID, got.Kind, storedID)
	}
	empty := t.TempDir()
	Resolve(Options{Assigned: assignedID, StateDir: empty})
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("an assigned boot wrote to the state dir: %v", entries)
	}
}

func TestAtomicWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(Options{StateDir: dir}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir holds %v, want only %s", names, FileName)
	}
}

// A state "directory" that is really a regular file can't hold node-id: the
// portable stand-in for an unwritable directory.
func TestUnwritableStateDirIsEphemeral(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "pb_data")
	os.WriteFile(notADir, []byte("x"), 0o644)
	var logged []string
	got, err := Resolve(Options{StateDir: notADir, Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	if err != nil || got.Kind != Ephemeral || !uuidV7.MatchString(got.NodeID) {
		t.Fatalf("got %s/%s, %v; want an ephemeral UUIDv7", got.NodeID, got.Kind, err)
	}
	if len(logged) == 0 {
		t.Error("an ephemeral id must be logged")
	}
	if data, _ := os.ReadFile(notADir); string(data) != "x" {
		t.Error("the file standing in for the state dir was modified")
	}
}

// A node-id that is a directory is present but cannot be read as a file: the
// portable stand-in for an unreadable file.
func TestUnreadableFileIsEphemeralAndLeftAlone(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, FileName), 0o755)
	got, err := Resolve(Options{StateDir: dir})
	if err != nil || got.Kind != Ephemeral {
		t.Fatalf("got %s/%s, %v; want ephemeral", got.NodeID, got.Kind, err)
	}
	if st, err := os.Stat(filepath.Join(dir, FileName)); err != nil || !st.IsDir() {
		t.Error("the unreadable node-id was replaced")
	}
}

func TestNoStateDirIsEphemeral(t *testing.T) {
	got, err := Resolve(Options{})
	if err != nil || got.Kind != Ephemeral {
		t.Errorf("got %s/%s, %v; want ephemeral", got.NodeID, got.Kind, err)
	}
}

func TestInvalidAssignmentFailsBoot(t *testing.T) {
	dir := t.TempDir()
	_, err := Resolve(Options{Assigned: "cqrs.telemetry.*", StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), EnvNodeID) {
		t.Fatalf("err = %v, want an error naming %s", err, EnvNodeID)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("a failed boot wrote to the state dir")
	}
}
