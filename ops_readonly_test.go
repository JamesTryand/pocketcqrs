package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/jamestryand/pocketcqrs/events"
)

func opsTestEvent(t *testing.T) (*core.RequestEvent, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	re := &core.RequestEvent{}
	re.Response = rec
	re.Request = httptest.NewRequest(http.MethodPost, "/api/cqrs/admin/mode", nil)
	return re, rec
}

func TestOpsStoreErrMapsReadOnlyToClean503(t *testing.T) {
	re, rec := opsTestEvent(t)

	// wrapped, the way a store method surfaces it
	if err := opsStoreErr(re, fmt.Errorf("set mode: %w", events.ErrReadOnly)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "read-only replica") {
		t.Fatalf("expected the clean replica message, got %q", body)
	}
	if strings.Contains(body, "store is read-only") {
		t.Fatalf("raw store error string leaked: %q", body)
	}
}

func TestOpsStoreErrLeavesOtherErrorsAs400(t *testing.T) {
	re, _ := opsTestEvent(t)

	err := opsStoreErr(re, errors.New("invalid mode \"nope\""))
	var apiErr *router.ApiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("expected a 400 ApiError, got %v", err)
	}
}
