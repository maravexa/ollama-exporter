package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPS_Success(t *testing.T) {
	want := PSResponse{
		Models: []RunningModel{
			{
				Name:     "llama3.1:8b",
				Model:    "llama3.1:8b",
				Size:     4815411200,
				SizeVRAM: 4815411200,
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.PS(context.Background())
	if err != nil {
		t.Fatalf("PS() error = %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("got %d models, want 1", len(got.Models))
	}
	if got.Models[0].Name != want.Models[0].Name {
		t.Errorf("name = %q, want %q", got.Models[0].Name, want.Models[0].Name)
	}
	if got.Models[0].SizeVRAM != want.Models[0].SizeVRAM {
		t.Errorf("size_vram = %d, want %d", got.Models[0].SizeVRAM, want.Models[0].SizeVRAM)
	}
}

func TestPS_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.PS(context.Background())
	if err == nil {
		t.Fatal("PS() expected error on 500 response, got nil")
	}
}

func TestPS_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{
		baseURL: srv.URL,
		httpClient: &http.Client{
			Timeout: 1 * time.Millisecond,
		},
	}
	_, err := c.PS(context.Background())
	if err == nil {
		t.Fatal("PS() expected timeout error, got nil")
	}
}

func TestShow_Success(t *testing.T) {
	want := ShowResponse{
		Details: ModelDetails{
			QuantizationLevel: "Q4_K_M",
			Family:            "llama",
			ParameterSize:     "8B",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.Show(context.Background(), "llama3.1:8b-q4_k_m")
	if err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	if got.Details.QuantizationLevel != want.Details.QuantizationLevel {
		t.Errorf("quant = %q, want %q", got.Details.QuantizationLevel, want.Details.QuantizationLevel)
	}
	if got.Details.Family != want.Details.Family {
		t.Errorf("family = %q, want %q", got.Details.Family, want.Details.Family)
	}
}

func TestShow_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Show(context.Background(), "missing-model")
	if err == nil {
		t.Fatal("Show() expected error on 500 response, got nil")
	}
}

func TestShow_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{
		baseURL: srv.URL,
		httpClient: &http.Client{
			Timeout: 1 * time.Millisecond,
		},
	}
	_, err := c.Show(context.Background(), "llama3.1:8b")
	if err == nil {
		t.Fatal("Show() expected timeout error, got nil")
	}
}
