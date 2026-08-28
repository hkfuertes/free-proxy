package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"free-proxy/provider"
)

const maxRequestBytes = 10 << 20

type app struct {
	models       map[string]resolvedModel
	modelIDs     []string
	defaultModel string
	clientKey    string
}

type resolvedModel struct {
	source provider.Provider
	id     string
}

func main() {
	server, listenAddr, err := appFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("listening on %s; default model %s", listenAddr, server.defaultModel)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func appFromEnv() (*app, string, error) {
	options, err := loadAddOnOptions()
	if err != nil {
		return nil, "", fmt.Errorf("read add-on options: %w", err)
	}
	openCode, err := provider.NewOpenCode(provider.OpenCodeConfig{
		APIKey:     envOr("OPENCODE_API_KEY", "public"),
		BaseURL:    envOr("OPENCODE_UPSTREAM", provider.DefaultOpenCodeBaseURL),
		CatalogURL: envOr("OPENCODE_CATALOG", provider.DefaultOpenCodeCatalogURL),
		UserAgent:  envOr("OPENCODE_USER_AGENT", provider.DefaultOpenCodeUserAgent),
	})
	if err != nil {
		return nil, "", fmt.Errorf("configure OpenCode: %w", err)
	}
	sources := []provider.Provider{openCode}

	if key := envOrOption("OPENROUTER_API_KEY", options.OpenRouterAPIKey, ""); key != "" {
		openRouter, err := provider.NewOpenRouter(provider.OpenRouterConfig{
			APIKey:  key,
			BaseURL: envOr("OPENROUTER_UPSTREAM", provider.DefaultOpenRouterBaseURL),
		})
		if err != nil {
			return nil, "", fmt.Errorf("configure OpenRouter: %w", err)
		}
		sources = append(sources, openRouter)
	}

	defaultOpenCodeModel := envOr("OPENCODE_DEFAULT_MODEL", provider.DefaultOpenCodeModel)
	defaultModel := envOrOption("DEFAULT_MODEL", options.DefaultModel, "opencode/"+defaultOpenCodeModel)
	server, err := newApp(sources, defaultModel, envOrOption("PROXY_API_KEY", options.ProxyAPIKey, ""))
	return server, envOrOption("LISTEN_ADDR", options.ListenAddr, "127.0.0.1:8080"), err
}

func newApp(sources []provider.Provider, defaultModel, clientKey string) (*app, error) {
	models := map[string]resolvedModel{}
	for _, source := range sources {
		if source == nil || source.ID() == "" {
			return nil, errors.New("provider has no ID")
		}
		for _, model := range source.Models() {
			if model.ID == "" {
				continue
			}
			publicID := source.ID() + "/" + model.ID
			if existing, exists := models[publicID]; exists {
				return nil, fmt.Errorf("model %q belongs to both %s and %s", publicID, existing.source.ID(), source.ID())
			}
			models[publicID] = resolvedModel{source: source, id: model.ID}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("no models configured")
	}
	if _, ok := models[defaultModel]; !ok {
		return nil, fmt.Errorf("default model %q is not configured", defaultModel)
	}
	modelIDs := make([]string, 0, len(models))
	for id := range models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	return &app{models: models, modelIDs: modelIDs, defaultModel: defaultModel, clientKey: clientKey}, nil
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !a.authorized(r) {
		writeAPIError(w, http.StatusUnauthorized, "invalid proxy API key", "authentication_error", "invalid_api_key")
		return
	}

	switch {
	case r.URL.Path == "/v1/models":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleModels(w)
	case strings.HasPrefix(r.URL.Path, "/v1/models/"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleModel(w, strings.TrimPrefix(r.URL.Path, "/v1/models/"))
	case r.URL.Path == "/v1/chat/completions":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		a.handleCompletion(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, "unknown endpoint", "invalid_request_error", "not_found")
	}
}

func (a *app) authorized(r *http.Request) bool {
	if a.clientKey == "" {
		return true
	}
	value := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return subtle.ConstantTimeCompare([]byte(value), []byte(a.clientKey)) == 1
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (a *app) handleModels(w http.ResponseWriter) {
	data := make([]modelInfo, 0, len(a.modelIDs))
	for _, id := range a.modelIDs {
		data = append(data, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: a.models[id].source.ID()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (a *app) handleModel(w http.ResponseWriter, id string) {
	model, ok := a.models[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "model not found", "invalid_request_error", "model_not_found")
		return
	}
	writeJSON(w, http.StatusOK, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: model.source.ID()})
}

func (a *app) handleCompletion(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error", "request_too_large")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "could not read request body", "invalid_request_error", "invalid_body")
		return
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		writeAPIError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "invalid_json")
		return
	}

	modelID := a.defaultModel
	if raw, ok := payload["model"]; ok {
		if err := json.Unmarshal(raw, &modelID); err != nil || modelID == "" {
			writeAPIError(w, http.StatusBadRequest, "model must be a non-empty string", "invalid_request_error", "invalid_model")
			return
		}
	}
	model, ok := a.models[modelID]
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "model is not configured", "invalid_request_error", "model_not_found")
		return
	}

	streamToClient := false
	if raw, ok := payload["stream"]; ok {
		if err := json.Unmarshal(raw, &streamToClient); err != nil {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean", "invalid_request_error", "invalid_stream")
			return
		}
	}
	response, err := model.source.Complete(r.Context(), provider.Request{
		Model:   model.id,
		Payload: payload,
		Stream:  streamToClient,
	})
	if err != nil {
		var requestErr *provider.RequestError
		if errors.As(err, &requestErr) {
			writeAPIError(w, http.StatusBadRequest, requestErr.Message, "invalid_request_error", "invalid_request")
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not reach "+model.source.ID(), "api_error", "upstream_unavailable")
		return
	}
	defer response.HTTP.Body.Close()
	if response.HTTP.StatusCode < http.StatusOK || response.HTTP.StatusCode >= http.StatusMultipleChoices {
		forwardResponse(w, response.HTTP)
		return
	}
	if !response.Stream || streamToClient {
		forwardResponse(w, response.HTTP)
		return
	}

	completion, err := aggregateStream(response.HTTP.Body, modelID)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "invalid streaming response from "+model.source.ID()+": "+err.Error(), "api_error", "invalid_upstream_response")
		return
	}
	writeJSON(w, http.StatusOK, completion)
}

