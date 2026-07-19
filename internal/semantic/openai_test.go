package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var _ Embedder = (*OpenAICompatible)(nil)

func TestOpenAICompatibleEmbedsAndReordersBatch(t *testing.T) {
	type requestBody struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer caller-key" {
			http.Error(w, "wrong authorization", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			http.Error(w, "wrong content type", http.StatusBadRequest)
			return
		}
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if body.Model != "embed-model" || body.Dimensions != 3 {
			http.Error(w, "wrong model options", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			index := len(body.Input) - i - 1
			data[i] = map[string]any{
				"index": index, "embedding": []float64{float64(index), 2, 3},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client, err := NewOpenAICompatible(OpenAIConfig{
		BaseURL: server.URL + "/v1/", Model: "embed-model", Revision: "r1",
		APIKey: "caller-key", Dimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedDocuments(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]float32{{0, 2, 3}, {1, 2, 3}}
	if !reflect.DeepEqual(vectors, want) {
		t.Fatalf("vectors = %v, want %v", vectors, want)
	}
	query, err := client.EmbedQuery(context.Background(), "query")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(query, []float32{0, 2, 3}) {
		t.Fatalf("query = %v", query)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d", got)
	}
}

func TestOpenAICompatibleEmptyBatchDoesNotCallProvider(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedDocuments(context.Background(), nil)
	if err != nil || vectors != nil || requests.Load() != 0 {
		t.Fatalf("EmbedDocuments = %v, %v; requests=%d", vectors, err, requests.Load())
	}
}

func TestOpenAICompatibleURLPolicy(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		wantURL   string
		wantError bool
	}{
		{name: "remote https", baseURL: "https://EXAMPLE.com", wantURL: "https://example.com/v1/embeddings"},
		{name: "default https port", baseURL: "https://example.com:443/v1", wantURL: "https://example.com/v1/embeddings"},
		{name: "default http port", baseURL: "http://localhost:80", wantURL: "http://localhost/v1/embeddings"},
		{name: "loopback ipv4 http", baseURL: "http://127.0.0.1:11434", wantURL: "http://127.0.0.1:11434/v1/embeddings"},
		{name: "loopback ipv6 http", baseURL: "http://[::1]:11434/v1", wantURL: "http://[::1]:11434/v1/embeddings"},
		{name: "localhost http", baseURL: "http://localhost:11434/v1/embeddings/", wantURL: "http://localhost:11434/v1/embeddings"},
		{name: "proxy prefix", baseURL: "https://example.com/openai", wantURL: "https://example.com/openai/v1/embeddings"},
		{name: "missing", wantError: true},
		{name: "relative", baseURL: "/v1", wantError: true},
		{name: "remote http", baseURL: "http://example.com", wantError: true},
		{name: "localhost suffix", baseURL: "http://localhost.example.com", wantError: true},
		{name: "userinfo", baseURL: "https://user:pass@example.com", wantError: true},
		{name: "query", baseURL: "https://example.com?v=1", wantError: true},
		{name: "fragment", baseURL: "https://example.com/#x", wantError: true},
		{name: "other scheme", baseURL: "file:///tmp/socket", wantError: true},
		{name: "escaped path", baseURL: "https://example.com/a%2Fb", wantError: true},
		{name: "dot segment", baseURL: "https://example.com/a/../b", wantError: true},
		{name: "double slash", baseURL: "https://example.com/a//b", wantError: true},
		{name: "empty port", baseURL: "https://example.com:", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: test.baseURL, Model: "m"})
			if test.wantError {
				if err == nil {
					t.Fatalf("NewOpenAICompatible succeeded: endpoint=%q", client.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if client.endpoint != test.wantURL {
				t.Fatalf("endpoint = %q, want %q", client.endpoint, test.wantURL)
			}
		})
	}
}

func TestOpenAICompatibleFingerprintIsStableAndCredentialFree(t *testing.T) {
	base := OpenAIConfig{BaseURL: "https://example.com/v1/", Model: "m", Revision: "r", APIKey: "first", Dimensions: 8}
	first, err := NewOpenAICompatible(base)
	if err != nil {
		t.Fatal(err)
	}
	base.APIKey = "second"
	second, _ := NewOpenAICompatible(base)
	base.Revision = "r2"
	different, _ := NewOpenAICompatible(base)
	base.Revision = "r"
	base.Dimensions = 4
	differentDimensions, _ := NewOpenAICompatible(base)
	equivalent, _ := NewOpenAICompatible(OpenAIConfig{
		BaseURL: "https://EXAMPLE.com:443/v1/embeddings/", Model: "m", Revision: "r", APIKey: "third", Dimensions: 8,
	})
	if first.Fingerprint() == "" || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("credential changed fingerprint: %q %q", first.Fingerprint(), second.Fingerprint())
	}
	if first.Fingerprint() != equivalent.Fingerprint() {
		t.Fatalf("equivalent endpoints changed fingerprint: %q %q", first.Fingerprint(), equivalent.Fingerprint())
	}
	if strings.Contains(first.Fingerprint(), "first") || first.Fingerprint() == different.Fingerprint() || first.Fingerprint() == differentDimensions.Fingerprint() {
		t.Fatalf("bad fingerprint separation: %q %q", first.Fingerprint(), different.Fingerprint())
	}
}

func TestOpenAICompatibleDoesNotReadAmbientCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			http.Error(w, "unexpected authorization", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedQuery(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAICompatibleRejectsRedirect(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stolen" {
			redirected.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/stolen", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.EmbedQuery(context.Background(), "private document")
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("client followed redirect")
	}
}

func TestOpenAICompatibleSanitizesHTTPErrorBody(t *testing.T) {
	const leaked = "private-document-and-api-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(leaked))
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", APIKey: leaked})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.EmbedQuery(context.Background(), leaked)
	if err == nil || strings.Contains(err.Error(), leaked) {
		t.Fatalf("unsafe error = %q", err)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("status error = %v", err)
	}
}

func TestOpenAICompatibleResourceLimits(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m", MaxBatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedDocuments(context.Background(), []string{"a", "b"})
		if !errors.Is(err, ErrBatchTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("request body", func(t *testing.T) {
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m", MaxRequestBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedQuery(context.Background(), strings.Repeat("x", 65))
		if !errors.Is(err, ErrRequestTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("encoded request body", func(t *testing.T) {
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m", MaxRequestBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedQuery(context.Background(), strings.Repeat("\x00", 16))
		if !errors.Is(err, ErrRequestTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("single text", func(t *testing.T) {
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m", MaxTextBytes: 4})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedQuery(context.Background(), "12345")
		if !errors.Is(err, ErrRequestTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("batch input", func(t *testing.T) {
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m", MaxInputBytes: 5})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedDocuments(context.Background(), []string{"123", "456"})
		if !errors.Is(err, ErrRequestTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("response body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0,1,2]}],"padding":"xxxxxxxxxxxxxxxxxxxxxxxx"}`))
		}))
		defer server.Close()
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", MaxResponseBytes: 32})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.EmbedQuery(context.Background(), "x")
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("dimensions", func(t *testing.T) {
		server := embeddingResponseServer(t, `{"data":[{"index":0,"embedding":[0,1,2]}]}`)
		defer server.Close()
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", MaxDimensions: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.EmbedQuery(context.Background(), "x"); err == nil {
			t.Fatal("oversized dimensions succeeded")
		}
	})
	t.Run("configured dimensions", func(t *testing.T) {
		server := embeddingResponseServer(t, `{"data":[{"index":0,"embedding":[0,1,2]}]}`)
		defer server.Close()
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", Dimensions: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.EmbedQuery(context.Background(), "x"); err == nil {
			t.Fatal("configured dimension mismatch succeeded")
		}
	})
}

func TestOpenAICompatibleRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		input []string
	}{
		{name: "invalid json", body: `{`, input: []string{"a"}},
		{name: "count", body: `{"data":[{"index":0,"embedding":[1]}]}`, input: []string{"a", "b"}},
		{name: "missing index", body: `{"data":[{"embedding":[1]}]}`, input: []string{"a"}},
		{name: "null index", body: `{"data":[{"index":null,"embedding":[1]}]}`, input: []string{"a"}},
		{name: "duplicate index", body: `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`, input: []string{"a", "b"}},
		{name: "high index", body: `{"data":[{"index":1,"embedding":[1]}]}`, input: []string{"a"}},
		{name: "negative index", body: `{"data":[{"index":-1,"embedding":[1]}]}`, input: []string{"a"}},
		{name: "empty", body: `{"data":[{"index":0,"embedding":[]}]}`, input: []string{"a"}},
		{name: "zero vector", body: `{"data":[{"index":0,"embedding":[0,-0]}]}`, input: []string{"a"}},
		{name: "null vector value", body: `{"data":[{"index":0,"embedding":[1,null]}]}`, input: []string{"a"}},
		{name: "dimension mismatch", body: `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3]}]}`, input: []string{"a", "b"}},
		{name: "float32 overflow", body: `{"data":[{"index":0,"embedding":[3.5e38]}]}`, input: []string{"a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := embeddingResponseServer(t, test.body)
			defer server.Close()
			client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.EmbedDocuments(context.Background(), test.input); err == nil {
				t.Fatal("malformed response succeeded")
			}
		})
	}
}

func TestOpenAICompatibleRejectsNonFiniteVector(t *testing.T) {
	client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		index := 0
		var payload openAIEmbeddingResponse
		payload.Data = append(payload.Data, openAIEmbeddingItem{Index: &index, Embedding: []float64{value}})
		if _, err := client.validateResponse(payload, 1); err == nil {
			t.Fatalf("value %v succeeded", value)
		}
	}
}

func TestOpenAICompatibleHonorsContextAndTimeout(t *testing.T) {
	t.Run("canceled before request", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		defer server.Close()
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := client.EmbedQuery(ctx, "x"); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		if requests.Load() != 0 {
			t.Fatalf("requests = %d", requests.Load())
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.Close()
		}()
		client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m", Timeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = client.EmbedQuery(context.Background(), "x")
		if err == nil || time.Since(started) > time.Second {
			t.Fatalf("timeout error = %v after %v", err, time.Since(started))
		}
	})
	if client, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.com", Model: "m"}); err != nil {
		t.Fatal(err)
	} else if _, err := client.EmbedQuery(nil, "x"); err == nil {
		t.Fatal("nil context succeeded")
	}
}

func TestOpenAICompatibleRejectsInvalidConfiguration(t *testing.T) {
	secret := "sensitive\ncredential"
	tests := []OpenAIConfig{
		{BaseURL: "https://example.com", Model: ""},
		{BaseURL: "https://example.com", Model: "m\nheader"},
		{BaseURL: "https://example.com", Model: strings.Repeat("m", maxProviderIDBytes+1)},
		{BaseURL: "https://example.com", Model: "m", Revision: "r\nheader"},
		{BaseURL: "https://example.com", Model: "m", Revision: strings.Repeat("r", maxProviderIDBytes+1)},
		{BaseURL: "https://example.com", Model: "m", APIKey: secret},
		{BaseURL: "https://example.com", Model: "m", APIKey: "secret\x01value"},
		{BaseURL: "https://example.com", Model: "m", APIKey: strings.Repeat("k", maxCredentialBytes+1)},
		{BaseURL: "https://example.com", Model: "m", Timeout: -1},
		{BaseURL: "https://example.com", Model: "m", Timeout: defaultHTTPTimeout + time.Second},
		{BaseURL: "https://example.com", Model: "m", MaxBatchSize: hardMaxBatchSize + 1},
		{BaseURL: "https://example.com", Model: "m", MaxTextBytes: hardMaxTextBytes + 1},
		{BaseURL: "https://example.com", Model: "m", MaxInputBytes: hardMaxInputBytes + 1},
		{BaseURL: "https://example.com", Model: "m", MaxRequestBytes: hardMaxRequestBytes + 1},
		{BaseURL: "https://example.com", Model: "m", MaxResponseBytes: hardMaxResponseBytes + 1},
		{BaseURL: "https://example.com", Model: "m", MaxDimensions: hardMaxDimensions + 1},
		{BaseURL: "https://example.com", Model: "m", Dimensions: defaultMaxDimensions + 1},
	}
	for i, config := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := NewOpenAICompatible(config); err == nil {
				t.Fatalf("configuration succeeded: %+v", config)
			} else if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("configuration error leaked API key: %q", err)
			}
		})
	}
}

func embeddingResponseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}
