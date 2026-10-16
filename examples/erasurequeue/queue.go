// Package erasurequeue is an example of a read model a host might build for
// its own administrator screen: the erasure requests open on each subject,
// where they stand. It is an ordinary projection over the dataSubject stream
// (package pii's core events and package retention's), not part of either;
// copy it and shape the rows to what your screen shows.
package erasurequeue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/retention"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// Collection holds one row per subject that has ever had a request, a legal
// hold or an erasure. No personal data: reasons are codes and the subject id
// is opaque. Superusers only; add it to the write-guarded collections.
const Collection = "erasureRequests"

// Row is one row of the queue.
type Row struct {
	SubjectID   string
	Stage       retention.Stage
	Reason      string
	RequestedBy string
	RequestedAt time.Time
	ReviewAt    time.Time
	LegalHold   bool
}

// Ensure creates the collection if it doesn't exist: no list or view rule,
// so superusers only.
func Ensure(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(Collection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(Collection)
	col.Fields.Add(&core.TextField{Name: "subjectId", Required: true}, &core.TextField{Name: "stage"},
		&core.TextField{Name: "reason"}, &core.TextField{Name: "requestedBy"},
		&core.TextField{Name: "requestedAt"}, &core.TextField{Name: "reviewAt"}, &core.BoolField{Name: "legalHold"})
	col.AddIndex("idx_erasureRequests_subjectId", true, "subjectId", "")
	return app.Save(col)
}

// Projection projects the subjects' streams into the queue, each event and
// its position in one transaction.
type Projection struct{ App core.App }

func (Projection) Name() string          { return "example:erasure-queue" }
func (Projection) Collections() []string { return []string{Collection} }

func (p Projection) Checkpoints() consumers.CheckpointStore {
	return projections.Positions{App: p.App}
}

func (p Projection) Commit(ctx context.Context, ev events.Event) error {
	return projections.Commit(ctx, p.App, p.Name(), ev.Position, func(tx core.App) error {
		return Projection{App: tx}.Apply(ctx, ev)
	})
}

// timeText is the one timestamp form the queue stores, so text order is
// time order.
func timeText(raw string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.000Z"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func (p Projection) Apply(ctx context.Context, ev events.Event) error {
	if ev.Aggregate != pii.SubjectAggregate {
		return nil
	}
	switch ev.Type {
	case pii.ErasureRequestedEvent, pii.SubjectErasedEvent, retention.ErasureHeldEvent, retention.ErasureApprovedEvent,
		retention.LegalHoldPlacedEvent, retention.LegalHoldReleasedEvent:
	default:
		return nil
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	str := func(name string) string { s, _ := data[name].(string); return s }

	rec, err := p.App.FindFirstRecordByData(Collection, "subjectId", ev.AggregateID)
	if errors.Is(err, sql.ErrNoRows) {
		col, cerr := p.App.FindCollectionByNameOrId(Collection)
		if cerr != nil {
			return cerr
		}
		rec = core.NewRecord(col)
		rec.Set("subjectId", ev.AggregateID)
		rec.Set("stage", string(retention.StageNone)) // a legal hold can come before any request
	} else if err != nil {
		return err
	}
	switch ev.Type {
	case pii.ErasureRequestedEvent:
		rec.Set("stage", string(retention.StageRequested))
		rec.Set("reason", str("reason"))
		rec.Set("requestedBy", str("requestedBy"))
		rec.Set("requestedAt", timeText(str("requestedAt")))
		rec.Set("reviewAt", "")
	case retention.ErasureHeldEvent:
		rec.Set("stage", string(retention.StageHeld))
		rec.Set("reason", str("reason"))
		rec.Set("reviewAt", timeText(str("reviewAt")))
	case retention.ErasureApprovedEvent:
		rec.Set("stage", string(retention.StageApproved))
		rec.Set("reviewAt", "")
	case pii.SubjectErasedEvent:
		rec.Set("stage", string(retention.StageErased))
		rec.Set("reviewAt", "")
	case retention.LegalHoldPlacedEvent:
		rec.Set("legalHold", true)
	case retention.LegalHoldReleasedEvent:
		rec.Set("legalHold", false)
	}
	return p.App.SaveWithContext(writeguard.MarkInternal(ctx), rec)
}

// List is the queue, narrowed to one stage unless stage is empty, oldest
// request first.
func List(ctx context.Context, app core.App, stage retention.Stage) ([]Row, error) {
	filter, params := "", dbx.Params{}
	if stage != "" {
		filter, params = "stage = {:stage}", dbx.Params{"stage": string(stage)}
	}
	recs, err := app.FindRecordsByFilter(Collection, filter, "requestedAt,subjectId", 0, 0, params)
	if err != nil {
		return nil, err
	}
	parse := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	out := make([]Row, 0, len(recs))
	for _, r := range recs {
		out = append(out, Row{
			SubjectID: r.GetString("subjectId"), Stage: retention.Stage(r.GetString("stage")),
			Reason: r.GetString("reason"), RequestedBy: r.GetString("requestedBy"),
			RequestedAt: parse(r.GetString("requestedAt")), ReviewAt: parse(r.GetString("reviewAt")),
			LegalHold: r.GetBool("legalHold"),
		})
	}
	return out, nil
}
