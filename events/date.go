package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Date is a //@schema date field in generated Go. It reads a calendar date
// ("2026-09-15"), an RFC 3339 date-time, or PocketBase's own layout
// ("2026-09-15 10:30:00.000Z"), and writes a calendar date back as one and a
// date-time as RFC 3339, so an event's date stays the date it was rather
// than becoming midnight somewhere. An absent or null value is the zero
// Date, written as null. The JS target keeps dates as the strings they came
// as; this is that, with the instant to hand (Time).
type Date struct {
	time.Time
	// day: read as a calendar date, written as one.
	day bool
}

// NewDate is a calendar date.
func NewDate(year int, month time.Month, day int) Date {
	return Date{Time: time.Date(year, month, day, 0, 0, 0, 0, time.UTC), day: true}
}

// IsDay reports whether d is a calendar date rather than an instant.
func (d Date) IsDay() bool { return d.day }

const dayLayout = "2006-01-02"

// UnmarshalJSON reads null, "", a date or a date-time.
func (d *Date) UnmarshalJSON(b []byte) error {
	var raw *string
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("a date is a string: %w", err)
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		*d = Date{}
		return nil
	}
	s := strings.TrimSpace(*raw)
	if t, err := time.Parse(dayLayout, s); err == nil {
		*d = Date{Time: t, day: true}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.000Z07:00", "2006-01-02 15:04:05.000Z", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			*d = Date{Time: t.UTC()}
			return nil
		}
	}
	return fmt.Errorf("%q is not a date (2006-01-02) or a date-time (RFC 3339)", s)
}

// MarshalJSON writes null, a date or an RFC 3339 date-time.
func (d Date) MarshalJSON() ([]byte, error) {
	switch {
	case d.Time.IsZero():
		return []byte("null"), nil
	case d.day:
		return json.Marshal(d.Time.Format(dayLayout))
	}
	return json.Marshal(d.Time.UTC().Format(time.RFC3339Nano))
}
