// Package embed calls an OpenAI-compatible embeddings API, such as
// OpenRouter's.
package embed

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
	"unicode/utf8"
)

// Embedder turns text into vectors.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// Model identifies the model and dimensions, so an index built with one
	// model is never queried with another.
	Model() string
}

// BatchSize is the number of inputs sent per request.
const BatchSize = 64

// ErrUnavailable means the API did not answer in time, could not be
// reached, or returned 429 or a 5xx status. Callers may try again later.
var ErrUnavailable = errors.New("embedding API unavailable")

// ErrNoZDREndpoint means the provider has no endpoint for the model that
// meets the requested data policy, such as zero data retention.
var ErrNoZDREndpoint = errors.New("no zero-data-retention endpoint for this model")

// Client is an Embedder backed by an HTTP API.
type Client struct {
	URL        string
	APIKey     string
	ModelName  string
	Dimensions int // 0 leaves the model's default
	HTTP       *http.Client
	// Timeout bounds each attempt; 0 means no limit. A timed-out attempt is
	// not retried.
	Timeout time.Duration
	// MaxAttempts is the number of tries for network errors, 429 and 5xx.
	MaxAttempts int
	// ZDR asks OpenRouter to route only to providers with zero data
	// retention that do not collect data.
	ZDR bool

	retryDelay time.Duration // base wait between attempts; 0 means 2 s
}

// New returns a client with a 30 s per-attempt timeout, three attempts and
// zero data retention requested.
func New(url, apiKey, model string, dims int) *Client {
	return &Client{
		URL:         url,
		APIKey:      apiKey,
		ModelName:   model,
		Dimensions:  dims,
		HTTP:        &http.Client{},
		Timeout:     30 * time.Second,
		MaxAttempts: 3,
		ZDR:         true,
	}
}

func (c *Client) Model() string {
	if c.Dimensions > 0 {
		return fmt.Sprintf("%s@%d", c.ModelName, c.Dimensions)
	}
	return c.ModelName
}

type provider struct {
	ZDR            bool   `json:"zdr"`
	DataCollection string `json:"data_collection"`
}

type request struct {
	Model      string    `json:"model"`
	Input      []string  `json:"input"`
	Dimensions int       `json:"dimensions,omitempty"`
	Provider   *provider `json:"provider,omitempty"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed returns one vector per input, sending BatchSize inputs per request.
// Errors never contain the API key.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if c.APIKey == "" {
		return nil, errors.New("embed: no API key (set OPENROUTER_API_KEY)")
	}
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += BatchSize {
		end := min(start+BatchSize, len(inputs))
		vecs, err := c.embedBatch(ctx, inputs[start:end])
		if err != nil {
			return nil, c.redact(err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	req := request{Model: c.ModelName, Input: inputs, Dimensions: c.Dimensions}
	if c.ZDR {
		req.Provider = &provider{ZDR: true, DataCollection: "deny"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	delay := c.retryDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt < max(c.MaxAttempts, 1); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * delay):
			}
		}
		vecs, retry, err := c.post(ctx, body, len(inputs))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) post(ctx context.Context, body []byte, n int) (vecs [][]float32, retry bool, err error) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if c.Timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	defer cancel()
	// transportErr classifies a failure to get a full response.
	transportErr := func(what string, err error) ([][]float32, bool, error) {
		switch {
		case ctx.Err() != nil:
			return nil, false, ctx.Err()
		case actx.Err() != nil:
			return nil, false, fmt.Errorf("embed: %w: no response within %s", ErrUnavailable, c.Timeout)
		}
		return nil, true, fmt.Errorf("embed: %w: %s%v", ErrUnavailable, what, err)
	}

	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportErr("", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return transportErr("reading response: ", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryable(resp.StatusCode), c.statusErr(resp, raw)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, false, fmt.Errorf("embed: decoding response: %w", err)
	}
	if r.Error != nil {
		return nil, false, fmt.Errorf("embed: %s", truncate([]byte(r.Error.Message), 300))
	}
	if len(r.Data) != n {
		return nil, false, fmt.Errorf("embed: got %d embeddings for %d inputs", len(r.Data), n)
	}
	vecs = make([][]float32, n)
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= n || vecs[d.Index] != nil {
			return nil, false, fmt.Errorf("embed: bad index %d in response", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, false, nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// statusErr builds the error for a non-200 response.
func (c *Client) statusErr(resp *http.Response, raw []byte) error {
	msg := apiMessage(raw)
	if c.noEndpoint(resp.StatusCode, msg) {
		hint := "choose another model or set SCREE_ZDR=0"
		what := "meets zero data retention"
		if !c.ZDR {
			hint = "choose another model or check the account's privacy settings"
			what = "is allowed by the account's data policy"
		}
		return &noEndpointError{fmt.Sprintf("embed: model %s has no endpoint that %s; %s (API: %s)", c.ModelName, what, hint, msg)}
	}
	if retryable(resp.StatusCode) {
		return fmt.Errorf("embed: %w: %s: %s", ErrUnavailable, resp.Status, msg)
	}
	return fmt.Errorf("embed: %s: %s", resp.Status, msg)
}

// noEndpoint recognizes "no endpoint matches the data policy" responses:
// a 400 or 404 whose message names the data policy. OpenRouter sends these
// when ZDR rules out every endpoint, or when the account's own data policy
// or guardrails do. Other 404s, such as a wrong URL, stay plain errors.
func (c *Client) noEndpoint(status int, msg string) bool {
	if status != http.StatusNotFound && status != http.StatusBadRequest {
		return false
	}
	m := strings.ToLower(msg)
	return strings.Contains(m, "zdr") || strings.Contains(m, "data policy") ||
		strings.Contains(m, "data retention") || strings.Contains(m, "guardrail")
}

type noEndpointError struct{ msg string }

func (e *noEndpointError) Error() string { return e.msg }
func (e *noEndpointError) Unwrap() error { return ErrNoZDREndpoint }

// apiMessage returns error.message from a JSON error body, or the raw body,
// cut to 300 bytes.
func apiMessage(raw []byte) string {
	var r response
	if json.Unmarshal(raw, &r) == nil && r.Error != nil && r.Error.Message != "" {
		raw = []byte(r.Error.Message)
	}
	return truncate(bytes.TrimSpace(raw), 300)
}

// redact replaces the API key in err's message, keeping err's chain for
// errors.Is.
func (c *Client) redact(err error) error {
	if err == nil || len(c.APIKey) < 8 || !strings.Contains(err.Error(), c.APIKey) {
		return err
	}
	return &redactedError{strings.ReplaceAll(err.Error(), c.APIKey, "[redacted]"), err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func truncate(b []byte, n int) string {
	if len(b) > n {
		for n > 0 && !utf8.RuneStart(b[n]) {
			n--
		}
		return string(b[:n]) + "…"
	}
	return string(b)
}
