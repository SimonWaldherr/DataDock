package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmbeddingClient turns text into vectors for the SQL-logic vector search
// feature (see logic_search.go). Mirrors LLMClient's one-method shape.
type EmbeddingClient interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// EmbeddingConfig configures an OpenAI-compatible embeddings endpoint,
// mirroring LLMConfig's shape one level down (see settings.go's
// currentEmbeddingConfig for how BaseURL/APIKey fall back to the LLM config
// when left blank).
type EmbeddingConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
	Verbose *VerboseLogger
}

// OpenAICompatibleEmbeddingClient calls an OpenAI-compatible "/embeddings"
// endpoint, the embeddings counterpart of OpenAICompatibleClient's
// "/chat/completions" call in llm.go.
type OpenAICompatibleEmbeddingClient struct {
	cfg        EmbeddingConfig
	httpClient *http.Client
}

func NewOpenAICompatibleEmbeddingClient(cfg EmbeddingConfig) *OpenAICompatibleEmbeddingClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	httpClient := &http.Client{Timeout: cfg.Timeout}
	if cfg.Verbose.Enabled() {
		httpClient = cfg.Verbose.HTTPClient(cfg.Timeout)
	}
	return &OpenAICompatibleEmbeddingClient{
		cfg:        cfg,
		httpClient: httpClient,
	}
}

// Embed returns one vector per input text, in the same order as texts.
func (c *OpenAICompatibleEmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		return nil, errors.New("embedding base URL is not configured")
	}
	if strings.TrimSpace(c.cfg.Model) == "" {
		return nil, errors.New("embedding model is not configured")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	body := embeddingsRequest{Model: c.cfg.Model, Input: texts}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, embeddingsURL(c.cfg.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding request failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	if c.cfg.Verbose.Enabled() {
		c.cfg.Verbose.Log(VerboseEvent{
			System:        "embedding",
			Direction:     "inbound",
			Operation:     "embed",
			Target:        embeddingsURL(c.cfg.BaseURL),
			Status:        resp.Status,
			ResponseBytes: int64(len(respBody)),
		})
	}

	var out embeddingsResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding response returned %d vectors for %d inputs", len(out.Data), len(texts))
	}
	// Don't trust response order: sort by the provider-reported index before
	// handing vectors back, so callers can zip them 1:1 against texts.
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vectors := make([][]float64, len(out.Data))
	for i, d := range out.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// embeddingsURL derives the "/embeddings" endpoint from an OpenAI-compatible
// base URL, mirroring chatCompletionsURL's suffix logic in llm.go exactly.
func embeddingsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/embeddings") {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/embeddings"
	}
	if u, err := url.Parse(baseURL); err == nil && u.Path == "" {
		return baseURL + "/v1/embeddings"
	}
	return baseURL + "/embeddings"
}
