package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

type OpenRouterConfig struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

type OpenRouter struct {
	endpoint   string
	catalogURL string
	apiKey     string
	models     []Model
	client     *http.Client
}

func NewOpenRouter(config OpenRouterConfig) (*OpenRouter, error) {
	if config.APIKey == "" {
		return nil, errors.New("OpenRouter API key is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultOpenRouterBaseURL
	}
	endpoint, err := completionEndpoint(config.BaseURL)
	if err != nil {
		return nil, err
	}
	catalogURL, err := modelsEndpoint(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if config.Client == nil {
		config.Client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := discoverFreeModels(ctx, config.Client, catalogURL, config.APIKey)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("OpenRouter returned no zero-price models")
	}
	return &OpenRouter{
		endpoint:   endpoint,
		catalogURL: catalogURL,
		apiKey:     config.APIKey,
		models:     models,
		client:     config.Client,
	}, nil
}

func (p *OpenRouter) ID() string { return "openrouter" }

func (p *OpenRouter) Models() []Model { return append([]Model(nil), p.models...) }

func (p *OpenRouter) Complete(ctx context.Context, request Request) (Response, error) {
	models, err := discoverFreeModels(ctx, p.client, p.catalogURL, p.apiKey)
	if err != nil {
		return Response{}, fmt.Errorf("check OpenRouter model pricing: %w", err)
	}
	if !containsModel(models, request.Model) {
		return Response{}, &RequestError{Message: "model is no longer free in OpenRouter", ModelUnavailable: true}
	}

	model, _ := json.Marshal(request.Model)
	stream, _ := json.Marshal(request.Stream)
	request.Payload["model"] = model
	request.Payload["stream"] = stream
	body, err := json.Marshal(request.Payload)
	if err != nil {
		return Response{}, err
	}

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+p.apiKey)
	if request.Stream {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}

	response, err := p.client.Do(upstream)
	if err != nil {
		return Response{}, err
	}
	return Response{HTTP: response, Stream: request.Stream}, nil
}

type openRouterCatalog struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID          string                     `json:"id"`
	Pricing     map[string]json.RawMessage `json:"pricing"`
	TopProvider struct {
		MaxTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Reasoning struct {
		Mandatory bool `json:"mandatory"`
	} `json:"reasoning"`
}

func discoverFreeModels(ctx context.Context, client *http.Client, endpoint, apiKey string) ([]Model, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenRouter model discovery returned HTTP %d", response.StatusCode)
	}

	var catalog openRouterCatalog
	if err := json.NewDecoder(io.LimitReader(response.Body, 5<<20)).Decode(&catalog); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(catalog.Data))
	for _, model := range catalog.Data {
		if model.ID != "" && freePricing(model.Pricing) {
			models = append(models, Model{ID: model.ID, MaxTokens: model.TopProvider.MaxTokens, ThinkingRequired: model.Reasoning.Mandatory})
		}
	}
	return models, nil
}

func freePricing(pricing map[string]json.RawMessage) bool {
	priceFields := 0
	for field, amount := range pricing {
		if field == "overrides" {
			if !freePricingOverrides(amount) {
				return false
			}
			continue
		}
		priceFields++
		if !freePrice(amount) {
			return false
		}
	}
	return priceFields > 0
}

func freePricingOverrides(raw json.RawMessage) bool {
	var overrides []map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &overrides) != nil {
		return false
	}
	for _, override := range overrides {
		for field, amount := range override {
			if overrideCondition(field) {
				continue
			}
			if !freePrice(amount) {
				return false
			}
		}
	}
	return true
}

func overrideCondition(field string) bool {
	switch field {
	case "utc_days", "utc_start", "utc_end", "min_prompt_tokens":
		return true
	default:
		return false
	}
}

func freePrice(raw json.RawMessage) bool {
	var amount string
	if json.Unmarshal(raw, &amount) != nil {
		return false
	}
	price, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	return ok && price.Sign() == 0
}
