package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout      = 30 * time.Second
	defaultMaxBatchSize     = 64
	defaultMaxTextBytes     = int64(16 << 10)
	defaultMaxInputBytes    = int64(512 << 10)
	defaultMaxRequestBytes  = int64(1 << 20)
	defaultMaxResponseBytes = int64(64 << 20)
	defaultMaxDimensions    = 4_096
	maxProviderIDBytes      = 4 << 10
	maxCredentialBytes      = 16 << 10

	hardMaxBatchSize     = 64
	hardMaxTextBytes     = int64(16 << 10)
	hardMaxInputBytes    = int64(512 << 10)
	hardMaxRequestBytes  = int64(1 << 20)
	hardMaxResponseBytes = int64(64 << 20)
	hardMaxDimensions    = 4_096
)

var (
	// ErrBatchTooLarge reports a caller batch that exceeds MaxBatchSize.
	ErrBatchTooLarge = errors.New("semantic: embedding batch exceeds limit")
	// ErrRequestTooLarge reports a request that exceeds MaxRequestBytes.
	ErrRequestTooLarge = errors.New("semantic: embedding request exceeds body limit")
	// ErrResponseTooLarge reports a response that exceeds MaxResponseBytes.
	ErrResponseTooLarge = errors.New("semantic: embedding response exceeds body limit")
)

// HTTPStatusError reports only the response status. Provider response bodies
// are deliberately excluded because they may echo documents or credentials.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("semantic: embedding service returned HTTP %d", e.StatusCode)
}

// OpenAIConfig configures an OpenAI-compatible /v1/embeddings endpoint. All
// fields come from the caller; this package never reads environment variables
// or repository files. BaseURL may be an origin, a proxy prefix, a /v1 base,
// or the complete /v1/embeddings endpoint.
type OpenAIConfig struct {
	BaseURL  string
	Model    string
	Revision string
	APIKey   string

	// Dimensions, when non-zero, is sent to providers that support the
	// standard dimensions option and is enforced on every returned vector.
	Dimensions int
	Timeout    time.Duration

	MaxBatchSize     int
	MaxTextBytes     int64
	MaxInputBytes    int64
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxDimensions    int

	// Transport is intended for controlled network policy and tests. Redirect
	// policy and timeout remain owned by this package.
	Transport http.RoundTripper
}

// OpenAICompatible implements the OpenAI embeddings wire protocol. Local
// Ollama deployments can use the same client through Ollama's /v1 endpoint.
type OpenAICompatible struct {
	endpoint         string
	model            string
	apiKey           string
	dimensions       int
	maxBatchSize     int
	maxTextBytes     int64
	maxInputBytes    int64
	maxRequestBytes  int64
	maxResponseBytes int64
	maxDimensions    int
	client           *http.Client
	fingerprint      string
}

