package linearapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Endpoint is fixed on purpose: there is no config/env override, so a
// misconfiguration can never redirect the Authorization header elsewhere.
const Endpoint = "https://api.linear.app/graphql"

// Doer is the injected HTTP client (*http.Client satisfies it); tests supply a
// fake so nothing ever reaches real Linear.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a minimal GraphQL client. Compared with driving Linear through the
// MCP it closes these limits: (1) many mutations ride one request via aliased
// batching (Batch) instead of one tool call per node; (2) rate limiting is seen
// and honoured (HTTP 429 / RATELIMITED + Retry-After, exponential backoff) rather
// than surfacing as an opaque tool failure; (3) list queries are paginated to
// exhaustion (Paginate) instead of truncating at the tool's page size; (4) the
// full schema is reachable (e.g. initiative<->project links) not just the ops
// the MCP happens to expose.
type Client struct {
	HTTP       Doer
	key        string
	MaxRetries int                 // retries after the first attempt on rate limit (default 5)
	BaseDelay  time.Duration       // first backoff step (default 1s, doubling)
	Sleep      func(time.Duration) // injectable so tests don't wait
}

// New builds a client around key. If httpc is nil a default client is used that
// refuses redirects (a redirect must never carry the key to another host).
func New(key string, httpc Doer) *Client {
	if httpc == nil {
		httpc = &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{HTTP: httpc, key: key, MaxRetries: 5, BaseDelay: time.Second, Sleep: time.Sleep}
}

// String/GoString keep the key out of any %v/%+v/%#v formatting of a Client.
func (c *Client) String() string   { return "linearapi.Client{key:<redacted>}" }
func (c *Client) GoString() string { return c.String() }

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// Do runs one query/mutation and decodes data into out (may be nil). It retries
// on rate limiting; every returned error is scrubbed of the key.
func (c *Client) Do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	delay := c.BaseDelay
	for attempt := 0; ; attempt++ {
		retryAfter, err := c.once(ctx, body, out)
		var rl *rateLimitedError
		if !errors.As(err, &rl) {
			return err
		}
		if attempt >= c.MaxRetries {
			return c.scrub(fmt.Errorf("linear: still rate limited after %d retries", c.MaxRetries))
		}
		wait := delay
		if retryAfter > wait {
			wait = retryAfter
		}
		c.Sleep(wait)
		delay *= 2
	}
}

type rateLimitedError struct{}

func (*rateLimitedError) Error() string { return "rate limited" }

func (c *Client) once(ctx context.Context, body []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, c.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.key) // Linear personal API keys are sent raw, no "Bearer"
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, c.scrub(fmt.Errorf("linear: request failed: %w", err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, c.scrub(fmt.Errorf("linear: reading response: %w", err))
	}
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	_ = json.Unmarshal(raw, &env)
	for _, e := range env.Errors {
		if e.Extensions.Code == "RATELIMITED" {
			return retryAfter, &rateLimitedError{}
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return retryAfter, &rateLimitedError{}
	}
	if len(env.Errors) > 0 {
		// Linear returns PARTIAL data alongside errors (e.g. one aliased mutation in
		// a batch failed). Decode what came back so callers can record the ones that
		// succeeded; the error is still returned.
		if out != nil && len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, out)
		}
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return 0, c.scrub(fmt.Errorf("linear: %s", strings.Join(msgs, "; ")))
	}
	if resp.StatusCode/100 != 2 {
		return 0, c.scrub(fmt.Errorf("linear: HTTP %d", resp.StatusCode))
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return 0, c.scrub(fmt.Errorf("linear: decoding data: %w", err))
		}
	}
	return 0, nil
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// scrub guarantees the key can't leave via an error string (e.g. a server that
// echoes headers, or a transport error quoting the request).
func (c *Client) scrub(err error) error {
	if err == nil || c.key == "" || !strings.Contains(err.Error(), c.key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.key, "<redacted>"))
}

// MaxBatch caps aliased operations per request (well under Linear's query
// complexity limit for small mutations).
const MaxBatch = 20

// BatchOp is one aliased mutation; "#" in Call/Decl/Vars keys is replaced by the op's index.
type BatchOp struct {
	Call string         // the mutation call, with variable names ending in the placeholder "#" (replaced per index)
	Decl string         // variable declarations, with the same "#" placeholder, e.g. "$id#: String!, $in#: IssueUpdateInput!"
	Vars map[string]any // variable values keyed with the "#" placeholder too
	Sel  string         // selection set, e.g. "success issue { id }"
}

// Batch sends ops as ONE request, each aliased op0..opN, and returns the raw
// per-alias results (op0 -> its JSON). On a partial failure it returns the
// results that DID come back (failed aliases are null/empty) together with the
// error — callers must process the results before surfacing the error. Closes the MCP's one-call-per-node limit.
func (c *Client) Batch(ctx context.Context, ops []BatchOp) ([]json.RawMessage, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	var decls, body []string
	vars := map[string]any{}
	for i, op := range ops {
		idx := strconv.Itoa(i)
		decls = append(decls, strings.ReplaceAll(op.Decl, "#", idx))
		body = append(body, fmt.Sprintf("op%s: %s { %s }", idx, strings.ReplaceAll(op.Call, "#", idx), op.Sel))
		for k, v := range op.Vars {
			vars[strings.ReplaceAll(k, "#", idx)] = v
		}
	}
	q := fmt.Sprintf("mutation Batch(%s) { %s }", strings.Join(decls, ", "), strings.Join(body, " "))
	var data map[string]json.RawMessage
	err := c.Do(ctx, q, vars, &data)
	res := make([]json.RawMessage, len(ops))
	for i := range ops {
		res[i] = data["op"+strconv.Itoa(i)]
	}
	return res, err
}

// Paginate drives a Relay-style connection to exhaustion. fetch is called with
// the current cursor ("" first) and returns the page's items, the next cursor,
// and whether more remain. Closes the MCP's silent page-size truncation.
func Paginate[T any](fetch func(after string) (items []T, next string, more bool, err error)) ([]T, error) {
	var all []T
	after := ""
	for {
		items, next, more, err := fetch(after)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if !more || next == "" {
			return all, nil
		}
		after = next
	}
}
