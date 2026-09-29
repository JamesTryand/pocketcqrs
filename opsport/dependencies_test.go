package opsport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Required dependencies, mode and functions (contract section 4.6,
// STATE-MACHINES.md machine 3 and the "Readiness: event_store",
// "shared_dependencies", "mode" and "functions" tables).

func states(d *Dependencies) string {
	var out []string
	for _, s := range d.States() {
		out = append(out, fmt.Sprintf("%s=%v", s.Name, s.Up))
	}
	return strings.Join(out, ",")
}

func TestAFirstCheckThatFailsIsDownAtOnce(t *testing.T) {
	d := NewDependencies(3, nil)
	d.Record("writer", false)
	if got := states(d); got != "writer=false" {
		t.Errorf("got %s", got)
	}
}

func TestAnUpDependencyGoesDownOnlyAfterNConsecutiveFailuresAndBackUpOnOneSuccess(t *testing.T) {
	var log []string
	d := NewDependencies(3, func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) })
	d.Record("writer", true)
	d.Record("writer", false)
	d.Record("writer", false)
	if got := states(d); got != "writer=true" {
		t.Fatalf("after 2 failures: %s", got)
	}
	d.Record("writer", false)
	if got := states(d); got != "writer=false" {
		t.Fatalf("after 3 failures: %s", got)
	}
	d.Record("writer", true)
	if got := states(d); got != "writer=true" {
		t.Fatalf("after a success: %s", got)
	}
	if strings.Join(log, "|") != "dependency writer: down|dependency writer: up again" {
		t.Errorf("log %v", log)
	}
}

func TestASuccessResetsTheFailureCount(t *testing.T) {
	d := NewDependencies(2, nil)
	for _, ok := range []bool{true, false, true, false} {
		d.Record("writer", ok)
	}
	if got := states(d); got != "writer=true" {
		t.Errorf("got %s", got)
	}
}

func TestChecksThatErrAreFailuresAndTheEventStoreIsListedFirst(t *testing.T) {
	d := NewDependencies(1, nil)
	d.Add(DepWriter, func(context.Context) error { return errors.New("refused") })
	d.Add(DepEventStore, func(context.Context) error { return nil })

	d.CheckAll(context.Background())

	if got := states(d); got != "event_store=true,writer=false" {
		t.Errorf("got %s", got)
	}
}

// --- Readiness ---

func servingWith(t *testing.T, role string, deps map[string]bool) *node {
	n := newNode(t, role)
	d := NewDependencies(1, nil)
	for name, up := range deps {
		d.Record(name, up)
	}
	n.health.SetDependencies(d)
	n.beginCatchUp()
	return n
}

func TestTheEventStoreDownIsLocalSoNotReadyExceptOnTheWriter(t *testing.T) {
	for role, want := range map[string]string{
		"writer": "200 degraded [event_store_unavailable]",
		"reader": "503 not_ready [event_store_unavailable]",
	} {
		n := servingWith(t, role, map[string]bool{DepEventStore: false})
		expect(t, n.readyz(), want)
		if _, body := n.health.Readyz(); body.Checks.Dependencies[DepEventStore] != "down" {
			t.Errorf("%s: dependencies %v", role, body.Checks.Dependencies)
		}
	}
}

func TestASharedDependencyDownIsDegradedOnAnyRole(t *testing.T) {
	for _, role := range []string{"writer", "reader"} {
		n := servingWith(t, role, map[string]bool{DepEventStore: true, DepWriter: false})
		expect(t, n.readyz(), "200 degraded [dependency_unavailable]")
	}
}

func TestEveryDependencyUpIsReadyAndListed(t *testing.T) {
	n := servingWith(t, "reader", map[string]bool{DepEventStore: true, DepWriter: true})
	expect(t, n.readyz(), "200 ready []")
	if _, body := n.health.Readyz(); fmt.Sprint(body.Checks.Dependencies) != "map[event_store:up writer:up]" {
		t.Errorf("dependencies %v", body.Checks.Dependencies)
	}
}

func TestMaintenanceModeIsDegraded(t *testing.T) {
	n := newNode(t, "writer")
	n.health.SetMode(func() string { return "maintenance" })
	n.beginCatchUp()
	expect(t, n.readyz(), "200 degraded [maintenance]")

	n.health.SetMode(func() string { return "running" })
	expect(t, n.readyz(), "200 ready []")
}

func TestSkippedFunctionsAreDegraded(t *testing.T) {
	n := newNode(t, "reader")
	n.health.SetFunctionsPartial(true)
	n.beginCatchUp()
	expect(t, n.readyz(), "200 degraded [functions_skipped]")
}

func TestADependencyDownShowsInMetricsAsZero(t *testing.T) {
	n := servingWith(t, "writer", map[string]bool{DepEventStore: true, DepWriter: false})
	body := n.health.Metrics().Render(context.Background(), n.health)
	expectValue(t, body, `cqrs_dependency_up{dependency="event_store"}`, "1")
	expectValue(t, body, `cqrs_dependency_up{dependency="writer"}`, "0")
}

// --- The master, on a secondary ---

func TestTheWriterCheckPassesOnlyWhenTheHeartbeatsWriterAnswers(t *testing.T) {
	up, _ := writer(t, http.StatusOK)
	down, _ := writer(t, http.StatusServiceUnavailable)
	ctx := context.Background()

	if err := monitor(&fakeHeartbeats{row: row(measuredAt, up)}).CheckWriter(ctx); err != nil {
		t.Errorf("writer up: %v", err)
	}
	if err := monitor(&fakeHeartbeats{row: row(measuredAt, down)}).CheckWriter(ctx); err == nil {
		t.Error("writer answering 503: want an error")
	}
	if err := monitor(&fakeHeartbeats{}).CheckWriter(ctx); err == nil {
		t.Error("no heartbeat: want an error")
	}
}
