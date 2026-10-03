package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// OpenAIModelsCacheTTL is how long a successful /v1/models fetch is reused.
const OpenAIModelsCacheTTL = 60 * time.Second

type openAIModelsCacheEntry struct {
	models    []ModelOption
	fetchedAt time.Time
}

var (
	openAIModelsCacheMu sync.Mutex
	openAIModelsCache   = map[string]openAIModelsCacheEntry{}
)

// ModelsEndpointURL joins base (with or without trailing /v1) to the models path.
func ModelsEndpointURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/models"
	}
	return baseURL + "/v1/models"
}

// MergeModelAliases overlays Alias/Group/Desc from configured shortcuts onto a live catalog.
// Matching is by model Name (case-sensitive id). Live order is preserved.
func MergeModelAliases(live, configured []ModelOption) []ModelOption {
	if len(live) == 0 || len(configured) == 0 {
		return live
	}
	byName := make(map[string]ModelOption, len(configured))
	for _, c := range configured {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		byName[name] = c
	}
	out := make([]ModelOption, len(live))
	for i, m := range live {
		if c, ok := byName[m.Name]; ok {
			if c.Alias != "" {
				m.Alias = c.Alias
			}
			if c.Group != "" {
				m.Group = c.Group
			}
			if c.Desc != "" && m.Desc == "" {
				m.Desc = c.Desc
			}
		}
		out[i] = m
	}
	return out
}

// FetchOpenAICompatibleModels GETs {base}/models (OpenAI-style) with Bearer auth.
// Results are cached briefly per (baseURL, apiKey) so /model menus stay snappy.
func FetchOpenAICompatibleModels(ctx context.Context, baseURL, apiKey string) []ModelOption {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil
	}
	cacheKey := baseURL + "\x00" + apiKey
	openAIModelsCacheMu.Lock()
	if ent, ok := openAIModelsCache[cacheKey]; ok && time.Since(ent.fetchedAt) < OpenAIModelsCacheTTL && len(ent.models) > 0 {
		out := append([]ModelOption(nil), ent.models...)
		openAIModelsCacheMu.Unlock()
		return out
	}
	openAIModelsCacheMu.Unlock()

	url := ModelsEndpointURL(baseURL)
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// New API and some Anthropic-compatible gateways also accept x-api-key.
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("core: fetch openai-compatible models failed", "url", url, "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("core: fetch openai-compatible models non-OK", "url", url, "status", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil
	}
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(result.Data))
	models := make([]ModelOption, 0, len(result.Data))
	for _, m := range result.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		desc := strings.TrimSpace(m.DisplayName)
		models = append(models, ModelOption{Name: id, Desc: desc})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	if len(models) == 0 {
		return nil
	}
	openAIModelsCacheMu.Lock()
	openAIModelsCache[cacheKey] = openAIModelsCacheEntry{models: append([]ModelOption(nil), models...), fetchedAt: time.Now()}
	openAIModelsCacheMu.Unlock()
	return models
}

// ResolveModelsAPICreds picks base URL + API key from explicit values, then common env vars
// used by New API / OpenAI-compatible gateways on this box.
func ResolveModelsAPICreds(baseURL, apiKey string) (string, string) {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		for _, k := range []string{"JUNG_API_KEY", "NEWAPI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				apiKey = v
				break
			}
		}
	}
	if baseURL == "" {
		for _, k := range []string{"NEWAPI_MODELS_BASE_URL", "GROK_MODELS_BASE_URL", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				baseURL = v
				break
			}
		}
	}
	return strings.TrimRight(baseURL, "/"), apiKey
}
