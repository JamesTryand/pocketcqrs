package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BackendClient talks to a pocketcqrs instance over its PUBLIC HTTP API
// only. The dashboard is an external dogfood consumer: it deliberately
// does not import any pocketcqrs package, so the JSON shapes below are the
// API contract exactly as an out-of-process consumer would see it.
type BackendClient struct {
	base string
	hc   *http.Client
}

// NewBackendClient targets the pocketcqrs instance at base
// (e.g. http://127.0.0.1:8090).
func NewBackendClient(base string) *BackendClient {
	return &BackendClient{
		base: strings.TrimSuffix(base, "/"),
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// ErrUnauthorized marks a 401/403 from the backend (expired or invalid
// superuser token) — or a failed auth-with-password (PocketBase answers
// failed logins with 400).
var ErrUnauthorized = errors.New("backend: unauthorized")

// HTTPError is any other non-2xx backend response.
type HTTPError struct {
	StatusCode int
	Detail     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("backend: %d: %s", e.StatusCode, e.Detail)
}

func (c *BackendClient) do(ctx context.Context, method, path, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("backend unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPError{StatusCode: resp.StatusCode, Detail: strings.TrimSpace(string(snippet))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding backend response: %w", err)
		}
	}
	return nil
}

// AuthWithPassword signs a superuser in through PocketBase's public auth
// endpoint and returns the auth token. Failed credentials surface as
// ErrUnauthorized (PocketBase answers them with 400, not 401).
func (c *BackendClient) AuthWithPassword(ctx context.Context, identity, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/api/collections/_superusers/auth-with-password", "",
		map[string]string{"identity": identity, "password": password}, &out)
	var he *HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusBadRequest {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("backend returned no token")
	}
	return out.Token, nil
}

// Catalog fetches the platform catalog (GET /api/cqrs/catalog).
func (c *BackendClient) Catalog(ctx context.Context, token string) (*Catalog, error) {
	var cat Catalog
	if err := c.do(ctx, http.MethodGet, "/api/cqrs/catalog", token, nil, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

// ---- the public catalog JSON contract (mirror of the API document) ----

type Catalog struct {
	GeneratedAt string       `json:"generatedAt"`
	Mode        string       `json:"mode"`
	Totals      Totals       `json:"totals"`
	Aggregates  []Aggregate  `json:"aggregates"`
	Consumers   []Consumer   `json:"consumers"`
	Collections []Collection `json:"collections"`
	Functions   Functions    `json:"functions"`
	Flows       []Flow       `json:"flows"`
	Mermaid     string       `json:"mermaid"`
}

type Totals struct {
	Events             int64 `json:"events"`
	Streams            int64 `json:"streams"`
	DeadLettersPending int64 `json:"deadLettersPending"`
}

type Aggregate struct {
	Name       string      `json:"name"`
	Origin     string      `json:"origin"`
	Handles    []string    `json:"handles,omitempty"`
	Transforms []string    `json:"transforms,omitempty"`
	Streams    int64       `json:"streams"`
	Events     []EventType `json:"events"`
}

type EventType struct {
	Type       string `json:"type"`
	Count      int64  `json:"count"`
	MinVersion int64  `json:"minVersion"`
	MaxVersion int64  `json:"maxVersion"`
}

type Consumer struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	EventTypes  []string `json:"eventTypes,omitempty"`
	Collections []string `json:"collections,omitempty"`
	Checkpoint  int64    `json:"checkpoint"`
}

type Collection struct {
	Name    string   `json:"name"`
	Guarded bool     `json:"guarded"`
	Owner   string   `json:"owner"`
	Key     string   `json:"key,omitempty"`
	Fields  []string `json:"fields"`
}

type Functions struct {
	HTTP []string  `json:"http"`
	Cron []CronJob `json:"cron"`
}

type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
}

type Flow struct {
	Reactor string `json:"reactor"`
	Cause   string `json:"cause"`
	Target  string `json:"target"`
	Count   int64  `json:"count"`
}