// NewOpenAICompatible validates all static security and resource boundaries.
func NewOpenAICompatible(config OpenAIConfig) (*OpenAICompatible, error) {
	endpoint, err := normalizeEmbeddingsEndpoint(config.BaseURL)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		return nil, fmt.Errorf("semantic: embedding model is required")
	}
	if len(model) > maxProviderIDBytes {
		return nil, fmt.Errorf("semantic: embedding model is too long")
	}
	if strings.ContainsAny(model, "\r\n") {
		return nil, fmt.Errorf("semantic: embedding model contains a line break")
	}
	revision := strings.TrimSpace(config.Revision)
	if len(revision) > maxProviderIDBytes {
		return nil, fmt.Errorf("semantic: embedding revision is too long")
	}
	if strings.ContainsAny(revision, "\r\n") {
		return nil, fmt.Errorf("semantic: embedding revision contains a line break")
	}
	if len(config.APIKey) > maxCredentialBytes || containsInvalidHeaderByte(config.APIKey) {
		// Never include the rejected value in the error: it is a credential.
		return nil, fmt.Errorf("semantic: embedding API key is not a valid header value")
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("semantic: embedding timeout must be positive")
	}
	if timeout > defaultHTTPTimeout {
		return nil, fmt.Errorf("semantic: embedding timeout must not exceed %s", defaultHTTPTimeout)
	}
	maxBatchSize := config.MaxBatchSize
	if maxBatchSize == 0 {
		maxBatchSize = defaultMaxBatchSize
	}
	if maxBatchSize < 1 || maxBatchSize > hardMaxBatchSize {
		return nil, fmt.Errorf("semantic: max batch size must be between 1 and %d", hardMaxBatchSize)
	}
	maxTextBytes := config.MaxTextBytes
	if maxTextBytes == 0 {
		maxTextBytes = defaultMaxTextBytes
	}
	if maxTextBytes < 1 || maxTextBytes > hardMaxTextBytes {
		return nil, fmt.Errorf("semantic: max text bytes must be between 1 and %d", hardMaxTextBytes)
	}
	maxInputBytes := config.MaxInputBytes
	if maxInputBytes == 0 {
		maxInputBytes = defaultMaxInputBytes
	}
	if maxInputBytes < 1 || maxInputBytes > hardMaxInputBytes {
		return nil, fmt.Errorf("semantic: max input bytes must be between 1 and %d", hardMaxInputBytes)
	}
	maxRequestBytes := config.MaxRequestBytes
	if maxRequestBytes == 0 {
		maxRequestBytes = defaultMaxRequestBytes
	}
	if maxRequestBytes < 1 || maxRequestBytes > hardMaxRequestBytes {
		return nil, fmt.Errorf("semantic: max request bytes must be between 1 and %d", hardMaxRequestBytes)
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	if maxResponseBytes < 1 || maxResponseBytes > hardMaxResponseBytes {
		return nil, fmt.Errorf("semantic: max response bytes must be between 1 and %d", hardMaxResponseBytes)
	}
	maxDimensions := config.MaxDimensions
	if maxDimensions == 0 {
		maxDimensions = defaultMaxDimensions
	}
	if maxDimensions < 1 || maxDimensions > hardMaxDimensions {
		return nil, fmt.Errorf("semantic: max dimensions must be between 1 and %d", hardMaxDimensions)
	}
	if config.Dimensions < 0 || config.Dimensions > maxDimensions {
		return nil, fmt.Errorf("semantic: requested dimensions must be between 0 and %d", maxDimensions)
	}

	client := &http.Client{
		Transport: config.Transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &OpenAICompatible{
		endpoint:         endpoint,
		model:            model,
		apiKey:           config.APIKey,
		dimensions:       config.Dimensions,
		maxBatchSize:     maxBatchSize,
		maxTextBytes:     maxTextBytes,
		maxInputBytes:    maxInputBytes,
		maxRequestBytes:  maxRequestBytes,
		maxResponseBytes: maxResponseBytes,
		maxDimensions:    maxDimensions,
		client:           client,
		fingerprint: fingerprint("openai-compatible-v1",
			providerMode(endpoint), endpoint, model, strconv.Itoa(config.Dimensions), revision,
			"dimensions-omitted-when-zero"),
	}, nil
}

func (c *OpenAICompatible) Fingerprint() string { return c.fingerprint }

func (c *OpenAICompatible) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := c.embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (c *OpenAICompatible) EmbedDocuments(ctx context.Context, documents []string) ([][]float32, error) {
	if len(documents) == 0 {
		if ctx == nil {
			return nil, fmt.Errorf("semantic: nil context")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return c.embed(ctx, documents)
}

type openAIEmbeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type openAIEmbeddingResponse struct {
	Data []openAIEmbeddingItem
}

type openAIEmbeddingItem struct {
	Index     *int
	Embedding []float64
}

func (c *OpenAICompatible) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if ctx == nil {
		return nil, fmt.Errorf("semantic: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > c.maxBatchSize {
		return nil, fmt.Errorf("%w: got %d, maximum %d", ErrBatchTooLarge, len(texts), c.maxBatchSize)
	}

	// Bound input before JSON expansion so Marshal cannot allocate from an
	// unbounded caller-controlled string. JSON escaping may expand bytes by up
	// to six times; the encoded-size check below remains authoritative.
	rawBytes := int64(0)
	for _, text := range texts {
		textBytes := int64(len(text))
		if textBytes > c.maxTextBytes || rawBytes > c.maxInputBytes-textBytes {
			return nil, ErrRequestTooLarge
		}
		rawBytes += textBytes
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	// HTML escaping is irrelevant for an application/json POST and can expand
	// common source text such as <T> or && by 6x. Keep only JSON-required
	// escaping; the encoded byte limit below remains authoritative.
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(openAIEmbeddingRequest{
		Model: c.model, Input: texts, Dimensions: c.dimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("semantic: encode embedding request: %w", err)
	}
	if int64(body.Len()) > c.maxRequestBytes {
		return nil, ErrRequestTooLarge
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("semantic: create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	response, err := c.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("semantic: embedding request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("semantic: read embedding response")
	}
	if int64(len(responseBody)) > c.maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	payload, err := decodeOpenAIEmbeddingResponse(responseBody, c.maxBatchSize, c.maxDimensions)
	if err != nil {
		// JSON decoder errors can include fragments of a provider response.
		return nil, fmt.Errorf("semantic: decode embedding response")
	}
	return c.validateResponse(payload, len(texts))
}

func decodeOpenAIEmbeddingResponse(body []byte, maxItems, maxDimensions int) (openAIEmbeddingResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := consumeJSONDelimiter(decoder, '{'); err != nil {
		return openAIEmbeddingResponse{}, err
	}
	var response openAIEmbeddingResponse
	dataSeen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return openAIEmbeddingResponse{}, err
		}
		name, ok := key.(string)
		if !ok {
			return openAIEmbeddingResponse{}, fmt.Errorf("semantic: embedding response has a non-string field name")
		}
		if name != "data" {
			if err := skipJSONValue(decoder); err != nil {
				return openAIEmbeddingResponse{}, err
			}
			continue
		}
		if dataSeen {
			return openAIEmbeddingResponse{}, fmt.Errorf("semantic: embedding response repeats data")
		}
		dataSeen = true
		items, err := decodeOpenAIEmbeddingItems(decoder, maxItems, maxDimensions)
		if err != nil {
			return openAIEmbeddingResponse{}, err
		}
		response.Data = items
	}
	if err := consumeJSONDelimiter(decoder, '}'); err != nil {
		return openAIEmbeddingResponse{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return openAIEmbeddingResponse{}, fmt.Errorf("semantic: embedding response has trailing data")
		}
		return openAIEmbeddingResponse{}, err
	}
	return response, nil
}

func decodeOpenAIEmbeddingItems(decoder *json.Decoder, maxItems, maxDimensions int) ([]openAIEmbeddingItem, error) {
	if err := consumeJSONDelimiter(decoder, '['); err != nil {
		return nil, err
	}
	items := make([]openAIEmbeddingItem, 0, min(maxItems, 8))
	for decoder.More() {
		if len(items) >= maxItems {
			return nil, fmt.Errorf("semantic: embedding response has too many items")
		}
		item, err := decodeOpenAIEmbeddingItem(decoder, maxDimensions)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := consumeJSONDelimiter(decoder, ']'); err != nil {
		return nil, err
	}
	return items, nil
}

func decodeOpenAIEmbeddingItem(decoder *json.Decoder, maxDimensions int) (openAIEmbeddingItem, error) {
	if err := consumeJSONDelimiter(decoder, '{'); err != nil {
		return openAIEmbeddingItem{}, err
	}
	var item openAIEmbeddingItem
	indexSeen := false
	embeddingSeen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return openAIEmbeddingItem{}, err
		}
		name, ok := key.(string)
		if !ok {
			return openAIEmbeddingItem{}, fmt.Errorf("semantic: embedding item has a non-string field name")
		}
		switch name {
		case "index":
			if indexSeen {
				return openAIEmbeddingItem{}, fmt.Errorf("semantic: embedding item repeats index")
			}
			indexSeen = true
			var index *int
			if err := decoder.Decode(&index); err != nil {
				return openAIEmbeddingItem{}, err
			}
			item.Index = index
		case "embedding":
			if embeddingSeen {
				return openAIEmbeddingItem{}, fmt.Errorf("semantic: embedding item repeats embedding")
			}
			embeddingSeen = true
			values, err := decodeEmbeddingValues(decoder, maxDimensions)
			if err != nil {
				return openAIEmbeddingItem{}, err
			}
			item.Embedding = values
		default:
			if err := skipJSONValue(decoder); err != nil {
				return openAIEmbeddingItem{}, err
			}
		}
	}
	if err := consumeJSONDelimiter(decoder, '}'); err != nil {
		return openAIEmbeddingItem{}, err
	}
	return item, nil
}

func decodeEmbeddingValues(decoder *json.Decoder, maxDimensions int) ([]float64, error) {
	if err := consumeJSONDelimiter(decoder, '['); err != nil {
		return nil, err
	}
	values := make([]float64, 0, min(maxDimensions, 256))
	for decoder.More() {
		if len(values) >= maxDimensions {
			return nil, fmt.Errorf("semantic: embedding response exceeds dimension limit")
		}
		var value *float64
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("semantic: embedding response contains a null value")
		}
		values = append(values, *value)
	}
	if err := consumeJSONDelimiter(decoder, ']'); err != nil {
		return nil, err
	}
	return values, nil
}

func consumeJSONDelimiter(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != want {
		return fmt.Errorf("semantic: embedding response has an unexpected JSON token")
	}
	return nil
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		return consumeJSONDelimiter(decoder, ']')
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		return consumeJSONDelimiter(decoder, '}')
	default:
		return fmt.Errorf("semantic: embedding response has an unexpected JSON delimiter")
	}
}

func (c *OpenAICompatible) validateResponse(payload openAIEmbeddingResponse, count int) ([][]float32, error) {
	if len(payload.Data) != count {
		return nil, fmt.Errorf("semantic: embedding response count is %d, expected %d", len(payload.Data), count)
	}
	vectors := make([][]float32, count)
	dimension := c.dimensions
	for position, item := range payload.Data {
		if item.Index == nil {
			return nil, fmt.Errorf("semantic: embedding response item %d has no index", position)
		}
		index := *item.Index
		if index < 0 || index >= count {
			return nil, fmt.Errorf("semantic: embedding response index %d is out of range", index)
		}
		if vectors[index] != nil {
			return nil, fmt.Errorf("semantic: embedding response index %d is duplicated", index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("semantic: embedding response index %d is empty", index)
		}
		if len(item.Embedding) > c.maxDimensions {
			return nil, fmt.Errorf("semantic: embedding response dimension %d exceeds maximum %d", len(item.Embedding), c.maxDimensions)
		}
		if dimension == 0 {
			dimension = len(item.Embedding)
		}
		if len(item.Embedding) != dimension {
			return nil, fmt.Errorf("semantic: embedding response index %d has dimension %d, expected %d", index, len(item.Embedding), dimension)
		}
		vector := make([]float32, dimension)
		nonZero := false
		for i, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) || value > math.MaxFloat32 || value < -math.MaxFloat32 {
				return nil, fmt.Errorf("semantic: embedding response index %d dimension %d is not a finite float32", index, i)
			}
			vector[i] = float32(value)
			nonZero = nonZero || vector[i] != 0
		}
		if !nonZero {
			return nil, fmt.Errorf("semantic: embedding response index %d is a zero vector", index)
		}
		vectors[index] = vector
	}
	return vectors, nil
}

