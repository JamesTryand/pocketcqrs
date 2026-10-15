package queryroute_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/queryroute"
)

// sse reads one live stream's events.
type sse struct {
	t      *testing.T
	events chan [2]string // event name, data
	cancel context.CancelFunc
	status int
}

func openLive(t *testing.T, base, token, path string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &sse{t: t, events: make(chan [2]string, 16), cancel: cancel, status: resp.StatusCode}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var name, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if name != "" {
					s.events <- [2]string{name, data}
				}
				name, data = "", ""
			}
		}
	}()
	return s
}

// next waits for the stream's next event. The deadline only bounds a test
// that would otherwise hang; nothing here polls.
func (s *sse) next() (string, string) {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			return "", ""
		}
		return ev[0], ev[1]
	case <-time.After(10 * time.Second):
		s.t.Fatal("no event within 10s")
		return "", ""
	}
}

func viewKeys(t *testing.T, data string, key string) string {
	t.Helper()
	var p struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var keys []string
	for _, it := range p.Items {
		keys = append(keys, it[key].(string))
	}
	return strings.Join(keys, ",")
}

// The live route sends the read route's own result for the viewer (the same
// access rules), then a new one when the collection changes, after the
// write commits, and only when the viewer's result changed.
func TestLiveRouteSendsTheViewersResultAndChanges(t *testing.T) {
	srv, app, tokens := accessFixture(t, timeEntries)
	live := openLive(t, srv.URL, tokens["s2"], queryroute.PathPrefix+"timeEntries/live")
	if live.status != http.StatusOK {
		t.Fatalf("live route: %d", live.status)
	}
	if name, data := live.next(); name != "view" || viewKeys(t, data, "entryId") != "e2" {
		t.Fatalf("first push is the viewer's own result: %s %s", name, data)
	}

	col, _ := app.FindCollectionByNameOrId("timeEntries")
	save := func(entry, staff string) {
		rec := core.NewRecord(col)
		rec.Set("entryId", entry)
		rec.Set("staffId", staff)
		rec.Set("projectId", "p2")
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	save("e5", "s3") // someone else's: s2's result is unchanged, nothing is pushed
	save("e6", "s2") // s2's own
	if name, data := live.next(); name != "view" || viewKeys(t, data, "entryId") != "e2,e6" {
		t.Fatalf("the next push is the changed result, not a repeat: %s %s", name, data)
	}
}

// A caller the read route refuses is refused here too, before any stream.
func TestLiveRouteRefusesWhoTheReadRouteRefuses(t *testing.T) {
	srv, _, tokens := accessFixture(t, timeEntries)
	if live := openLive(t, srv.URL, tokens["nobody"], queryroute.PathPrefix+"timeEntries/live"); live.status != http.StatusForbidden {
		t.Fatalf("want 403, got %d", live.status)
	}
	if live := openLive(t, srv.URL, "", queryroute.PathPrefix+"timeEntries/live"); live.status == http.StatusOK {
		t.Fatalf("an anonymous caller must not get a stream")
	}
}

// Erasure changes no row (the ciphertext stays), so the record hooks don't
// fire; the reveal cache marking the subject erased pushes the redacted
// result instead.
func TestLiveRoutePushesTheRedactedResultOnErasure(t *testing.T) {
	f := setup(t, nil, nil)
	live := openLive(t, f.srv.URL, f.tokens["alice"], queryroute.PathPrefix+"customers/live")
	if live.status != http.StatusOK {
		t.Fatalf("live route: %d", live.status)
	}
	if _, data := live.next(); !strings.Contains(data, "alice@example.com") {
		t.Fatalf("first push reveals alice's own email: %s", data)
	}
	_ = f.kms.DestroyKey(context.Background(), "c1")
	f.cache.MarkErased("c1")
	if _, data := live.next(); strings.Contains(data, "alice@example.com") || !strings.Contains(data, `"$redacted":true`) {
		t.Fatalf("after erasure the push is redacted: %s", data)
	}
}

// The stream ends when the viewer's token expires, with an `expired` event.
func TestLiveRouteEndsWhenTheTokenExpires(t *testing.T) {
	srv, app, _ := accessFixture(t, timeEntries)
	people, _ := app.FindCollectionByNameOrId("people")
	rec := core.NewRecord(people)
	rec.SetEmail("brief@people.test")
	rec.SetPassword("1234567890")
	rec.Set("role", "manager")
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	token, err := rec.NewStaticAuthToken(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	live := openLive(t, srv.URL, token, queryroute.PathPrefix+"timeEntries/live")
	if name, _ := live.next(); name != "view" {
		t.Fatalf("first event: %s", name)
	}
	if name, _ := live.next(); name != "expired" {
		t.Fatalf("want expired, got %q", name)
	}
	if name, _ := live.next(); name != "" {
		t.Fatalf("the stream must end after expired, got %q", name)
	}
}
