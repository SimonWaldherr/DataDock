package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type LLMDiscoveryResult struct {
	Host        string                `json:"host"`
	Port        string                `json:"port"`
	Servers     []LLMDiscoveredServer `json:"servers"`
	Recommended *LLMDiscoveredServer  `json:"recommended,omitempty"`
}

type LLMDiscoveredServer struct {
	Provider string   `json:"provider"`
	Name     string   `json:"name"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	Error    string   `json:"error,omitempty"`
}

type llmDiscoveryCandidate struct {
	provider  string
	name      string
	baseURL   string
	modelsURL string
	ollamaURL string
}

func discoverLLMServers(ctx context.Context, client *http.Client, host, port string) LLMDiscoveryResult {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		host = "127.0.0.1"
	}
	if client == nil {
		client = &http.Client{Timeout: 1200 * time.Millisecond}
	}
	result := LLMDiscoveryResult{Host: host, Port: port}
	for _, candidate := range llmDiscoveryCandidates(host, port) {
		server := probeLLMServer(ctx, client, candidate)
		if len(server.Models) == 0 && server.Error != "" {
			continue
		}
		result.Servers = append(result.Servers, server)
	}
	sort.SliceStable(result.Servers, func(i, j int) bool {
		if len(result.Servers[i].Models) != len(result.Servers[j].Models) {
			return len(result.Servers[i].Models) > len(result.Servers[j].Models)
		}
		return result.Servers[i].Provider < result.Servers[j].Provider
	})
	if len(result.Servers) > 0 {
		recommended := result.Servers[0]
		result.Recommended = &recommended
	}
	return result
}

func llmDiscoveryCandidates(host, port string) []llmDiscoveryCandidate {
	if base, ok := explicitLLMBase(host); ok {
		root := strings.TrimRight(base, "/")
		return []llmDiscoveryCandidate{
			{
				provider:  "openai-compatible",
				name:      "OpenAI-compatible server",
				baseURL:   ensureV1BaseURL(root),
				modelsURL: ensureV1BaseURL(root) + "/models",
			},
			{
				provider:  "ollama",
				name:      "Ollama",
				baseURL:   ensureV1BaseURL(root),
				modelsURL: ensureV1BaseURL(root) + "/models",
				ollamaURL: root + "/api/tags",
			},
		}
	}
	if port == "" {
		port = "1234"
	}
	candidates := []llmDiscoveryCandidate{
		openAICompatibleCandidate("lmstudio", "LM Studio / llmster", host, port),
		openAICompatibleCandidate("openai-compatible", "OpenAI-compatible server", host, port),
	}
	if port != "11434" {
		candidates = append(candidates, ollamaCandidate(host, "11434"))
	}
	candidates = append(candidates, ollamaCandidate(host, port))
	return dedupeLLMDiscoveryCandidates(candidates)
}

func openAICompatibleCandidate(provider, name, host, port string) llmDiscoveryCandidate {
	root := "http://" + net.JoinHostPort(host, port)
	base := root + "/v1"
	return llmDiscoveryCandidate{
		provider:  provider,
		name:      name,
		baseURL:   base,
		modelsURL: base + "/models",
	}
}

func ollamaCandidate(host, port string) llmDiscoveryCandidate {
	root := "http://" + net.JoinHostPort(host, port)
	return llmDiscoveryCandidate{
		provider:  "ollama",
		name:      "Ollama",
		baseURL:   root + "/v1",
		modelsURL: root + "/v1/models",
		ollamaURL: root + "/api/tags",
	}
}

func dedupeLLMDiscoveryCandidates(in []llmDiscoveryCandidate) []llmDiscoveryCandidate {
	seen := make(map[string]bool)
	out := make([]llmDiscoveryCandidate, 0, len(in))
	for _, candidate := range in {
		key := candidate.provider + "|" + candidate.baseURL + "|" + candidate.modelsURL + "|" + candidate.ollamaURL
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, candidate)
	}
	return out
}

func explicitLLMBase(host string) (string, bool) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return strings.TrimRight(host, "/"), true
}

func ensureV1BaseURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}

func probeLLMServer(ctx context.Context, client *http.Client, candidate llmDiscoveryCandidate) LLMDiscoveredServer {
	server := LLMDiscoveredServer{
		Provider: candidate.provider,
		Name:     candidate.name,
		BaseURL:  candidate.baseURL,
	}
	var errs []string
	if candidate.ollamaURL != "" {
		if models, err := fetchOllamaModels(ctx, client, candidate.ollamaURL); err == nil {
			server.Models = models
			return server
		} else {
			errs = append(errs, err.Error())
		}
	}
	if candidate.modelsURL != "" {
		if models, err := fetchOpenAIModels(ctx, client, candidate.modelsURL); err == nil {
			server.Models = models
			return server
		} else {
			errs = append(errs, err.Error())
		}
	}
	server.Error = strings.Join(errs, "; ")
	return server
}

func fetchOpenAIModels(ctx context.Context, client *http.Client, endpoint string) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := fetchLLMDiscoveryJSON(ctx, client, endpoint, &out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, model := range out.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			models = append(models, id)
		}
	}
	return sortedUniqueModels(models), nil
}

func fetchOllamaModels(ctx context.Context, client *http.Client, endpoint string) ([]string, error) {
	var out struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := fetchLLMDiscoveryJSON(ctx, client, endpoint, &out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Models))
	for _, model := range out.Models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = strings.TrimSpace(model.Model)
		}
		if name != "" {
			models = append(models, name)
		}
	}
	return sortedUniqueModels(models), nil
}

func fetchLLMDiscoveryJSON(ctx context.Context, client *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s returned invalid JSON: %w", endpoint, err)
	}
	return nil
}

func sortedUniqueModels(models []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}
