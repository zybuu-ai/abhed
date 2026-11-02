package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// OpenAIEmbedder calls an OpenAI-compatible /embeddings endpoint.
//
// The same on-prem serving stack that runs the reasoning model can serve
// embeddings (vLLM, TEI, Ollama and Infinity all expose this shape), so RAG
// adds no new external dependency to an air-gapped install.
type OpenAIEmbedder struct {
	BaseURL string
	APIKey  string
	Model   string
	Dims    int
	HTTP    *http.Client
}

func NewOpenAIEmbedder(baseURL, apiKey, model string, dims int) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Dims:    dims,
		HTTP:    &http.Client{Timeout: 2 * time.Minute, Transport: egress.UpstreamRoundTripper{}},
	}
}

func (e *OpenAIEmbedder) Dimensions() int { return e.Dims }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: e.Model, Input: texts})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call embeddings endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embeddings endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("embeddings error: %s", out.Error.Message)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(out.Data))
	}

	// Responses are not guaranteed to be in request order.
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("missing embedding for input %d", i)
		}
	}
	return vecs, nil
}
