package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
