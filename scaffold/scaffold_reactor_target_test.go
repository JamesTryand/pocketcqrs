package scaffold

import (
	"strings"
	"testing"
)

// A reactor's target is the stream named by the triggering event's IDField
// (eventmodelschema 5.0.0 targetIdField), in both generators. Nothing is
// derived from the event's own id or its stream's.
func TestReactorTargetIDs(t *testing.T) {
	d := Domain{
		Aggregate: "order",
		Commands:  []Command{{Name: "AddComment", Events: []Event{{Name: "CommentAdded", NoFields: true}}}},
		Reactors: []Reactor{
			{Name: "notify", On: []string{"CommentAdded"}, Aggregate: "notification", Command: "Raise", IDField: "notificationId"},
			{Name: "touch", On: []string{"CommentAdded"}, Aggregate: "order", Command: "Touch", IDField: "orderId"},
		},
	}
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	gen, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	var goFiles []File
	for _, f := range gen {
		goFiles = append(goFiles, File{Name: f.Name, Source: f.Source})
	}
	src := func(files []File, name string) string {
		for _, f := range files {
			if strings.HasPrefix(f.Name, name+".") {
				return f.Source
			}
		}
		t.Fatalf("no file %s", name)
		return ""
	}
	for name, field := range map[string]string{"notify": "notificationId", "touch": "orderId"} {
		if s := src(js, name); !strings.Contains(s, `id: event.data["`+field+`"],`) {
			t.Errorf("JS reactor %s:\n%s", name, s)
		}
		if s := src(goFiles, name); !strings.Contains(s, `ID:        reactors.TargetID(ev, "`+field+`"),`) {
			t.Errorf("Go reactor %s:\n%s", name, s)
		}
	}
	for _, f := range append(js, goFiles...) {
		if strings.Contains(f.Source, "+ event.id") || strings.Contains(f.Source, "+ ev.ID") ||
			strings.Contains(f.Source, "+ event.aggregateId") || strings.Contains(f.Source, "+ ev.AggregateID") {
			t.Errorf("%s still derives a target id:\n%s", f.Name, f.Source)
		}
	}
}
