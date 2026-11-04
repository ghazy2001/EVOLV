// Package providers abstracts model backends behind one streaming chat call.
// Default: Kilo Gateway anonymous (free, IP-based, zero signup) — with BYOK
// for OpenAI / Anthropic / OpenRouter / Groq / Ollama / any OpenAI-compatible
// endpoint.
package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Message is one conversation turn.
type Message struct {
	Role    string   `json:"role"` // system | user | assistant
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

// Delta is a streamed token (or reasoning) chunk.
type Delta struct {
	Content   string // visible text
	Reasoning string // thinking tokens, if any
	Err       error
	Done      bool
}

// Provider is a chat backend.
type Provider interface {
	Name() string
	Models(ctx context.Context) ([]string, error)
	Stream(ctx context.Context, model string, msgs []Message, onDelta func(Delta)) (string, error)
}

// Registry resolves provider names.
type Registry struct{ m map[string]Provider }

// NewRegistry builds a registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Provider{}} }

// Add registers a provider.
func (r *Registry) Add(p Provider) {
	if r == nil || p == nil {
		return
	}
	if r.m == nil {
		r.m = map[string]Provider{}
	}
	r.m[p.Name()] = p
}

// Get resolves a provider by name. A nil registry resolves to nil instead of
// panicking, so read-only surfaces stay safe in partial deployments.
func (r *Registry) Get(name string) Provider {
	if r == nil {
		return nil
	}
	return r.m[name]
}

// Names lists registered providers in a stable order.
func (r *Registry) Names() []string {
	out := []string{}
	if r == nil {
		return out
	}
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- shared OpenAI-compatible streaming client ----------

type openaiCompat struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

func (o *openaiCompat) Name() string { return o.name }

type oaiModelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (o *openaiCompat) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(o.baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	o.decorate(req)
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var mr oaiModelsResp
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

func (o *openaiCompat) decorate(req *http.Request) {
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
}

type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
			// reasoning_content is what DeepSeek, vLLM, Groq, Together and
			// most self-hosted reasoning models use instead of "reasoning".
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// oaiFullResp is the non-streaming shape some gateways return even when
// stream:true is requested.
type oaiFullResp struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// reasoningOf picks the first non-empty reasoning field across the dialects.
func reasoningOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func formatOpenAIMessages(msgs []Message) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		if len(m.Images) == 0 {
			out[i] = m
			continue
		}
		parts := []map[string]any{
			{"type": "text", "text": m.Content},
		}
		for _, img := range m.Images {
			url := img
			if !strings.HasPrefix(url, "data:") {
				url = "data:image/png;base64," + img
			}
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
		}
		out[i] = map[string]any{
			"role":    m.Role,
			"content": parts,
		}
	}
	return out
}

func (o *openaiCompat) Stream(ctx context.Context, model string, msgs []Message, onDelta func(Delta)) (string, error) {
	body := map[string]any{
		"model":       model,
		"messages":    formatOpenAIMessages(msgs),
		"max_tokens":  8192,
		"temperature": 0.8,
		"stream":      true,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(o.baseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	o.decorate(req)

	resp, err := o.client.Do(req)
	if err != nil {
		return "", Classify(o.name, 0, err)
	}
	defer resp.Body.Close()

	debug := os.Getenv("EVOLV_DEBUG") != ""
	if debug {
		fmt.Fprintf(os.Stderr, "[evolv-debug] %s status=%d content-type=%s\n", o.baseURL, resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		pe := Classify(o.name, resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(b))))
		pe.RetryIn = retryAfter(resp.Header.Get("Retry-After"))
		return "", pe
	}

	var full strings.Builder
	if err := o.consumeStream(resp, &full, onDelta); err != nil {
		return full.String(), err
	}
	if onDelta != nil {
		onDelta(Delta{Done: true})
	}
	return full.String(), nil
}

// consumeStream reads either an SSE token stream or a single JSON body. Some
// gateways ignore stream:true and answer with one complete chat completion,
// which used to look like an empty reply.
func (o *openaiCompat) consumeStream(resp *http.Response, full *strings.Builder, onDelta func(Delta)) error {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return o.consumeSSE(resp.Body, full, onDelta)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	// A few gateways stream with the wrong content type but still use SSE.
	if bytes.Contains(raw, []byte("data:")) {
		return o.consumeSSE(bytes.NewReader(raw), full, onDelta)
	}
	return o.consumeJSON(raw, full, onDelta)
}

func (o *openaiCompat) consumeJSON(raw []byte, full *strings.Builder, onDelta func(Delta)) error {
	var fr oaiFullResp
	if err := json.Unmarshal(raw, &fr); err != nil {
		return fmt.Errorf("%s: unreadable response: %w", o.name, err)
	}
	if fr.Error != nil {
		return fmt.Errorf("%s: %s", o.name, fr.Error.Message)
	}
	if len(fr.Choices) == 0 {
		return fmt.Errorf("%s: empty response", o.name)
	}
	m := fr.Choices[0].Message
	if m.Content != "" {
		full.WriteString(m.Content)
	}
	if onDelta != nil && (m.Content != "" || m.Reasoning != "" || m.ReasoningContent != "") {
		onDelta(Delta{Content: m.Content, Reasoning: reasoningOf(m.Reasoning, m.ReasoningContent)})
	}
	return nil
}

// consumeSSE decodes a server-sent event stream without assuming one event
// per line or one line per network chunk. It tolerates CRLF, comment lines,
// multi-line data fields, empty events, malformed payloads (skipped, never
// fatal) and streams that end without [DONE].
func (o *openaiCompat) consumeSSE(r io.Reader, full *strings.Builder, onDelta func(Delta)) error {
	return readSSE(r, func(payload string) error {
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var ch oaiChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			// Some gateways prefix the JSON with "event:" style payloads or
			// send keep-alives; try to recover the object before giving up.
			if obj := scanJSON(payload); obj != "" {
				if json.Unmarshal([]byte(obj), &ch) != nil {
					return nil
				}
			} else {
				return nil
			}
		}
		if ch.Error != nil {
			return Classify(o.name, 0, fmt.Errorf("%s", ch.Error.Message))
		}
		if len(ch.Choices) == 0 {
			return nil
		}
		d := ch.Choices[0].Delta
		dv := Delta{
			Content:   d.Content,
			Reasoning: reasoningOf(d.Reasoning, d.ReasoningContent),
		}
		if dv.Content != "" {
			full.WriteString(dv.Content)
		}
		if onDelta != nil && (dv.Content != "" || dv.Reasoning != "") {
			onDelta(dv)
		}
		return nil
	})
}

// readSSE parses an SSE byte stream into event payloads. Data fields are
// joined with newlines per the SSE spec; a blank line dispatches the event.
func readSSE(r io.Reader, handle func(payload string) error) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	var data []string
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if len(data) > 0 {
					if herr := handle(strings.Join(data, "\n")); herr != nil {
						return herr
					}
					data = data[:0]
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			case strings.HasPrefix(line, "data:"):
				field := strings.TrimPrefix(line, "data:")
				data = append(data, strings.TrimPrefix(field, " "))
			default:
				// event:, id:, retry: are not needed for token streaming
			}
		}
		if err != nil {
			// Flush a final event that arrived without its blank-line
			// terminator, then surface only real read failures.
			if len(data) > 0 {
				if herr := handle(strings.Join(data, "\n")); herr != nil {
					return herr
				}
			}
			if err == io.EOF {
				return nil
			}
			if strings.Contains(err.Error(), "unexpected EOF") {
				return nil
			}
			return Classify("", 0, err)
		}
	}
}

