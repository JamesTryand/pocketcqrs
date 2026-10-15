package scaffold

import "testing"

// dayDomain: a booking for a calendar date. The model's dates are
// "2026-09-15", so the generated Go must fold one and produce it back as it
// came: time.Time read only RFC 3339, and the timesheets model's own events
// didn't fold (found by schema verify --lang go).
func dayDomain() Domain {
	booked := Event{Name: "DayBooked", Fields: []Field{{Name: "bookingId", Type: "text"}, {Name: "day", Type: "date"}}}
	moved := Event{Name: "DayMoved", Fields: []Field{{Name: "bookingId", Type: "text"}, {Name: "day", Type: "date"}}}
	return Domain{
		Aggregate: "booking",
		Commands: []Command{
			{Name: "Book", Fields: booked.Fields, Events: []Event{booked}, Once: true},
			{Name: "Move", Fields: moved.Fields, Events: []Event{moved}, RequiresExisting: true},
		},
	}
}

const dayBehaviour = `package booking

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func TestDays(t *testing.T) {
	d := Booking()
	s, err := d.Evolve(d.InitialState(), events.Event{Type: DayBooked, Data: json.RawMessage(` + "`" + `{"bookingId":"b1","day":"2026-09-15"}` + "`" + `)})
	if err != nil {
		t.Fatalf("a calendar date didn't fold: %v", err)
	}
	for in, want := range map[string]string{
		` + "`" + `{"bookingId":"b1","day":"2026-09-16"}` + "`" + `:                  ` + "`" + `{"bookingId":"b1","day":"2026-09-16"}` + "`" + `,
		` + "`" + `{"bookingId":"b1","day":"2026-09-16T10:30:00Z"}` + "`" + `:        ` + "`" + `{"bookingId":"b1","day":"2026-09-16T10:30:00Z"}` + "`" + `,
		` + "`" + `{"bookingId":"b1","day":"2026-09-16 10:30:00.000Z"}` + "`" + `:    ` + "`" + `{"bookingId":"b1","day":"2026-09-16T10:30:00Z"}` + "`" + `,
		` + "`" + `{"bookingId":"b1"}` + "`" + `:                                    ` + "`" + `{"bookingId":"b1","day":null}` + "`" + `,
	} {
		evs, err := d.Decide(decider.Command{Name: "Move", Payload: json.RawMessage(in)}, s)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := string(evs[0].Data); got != want {
			t.Errorf("%s: produced %s, want %s", in, got, want)
		}
	}
}
`

func TestGoDatesAreCalendarDates(t *testing.T) {
	runGeneratedGo(t, dayDomain(), dayBehaviour)
}
