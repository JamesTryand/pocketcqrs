package emschema

import (
	"strings"
	"testing"
)

// piiDocument is dateRangeDocument with agentId as personal data on the
// command, the event and the read model, subject ticketId on each — a
// document that imports clean.
func piiDocument() *Document {
	doc := dateRangeDocument()
	for _, id := range []string{"open-ticket"} {
		c := doc.Commands[id]
		c.Fields[1].PII, c.Fields[1].PIISubject = true, "ticketId"
		doc.Commands[id] = c
	}
	ev := doc.Events["ticket-opened"]
	ev.Fields[1].PII, ev.Fields[1].PIISubject = true, "ticketId"
	doc.Events["ticket-opened"] = ev
	rm := doc.ReadModels["tickets"]
	rm.Fields[1].PII, rm.Fields[1].PIISubject = true, "ticketId"
	doc.ReadModels["tickets"] = rm
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
				if f.Name == "agentId" && f.PII && f.PIISubject == "ticketId" {
					seen++
				}
			}
			for _, e := range c.Events {
				for _, f := range e.Fields {
					if f.Name == "agentId" && f.PII && f.PIISubject == "ticketId" {
						seen++
					}
				}
			}
		}
		for _, rm := range d.ReadModels {
			for _, f := range rm.Fields {
				if f.Name == "agentId" && f.PII {
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
			ev := d.Events["ticket-opened"]
			ev.Fields[1].PIISubject = ""
			d.Events["ticket-opened"] = ev
		}, "declares no piiSubject"},
		{"piiSubject without pii", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields[2].PIISubject = "ticketId"
			d.Events["ticket-opened"] = ev
		}, "is not pii"},
		{"piiSubject not a sibling", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields[1].PIISubject = "customerId"
			d.Events["ticket-opened"] = ev
		}, "not a sibling"},
		{"piiSubject is itself pii", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields[0].PII, ev.Fields[0].PIISubject = true, "agentId"
			d.Events["ticket-opened"] = ev
		}, "itself pii"},
		{"piiSubject not text", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields[1].PIISubject = "openedOn"
			d.Events["ticket-opened"] = ev
		}, "not a text field"},
		{"read model pii, event plaintext", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields[1].PII, ev.Fields[1].PIISubject = false, ""
			d.Events["ticket-opened"] = ev
		}, "carries it as plaintext"},
		{"event pii, read model plaintext", func(d *Document) {
			rm := d.ReadModels["tickets"]
			rm.Fields[1].PII, rm.Fields[1].PIISubject = false, ""
			d.ReadModels["tickets"] = rm
		}, "carries it as pii"},
		{"dateRange over pii", func(d *Document) {
			for _, id := range []string{"ticket-opened"} {
				ev := d.Events[id]
				ev.Fields[2].PII, ev.Fields[2].PIISubject = true, "ticketId"
				d.Events[id] = ev
			}
			rm := d.ReadModels["tickets"]
			rm.Fields[2].PII, rm.Fields[2].PIISubject = true, "ticketId"
			d.ReadModels["tickets"] = rm
		}, "cannot be compared"},
		{"pii key", func(d *Document) {
			rm := d.ReadModels["tickets"]
			rm.Fields[0].PII, rm.Fields[0].PIISubject = true, "agentId"
			d.ReadModels["tickets"] = rm
		}, "key field"},
		{"pii subfield", func(d *Document) {
			ev := d.Events["ticket-opened"]
			ev.Fields = append(ev.Fields, Field{Name: "address", Type: "custom",
				Subfields: []Field{{Name: "street", Type: "string", PII: true, PIISubject: "ticketId"}}})
			d.Events["ticket-opened"] = ev
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
	ev := doc.Events["ticket-opened"]
	ev.Fields = append(ev.Fields, Field{Name: "contactEmail", Type: "string"}, Field{Name: "note", Type: "string", PII: true, PIISubject: "contactEmail"})
	doc.Events["ticket-opened"] = ev
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
