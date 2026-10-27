package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve stands in for a provider, recording what it was asked for so the shape
// of each request can be checked without calling anyone's API.
func serve(t *testing.T, status int, body string) (*httptest.Server, *http.Request, *map[string]any) {
	t.Helper()
	var (
		gotRequest http.Request
		gotBody    map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = *r
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &gotRequest, &gotBody
}

func TestOpenAIEmbedsABatchAndKeepsItsOrder(t *testing.T) {
	// Answered out of order on purpose: the API indexes its rows, and the
	// vectors have to come back matching the texts that were sent.
	server, request, body := serve(t, 200, `{"data":[
		{"index":1,"embedding":[0.3,0.4]},
		{"index":0,"embedding":[0.1,0.2]}
	]}`)

	vectors, err := Texts(context.Background(), Settings{
		Provider: "openai", Model: "text-embedding-3-small", APIKey: "sk-test", BaseURL: server.URL,
	}, []string{"first", "second"}, Document)
	if err != nil {
		t.Fatal(err)
	}

	if len(vectors) != 2 || vectors[0][0] != 0.1 || vectors[1][0] != 0.3 {
		t.Fatalf("vectors = %v, want them in the order the texts were sent", vectors)
	}
	if request.URL.Path != "/v1/embeddings" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", got)
	}
	if (*body)["model"] != "text-embedding-3-small" {
		t.Fatalf("model = %v", (*body)["model"])
	}
	// One request for the whole batch, not one per text.
	if input, ok := (*body)["input"].([]any); !ok || len(input) != 2 {
		t.Fatalf("input = %v, want both texts in one call", (*body)["input"])
	}
}

// Both spellings of an OpenAI-compatible base URL are in circulation, and
// appending /v1/embeddings to the second would ask for /v1/v1/embeddings.
func TestOpenAIBaseURLAcceptsEitherSpelling(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/"} {
		server, request, _ := serve(t, 200, `{"data":[{"index":0,"embedding":[1]}]}`)
		_, err := Texts(context.Background(), Settings{
			Provider: "custom", Model: "nomic-embed-text", BaseURL: server.URL + suffix,
		}, []string{"x"}, Query)
		if err != nil {
			t.Fatalf("base %q: %v", suffix, err)
		}
		if request.URL.Path != "/v1/embeddings" {
			t.Fatalf("base %q produced path %q", suffix, request.URL.Path)
		}
	}
}

func TestCustomProviderNeedsABaseURLAndAModel(t *testing.T) {
	_, err := Texts(context.Background(), Settings{Provider: "custom", Model: "m"}, []string{"x"}, Query)
	if err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("err = %v, want it to ask for a base URL", err)
	}
	server, _, _ := serve(t, 200, `{"data":[]}`)
	_, err = Texts(context.Background(), Settings{Provider: "custom", BaseURL: server.URL}, []string{"x"}, Query)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v, want it to ask for a model", err)
	}
}

// Document and query embeddings land in the same space only if each is sent
// with the instruction it belongs to, so the purpose has to reach the wire.
func TestCohereSendsTheInputTypeForThePurpose(t *testing.T) {
	cases := map[Purpose]string{Document: "search_document", Query: "search_query"}
	for purpose, want := range cases {
		server, request, body := serve(t, 200, `{"embeddings":{"float":[[0.5]]}}`)
		if _, err := Texts(context.Background(), Settings{
			Provider: "cohere", APIKey: "co-test", BaseURL: server.URL,
		}, []string{"x"}, purpose); err != nil {
			t.Fatal(err)
		}
		if (*body)["input_type"] != want {
			t.Errorf("purpose %q sent input_type %v, want %q", purpose, (*body)["input_type"], want)
		}
		if (*body)["model"] != "embed-english-v3.0" {
			t.Errorf("model = %v, want the provider default", (*body)["model"])
		}
		if request.URL.Path != "/v2/embed" {
			t.Errorf("path = %q", request.URL.Path)
		}
	}
}

