// Package litefswatch wakes a secondary when LiteFS has applied a
// transaction to its replica of the log (R7).
//
// A secondary's events.db is a LiteFS replica: the master's appends reach it
// in another process, so nothing in this one says "new events". LiteFS's
// HTTP API streams an event per applied transaction (GET /events, one JSON
// object per line: "init" first, then a "tx" naming the database), sent
// after the transaction is applied, so the data is readable when it arrives.
// Watch follows that stream and calls Notify for each "tx" on the watched
// database; the engine's tick stays as the guarantee for anything missed.
//
// A dropped stream is subscribed to again, after a wait that grows while it
// keeps failing (MinWait doubling to MaxWait) and starts again once a
// stream is open. That wait is for reconnecting the subscription, not for
// data: nothing is polled, and the tick covers the gap.
package litefswatch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Config is what Watch follows.
type Config struct {
	// URL is LiteFS's event stream, e.g. http://localhost:20202/events.
	URL string
	// DB is the database whose transactions matter: the log's file name.
	DB string
	// Notify is called for each transaction on DB (Engine.Nudge).
	Notify func()
	// MinWait and MaxWait bound the wait before subscribing again after a
	// drop (1s and 30s when zero).
	MinWait, MaxWait time.Duration
	// Logger receives operational lines; nil is a no-op.
	Logger func(msg string, args ...any)
	// Client is the HTTP client (one with no timeout when nil: the stream is
	// long-lived).
	Client *http.Client
}

type event struct {
	Type string `json:"type"`
	DB   string `json:"db"`
}

// Watch follows the stream until ctx is done.
func Watch(ctx context.Context, c Config) {
	if c.MinWait <= 0 {
		c.MinWait = time.Second
	}
	if c.MaxWait < c.MinWait {
		c.MaxWait = max(30*time.Second, c.MinWait)
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	log := c.Logger
	if log == nil {
		log = func(string, ...any) {}
	}
	wait := c.MinWait
	for ctx.Err() == nil {
		opened, err := follow(ctx, c)
		if ctx.Err() != nil {
			return
		}
		if opened {
			wait = c.MinWait
		}
		log("litefs event stream ended; subscribing again", "url", c.URL, "in", wait, "error", err)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, c.MaxWait)
	}
}

// follow reads one connection's stream; opened reports whether it got one.
func follow(ctx context.Context, c Config) (opened bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("litefs events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type == "tx" && ev.DB == c.DB {
			c.Notify()
		}
	}
	return true, sc.Err()
}
