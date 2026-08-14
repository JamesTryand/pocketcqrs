package writeguard

import (
	"context"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestRegisterDeniesExternalWriteOnAuthOrigins(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	Register(app, AuthOrigins)

	collection, err := app.FindCollectionByNameOrId(AuthOrigins)
	if err != nil {
		t.Fatal(err)
	}
	superusers, err := app.FindCollectionByNameOrId("_superusers")
	if err != nil {
		t.Fatal(err)
	}
	superuserRecords, err := app.FindAllRecords(superusers)
	if err != nil || len(superuserRecords) == 0 {
		t.Fatalf("expected at least one seeded superuser record, err=%v", err)
	}
	record := core.NewRecord(collection)
	record.Set("collectionRef", superusers.Id)
	record.Set("recordRef", superuserRecords[0].Id)
	record.Set("fingerprint", "test-fingerprint")

	err = app.Save(record)
	if err == nil {
		t.Fatal("expected an external write to _authOrigins to be denied")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected the writeguard denial message, got a different error: %v", err)
	}
}

func TestRegisterAllowsInternalWriteOnAuthOrigins(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	Register(app, AuthOrigins)

	collection, err := app.FindCollectionByNameOrId(AuthOrigins)
	if err != nil {
		t.Fatal(err)
	}
	superusers, err := app.FindCollectionByNameOrId("_superusers")
	if err != nil {
		t.Fatal(err)
	}
	superuserRecords, err := app.FindAllRecords(superusers)
	if err != nil || len(superuserRecords) == 0 {
		t.Fatalf("expected at least one seeded superuser record, err=%v", err)
	}
	record := core.NewRecord(collection)
	record.Set("collectionRef", superusers.Id)
	record.Set("recordRef", superuserRecords[0].Id)
	record.Set("fingerprint", "test-fingerprint")

	ctx := MarkInternal(context.Background())
	if err := app.SaveWithContext(ctx, record); err != nil {
		t.Fatalf("expected an internal write to _authOrigins to succeed, got %v", err)
	}
}