// scanJSON extracts the first balanced JSON object from s, or "".
func scanJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// retryAfter parses a Retry-After header (seconds or HTTP date).
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ---------- Anthropic native ----------

type anthropic struct {
	apiKey  string
	client  *http.Client
	baseURL string // overridable in tests; defaults to the real endpoint
}

func (a *anthropic) Name() string { return "anthropic" }

func (a *anthropic) Models(ctx context.Context) ([]string, error) {
	return []string{
		"claude-opus-4-7", "claude-sonnet-4-7", "claude-sonnet-4-6",
		"claude-haiku-4-7", "claude-haiku-4-5",
	}, nil
}

type antStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicThinking reports whether extended thinking was opted into. It stays
// off by default so ordinary chats keep their current latency and cost.
func anthropicThinking() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EVOLV_THINKING"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

func (a *anthropic) Stream(ctx context.Context, model string, msgs []Message, onDelta func(Delta)) (string, error) {
	// split system messages
	var system strings.Builder
	rest := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			system.WriteString(m.Content + "\n\n")
		} else if len(m.Images) == 0 {
			rest = append(rest, m)
		} else {
			parts := []map[string]any{
				{"type": "text", "text": m.Content},
			}
			for _, img := range m.Images {
				raw := img
				mediaType := "image/png"
				if strings.HasPrefix(raw, "data:") {
					partsData := strings.SplitN(raw, ",", 2)
					if len(partsData) == 2 {
						mediaType = strings.TrimSuffix(strings.TrimPrefix(partsData[0], "data:"), ";base64")
						raw = partsData[1]
					}
				}
				parts = append(parts, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": mediaType,
						"data":       raw,
					},
				})
			}
			rest = append(rest, map[string]any{
				"role":    m.Role,
				"content": parts,
			})
		}
	}
	build := func(thinking bool) []byte {
		body := map[string]any{
			"model":      model,
			"max_tokens": 8192,
			"stream":     true,
			"messages":   rest,
		}
		if s := strings.TrimSpace(system.String()); s != "" {
			body["system"] = s
		}
		if thinking {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2048}
		}
		raw, _ := json.Marshal(body)
		return raw
	}
	send := func(raw []byte) (*http.Response, error) {
		endpoint := a.baseURL
		if endpoint == "" {
			endpoint = "https://api.anthropic.com/v1/messages"
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return a.client.Do(req)
	}

	thinking := anthropicThinking()
	resp, err := send(build(thinking))
	if err != nil {
		return "", Classify("anthropic", 0, err)
	}
	if thinking && resp.StatusCode == http.StatusBadRequest {
		// The model does not accept extended thinking — retry plainly rather
		// than failing the turn.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if !strings.Contains(strings.ToLower(string(b)), "thinking") {
			return "", Classify("anthropic", resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(b))))
		}
		resp, err = send(build(false))
		if err != nil {
			return "", Classify("anthropic", 0, err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		pe := Classify("anthropic", resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(b))))
		pe.RetryIn = retryAfter(resp.Header.Get("Retry-After"))
		return "", pe
	}

	var full strings.Builder
	err = readSSE(resp.Body, func(payload string) error {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return nil
		}
		var ev antStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil // unknown event shapes are ignored, not fatal
		}
		switch ev.Type {
		case "content_block_delta":
			switch {
			case ev.Delta.Type == "thinking_delta" || (ev.Delta.Thinking != "" && ev.Delta.Text == ""):
				if ev.Delta.Thinking != "" && onDelta != nil {
					onDelta(Delta{Reasoning: ev.Delta.Thinking})
				}
			case ev.Delta.Text != "":
				full.WriteString(ev.Delta.Text)
				if onDelta != nil {
					onDelta(Delta{Content: ev.Delta.Text})
				}
			}
		case "error":
			if ev.Error != nil {
				return Classify("anthropic", 0, fmt.Errorf("%s", ev.Error.Message))
			}
		}
		return nil
	})
	if err != nil {
		return full.String(), err
	}
	if onDelta != nil {
		onDelta(Delta{Done: true})
	}
	return full.String(), nil
}

