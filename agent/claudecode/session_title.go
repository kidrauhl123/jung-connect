package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

const (
	sessionTitleMaxInputChars = 1000
	sessionTitleMaxTokens     = 80
	sessionTitleHTTPTimeout   = 12 * time.Second
)

const sessionTitleSystemPrompt = `You are naming a coding session so the user can pick it out of a long list of sessions. The title is a name for what the session is about, not a sentence describing the task: a short noun phrase of two to five words, in sentence case (capitalize only the first word, plus proper nouns, acronyms, and code identifiers exactly as written). Write the title in the predominant language of the session. Return JSON with a single "title" field. Do not echo the user's message.`

func titleSourceText(prompt string) string {
	s := strings.TrimSpace(prompt)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[cc-connect ") {
		if i := strings.Index(s, "]\n"); i >= 0 {
			s = strings.TrimSpace(s[i+2:])
		} else if i := strings.IndexByte(s, ']'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return ""
	}
	if utf8.RuneCountInString(s) > sessionTitleMaxInputChars {
		s = string([]rune(s)[:sessionTitleMaxInputChars])
	}
	return s
}

func acceptGeneratedSessionTitle(source, title string) string {
	title = strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(title), "\n", " ")), " ")
	title = strings.Trim(title, `"'“”‘’`)
	if title == "" {
		return ""
	}
	src := strings.Join(strings.Fields(strings.TrimSpace(source)), " ")
	if src != "" && strings.EqualFold(title, src) {
		return ""
	}
	return title
}

func extractJSONTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	var obj struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		return strings.TrimSpace(obj.Title)
	}
	// Loose scan: {"title": "..."} buried in prose.
	const key = `"title"`
	if i := strings.Index(strings.ToLower(raw), key); i >= 0 {
		rest := raw[i+len(key):]
		if j := strings.Index(rest, `"`); j >= 0 {
			rest = rest[j+1:]
			var b strings.Builder
			esc := false
			for _, r := range rest {
				if esc {
					b.WriteRune(r)
					esc = false
					continue
				}
				if r == '\\' {
					esc = true
					continue
				}
				if r == '"' {
					return strings.TrimSpace(b.String())
				}
				b.WriteRune(r)
			}
		}
	}
	return ""
}

type envSessionTitleClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func newEnvSessionTitleClient(env []string) sessionTitleClient {
	var base, auth, apiKey, haiku, model string
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch k {
		case "ANTHROPIC_BASE_URL":
			base = strings.TrimSpace(v)
		case "ANTHROPIC_AUTH_TOKEN":
			auth = strings.TrimSpace(v)
		case "ANTHROPIC_API_KEY":
			apiKey = strings.TrimSpace(v)
		case "ANTHROPIC_DEFAULT_HAIKU_MODEL":
			haiku = strings.TrimSpace(v)
		case "ANTHROPIC_MODEL":
			model = strings.TrimSpace(v)
		}
	}
	key := auth
	if key == "" {
		key = apiKey
	}
	if haiku != "" {
		model = haiku
	}
	if base == "" || key == "" || model == "" {
		return nil
	}
	return &envSessionTitleClient{
		baseURL: base,
		apiKey:  key,
		model:   model,
		http:    &http.Client{Timeout: sessionTitleHTTPTimeout},
	}
}

func anthropicMessagesURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func (c *envSessionTitleClient) GenerateTitle(ctx context.Context, prompt string) (string, error) {
	if c == nil || c.http == nil {
		return "", fmt.Errorf("title client not configured")
	}
	body, err := json.Marshal(map[string]any{
		"model":      c.model,
		"max_tokens": sessionTitleMaxTokens,
		"system":     sessionTitleSystemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": "<session>\n" + prompt + "\n</session>"},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL(c.baseURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("title http %d", resp.StatusCode)
	}

	var anth struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &anth); err == nil {
		for _, part := range anth.Content {
			if part.Type == "text" || part.Type == "" {
				if title := extractJSONTitle(part.Text); title != "" {
					return title, nil
				}
				if t := strings.TrimSpace(part.Text); t != "" {
					return t, nil
				}
			}
		}
	}
	var oai struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &oai); err == nil && len(oai.Choices) > 0 {
		text := oai.Choices[0].Message.Content
		if title := extractJSONTitle(text); title != "" {
			return title, nil
		}
		return strings.TrimSpace(text), nil
	}
	if title := extractJSONTitle(string(raw)); title != "" {
		return title, nil
	}
	return "", fmt.Errorf("no title in response")
}

func (cs *claudeSession) maybeGenerateSessionTitle(prompt string) {
	if cs == nil || cs.titleClient == nil {
		return
	}
	source := titleSourceText(prompt)
	if source == "" {
		return
	}
	if !cs.titled.CompareAndSwap(false, true) {
		return
	}
	go cs.runSessionTitleGeneration(source)
}

func (cs *claudeSession) runSessionTitleGeneration(source string) {
	ctx, cancel := context.WithTimeout(cs.ctx, sessionTitleHTTPTimeout)
	defer cancel()
	raw, err := cs.titleClient.GenerateTitle(ctx, source)
	if err != nil {
		cs.titled.Store(false)
		slog.Debug("claudecode: session title generation failed", "error", err)
		return
	}
	title := acceptGeneratedSessionTitle(source, raw)
	if title == "" {
		cs.titled.Store(false)
		slog.Debug("claudecode: session title rejected", "reason", "empty or echoed prompt")
		return
	}
	evt := core.Event{Type: core.EventSessionTitle, Content: title, SessionID: cs.CurrentSessionID()}
	select {
	case cs.events <- evt:
		slog.Info("claudecode: generated session title", "title", title)
	case <-cs.ctx.Done():
	}
}
