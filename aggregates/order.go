package aggregates

import (
	"encoding/json"
	"errors"

	"github.com/JamesTryand/pocketcqrs/decider"
	"github.com/JamesTryand/pocketcqrs/events"
)

// OrderAggregate is the stream/aggregate name for orders.
const OrderAggregate = "order"

// Order command names.
const (
	CmdPlaceOrder   = "PlaceOrder"
	CmdAddOrderLine = "AddOrderLine"
	CmdConfirmOrder = "ConfirmOrder"
	CmdCancelOrder  = "CancelOrder"
)

// Order event types.
const (
	OrderPlaced    = "OrderPlaced"
	OrderLineAdded = "OrderLineAdded"
	OrderConfirmed = "OrderConfirmed"
	OrderCancelled = "OrderCancelled"
)

// Order statuses (as folded from events).
const (
	OrderStatusOpen      = "open"
	OrderStatusConfirmed = "confirmed"
	OrderStatusCancelled = "cancelled"
)

// OrderLine is one line of an order. Price is in the currency's minor unit.
type OrderLine struct {
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
	Price int64  `json:"price"`
}

// OrderState is the folded state of an order stream.
type OrderState struct {
	Exists      bool
	Status      string
	CustomerRef string
	Lines       []OrderLine
}

func (s OrderState) open() bool { return s.Exists && s.Status == OrderStatusOpen }

// Order returns the decider for the order aggregate.
func Order() *decider.Decider[OrderState] {
	return &decider.Decider[OrderState]{
		Commands:     []string{CmdPlaceOrder, CmdAddOrderLine, CmdConfirmOrder, CmdCancelOrder},
		InitialState: func() OrderState { return OrderState{} },

		Decide: func(cmd decider.Command, state OrderState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case CmdPlaceOrder:
				var p struct {
					CustomerRef string `json:"customerRef"`
				}
				if err := json.Unmarshal(cmd.Payload, &p); err != nil {
					return nil, err
				}
				if state.Exists {
					return nil, errors.New("order already exists")
				}
				if p.CustomerRef == "" {
					return nil, errors.New("customerRef is required")
				}
				data, _ := json.Marshal(map[string]string{"customerRef": p.CustomerRef})
				return []events.NewEvent{{Type: OrderPlaced, Data: data}}, nil

			case CmdAddOrderLine:
				var p OrderLine
				if err := json.Unmarshal(cmd.Payload, &p); err != nil {
					return nil, err
				}
				if !state.open() {
					return nil, errors.New("order is not open")
				}
				if p.SKU == "" {
					return nil, errors.New("sku is required")
				}
				if p.Qty <= 0 {
					return nil, errors.New("qty must be positive")
				}
				if p.Price < 0 {
					return nil, errors.New("price must not be negative")
				}
				data, _ := json.Marshal(p)
				return []events.NewEvent{{Type: OrderLineAdded, Data: data}}, nil

			case CmdConfirmOrder:
				if !state.open() {
					return nil, errors.New("order is not open")
				}
				if len(state.Lines) == 0 {
					return nil, errors.New("cannot confirm an empty order")
				}
				return []events.NewEvent{{Type: OrderConfirmed, Data: json.RawMessage(`{}`)}}, nil

			case CmdCancelOrder:
				if !state.open() {
					return nil, errors.New("order is not open")
				}
				return []events.NewEvent{{Type: OrderCancelled, Data: json.RawMessage(`{}`)}}, nil

			default:
				return nil, errors.New("unknown command: " + cmd.Name)
			}
		},

		Evolve: func(state OrderState, ev events.Event) (OrderState, error) {
			switch ev.Type {
			case OrderPlaced:
				var p struct {
					CustomerRef string `json:"customerRef"`
				}
				if err := json.Unmarshal(ev.Data, &p); err != nil {
					return state, err
				}
				state.Exists = true
				state.Status = OrderStatusOpen
				state.CustomerRef = p.CustomerRef
			case OrderLineAdded:
				var line OrderLine
				if err := json.Unmarshal(ev.Data, &line); err != nil {
					return state, err
				}
				state.Lines = append(state.Lines, line)
			case OrderConfirmed:
				state.Status = OrderStatusConfirmed
			case OrderCancelled:
				state.Status = OrderStatusCancelled
			}
			return state, nil
		},
	}
}
