package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbedBatchesAndOrders(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var req request
		json.NewDecoder(r.Body).Decode(&req)
		if req.Dimensions != 8 || req.Model != "m" {
			t.Errorf("request = %+v", req)
		}
		var resp response
		// Reverse order to check that results are placed by index.
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{i, []float32{float32(len(req.Input[i]))}})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m", 8)
	inputs := make([]string, BatchSize+3)
	for i := range inputs {
		inputs[i] = string(make([]byte, i))
	}
	vecs, err := c.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Fatalf("vecs[%d] = %v", i, v)
		}
	}
	if c.Model() != "m@8" {
		t.Errorf("Model() = %q", c.Model())
	}
}

func TestEmbedNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"bad model"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k", "m", 0).Embed(context.Background(), []string{"x"})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

// okHandler answers every request with one 1-dimensional vector per input.
func okHandler(w http.ResponseWriter, r *http.Request) {
	var req request
	json.NewDecoder(r.Body).Decode(&req)
	var resp response
	for i := range req.Input {
		resp.Data = append(resp.Data, struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{i, []float32{1}})
	}
	json.NewEncoder(w).Encode(resp)
}

func TestRequestSendsProviderZDR(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "k", "m", 0).Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	p, _ := body["provider"].(map[string]any)
	if p["zdr"] != true || p["data_collection"] != "deny" {
		t.Fatalf("provider = %v", body["provider"])
	}
}

func TestZDROffOmitsProvider(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "m", 0)
	c.ZDR = false
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["provider"]; ok {
		t.Fatalf("provider sent with ZDR off: %v", body)
	}
}

func TestNoZDREndpointError(t *testing.T) {
	// Messages seen from OpenRouter with a real key.
	const (
		zdrMsg       = "No endpoints found matching your data policy (Zero data retention)"
		guardrailMsg = "0 endpoints out of 1 requested are available matching your guardrail restrictions and data policy"
	)
	cases := []struct {
		name, msg string
		status    int
		zdr       bool
		want      bool
	}{
		{"zdr 404", zdrMsg, 404, true, true},
		{"guardrail 404 with zdr", guardrailMsg, 404, true, true},
		{"guardrail 404 without zdr", guardrailMsg, 404, false, true},
		{"zdr 400", "provider does not support ZDR", 400, true, true},
		{"plain 404 without zdr", "not found", 404, false, false},
		{"plain 404 with zdr", "not found", 404, true, false},
		{"plain 400", "bad model", 400, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": c.msg, "code": c.status}})
				w.WriteHeader(c.status)
				w.Write(body)
			}))
			defer srv.Close()
			cl := New(srv.URL, "k", "openai/x", 0)
			cl.ZDR = c.zdr
			_, err := cl.Embed(context.Background(), []string{"x"})
			if got := errors.Is(err, ErrNoZDREndpoint); got != c.want {
				t.Fatalf("Is(ErrNoZDREndpoint) = %v for %v", got, err)
			}
			if c.want && (!strings.Contains(err.Error(), "model openai/x has no endpoint") || !strings.Contains(err.Error(), c.msg)) {
				t.Errorf("err = %v", err)
			}
			if c.want && c.zdr && !strings.Contains(err.Error(), "SCREE_ZDR=0") {
				t.Errorf("err lacks the SCREE_ZDR hint: %v", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestTimeoutNotRetried(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, "k", "m", 0)
	c.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := c.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no response within 100ms") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRetry503ThenOK(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		okHandler(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "m", 0)
	c.retryDelay = time.Millisecond
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestRetriesExhaustedUnavailable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "m", 0)
	c.retryDelay, c.MaxAttempts = time.Millisecond, 2
	if _, err := c.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrUnavailable) || calls.Load() != 2 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestNetworkErrorUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(okHandler))
	url := srv.URL
	srv.Close()
	c := New(url, "k", "m", 0)
	c.retryDelay = time.Millisecond
	if _, err := c.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorRedactsKey(t *testing.T) {
	const key = "sk-or-v1-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid credentials: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.URL, key, "m", 0).Embed(context.Background(), []string{"x"})
	if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorUsesJSONMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"message":"Insufficient credits","code":402},"user_id":"u"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k", "m", 0).Embed(context.Background(), []string{"x"})
	if err == nil || err.Error() != "embed: 402 Payment Required: Insufficient credits" {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("402 counted as unavailable")
	}
}

func TestErrorRedactsKeyAtCutAndSplit(t *testing.T) {
	const key = "sk-or-v1-0123456789abcdef0123456789abcdef"
	cases := map[string]string{
		// The key straddles the 300-byte cut.
		"at the cut": strings.Repeat("x", 292) + key,
		// An invisible character splits the key.
		"split": "bad key: " + key[:9] + "​" + key[9:],
		// Only the key's tail survives.
		"tail only": "key ends in " + key[len(key)-14:],
	}
	for name, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, body, http.StatusUnauthorized)
		}))
		_, err := New(srv.URL, key, "m", 0).Embed(context.Background(), []string{"x"})
		srv.Close()
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		msg := err.Error()
		for _, part := range []string{key, key[:8], key[len(key)-12:]} {
			if strings.Contains(msg, part) {
				t.Errorf("%s: error leaks %q: %q", name, part, msg)
			}
		}
	}
}