// ---------- Mock (offline demo / self-evolution showcase) ----------

type mock struct{ turn int }

func (m *mock) Name() string { return "mock" }
func (m *mock) Models(ctx context.Context) ([]string, error) {
	return []string{"evolv-sim-1"}, nil
}

func (m *mock) Stream(ctx context.Context, model string, msgs []Message, onDelta func(Delta)) (string, error) {
	m.turn++
	script := mockScript(m.turn, msgs)
	// a short streamed "thought" so the reasoning panel is visible offline
	if onDelta != nil {
		for _, w := range []string{"reading the request", " · inspecting the living workspace", " · drafting a response"} {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(30 * time.Millisecond):
			}
			onDelta(Delta{Reasoning: w})
		}
	}
	// stream it word by word for the full live effect
	words := strings.SplitAfter(script, " ")
	for _, w := range words {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(18 * time.Millisecond):
		}
		if onDelta != nil {
			onDelta(Delta{Content: w})
		}
	}
	if onDelta != nil {
		onDelta(Delta{Done: true})
	}
	return script, nil
}

// ---------- Factory ----------

// KiloBaseURL is Kilo Gateway's OpenAI-compatible endpoint.
const KiloBaseURL = "https://ai-gateway.kilo.ai/api/v1"

// KiloFreeModels are the curated anonymous-tier defaults (IP rate-limited,
// no key). kilo-auto/free rotates the live free pool server-side.
var KiloFreeModels = []string{
	"kilo-auto/free",
	"openrouter/free",
	"nvidia/nemotron-3.5-super-vl:free",
	"nvidia/nemotron-3.5-lightning:free",
	"nvidia/nemotron-3-ultra-550b-a55b:free",
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
	"cohere/north-mini-code:free",
	"poolside/laguna-s-2.1:free",
	"poolside/laguna-xs-2.1:free",
	"reflection/beam-free",
	"recursive/flint-v1.0:free",
	"liquid/lfm-2.5-2.6b:free",
	"dots-studio/dots-3-note-preview:free",
	"xingchen-agi/xing4.0-29b-a4b:free",
	"inclusionai/ling-3.1-flash-1019-free",
}

