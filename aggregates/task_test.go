package aggregates

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"pocketcqrs/decider"
	"pocketcqrs/events"
)

func setup(t *testing.T) *decider.Registry {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := decider.NewRegistry(store)
	RegisterAll(r)
	return r
}

func cmd(name string, payload any) decider.Command {
	data, _ := json.Marshal(payload)
	return decider.Command{Name: name, Payload: data}
}

func TestTaskLifecycle(t *testing.T) {
	r := setup(t)
	ctx := t.Context()

	// create
	evts, err := r.Handle(ctx, TaskAggregate, "t1", cmd(CmdCreateTask, map[string]string{"title": "write tests"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 1 || evts[0].Type != TaskCreated {
		t.Fatalf("unexpected events: %+v", evts)
	}

	// duplicate create is rejected
	if _, err := r.Handle(ctx, TaskAggregate, "t1", cmd(CmdCreateTask, map[string]string{"title": "again"})); err == nil {
		t.Fatal("expected domain error on duplicate create")
	}

	// complete
	if _, err := r.Handle(ctx, TaskAggregate, "t1", cmd(CmdCompleteTask, nil)); err != nil {
		t.Fatal(err)
	}

	// double complete is rejected
	if _, err := r.Handle(ctx, TaskAggregate, "t1", cmd(CmdCompleteTask, nil)); err == nil {
		t.Fatal("expected domain error on double complete")
	}
}

func TestTaskValidations(t *testing.T) {
	r := setup(t)
	ctx := t.Context()

	if _, err := r.Handle(ctx, TaskAggregate, "t1", cmd(CmdCreateTask, map[string]string{"title": ""})); err == nil {
		t.Fatal("expected error on empty title")
	}
	if _, err := r.Handle(ctx, TaskAggregate, "missing", cmd(CmdCompleteTask, nil)); err == nil {
		t.Fatal("expected error completing missing task")
	}
	if _, err := r.Handle(ctx, TaskAggregate, "t1", cmd("Nonsense", nil)); err == nil {
		t.Fatal("expected error on unknown command")
	}
	if _, err := r.Handle(ctx, "nope", "t1", cmd(CmdCreateTask, map[string]string{"title": "x"})); !errors.Is(err, decider.ErrUnknownAggregate) {
		t.Fatalf("expected ErrUnknownAggregate, got %v", err)
	}
}
