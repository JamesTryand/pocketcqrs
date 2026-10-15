package scaffold

import (
	"strings"
	"testing"
)

// R3: a PerEvent reactor keys its target by the triggering event's id, in
// both generators; otherwise by the source stream's id, as before.
func TestReactorTargetIDs(t *testing.T) {
	d := Domain{
		Aggregate: "order",
		Commands:  []Command{{Name: "AddComment", Events: []Event{{Name: "CommentAdded", NoFields: true}}}},
		Reactors: []Reactor{
			{Name: "notify", On: []string{"CommentAdded"}, Aggregate: "notification", Command: "Raise", IDPrefix: "notify-", PerEvent: true},
			{Name: "touch", On: []string{"CommentAdded"}, Aggregate: "order", Command: "Touch"},
		},
	}
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	goFiles, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
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
	goSrc := func(name string) string {
		for _, f := range goFiles {
			if strings.HasPrefix(f.Name, name+".") {
				return f.Source
			}
		}
		t.Fatalf("no Go file %s", name)
		return ""
	}
	if s := src(js, "notify"); !strings.Contains(s, "id: 'notify-' + event.id,") {
		t.Errorf("JS per-event reactor:\n%s", s)
	}
	if s := src(js, "touch"); !strings.Contains(s, "id: '' + event.aggregateId,") {
		t.Errorf("JS same-stream reactor:\n%s", s)
	}
	if s := goSrc("notify"); !strings.Contains(s, `ID:        "notify-" + ev.ID,`) {
		t.Errorf("Go per-event reactor:\n%s", s)
	}
	if s := goSrc("touch"); !strings.Contains(s, `ID:        "" + ev.AggregateID,`) {
		t.Errorf("Go same-stream reactor:\n%s", s)
	}
}
