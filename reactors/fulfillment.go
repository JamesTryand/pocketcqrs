package reactors

import (
	"encoding/json"

	"pocketcqrs/aggregates"
	"pocketcqrs/decider"
	"pocketcqrs/events"
)

// Fulfillment returns the reactor that opens a fulfillment task for every
// confirmed order, on the task aggregate "fulfill-<orderId>". The
// deterministic target id makes replays idempotent: the second dispatch is
// rejected by the task decider ("task already exists") and skipped.
func Fulfillment() Reactor { return fulfillment{} }

type fulfillment struct{}

func (fulfillment) Name() string { return "fulfillment" }

func (fulfillment) React(ev events.Event) []Reaction {
	if ev.Aggregate != aggregates.OrderAggregate || ev.Type != aggregates.OrderConfirmed {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"title": "fulfil order " + ev.AggregateID})
	return []Reaction{{
		Aggregate: aggregates.TaskAggregate,
		ID:        "fulfill-" + ev.AggregateID,
		Command:   decider.Command{Name: aggregates.CmdCreateTask, Payload: payload},
	}}
}
