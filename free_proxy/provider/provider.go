package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Model is an upstream model ID exposed unchanged by the proxy.
type Model struct {
	ID               string
	MaxTokens        int
	ThinkingRequired bool
}

// Request is an OpenAI chat-completion payload destined for one provider.
type Request struct {
	Model   string
	Payload map[string]json.RawMessage
	Stream  bool
}

// Response wraps the upstream HTTP response and whether its body is SSE.
type Response struct {
	HTTP   *http.Response
	Stream bool
}

// RequestError describes a rejected request or an unavailable free model.
type RequestError struct {
	Message          string
	ModelUnavailable bool
}

func (e *RequestError) Error() string { return e.Message }

// Provider owns its credentials and upstream request format.
type Provider interface {
	ID() string
	Models() []Model
	Complete(context.Context, Request) (Response, error)
}

func containsModel(models []Model, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

func completionEndpoint(baseURL string) (string, error) {
	return endpoint(baseURL, "chat/completions")
}

func modelsEndpoint(baseURL string) (string, error) {
	return endpoint(baseURL, "models")
}

func endpoint(baseURL, resource string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid upstream URL %q", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + resource
	parsed.RawQuery = ""
	return parsed.String(), nil
}
