//go:build smoke

package smoke

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecondaryCatalogReportsItsOwnCheckpoints: a secondary checkpoints its
// consumers in its own local checkpoints.db, so its catalog must report
// THOSE -- not the master's checkpoints read out of the replicated
// events.db. A projection that exists only on the secondary makes the
// difference visible: the master never checkpointed it at all.
func TestSecondaryCatalogReportsItsOwnCheckpoints(t *testing.T) {
	master := startBackend(t, nil) // --tutorial
	master.command("task", "t1", "CreateTask", map[string]string{"title": "only here"})

	const secondaryOnly = `//@trigger projection sec_titles on TaskCreated
//@schema sec_titles title:text
//@key title
function project(event) { return [{ upsert: { key: event.data.title, fields: { title: event.data.title } } }]; }
`
	secondary := startSecondaryWith(t, master, filepath.Join(master.DataDir, "events.db"),
		map[string]string{"sec_titles.js": secondaryOnly})

	eventually(t, "the secondary-only projection to fold t1", func() bool {
		var records struct {
			Items []struct {
				Title string `json:"title"`
			} `json:"items"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/collections/sec_titles/records", nil, &records)
		return status == http.StatusOK && len(records.Items) == 1
	})

	var cat struct {
		Totals struct {
			MaxPosition int64 `json:"maxPosition"`
		} `json:"totals"`
		Consumers []struct {
			Name       string `json:"name"`
			Checkpoint int64  `json:"checkpoint"`
		} `json:"consumers"`
	}
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &cat)
	var found bool
	for _, c := range cat.Consumers {
		if !strings.Contains(c.Name, "sec_titles") {
			continue
		}
		found = true
		if c.Checkpoint != cat.Totals.MaxPosition || c.Checkpoint == 0 {
			t.Fatalf("secondary catalog reports checkpoint %d for %s, want its own local checkpoint %d",
				c.Checkpoint, c.Name, cat.Totals.MaxPosition)
		}
	}
	if !found {
		t.Fatalf("sec_titles missing from the secondary's catalog: %+v", cat.Consumers)
	}
}
