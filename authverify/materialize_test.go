package authverify

import (
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestMaterializeBindsTheLocalCollection(t *testing.T) {
	// the verdict comes from the master; the app materializing it is a
	// DIFFERENT node that has the collection (from migrations) but not the
	// record row — exactly the multi-node shape
	master := openTestApp(t)
	rec, _ := newSuperuser(t, master, "f13-mat@example.com")
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}

	secondary := openTestApp(t)
	got, err := Materialize(secondary, &Verdict{
		CollectionName: core.CollectionNameSuperusers,
		CollectionID:   rec.Collection().Id,
		Record:         raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Id != rec.Id {
		t.Fatalf("id did not survive: %q != %q", got.Id, rec.Id)
	}
	// everything downstream of re.Auth reads these (gateway.actorMeta,
	// requireAuth's collection check, IsSuperuser)
	if got.Collection().Name != core.CollectionNameSuperusers {
		t.Fatalf("not bound to the local collection: %q", got.Collection().Name)
	}
	if !got.IsSuperuser() {
		t.Fatal("expected the materialized record to read as a superuser")
	}
	// the serialization helper keys must not have leaked in as data
	if got.GetString("collectionName") != "" || got.GetString("collectionId") != "" {
		t.Fatal("serialization helper keys leaked into the record data")
	}
}

func TestMaterializeRefusesNonAuthAndUnknownCollections(t *testing.T) {
	app := openTestApp(t)

	if _, err := Materialize(app, &Verdict{
		CollectionName: "no-such-collection",
		Record:         json.RawMessage(`{"id":"x"}`),
	}); err == nil {
		t.Fatal("expected an unknown collection to be refused")
	}
	if _, err := Materialize(app, &Verdict{
		CollectionName: core.CollectionNameAuthOrigins, // real, but not auth-type
		Record:         json.RawMessage(`{"id":"x"}`),
	}); err == nil {
		t.Fatal("expected a non-auth collection to be refused")
	}
	if _, err := Materialize(app, &Verdict{
		CollectionName: core.CollectionNameSuperusers,
		Record:         json.RawMessage(`{"email":"x@y.z"}`), // no id
	}); err == nil {
		t.Fatal("expected a record without an id to be refused")
	}
}
