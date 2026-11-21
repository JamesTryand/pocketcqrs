package decider

import (
	"errors"
	"fmt"
	"testing"
)

func TestRefusalReason(t *testing.T) {
	cases := []struct {
		err       error
		aggregate string
		want      string
	}{
		{errors.New("customer contact does not exist"), "customerContact", ReasonDoesNotExist},
		{errors.New("customer contact already exists"), "customerContact", ReasonAlreadyExists},
		{&RejectionError{Err: errors.New("entitlement does not exist")}, "entitlement", ReasonDoesNotExist},
		// only the whole generated message, for this aggregate
		{errors.New("the customer contact does not exist yet"), "customerContact", ""},
		{errors.New("entitlement does not exist"), "customerContact", ""},
		{errors.New("Entitlement does not exist."), "entitlement", ""},
		{errors.New("amount must be positive"), "entitlement", ""},
		{&RejectionError{Err: Refuse("over-limit", "the limit is 3")}, "entitlement", "over-limit"},
		{fmt.Errorf("wrapped: %w", Refuse("over-limit", "x")), "entitlement", "over-limit"},
		{nil, "entitlement", ""},
	}
	for _, c := range cases {
		if got := RefusalReason(c.err, c.aggregate); got != c.want {
			t.Errorf("RefusalReason(%v, %q) = %q, want %q", c.err, c.aggregate, got, c.want)
		}
	}
	if AggregateWords("orderPDF") != "order PDF" || AggregateWords("loanRequest") != "loan request" {
		t.Fatal("AggregateWords changed")
	}
}
