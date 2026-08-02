package aggregates

import (
	"testing"
)

func TestOrderLifecycle(t *testing.T) {
	r := setup(t)
	ctx := t.Context()

	if _, err := r.Handle(ctx, OrderAggregate, "o1", cmd(CmdPlaceOrder, map[string]string{"customerRef": "cust-1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handle(ctx, OrderAggregate, "o1", cmd(CmdAddOrderLine, OrderLine{SKU: "widget", Qty: 2, Price: 500})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handle(ctx, OrderAggregate, "o1", cmd(CmdAddOrderLine, OrderLine{SKU: "gadget", Qty: 1, Price: 1200})); err != nil {
		t.Fatal(err)
	}
	evts, err := r.Handle(ctx, OrderAggregate, "o1", cmd(CmdConfirmOrder, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 1 || evts[0].Type != OrderConfirmed || evts[0].Sequence != 4 {
		t.Fatalf("unexpected events: %+v", evts)
	}
}

func TestOrderInvariants(t *testing.T) {
	cases := []struct {
		name  string
		setup []decider0
		cmd   decider0
	}{
		{"place twice", []decider0{{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
			decider0{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
		{"place missing customerRef", nil,
			decider0{CmdPlaceOrder, map[string]string{}}},
		{"add line to missing order", nil,
			decider0{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 1, Price: 1}}},
		{"add line empty sku", []decider0{{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
			decider0{CmdAddOrderLine, OrderLine{SKU: "", Qty: 1, Price: 1}}},
		{"add line zero qty", []decider0{{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
			decider0{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 0, Price: 1}}},
		{"add line negative price", []decider0{{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
			decider0{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 1, Price: -5}}},
		{"confirm empty order", []decider0{{CmdPlaceOrder, map[string]string{"customerRef": "c"}}},
			decider0{CmdConfirmOrder, nil}},
		{"cancel missing order", nil,
			decider0{CmdCancelOrder, nil}},
		{"confirm then add line", []decider0{
			{CmdPlaceOrder, map[string]string{"customerRef": "c"}},
			{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 1, Price: 1}},
			{CmdConfirmOrder, nil}},
			decider0{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 1, Price: 1}}},
		{"confirm then cancel", []decider0{
			{CmdPlaceOrder, map[string]string{"customerRef": "c"}},
			{CmdAddOrderLine, OrderLine{SKU: "w", Qty: 1, Price: 1}},
			{CmdConfirmOrder, nil}},
			decider0{CmdCancelOrder, nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := setup(t)
			ctx := t.Context()
			for i, s := range tc.setup {
				if _, err := r.Handle(ctx, OrderAggregate, "o1", cmd(s.name, s.payload)); err != nil {
					t.Fatalf("setup command %d failed: %v", i, err)
				}
			}
			if _, err := r.Handle(ctx, OrderAggregate, "o1", cmd(tc.cmd.name, tc.cmd.payload)); err == nil {
				t.Fatal("expected domain error")
			}
		})
	}
}

// decider0 is a tiny helper for table-driven command sequences.
type decider0 struct {
	name    string
	payload any
}
