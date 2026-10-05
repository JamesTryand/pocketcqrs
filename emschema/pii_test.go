package emschema

import (
	"strings"
	"testing"
)

// piiDocument is dateRangeDocument with staffId as personal data on the
// command, the event and the read model, subject entryId on each — a
// document that imports clean.
func piiDocument() *Document {
	doc := dateRangeDocument()
	for _, id := range []string{"log-time"} {
		c := doc.Commands[id]
		c.Fields[1].PII, c.Fields[1].PIISubject = true, "entryId"
		doc.Commands[id] = c
	}
	ev := doc.Events["time-logged"]
	ev.Fields[1].PII, ev.Fields[1].PIISubject = true, "entryId"
	doc.Events["time-logged"] = ev
	rm := doc.ReadModels["time-entries"]
	rm.Fields[1].PII, rm.Fields[1].PIISubject = true, "entryId"
	doc.ReadModels["time-entries"] = rm
	return doc
}

func TestPIIDocumentImportsAndCarriesTheFlag(t *testing.T) {
	mapped, err := Map(piiDocument(), Options{})
	if err != nil {
		t.Fatalf("a well-formed pii document must import: %v", err)
	}
	var seen int
	for _, d := range mapped.Domains {
		for _, c := range d.Commands {
			for _, f := range c.Fields {
				if f.Name == "staffId" && f.PII && f.PIISubject == "entryId" {
					seen++
				}
			}
			for _, e := range c.Events {
				for _, f := range e.Fields {
					if f.Name == "staffId" && f.PII && f.PIISubject == "entryId" {
						seen++
					}
				}
			}
		}
		for _, rm := range d.ReadModels {
			for _, f := range rm.Fields {
				if f.Name == "staffId" && f.PII {
					seen++
				}
			}
		}
		if len(d.PIIFields()) == 0 {
			t.Error("the domain must report its pii fields")
		}
		if _, err := d.Generate(); err == nil {
			t.Error("the JS target must refuse a pii domain")
		}
	}
	if seen != 3 {
		t.Fatalf("pii and piiSubject must reach command, event and read model; saw %d", seen)
	}
}

func TestPIIRulesRejected(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(d *Document)
		want   string
	}{
		{"pii without piiSubject", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields[1].PIISubject = ""
			d.Events["time-logged"] = ev
		}, "declares no piiSubject"},
		{"piiSubject without pii", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields[2].PIISubject = "entryId"
			d.Events["time-logged"] = ev
		}, "is not pii"},
		{"piiSubject not a sibling", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields[1].PIISubject = "customerId"
			d.Events["time-logged"] = ev
		}, "not a sibling"},
		{"piiSubject is itself pii", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields[0].PII, ev.Fields[0].PIISubject = true, "staffId"
			d.Events["time-logged"] = ev
		}, "itself pii"},
		{"read model pii, event plaintext", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields[1].PII, ev.Fields[1].PIISubject = false, ""
			d.Events["time-logged"] = ev
		}, "carries it as plaintext"},
		{"event pii, read model plaintext", func(d *Document) {
			rm := d.ReadModels["time-entries"]
			rm.Fields[1].PII, rm.Fields[1].PIISubject = false, ""
			d.ReadModels["time-entries"] = rm
		}, "carries it as pii"},
		{"dateRange over pii", func(d *Document) {
			for _, id := range []string{"time-logged"} {
				ev := d.Events[id]
				ev.Fields[2].PII, ev.Fields[2].PIISubject = true, "entryId"
				d.Events[id] = ev
			}
			rm := d.ReadModels["time-entries"]
			rm.Fields[2].PII, rm.Fields[2].PIISubject = true, "entryId"
			d.ReadModels["time-entries"] = rm
		}, "cannot be compared"},
		{"pii key", func(d *Document) {
			rm := d.ReadModels["time-entries"]
			rm.Fields[0].PII, rm.Fields[0].PIISubject = true, "staffId"
			d.ReadModels["time-entries"] = rm
		}, "key field"},
		{"pii subfield", func(d *Document) {
			ev := d.Events["time-logged"]
			ev.Fields = append(ev.Fields, Field{Name: "address", Type: "custom",
				Subfields: []Field{{Name: "street", Type: "string", PII: true, PIISubject: "entryId"}}})
			d.Events["time-logged"] = ev
		}, "subfield"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := piiDocument()
			c.break_(doc)
			_, err := Map(doc, Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal mentioning %q, got %v", c.want, err)
			}
		})
	}
}

func TestPIISubjectThatLooksLikePersonalDataWarns(t *testing.T) {
	doc := piiDocument()
	ev := doc.Events["time-logged"]
	ev.Fields = append(ev.Fields, Field{Name: "contactEmail", Type: "string"}, Field{Name: "note", Type: "string", PII: true, PIISubject: "contactEmail"})
	doc.Events["time-logged"] = ev
	rep := Lint(doc)
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "contactEmail") && strings.Contains(w, "opaque id") {
			return
		}
	}
	t.Fatalf("an email-ish subject id must be warned about: %v", rep.Warnings)
}
