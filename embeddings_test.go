package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAICompatibleEmbeddingClientSendsCorrectRequest verifies the
// request shape (model/input, path, auth header) and that the client
// doesn't trust response ordering — it must re-sort by the provider's
// reported "index" before handing vectors back.
func TestOpenAICompatibleEmbeddingClientSendsCorrectRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody embeddingsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		// Deliberately out of order: index 1 first, index 0 second.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	client := NewOpenAICompatibleEmbeddingClient(EmbeddingConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-embed-model",
	})

	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if gotPath != "/v1/embeddings" {
		t.Errorf("expected path /v1/embeddings, got %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("expected Authorization: Bearer test-key, got %q", gotAuth)
	}
	if gotBody.Model != "test-embed-model" {
		t.Errorf("expected model %q, got %q", "test-embed-model", gotBody.Model)
	}
	if len(gotBody.Input) != 2 || gotBody.Input[0] != "first" || gotBody.Input[1] != "second" {
		t.Errorf("expected input [first second], got %v", gotBody.Input)
	}

	if len(vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vectors))
	}
	if vectors[0][0] != 1 || vectors[0][1] != 0 {
		t.Errorf("expected vectors[0] (index 0) to be [1,0] after sorting, got %v", vectors[0])
	}
	if vectors[1][0] != 0 || vectors[1][1] != 1 {
		t.Errorf("expected vectors[1] (index 1) to be [0,1] after sorting, got %v", vectors[1])
	}
}

// TestOpenAICompatibleEmbeddingClientRequiresConfig guards the two
// "not configured" error paths so a missing BaseURL/Model fails fast with a
// clear message instead of an obscure HTTP error.
func TestOpenAICompatibleEmbeddingClientRequiresConfig(t *testing.T) {
	client := NewOpenAICompatibleEmbeddingClient(EmbeddingConfig{})
	if _, err := client.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("expected an error when BaseURL and Model are both unset")
	}

	client = NewOpenAICompatibleEmbeddingClient(EmbeddingConfig{BaseURL: "http://example.invalid"})
	if _, err := client.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("expected an error when Model is unset")
	}
}

func TestEmbeddingsURLSuffixHandling(t *testing.T) {
	cases := map[string]string{
		"http://host:1234":            "http://host:1234/v1/embeddings",
		"http://host:1234/v1":         "http://host:1234/v1/embeddings",
		"http://host:1234/v1/":        "http://host:1234/v1/embeddings",
		"http://host:1234/custom":     "http://host:1234/custom/embeddings",
		"http://host:1234/embeddings": "http://host:1234/embeddings",
	}
	for in, want := range cases {
		if got := embeddingsURL(in); got != want {
			t.Errorf("embeddingsURL(%q) = %q, want %q", in, got, want)
		}
	}
}