func containsInvalidHeaderByte(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

func normalizeEmbeddingsEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("semantic: embedding base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("semantic: invalid embedding base URL")
	}
	if u.Opaque != "" || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("semantic: embedding base URL must be absolute")
	}
	if u.User != nil {
		return "", fmt.Errorf("semantic: embedding base URL must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("semantic: embedding base URL must not contain a query or fragment")
	}
	if u.RawPath != "" {
		return "", fmt.Errorf("semantic: embedding base URL must not contain an escaped path")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("semantic: embedding base URL scheme must be http or https")
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("semantic: embedding base URL has no hostname")
	}
	if u.Scheme == "http" && !isLoopbackHostname(hostname) {
		return "", fmt.Errorf("semantic: remote embedding endpoints require https")
	}
	if strings.Contains(hostname, "%") || strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("semantic: embedding base URL host is not canonical")
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		u.Host = "[" + hostname + "]"
	} else {
		u.Host = hostname
	}
	if strings.Contains(u.Path, "//") {
		return "", fmt.Errorf("semantic: embedding base URL path is not canonical")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("semantic: embedding base URL path is not canonical")
		}
	}

	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v1/embeddings"):
	case strings.HasSuffix(path, "/v1"):
		path += "/embeddings"
	default:
		path += "/v1/embeddings"
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func providerMode(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err == nil && isLoopbackHostname(strings.ToLower(u.Hostname())) {
		return "ollama"
	}
	return "remote"
}

func isLoopbackHostname(hostname string) bool {
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}
