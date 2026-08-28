package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	DefaultOpenCodeBaseURL    = "https://opencode.ai/zen/v1"
	DefaultOpenCodeCatalogURL = "https://models.opencode.ai/api.json"
	DefaultOpenCodeUserAgent  = "opencode/1.18.16"
	DefaultOpenCodeModel      = "big-pickle"
)

type OpenCodeConfig struct {
	APIKey     string
	BaseURL    string
	CatalogURL string
	UserAgent  string
	Client     *http.Client
}

type OpenCode struct {
	endpoint   string
	catalogURL string
	apiKey     string
	userAgent  string
	models     []Model
	projectID  string
	client     *http.Client
}

func NewOpenCode(config OpenCodeConfig) (*OpenCode, error) {
	if config.BaseURL == "" {
		config.BaseURL = DefaultOpenCodeBaseURL
	}
	endpoint, err := completionEndpoint(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if config.CatalogURL == "" {
		config.CatalogURL = DefaultOpenCodeCatalogURL
	}
	if config.APIKey == "" {
		config.APIKey = "public"
	}
	if config.UserAgent == "" {
		config.UserAgent = DefaultOpenCodeUserAgent
	}
	if config.Client == nil {
		config.Client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := discoverFreeOpenCodeModels(ctx, config.Client, config.CatalogURL)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("OpenCode returned no zero-price models")
	}
	return &OpenCode{
		endpoint:   endpoint,
		catalogURL: config.CatalogURL,
		apiKey:     config.APIKey,
		userAgent:  config.UserAgent,
		models:     models,
		projectID:  newID("proj"),
		client:     config.Client,
	}, nil
}

func (p *OpenCode) ID() string { return "opencode" }

func (p *OpenCode) Models() []Model { return append([]Model(nil), p.models...) }

func (p *OpenCode) Complete(ctx context.Context, request Request) (Response, error) {
	models, err := discoverFreeOpenCodeModels(ctx, p.client, p.catalogURL)
	if err != nil {
		return Response{}, fmt.Errorf("check OpenCode model pricing: %w", err)
	}
	if !containsModel(models, request.Model) {
		return Response{}, &RequestError{Message: "model is no longer free in OpenCode"}
	}

	model, _ := json.Marshal(request.Model)
	request.Payload["model"] = model
	request.Payload["stream"] = json.RawMessage("true") // Anonymous OpenCode free requests are streamed upstream.
	if err := enableUsage(request.Payload); err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(request.Payload)
	if err != nil {
		return Response{}, err
	}

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")
	upstream.Header.Set("Authorization", "Bearer "+p.apiKey)
	upstream.Header.Set("User-Agent", p.userAgent)
	upstream.Header.Set("X-OpenCode-Project", p.projectID)
	upstream.Header.Set("X-OpenCode-Session", newID("ses"))
	upstream.Header.Set("X-OpenCode-Request", newID("msg"))
	upstream.Header.Set("X-OpenCode-Client", "cli")

	response, err := p.client.Do(upstream)
	if err != nil {
		return Response{}, err
	}
	return Response{HTTP: response, Stream: true}, nil
}

type openCodeCatalog struct {
	OpenCode struct {
		Models map[string]openCodeModel `json:"models"`
	} `json:"opencode"`
}

type openCodeModel struct {
	Cost   map[string]json.RawMessage `json:"cost"`
	Status string                     `json:"status"`
}

func discoverFreeOpenCodeModels(ctx context.Context, client *http.Client, endpoint string) ([]Model, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", DefaultOpenCodeUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenCode model discovery returned HTTP %d", response.StatusCode)
	}

	var catalog openCodeCatalog
	if err := json.NewDecoder(io.LimitReader(response.Body, 5<<20)).Decode(&catalog); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(catalog.OpenCode.Models))
	for id, model := range catalog.OpenCode.Models {
		if model.Status != "deprecated" && freeOpenCodePricing(model.Cost) {
			models = append(models, Model{ID: id})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func freeOpenCodePricing(cost map[string]json.RawMessage) bool {
	for _, key := range []string{"input", "output"} {
		value, ok := cost[key]
		if !ok || !zeroPrice(value) {
			return false
		}
	}
	return nestedPricesAreZero(cost)
}

func nestedPricesAreZero(values map[string]json.RawMessage) bool {
	for key, value := range values {
		if isPriceField(key) && !zeroPrice(value) {
			return false
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil && nested != nil && !nestedPricesAreZero(nested) {
			return false
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(value, &items) == nil {
			for _, item := range items {
				if !nestedPricesAreZero(item) {
					return false
				}
			}
		}
	}
	return true
}

func isPriceField(key string) bool {
	switch key {
	case "input", "output", "cache_read", "cache_write":
		return true
	default:
		return false
	}
}

func zeroPrice(raw json.RawMessage) bool {
	var price string
	if err := json.Unmarshal(raw, &price); err != nil {
		price = string(raw)
	}
	value, ok := new(big.Rat).SetString(strings.TrimSpace(price))
	return ok && value.Sign() == 0
}

func enableUsage(payload map[string]json.RawMessage) error {
	options := map[string]json.RawMessage{}
	if raw, ok := payload["stream_options"]; ok {
		if err := json.Unmarshal(raw, &options); err != nil || options == nil {
			return &RequestError{Message: "stream_options must be an object"}
		}
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return err
	}
	payload["stream_options"] = encoded
	return nil
}

func newID(prefix string) string {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err == nil {
		return prefix + "_" + hex.EncodeToString(bytes)
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