func TestGeminiNormalisesTheModelNameAndSendsTheTaskType(t *testing.T) {
	for _, model := range []string{"text-embedding-004", "models/text-embedding-004"} {
		server, request, body := serve(t, 200, `{"embeddings":[{"values":[0.1,0.2]}]}`)
		if _, err := Texts(context.Background(), Settings{
			Provider: "gemini", Model: model, APIKey: "g-test", BaseURL: server.URL,
		}, []string{"x"}, Query); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(request.URL.Path, "models/text-embedding-004") {
			t.Errorf("model %q produced path %q", model, request.URL.Path)
		}
		if request.URL.Query().Get("key") != "g-test" {
			t.Errorf("the key did not reach the query string: %q", request.URL.RawQuery)
		}
		requests, _ := (*body)["requests"].([]any)
		if len(requests) != 1 {
			t.Fatalf("requests = %v", (*body)["requests"])
		}
		first, _ := requests[0].(map[string]any)
		if first["taskType"] != "RETRIEVAL_QUERY" {
			t.Errorf("taskType = %v, want RETRIEVAL_QUERY for a query", first["taskType"])
		}
	}
}

func TestGeminiAndCohereNeedTheirKeys(t *testing.T) {
	for _, provider := range []string{"gemini", "cohere"} {
		_, err := Texts(context.Background(), Settings{Provider: provider, Model: "m"}, []string{"x"}, Query)
		if err == nil || !strings.Contains(err.Error(), "API key") {
			t.Errorf("%s: err = %v, want it to ask for an API key", provider, err)
		}
	}
}

// A wrong key and a wrong model are both an HTTP error, and only the
// provider's own message tells them apart.
func TestAProviderErrorCarriesItsMessage(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"invalid api key"}}`: "invalid api key",
		`{"error":"model not found"}`:             "model not found",
		`{"message":"rate limited"}`:              "rate limited",
		`not json at all`:                         "not json at all",
	}
	for body, want := range cases {
		server, _, _ := serve(t, 401, body)
		_, err := Texts(context.Background(), Settings{
			Provider: "openai", Model: "m", APIKey: "k", BaseURL: server.URL,
		}, []string{"x"}, Query)
		if err == nil {
			t.Fatalf("body %q produced no error", body)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("body %q gave %q, want it to carry %q", body, err, want)
		}
	}
}

func TestAShortAnswerIsRefused(t *testing.T) {
	// Two texts, one vector back: silently pairing them up would attach the
	// wrong vector to a document.
	server, _, _ := serve(t, 200, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	_, err := Texts(context.Background(), Settings{
		Provider: "openai", Model: "m", APIKey: "k", BaseURL: server.URL,
	}, []string{"one", "two"}, Document)
	if err == nil || !strings.Contains(err.Error(), "1 vectors for 2 texts") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoProviderIsReportedAsSuch(t *testing.T) {
	for _, provider := range []string{"", "none", "None"} {
		settings := Settings{Provider: provider}
		if settings.Enabled() {
			t.Errorf("provider %q reads as enabled", provider)
		}
		if _, err := Texts(context.Background(), settings, []string{"x"}, Query); !errors.Is(err, ErrNoProvider) {
			t.Errorf("provider %q gave %v, want ErrNoProvider", provider, err)
		}
	}
}

func TestAnUnknownProviderNamesTheOnesThatWork(t *testing.T) {
	_, err := Texts(context.Background(), Settings{Provider: "mystery"}, []string{"x"}, Query)
	if err == nil || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("err = %v, want it to list the providers", err)
	}
}

// The known-model table is documentation the forms quote, so it should at
// least be internally consistent.
func TestKnownModelsAreUsable(t *testing.T) {
	for provider, models := range KnownModels {
		if len(models) == 0 {
			t.Errorf("%s lists no models", provider)
		}
		for _, model := range models {
			if model.Model == "" || model.Dim <= 0 {
				t.Errorf("%s has a malformed entry: %+v", provider, model)
			}
		}
	}
}