type streamChunk struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []streamChoice  `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent *string          `json:"reasoning_content"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
}

type streamToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type completionMessage struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   json.RawMessage    `json:"usage,omitempty"`
}

type choiceState struct {
	role      string
	content   string
	reasoning string
	finish    string
	tools     map[int]*toolCall
	toolOrder []int
}

func aggregateStream(body io.Reader, fallbackModel string) (completionResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32*1024), 4<<20)
	states := map[int]*choiceState{}
	var id, model string
	var created int64
	var usage json.RawMessage
	seen := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return completionResponse{}, err
		}
		seen = true
		if id == "" {
			id = chunk.ID
		}
		if model == "" {
			model = chunk.Model
		}
		if created == 0 {
			created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = append(usage[:0], chunk.Usage...)
		}
		for _, choice := range chunk.Choices {
			state := states[choice.Index]
			if state == nil {
				state = &choiceState{tools: map[int]*toolCall{}}
				states[choice.Index] = state
			}
			if choice.Delta.Role != "" {
				state.role = choice.Delta.Role
			}
			if choice.Delta.Content != nil {
				state.content += *choice.Delta.Content
			}
			if choice.Delta.ReasoningContent != nil {
				state.reasoning += *choice.Delta.ReasoningContent
			}
			for _, incoming := range choice.Delta.ToolCalls {
				index := len(state.toolOrder)
				if incoming.Index != nil {
					index = *incoming.Index
				}
				tool := state.tools[index]
				if tool == nil {
					tool = &toolCall{}
					state.tools[index] = tool
					state.toolOrder = append(state.toolOrder, index)
				}
				if incoming.ID != "" {
					tool.ID = incoming.ID
				}
				if incoming.Type != "" {
					tool.Type = incoming.Type
				}
				if incoming.Function.Name != "" && tool.Function.Name == "" {
					tool.Function.Name = incoming.Function.Name
				}
				tool.Function.Arguments += incoming.Function.Arguments
			}
			if choice.FinishReason != nil {
				state.finish = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return completionResponse{}, err
	}
	if !seen {
		return completionResponse{}, errors.New("no SSE chunks received")
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if model == "" {
		model = fallbackModel
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	indexes := make([]int, 0, len(states))
	for index := range states {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	choices := make([]completionChoice, 0, len(indexes))
	for _, index := range indexes {
		state := states[index]
		message := completionMessage{Role: state.role, Content: state.content, ReasoningContent: state.reasoning}
		if message.Role == "" {
			message.Role = "assistant"
		}
		for _, toolIndex := range state.toolOrder {
			message.ToolCalls = append(message.ToolCalls, *state.tools[toolIndex])
		}
		if len(message.ToolCalls) > 0 && state.content == "" {
			message.Content = nil
		}
		finish := state.finish
		if finish == "" {
			finish = "stop"
		}
		choices = append(choices, completionChoice{Index: index, Message: message, FinishReason: finish})
	}
	return completionResponse{ID: id, Object: "chat.completion", Created: created, Model: model, Choices: choices, Usage: usage}, nil
}

func forwardResponse(w http.ResponseWriter, response *http.Response) {
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if err := copyStream(w, response.Body); err != nil {
		log.Printf("copying upstream response: %v", err)
	}
}

func copyStream(w http.ResponseWriter, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return err
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		if hopByHopHeaders[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func parseModels(value string) ([]string, error) {
	seen := map[string]bool{}
	var models []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		models = append(models, item)
	}
	if len(models) == 0 {
		return nil, errors.New("model list must contain at least one model")
	}
	return models, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
}

func writeAPIError(w http.ResponseWriter, status int, message, errorType, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": errorType, "code": code},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