// KiloPaidModels are suggestions unlocked when the user adds a Kilo key (BYOK).
var KiloPaidModels = []string{
	"kilo-auto/frontier", "kilo-auto/efficient", "kilo-auto/small",
	"anthropic/claude-opus-4.7", "anthropic/claude-sonnet-4.6",
	"openai/gpt-6-luna", "z-ai/glm-5.3", "moonshotai/kimi-k3",
	"qwen/qwen3.8-max-0902", "deepseek/deepseek-v4.1-flash-1017",
	"x-ai/grok-4.7", "mistralai/mistral-large-4-0",
}

// Build wires the provider registry from config values.
func Build(provider, model, apiKey string, base_urls map[string]string) (Provider, string) {
	client := &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
	key := func(p string) string {
		if apiKey != "" {
			return apiKey
		}
		return ""
	}
	_ = key
	switch provider {
	case "kilo", "":
		return &openaiCompat{name: "kilo", baseURL: KiloBaseURL, apiKey: apiKey, client: client}, defaultOr(model, "kilo-auto/free")
	case "openai":
		base := base_urls["openai"]
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return &openaiCompat{name: "openai", baseURL: base, apiKey: apiKey, client: client}, defaultOr(model, "gpt-5.2")
	case "openrouter":
		return &openaiCompat{name: "openrouter", baseURL: "https://openrouter.ai/api/v1", apiKey: apiKey, client: client,
			headers: map[string]string{"HTTP-Referer": "https://github.com/dxn111/EVOLV", "X-Title": "EVOLV"}}, defaultOr(model, "openrouter/free")
	case "groq":
		return &openaiCompat{name: "groq", baseURL: "https://api.groq.com/openai/v1", apiKey: apiKey, client: client}, defaultOr(model, "llama-3.3-70b-versatile")
	case "ollama":
		base := base_urls["ollama"]
		if base == "" {
			base = "http://127.0.0.1:11434/v1"
		}
		return &openaiCompat{name: "ollama", baseURL: base, client: client}, defaultOr(model, "llama3.2")
	case "custom":
		base := base_urls["custom"]
		if base == "" {
			base = "http://localhost:8000/v1"
		}
		return &openaiCompat{name: "custom", baseURL: base, apiKey: apiKey, client: client}, defaultOr(model, "default")
	case "anthropic":
		return &anthropic{apiKey: apiKey, client: client}, defaultOr(model, "claude-sonnet-4-6")
	case "mock":
		return &mock{}, "evolv-sim-1"
	default:
		return &openaiCompat{name: "kilo", baseURL: KiloBaseURL, apiKey: apiKey, client: client}, defaultOr(model, "kilo-auto/free")
	}
}

func defaultOr(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}
