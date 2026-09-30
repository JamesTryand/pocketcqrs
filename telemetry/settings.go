package telemetry

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// URLName and IntervalName are the contract's names (section 9); the
	// flags are --cqrsTelemetryURL and --cqrsTelemetryInterval.
	URLName      = "CQRS_TELEMETRY_URL"
	IntervalName = "CQRS_TELEMETRY_INTERVAL"

	// DefaultInterval is the seconds between snapshots when none is set.
	DefaultInterval = 15 * time.Second
)

// Settings say whether and how the node pushes telemetry: URL, where the
// scheme selects the transport and nil means off, and Interval between
// snapshots.
type Settings struct {
	URL      *url.URL
	Interval time.Duration
}

// Enabled reports whether a URL is set.
func (s Settings) Enabled() bool { return s.URL != nil }

// InvalidSettingError is an invalid telemetry setting: the boot fails, like
// any other invalid setting. An unreachable bus is not one.
type InvalidSettingError struct{ Name, Problem string }

func (e *InvalidSettingError) Error() string { return e.Name + " " + e.Problem }

// ParseSettings reads the URL and the interval. The interval is seconds,
// decimals allowed ("15", "0.5"), as the contract says; a Go duration
// ("15s") is accepted too, since a flag is one. Empty means the default; the
// URL may carry credentials, so an error never quotes it.
func ParseSettings(rawURL, rawInterval string) (Settings, error) {
	s := Settings{Interval: DefaultInterval}
	if u := strings.TrimSpace(rawURL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Settings{}, &InvalidSettingError{Name: URLName,
				Problem: "is not a URL: use the bus's address, such as nats://host:4222."}
		}
		s.URL = parsed
	}
	if v := strings.TrimSpace(rawInterval); v != "" {
		d, ok := parseInterval(v)
		if !ok || d <= 0 {
			return Settings{}, &InvalidSettingError{Name: IntervalName,
				Problem: fmt.Sprintf("%q is not a positive number of seconds: use a number such as 15 or 0.5.", v)}
		}
		s.Interval = d
	}
	return s, nil
}

func parseInterval(v string) (time.Duration, bool) {
	if seconds, err := strconv.ParseFloat(v, 64); err == nil {
		if seconds > float64(time.Duration(1<<62)/time.Second) {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	d, err := time.ParseDuration(v)
	return d, err == nil
}
