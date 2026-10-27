// Package embed turns text into vectors, so the zvec actions can store and
// search content rather than hand-typed embeddings.
//
// zvec is an embedded database and embeds nothing itself: a vector field holds
// whatever numbers it is given. One embedding provider is configured per
// account on the plugin's settings profile and arrives folded into every call
// as body.settings, exactly like the data directory — the plugin stores
// nothing.
//
// Every provider here is a single HTTP round-trip that takes a batch of texts
// and returns one vector per text, in the same order.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Settings is a resolved embedding provider.
type Settings struct {
	// Provider is openai, gemini, cohere or custom. Empty means the account
	// configured none, and the actions then require a vector to be supplied.
	Provider string
	// Model is the model id; empty uses the provider's default below.
	Model string
	// APIKey is sent the way the provider expects — a bearer token, or a query
	// parameter for Gemini.
	APIKey string
	// BaseURL overrides the provider's host. It is required for "custom" (an
	// OpenAI-compatible endpoint such as Ollama, LM Studio or vLLM) and
	// optional elsewhere, for a gateway or a proxy.
	BaseURL string
}

// Purpose is how the text will be used, which some providers must be told: a
// stored document and a search query are embedded into the same space but with
// different instructions, and getting it backwards quietly costs recall.
type Purpose string

const (
	Document Purpose = "document"
	Query    Purpose = "query"
)

// Providers are the backends this package can call, as a form's drop-down
// lists them.
var Providers = []string{"none", "openai", "gemini", "cohere", "custom"}

// KnownModel is a well-known model and the vector size it produces, so a
// create-collection form can be given the right dimension. It is documentation
// rather than a gate: the true size is whatever the API returns, and an unknown
// model id is allowed.
type KnownModel struct {
	Model string
	Dim   int
}

// KnownModels lists the models each provider is usually used with.
var KnownModels = map[string][]KnownModel{
	"openai": {
		{"text-embedding-3-small", 1536},
		{"text-embedding-3-large", 3072},
		{"text-embedding-ada-002", 1536},
	},
	"gemini": {
		{"text-embedding-004", 768},
		{"gemini-embedding-001", 3072},
	},
	"cohere": {
		{"embed-english-v3.0", 1024},
		{"embed-multilingual-v3.0", 1024},
		{"embed-v4.0", 1536},
	},
}

var defaultModel = map[string]string{
	"openai": "text-embedding-3-small",
	"gemini": "text-embedding-004",
	"cohere": "embed-english-v3.0",
	"custom": "", // no sensible default — the user must name their own model
}

var defaultBase = map[string]string{
	"openai": "https://api.openai.com",
	"gemini": "https://generativelanguage.googleapis.com",
	"cohere": "https://api.cohere.com",
	"custom": "", // required from the user
}

// ErrNoProvider is returned when text needs embedding and no provider is
// configured. It is not an error to have no provider — only to need one.
var ErrNoProvider = fmt.Errorf("no embedding provider is configured: set one in the node's settings profile, or give a vector instead of text")

// Enabled reports whether a provider is configured at all.
func (s Settings) Enabled() bool {
	provider := strings.ToLower(strings.TrimSpace(s.Provider))
	return provider != "" && provider != "none"
}

// client is shared across calls so connections to a provider are reused. The
// timeout bounds one embedding call, well inside an action's own budget.
var client = &http.Client{Timeout: 60 * time.Second}

// Texts embeds a batch of texts, returning one vector per text in the same
// order.
func Texts(ctx context.Context, settings Settings, texts []string, purpose Purpose) ([][]float32, error) {
	if !settings.Enabled() {
		return nil, ErrNoProvider
	}
	if len(texts) == 0 {
		return nil, nil
	}

	provider := strings.ToLower(strings.TrimSpace(settings.Provider))
	model := strings.TrimSpace(settings.Model)
	if model == "" {
		model = defaultModel[provider]
	}
	base := strings.TrimSpace(settings.BaseURL)
	if base == "" {
		base = defaultBase[provider]
	}
	base = normaliseBase(base, provider)
	key := strings.TrimSpace(settings.APIKey)

	var (
		vectors [][]float32
		err     error
	)
	switch provider {
	case "openai", "custom":
		vectors, err = openAI(ctx, base, model, key, texts, provider)
	case "gemini":
		vectors, err = gemini(ctx, base, model, key, texts, purpose)
	case "cohere":
		vectors, err = cohere(ctx, base, model, key, texts, purpose)
	default:
		return nil, fmt.Errorf("%q is not an embedding provider: use one of %s", settings.Provider, strings.Join(Providers, ", "))
	}
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("%s returned %d vectors for %d texts", provider, len(vectors), len(texts))
	}
	return vectors, nil
}

