package pii

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbox"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// ErasureRequestsCollection is the erasure request queue: one row per
// subject that has ever had a request or a legal hold. What an
// administrator screen lists ("pending", "held until..."); it holds no
// personal data (reasons are codes, the subject id is opaque). Superusers
// only; add it to the write-guarded collections.
const ErasureRequestsCollection = "erasureRequests"

// ErasureRequestRow is one row of the queue.
type ErasureRequestRow struct {
	SubjectID   string
	Stage       ErasureStage
	Reason      string
	RequestedBy string
	RequestedAt time.Time
	ReviewAt    time.Time
	LegalHold   bool
}

// EnsureErasureRequests creates the queue collection if it doesn't exist:
// no list or view rule, so superusers only.
func EnsureErasureRequests(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(ErasureRequestsCollection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(ErasureRequestsCollection)
	col.Fields.Add(&core.TextField{Name: "subjectId", Required: true}, &core.TextField{Name: "stage"},
		&core.TextField{Name: "reason"}, &core.TextField{Name: "requestedBy"},
		&core.TextField{Name: "requestedAt"}, &core.TextField{Name: "reviewAt"}, &core.BoolField{Name: "legalHold"})
	col.AddIndex("idx_erasureRequests_subjectId", true, "subjectId", "")
	return app.Save(col)
}

// ErasureRequests projects the subjects' streams into the queue, each event
// and its position in one transaction (R2).
type ErasureRequests struct{ App core.App }

func (ErasureRequests) Name() string          { return "pii:erasure-requests" }
func (ErasureRequests) Collections() []string { return []string{ErasureRequestsCollection} }

func (p ErasureRequests) Checkpoints() consumers.CheckpointStore {
	return projections.Positions{App: p.App}
}

func (p ErasureRequests) Commit(ctx context.Context, ev events.Event) error {
	return projections.Commit(ctx, p.App, p.Name(), ev.Position, func(tx core.App) error {
		return ErasureRequests{App: tx}.Apply(ctx, ev)
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

func (p ErasureRequests) Apply(ctx context.Context, ev events.Event) error {
	if ev.Aggregate != SubjectAggregate {
		return nil
	}
	switch ev.Type {
	case ErasureRequestedEvent, ErasureHeldEvent, ErasureApprovedEvent, SubjectErasedEvent, LegalHoldPlacedEvent, LegalHoldReleasedEvent:
	default:
		return nil
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	str := func(name string) string { s, _ := data[name].(string); return s }

	rec, err := p.App.FindFirstRecordByData(ErasureRequestsCollection, "subjectId", ev.AggregateID)
	if errors.Is(err, sql.ErrNoRows) {
		col, cerr := p.App.FindCollectionByNameOrId(ErasureRequestsCollection)
		if cerr != nil {
			return cerr
		}
		rec = core.NewRecord(col)
		rec.Set("subjectId", ev.AggregateID)
		rec.Set("stage", string(StageNone)) // a legal hold can come before any request
	} else if err != nil {
		return err
	}
	switch ev.Type {
	case ErasureRequestedEvent:
		rec.Set("stage", string(StageRequested))
		rec.Set("reason", str("reason"))
		rec.Set("requestedBy", str("requestedBy"))
		rec.Set("requestedAt", timeText(str("requestedAt")))
		rec.Set("reviewAt", "")
	case ErasureHeldEvent:
		rec.Set("stage", string(StageHeld))
		rec.Set("reason", str("reason"))
		rec.Set("reviewAt", timeText(str("reviewAt")))
	case ErasureApprovedEvent:
		rec.Set("stage", string(StageApproved))
		rec.Set("reviewAt", "")
	case SubjectErasedEvent:
		rec.Set("stage", string(StageErased))
		rec.Set("reviewAt", "")
	case LegalHoldPlacedEvent:
		rec.Set("legalHold", true)
	case LegalHoldReleasedEvent:
		rec.Set("legalHold", false)
	}
	return p.App.SaveWithContext(writeguard.MarkInternal(ctx), rec)
}

// ListErasureRequests is the queue, narrowed to one stage unless stage is
// empty, oldest request first.
func ListErasureRequests(ctx context.Context, app core.App, stage ErasureStage) ([]ErasureRequestRow, error) {
	filter, params := "", dbx.Params{}
	if stage != "" {
		filter, params = "stage = {:stage}", dbx.Params{"stage": string(stage)}
	}
	recs, err := app.FindRecordsByFilter(ErasureRequestsCollection, filter, "requestedAt,subjectId", 0, 0, params)
	if err != nil {
		return nil, err
	}
	parse := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	out := make([]ErasureRequestRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, ErasureRequestRow{
			SubjectID: r.GetString("subjectId"), Stage: ErasureStage(r.GetString("stage")),
			Reason: r.GetString("reason"), RequestedBy: r.GetString("requestedBy"),
			RequestedAt: parse(r.GetString("requestedAt")), ReviewAt: parse(r.GetString("reviewAt")),
			LegalHold: r.GetBool("legalHold"),
		})
	}
	return out, nil
}

// RegisterErasureGovernance wires governed erasure on the writer: the queue
// collection and its projection, and the governor. With box it runs the
// governor's review worker (held requests looked at again on their review
// date) until ctx is done, so hand it a context that lives as long as the
// host. The host still wires the key destroyer (Wire does) and guards
// ErasureRequestsCollection, and its Authorize hook must cover every
// dataSubject command (DefaultRequiredRoles).
func RegisterErasureGovernance(ctx context.Context, app core.App, engine *consumers.Engine, registry *decider.Registry,
	loader decider.StreamLoader, policy RetentionPolicy, box *outbox.Store, logger func(string, ...any)) (*ErasureGovernor, error) {
	if err := EnsureErasureRequests(app); err != nil {
		return nil, err
	}
	g := &ErasureGovernor{Registry: registry, Loader: loader, Policy: policy, Outbox: box, Logger: logger}
	engine.Register(ErasureRequests{App: app})
	engine.Register(g)
	if box != nil {
		w := &outbox.Worker{Store: box, Handlers: map[string]outbox.Handler{ErasureReviewConsumer: g.Review}, Logger: logger}
		go w.Run(ctx)
	}
	return g, nil
}
