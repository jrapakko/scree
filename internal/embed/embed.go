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
	"time"
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

// Client is an Embedder backed by an HTTP API.
type Client struct {
	URL        string
	APIKey     string
	ModelName  string
	Dimensions int // 0 leaves the model's default
	HTTP       *http.Client
}

func New(url, apiKey, model string, dims int) *Client {
	return &Client{
		URL:        url,
		APIKey:     apiKey,
		ModelName:  model,
		Dimensions: dims,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Model() string {
	if c.Dimensions > 0 {
		return fmt.Sprintf("%s@%d", c.ModelName, c.Dimensions)
	}
	return c.ModelName
}

type request struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
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

func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if c.APIKey == "" {
		return nil, errors.New("embed: no API key (set OPENROUTER_API_KEY)")
	}
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += BatchSize {
		end := min(start+BatchSize, len(inputs))
		vecs, err := c.embedBatch(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(request{Model: c.ModelName, Input: inputs, Dimensions: c.Dimensions})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("embed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, fmt.Errorf("embed: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retry, fmt.Errorf("embed: %s: %s", resp.Status, truncate(raw, 300))
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, false, fmt.Errorf("embed: decoding response: %w", err)
	}
	if r.Error != nil {
		return nil, false, fmt.Errorf("embed: %s", r.Error.Message)
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

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