// One embeds a single text, which is what a query needs.
func One(ctx context.Context, settings Settings, text string, purpose Purpose) ([]float32, error) {
	vectors, err := Texts(ctx, settings, []string{text}, purpose)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// openAI covers OpenAI and every OpenAI-compatible endpoint — Ollama, LM
// Studio, vLLM, gateways — which all share this shape: POST
// {base}/v1/embeddings with {model, input}, answering {data:[{index,
// embedding}]} that has to be put back in index order.
func openAI(ctx context.Context, base, model, key string, texts []string, provider string) ([][]float32, error) {
	if model == "" {
		return nil, fmt.Errorf("no embedding model is set: name the model in the node's settings profile")
	}
	if provider == "custom" && base == "" {
		return nil, fmt.Errorf("a custom embedding provider needs a base URL: set it in the node's settings profile, e.g. http://localhost:11434 for Ollama")
	}

	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	var answer struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	body := map[string]any{"model": model, "input": texts}
	if err := post(ctx, base+"/v1/embeddings", headers, body, provider, &answer); err != nil {
		return nil, err
	}
	if len(answer.Data) == 0 {
		return nil, fmt.Errorf("%s: the embeddings response carried no vectors", provider)
	}

	rows := answer.Data
	sort.Slice(rows, func(i, j int) bool { return rows[i].Index < rows[j].Index })
	vectors := make([][]float32, len(rows))
	for i, row := range rows {
		if err := checkVector(row.Embedding, provider, i); err != nil {
			return nil, err
		}
		vectors[i] = row.Embedding
	}
	return vectors, nil
}

// gemini uses the Generative Language API's batchEmbedContents, with the key in
// the query string and the task type steering document against query. Model
// ids may be written bare or as "models/…".
func gemini(ctx context.Context, base, model, key string, texts []string, purpose Purpose) ([][]float32, error) {
	if key == "" {
		return nil, fmt.Errorf("gemini needs an API key: set it in the node's settings profile")
	}
	name := model
	if !strings.HasPrefix(name, "models/") {
		name = "models/" + name
	}
	taskType := "RETRIEVAL_DOCUMENT"
	if purpose == Query {
		taskType = "RETRIEVAL_QUERY"
	}

	requests := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		requests = append(requests, map[string]any{
			"model":    name,
			"content":  map[string]any{"parts": []map[string]string{{"text": text}}},
			"taskType": taskType,
		})
	}

	var answer struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	url := fmt.Sprintf("%s/v1beta/%s:batchEmbedContents?key=%s", base, name, key)
	if err := post(ctx, url, nil, map[string]any{"requests": requests}, "gemini", &answer); err != nil {
		return nil, err
	}

	vectors := make([][]float32, len(answer.Embeddings))
	for i, embedding := range answer.Embeddings {
		if err := checkVector(embedding.Values, "gemini", i); err != nil {
			return nil, err
		}
		vectors[i] = embedding.Values
	}
	return vectors, nil
}

// cohere uses the v2 embed endpoint, which requires an input_type — the reason
// the purpose is threaded all the way through this package — and returns float
// embeddings only when they are asked for by name.
func cohere(ctx context.Context, base, model, key string, texts []string, purpose Purpose) ([][]float32, error) {
	if key == "" {
		return nil, fmt.Errorf("cohere needs an API key: set it in the node's settings profile")
	}
	inputType := "search_document"
	if purpose == Query {
		inputType = "search_query"
	}

	var answer struct {
		Embeddings struct {
			Float [][]float32 `json:"float"`
		} `json:"embeddings"`
	}
	body := map[string]any{
		"model":           model,
		"texts":           texts,
		"input_type":      inputType,
		"embedding_types": []string{"float"},
	}
	headers := map[string]string{"Authorization": "Bearer " + key}
	if err := post(ctx, base+"/v2/embed", headers, body, "cohere", &answer); err != nil {
		return nil, err
	}

	vectors := answer.Embeddings.Float
	for i, vector := range vectors {
		if err := checkVector(vector, "cohere", i); err != nil {
			return nil, err
		}
	}
	return vectors, nil
}

// post makes one embedding call and decodes the answer, turning a transport
// failure and an API-level error body alike into an error carrying the
// provider's own message — which is what tells a wrong key from a wrong model.
func post(ctx context.Context, url string, headers map[string]string, body any, who string, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: cannot encode the request: %w", who, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%s: %w", who, err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("cannot reach the %s embeddings endpoint: %w", who, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%s: cannot read the response: %w", who, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("%s %d: %s", who, response.StatusCode, providerError(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: unexpected embeddings response: %w", who, err)
	}
	return nil
}

// providerError digs the human-readable message out of whichever error envelope
// the provider used, falling back to the raw body.
func providerError(raw []byte) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		var text string
		if json.Unmarshal(envelope.Error, &text) == nil && text != "" {
			return text
		}
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
			return nested.Message
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 500)
}

func checkVector(vector []float32, who string, i int) error {
	if len(vector) == 0 {
		return fmt.Errorf("%s: no embedding came back for input %d", who, i)
	}
	return nil
}

// normaliseBase drops a trailing slash and, for the OpenAI-compatible shapes,
// a trailing "/v1" too — both spellings of the base URL are in circulation and
// appending "/v1/embeddings" to the second one would ask for "/v1/v1/…".
func normaliseBase(url, provider string) string {
	base := strings.TrimRight(strings.TrimSpace(url), "/")
	if (provider == "openai" || provider == "custom") && strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return base
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
