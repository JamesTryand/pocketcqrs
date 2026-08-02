package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"

	"github.com/JamesTryand/pocketcqrs/aggregates"
	"github.com/JamesTryand/pocketcqrs/events"
	"github.com/JamesTryand/pocketcqrs/writeguard"
)

// NewOrders projects order events into the "orders" and "order_lines"
// collections. Both collections are owned by this one projection so that
// within a single Apply the line upsert always precedes the parent totals
// recompute (independent consumers could interleave).
func NewOrders(app core.App) Projection { return ordersProjection{app: app} }

type ordersProjection struct {
	app core.App
}

func (ordersProjection) Name() string          { return "orders" }
func (ordersProjection) Collections() []string { return []string{"orders", "order_lines"} }

// EventTypes declares the consumed event types (introspection; the engine
// still delivers everything — Apply self-filters).
func (ordersProjection) EventTypes() []string {
	return []string{aggregates.OrderPlaced, aggregates.OrderLineAdded, aggregates.OrderConfirmed, aggregates.OrderCancelled}
}

func (p ordersProjection) Apply(ctx context.Context, ev events.Event) error {
	if ev.Aggregate != aggregates.OrderAggregate {
		return nil
	}
	ctx = writeguard.MarkInternal(ctx)

	switch ev.Type {
	case aggregates.OrderPlaced:
		var data struct {
			CustomerRef string `json:"customerRef"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return err
		}
		order, err := p.findOrder(ev.AggregateID)
		if err != nil {
			return err
		}
		if order != nil {
			return nil // already applied
		}
		col, err := p.app.FindCollectionByNameOrId("orders")
		if err != nil {
			return err
		}
		order = core.NewRecord(col)
		order.Set("orderId", ev.AggregateID)
		order.Set("customerRef", data.CustomerRef)
		order.Set("status", aggregates.OrderStatusOpen)
		order.Set("total", 0)
		order.Set("lineCount", 0)
		return p.app.SaveWithContext(ctx, order)

	case aggregates.OrderLineAdded:
		var line aggregates.OrderLine
		if err := json.Unmarshal(ev.Data, &line); err != nil {
			return err
		}
		order, err := p.findOrder(ev.AggregateID)
		if err != nil {
			return err
		}
		if order == nil {
			return fmt.Errorf("order_lines projection: order %q not found", ev.AggregateID)
		}

		// upsert the line (idempotent via lineKey = orderId:sku)
		lineKey := ev.AggregateID + ":" + line.SKU
		lineRec, err := p.findLine(lineKey)
		if err != nil {
			return err
		}
		if lineRec == nil {
			col, err := p.app.FindCollectionByNameOrId("order_lines")
			if err != nil {
				return err
			}
			lineRec = core.NewRecord(col)
			lineRec.Set("lineKey", lineKey)
			lineRec.Set("order", order.Id)
		}
		lineRec.Set("sku", line.SKU)
		lineRec.Set("qty", line.Qty)
		lineRec.Set("price", line.Price)
		if err := p.app.SaveWithContext(ctx, lineRec); err != nil {
			return err
		}

		// recompute (not increment) the parent totals: idempotent under
		// at-least-once delivery
		return p.recomputeTotals(ctx, order)

	case aggregates.OrderConfirmed, aggregates.OrderCancelled:
		order, err := p.findOrder(ev.AggregateID)
		if err != nil {
			return err
		}
		if order == nil {
			return fmt.Errorf("orders projection: order %q not found", ev.AggregateID)
		}
		status := aggregates.OrderStatusConfirmed
		if ev.Type == aggregates.OrderCancelled {
			status = aggregates.OrderStatusCancelled
		}
		if order.GetString("status") == status {
			return nil // already applied
		}
		order.Set("status", status)
		return p.app.SaveWithContext(ctx, order)
	}
	return nil
}

func (p ordersProjection) findOrder(orderID string) (*core.Record, error) {
	rec, err := p.app.FindFirstRecordByData("orders", "orderId", orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

func (p ordersProjection) findLine(lineKey string) (*core.Record, error) {
	rec, err := p.app.FindFirstRecordByData("order_lines", "lineKey", lineKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// recomputeTotals folds the order's current lines into total/lineCount.
func (p ordersProjection) recomputeTotals(ctx context.Context, order *core.Record) error {
	lines, err := p.app.FindRecordsByFilter("order_lines", "order = {:order}", "", -1, 0,
		map[string]any{"order": order.Id})
	if err != nil {
		return err
	}
	var total int64
	for _, l := range lines {
		total += int64(l.GetInt("qty")) * int64(l.GetInt("price"))
	}
	if order.GetInt("total") == int(total) && order.GetInt("lineCount") == len(lines) {
		return nil // already up to date
	}
	order.Set("total", total)
	order.Set("lineCount", len(lines))
	return p.app.SaveWithContext(ctx, order)
}
