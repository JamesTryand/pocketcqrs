package authverify

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// RegisterEndpoint binds POST /api/cqrs/auth/verify. Body: {"token": "..."}.
// Answers 200 with a Verdict for a valid token, 401 {"valid": false} for a
// definitive rejection.
//
// The endpoint carries no auth gate of its own, deliberately: it is a
// validity oracle. The caller must already hold the token, and the verdict
// contains exactly what PocketBase's own auth-refresh would hand that same
// holder — no signing material, no other user's data. It runs
// FindAuthRecordByToken, the byte-for-byte same check the master's own
// loadAuthToken middleware applies to direct requests, so a node populated
// from a verdict authorizes exactly what the master itself would have.
// It does NOT fire OnRecordAuthRequest hooks, re-evaluate the collection's
// AuthRule, or run MFA — those govern authentication events (logins), and
// verification of an already-issued token is not one; auth-refresh exists
// for callers that want the full flow.
//
// Registered on every node. On a node with a Verifier (a secondary), the
// verdict is fetched fresh from the master instead of computed locally —
// a secondary's own data.db could only ever say "no" — so the endpoint
// answers authoritatively wherever it is asked.
func RegisterEndpoint(e *core.ServeEvent, v *Verifier) {
	e.Router.POST(verifyPath, func(re *core.RequestEvent) error {
		payload, err := io.ReadAll(io.LimitReader(re.Request.Body, maxVerdictBytes))
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return apis.NewBadRequestError("invalid JSON body: "+err.Error(), err)
		}
		if body.Token == "" {
			return apis.NewBadRequestError("missing token", nil)
		}

		if v != nil {
			verdict, err := v.VerifyFresh(re.Request.Context(), body.Token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				return re.JSON(http.StatusUnauthorized, map[string]bool{"valid": false})
			case err != nil:
				return re.JSON(http.StatusServiceUnavailable,
					map[string]string{"error": "cannot reach the master to verify"})
			}
			return re.JSON(http.StatusOK, verdict)
		}

		record, err := re.App.FindAuthRecordByToken(body.Token, core.TokenTypeAuth)
		if err != nil {
			return re.JSON(http.StatusUnauthorized, map[string]bool{"valid": false})
		}

		// serialize as the master would for the token's own holder: a
		// superuser sees every field, and the record's own email is always
		// included — mirroring recordAuthResponse (apis/record_helpers.go)
		if record.IsSuperuser() {
			record.Unhide(record.Collection().Fields.FieldNames()...)
		}
		record.IgnoreEmailVisibility(true)
		raw, err := json.Marshal(record)
		if err != nil {
			return apis.NewApiError(http.StatusInternalServerError, "failed serializing record", err)
		}

		return re.JSON(http.StatusOK, &Verdict{
			CollectionName: record.Collection().Name,
			CollectionID:   record.Collection().Id,
			Record:         raw,
		})
	})
}
