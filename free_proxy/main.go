package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"free-proxy/provider"
)

const (
	maxRequestBytes = 10 << 20
	defaultModelID  = "default"
)

var defaultModelRanking = []string{
	"openrouter/z-ai/glm-5.2:free",
	"openrouter/minimax/minimax-m3:free",
	"openrouter/thinkingmachines/inkling:free",
	"openrouter/thinkingmachines/inkling-small:free",
	"openrouter/minimax/minimax-m2.7:free",
	"openrouter/nvidia/nemotron-3-ultra-550b-a55b:free",
	"opencode/nemotron-3-ultra-free",
	"opencode/muse-spark-1.2-contributor-free",
	"opencode/hy3-free",
	"opencode/big-pickle",
	"openrouter/google/gemma-4-31b-it:free",
	"openrouter/google/gemma-4-26b-a4b-it:free",
	"openrouter/openrouter/free",
}

type app struct {
	models            map[string]resolvedModel
	modelIDs          []string
	defaultModel      string
	defaultCandidates []string
	rankingPath       string
	clientKey         string
	thinking          bool
	rankingMu         sync.RWMutex
	rerankMu          sync.Mutex
}

type resolvedModel struct {
	source           provider.Provider
	id               string
	maxTokens        int
	thinkingRequired bool
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
	log.Printf("listening on %s; default model %s; thinking enabled=%t", listenAddr, server.defaultModel, server.thinking)
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

	fallbackModel := defaultModelID
	if model := strings.TrimSpace(os.Getenv("OPENCODE_DEFAULT_MODEL")); model != "" {
		fallbackModel = "opencode/" + model
	}
	defaultModel := envOrOption("DEFAULT_MODEL", options.DefaultModel, fallbackModel)
	thinking, err := boolEnvOrOption("ENABLE_THINKING", options.EnableThinking, false)
	if err != nil {
		return nil, "", fmt.Errorf("configure thinking: %w", err)
	}
	server, err := newApp(sources, defaultModel, envOrOption("PROXY_API_KEY", options.ProxyAPIKey, ""))
	if err != nil {
		return nil, "", err
	}
	server.thinking = thinking
	for _, source := range sources {
		log.Printf("provider %s: %d free models", source.ID(), len(source.Models()))
	}
	log.Printf("configured %d free models", len(server.models))
	server.rankingPath = envOr("RANKING_PATH", "/data/free-proxy-ranking.json")
	if err := server.loadOrCreateRanking(context.Background()); err != nil {
		log.Printf("prepare ranking: %v", err)
	}
	return server, envOrOption("LISTEN_ADDR", options.ListenAddr, "127.0.0.1:8080"), nil
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
			models[publicID] = resolvedModel{source: source, id: model.ID, maxTokens: model.MaxTokens, thinkingRequired: model.ThinkingRequired}
		}
	}
	if len(models) == 0 {
		return nil, errors.New("no models configured")
	}
	if defaultModel != defaultModelID {
		if _, ok := models[defaultModel]; !ok {
			return nil, fmt.Errorf("default model %q is not configured", defaultModel)
		}
	}
	modelIDs := make([]string, 0, len(models)+1)
	for id := range models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	candidates := rankedDefaultCandidates(models, modelIDs)
	modelIDs = append([]string{defaultModelID}, modelIDs...)
	return &app{models: models, modelIDs: modelIDs, defaultModel: defaultModel, defaultCandidates: candidates, clientKey: clientKey}, nil
}

func rankedDefaultCandidates(models map[string]resolvedModel, modelIDs []string) []string {
	return mergeRanking(defaultModelRanking, modelIDs, models)
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
	if !a.authorized(r) && !isIngressRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, "invalid proxy API key", "authentication_error", "invalid_api_key")
		return
	}

	switch {
	case r.URL.Path == "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleUI(w, r)
	case r.URL.Path == "/rerank":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		a.handleRerank(w, r)
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

func isIngressRequest(r *http.Request) bool {
	switch r.URL.Path {
	case "/", "/rerank", "/v1/models":
	default:
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && host == "172.30.32.2"
}

func (a *app) handleUI(w http.ResponseWriter, r *http.Request) {
	base, _ := json.Marshal(strings.TrimRight(r.Header.Get("X-Ingress-Path"), "/"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// ponytail: Tailwind's browser CDN avoids a frontend build; bundle generated CSS if ingress blocks the CDN.
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Free Proxy</title><script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script></head><body class="min-h-screen bg-slate-950 text-slate-100"><main class="mx-auto max-w-5xl p-6 sm:p-10"><header class="flex flex-col gap-5 border-b border-slate-800 pb-8 sm:flex-row sm:items-center sm:justify-between"><div><p class="text-sm font-medium text-cyan-300">Home Assistant add-on</p><h1 class="mt-1 text-3xl font-bold tracking-tight">Free Proxy</h1><p class="mt-2 text-slate-400">Free OpenCode and OpenRouter models, routed through one OpenAI-compatible API.</p></div><button id="rerank" class="rounded-lg bg-cyan-400 px-4 py-2.5 font-semibold text-slate-950 shadow-sm transition hover:bg-cyan-300 disabled:cursor-wait disabled:opacity-60">Rerank free models</button></header><p id="status" class="mt-4 min-h-5 text-sm" aria-live="polite"></p><section class="mt-8 rounded-xl border border-slate-800 bg-slate-900/60 p-5 shadow-2xl shadow-slate-950/30"><div class="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between"><h2 class="text-xl font-semibold">Available models</h2><p class="text-sm text-slate-400">Copy an ID, then paste it into <code class="rounded bg-slate-800 px-1 py-0.5 text-slate-200">default_model</code> in add-on configuration.</p></div><div id="models" class="mt-5 grid gap-3 sm:grid-cols-2"><p class="text-slate-400">Loading models...</p></div></section></main><script>
const base=%s, button=document.querySelector("#rerank"), status=document.querySelector("#status"), models=document.querySelector("#models");
function setStatus(message,error){status.textContent=message;status.className="mt-4 min-h-5 text-sm "+(error?"text-rose-300":"text-emerald-300")}
async function copyModel(id){try{await navigator.clipboard.writeText(id);setStatus("Copied "+id+". Set it as default_model in add-on configuration.")}catch(error){window.prompt("Copy model ID",id)}}
function renderModels(data){models.replaceChildren();for(const model of data){const card=document.createElement("article"),details=document.createElement("div"),id=document.createElement("p"),meta=document.createElement("p"),copy=document.createElement("button");card.className="flex items-center justify-between gap-4 rounded-lg border border-slate-800 bg-slate-950/60 p-4";details.className="min-w-0";id.className="truncate font-mono text-sm text-slate-100";id.textContent=model.id;meta.className="mt-1 text-xs text-slate-400";meta.textContent=model.id==="default"?"Ranked free-model route":model.owned_by+(model.max_tokens?" / max "+model.max_tokens+" tokens":"");copy.className="shrink-0 rounded-md border border-slate-700 px-3 py-1.5 text-sm font-medium text-cyan-300 transition hover:border-cyan-400 hover:text-cyan-200";copy.textContent="Copy";copy.addEventListener("click",()=>copyModel(model.id));details.append(id,meta);card.append(details,copy);models.append(card)}}
async function loadModels(){const response=await fetch(base+"/v1/models"),result=await response.json();if(!response.ok)throw new Error(result.error?.message||response.status);renderModels(result.data)}
button.addEventListener("click",async()=>{button.disabled=true;setStatus("Ranking...");try{const response=await fetch(base+"/rerank",{method:"POST"}),result=await response.json();if(!response.ok)throw new Error(result.error?.message||response.status);setStatus("Saved "+result.models.length+" models, ranked by "+result.ranked_by+".");await loadModels()}catch(error){setStatus("Error: "+error.message,true)}finally{button.disabled=false}});
loadModels().catch(error=>setStatus("Error loading models: "+error.message,true));
</script></body></html>`, base)
}

func (a *app) handleRerank(w http.ResponseWriter, r *http.Request) {
	ranking, err := a.rerank(r.Context())
	if err != nil {
		log.Printf("rerank: failed: %v", err)
		writeAPIError(w, http.StatusBadGateway, "could not rerank free models: "+err.Error(), "api_error", "rerank_failed")
		return
	}
	writeJSON(w, http.StatusOK, ranking)
}

type modelInfo struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Created   int64  `json:"created"`
	OwnedBy   string `json:"owned_by"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

func (a *app) handleModels(w http.ResponseWriter) {
	data := make([]modelInfo, 0, len(a.modelIDs))
	for _, id := range a.modelIDs {
		if id == defaultModelID {
			data = append(data, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: "free-proxy"})
			continue
		}
		model := a.models[id]
		data = append(data, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: model.source.ID(), MaxTokens: model.maxTokens})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (a *app) handleModel(w http.ResponseWriter, id string) {
	if id == defaultModelID {
		writeJSON(w, http.StatusOK, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: "free-proxy"})
		return
	}
	model, ok := a.models[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "model not found", "invalid_request_error", "model_not_found")
		return
	}
	writeJSON(w, http.StatusOK, modelInfo{ID: id, Object: "model", Created: 0, OwnedBy: model.source.ID(), MaxTokens: model.maxTokens})
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
	if modelID != defaultModelID {
		model, ok := a.models[modelID]
		if !ok {
			writeAPIError(w, http.StatusBadRequest, "model is not configured", "invalid_request_error", "model_not_found")
			return
		}
		if !a.thinking && model.thinkingRequired {
			writeAPIError(w, http.StatusBadRequest, "model requires thinking; enable thinking or use default", "invalid_request_error", "thinking_required")
			return
		}
	}

	streamToClient := false
	if raw, ok := payload["stream"]; ok {
		if err := json.Unmarshal(raw, &streamToClient); err != nil {
			writeAPIError(w, http.StatusBadRequest, "stream must be a boolean", "invalid_request_error", "invalid_stream")
			return
		}
	}

	var response provider.Response
	sourceID := modelID
	if modelID == defaultModelID {
		response, sourceID, err = a.completeDefault(r.Context(), payload, streamToClient)
	} else {
		model := a.models[modelID]
		attempt := copyPayload(payload)
		configureThinking(attempt, a.thinking)
		adjustments := clampMaxTokens(attempt, model.maxTokens)
		log.Printf("completion: trying model=%s stream=%t thinking=%t max_tokens=%d adjustments=%q", modelID, streamToClient, a.thinking, model.maxTokens, strings.Join(adjustments, ","))
		response, err = model.source.Complete(r.Context(), provider.Request{Model: model.id, Payload: attempt, Stream: streamToClient})
	}
	if err != nil {
		log.Printf("completion: model=%s failed: %v", sourceID, err)
		var requestErr *provider.RequestError
		if errors.As(err, &requestErr) {
			writeAPIError(w, http.StatusBadRequest, requestErr.Message, "invalid_request_error", "invalid_request")
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not reach "+sourceID, "api_error", "upstream_unavailable")
		return
	}
	defer response.HTTP.Body.Close()
	log.Printf("completion: model=%s status=%d upstream_stream=%t", sourceID, response.HTTP.StatusCode, response.Stream)
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
		log.Printf("completion: model=%s invalid stream: %v", sourceID, err)
		writeAPIError(w, http.StatusBadGateway, "invalid streaming response from "+sourceID+": "+err.Error(), "api_error", "invalid_upstream_response")
		return
	}
	writeJSON(w, http.StatusOK, completion)
}

func (a *app) completeDefault(ctx context.Context, payload map[string]json.RawMessage, stream bool) (provider.Response, string, error) {
	var lastResponse provider.Response
	var lastModel string
	var lastErr error
	for _, id := range a.defaultCandidateIDs() {
		model := a.models[id]
		attempt := copyPayload(payload)
		configureThinking(attempt, a.thinking)
		adjustments := clampMaxTokens(attempt, model.maxTokens)
		log.Printf("completion: trying model=%s stream=%t thinking=%t max_tokens=%d adjustments=%q", id, stream, a.thinking, model.maxTokens, strings.Join(adjustments, ","))
		response, err := model.source.Complete(ctx, provider.Request{Model: model.id, Payload: attempt, Stream: stream})
		if err != nil {
			log.Printf("completion: model=%s failed: %v", id, err)
			var requestErr *provider.RequestError
			if errors.As(err, &requestErr) && !requestErr.ModelUnavailable {
				closeResponse(lastResponse)
				return provider.Response{}, id, err
			}
			lastErr = err
			lastModel = id
			continue
		}
		if retryableStatus(response.HTTP.StatusCode) {
			log.Printf("completion: model=%s status=%d; trying fallback", id, response.HTTP.StatusCode)
			closeResponse(lastResponse)
			lastResponse = response
			lastModel = id
			continue
		}
		closeResponse(lastResponse)
		return response, id, nil
	}
	if lastResponse.HTTP != nil {
		return lastResponse, lastModel, nil
	}
	if lastErr != nil {
		return provider.Response{}, lastModel, lastErr
	}
	return provider.Response{}, defaultModelID, errors.New("no default model available")
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func closeResponse(response provider.Response) {
	if response.HTTP != nil && response.HTTP.Body != nil {
		response.HTTP.Body.Close()
	}
}

func copyPayload(payload map[string]json.RawMessage) map[string]json.RawMessage {
	copy := make(map[string]json.RawMessage, len(payload))
	for key, value := range payload {
		copy[key] = value
	}
	return copy
}

func configureThinking(payload map[string]json.RawMessage, enabled bool) {
	if enabled {
		return
	}
	delete(payload, "include_reasoning")
	delete(payload, "reasoning_effort")
	payload["reasoning"] = json.RawMessage(`{"enabled":false}`)
}

func clampMaxTokens(payload map[string]json.RawMessage, maximum int) []string {
	if maximum <= 0 {
		return nil
	}
	var adjustments []string
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := payload[field]
		if !ok {
			continue
		}
		var requested int
		if err := json.Unmarshal(raw, &requested); err != nil || requested <= maximum {
			continue
		}
		payload[field] = json.RawMessage(strconv.Itoa(maximum))
		adjustments = append(adjustments, fmt.Sprintf("%s=%d->%d", field, requested, maximum))
	}
	return adjustments
}

type savedRanking struct {
	Models    []string       `json:"models"`
	MaxTokens map[string]int `json:"max_tokens,omitempty"`
	RankedBy  string         `json:"ranked_by"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (a *app) rerank(ctx context.Context) (savedRanking, error) {
	a.rerankMu.Lock()
	defer a.rerankMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ids := a.concreteModelIDs()
	log.Printf("rerank: requesting rank for %d free models", len(ids))
	messages, err := json.Marshal([]map[string]string{{"role": "user", "content": rankingPrompt(ids)}})
	if err != nil {
		return savedRanking{}, err
	}
	response, sourceID, err := a.completeDefault(ctx, map[string]json.RawMessage{
		"messages":    messages,
		"max_tokens":  json.RawMessage("2048"),
		"temperature": json.RawMessage("0"),
	}, false)
	if err != nil {
		return savedRanking{}, err
	}
	defer response.HTTP.Body.Close()
	if response.HTTP.StatusCode < http.StatusOK || response.HTTP.StatusCode >= http.StatusMultipleChoices {
		return savedRanking{}, fmt.Errorf("ranking model returned HTTP %d", response.HTTP.StatusCode)
	}
	content, err := rankingContent(response)
	if err != nil {
		return savedRanking{}, err
	}
	models, err := parseRanking(content, a.models, rankedDefaultCandidates(a.models, ids))
	if err != nil {
		return savedRanking{}, err
	}
	ranking := savedRanking{Models: models, MaxTokens: a.rankingMaxTokens(models), RankedBy: sourceID, UpdatedAt: time.Now().UTC()}
	if err := saveRanking(a.rankingPath, ranking); err != nil {
		return savedRanking{}, err
	}
	a.setDefaultCandidates(models)
	log.Printf("rerank: saved %d models ranked by %s", len(models), sourceID)
	return ranking, nil
}

func rankingPrompt(ids []string) string {
	encoded, _ := json.Marshal(ids)
	return "Rank these currently free chat models for general reasoning and coding. Return only one JSON array containing every exact ID once, best first: " + string(encoded)
}

func rankingContent(response provider.Response) (string, error) {
	if response.Stream {
		completion, err := aggregateStream(response.HTTP.Body, defaultModelID)
		if err != nil {
			return "", err
		}
		if len(completion.Choices) == 0 {
			return "", errors.New("ranking model returned no choices")
		}
		content, ok := completion.Choices[0].Message.Content.(string)
		if !ok || content == "" {
			return "", errors.New("ranking model returned no text")
		}
		return content, nil
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(response.HTTP.Body).Decode(&completion); err != nil {
		return "", err
	}
	if len(completion.Choices) == 0 || completion.Choices[0].Message.Content == "" {
		return "", errors.New("ranking model returned no text")
	}
	return completion.Choices[0].Message.Content, nil
}

func parseRanking(content string, models map[string]resolvedModel, fallback []string) ([]string, error) {
	var ranked []string
	content = strings.TrimSpace(content)
	if err := json.Unmarshal([]byte(content), &ranked); err != nil {
		start, end := strings.Index(content, "["), strings.LastIndex(content, "]")
		if start < 0 || end <= start || json.Unmarshal([]byte(content[start:end+1]), &ranked) != nil {
			return nil, errors.New("ranking model did not return a JSON array")
		}
	}
	known := 0
	for _, id := range ranked {
		if _, ok := models[id]; ok {
			known++
		}
	}
	if known == 0 {
		return nil, errors.New("ranking model returned no known models")
	}
	return mergeRanking(ranked, fallback, models), nil
}

func mergeRanking(ranked, fallback []string, models map[string]resolvedModel) []string {
	result := make([]string, 0, len(models))
	seen := map[string]bool{}
	add := func(id string) {
		if _, ok := models[id]; ok && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	for _, id := range ranked {
		add(id)
	}
	for _, id := range fallback {
		add(id)
	}
	return result
}

func (a *app) concreteModelIDs() []string {
	ids := make([]string, 0, len(a.models))
	for id := range a.models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (a *app) rankingMaxTokens(ids []string) map[string]int {
	limits := make(map[string]int, len(ids))
	for _, id := range ids {
		if maximum := a.models[id].maxTokens; maximum > 0 {
			limits[id] = maximum
		}
	}
	return limits
}

func (a *app) applySavedMaxTokens(limits map[string]int) {
	for id, maximum := range limits {
		model, ok := a.models[id]
		if !ok || maximum <= 0 || model.maxTokens > 0 {
			continue
		}
		model.maxTokens = maximum
		a.models[id] = model
	}
}

func (a *app) defaultCandidateIDs() []string {
	a.rankingMu.RLock()
	candidates := append([]string(nil), a.defaultCandidates...)
	a.rankingMu.RUnlock()
	if a.thinking {
		return candidates
	}
	available := candidates[:0]
	for _, id := range candidates {
		if !a.models[id].thinkingRequired {
			available = append(available, id)
		}
	}
	return available
}

func (a *app) setDefaultCandidates(ranked []string) {
	candidates := mergeRanking(ranked, rankedDefaultCandidates(a.models, a.concreteModelIDs()), a.models)
	a.rankingMu.Lock()
	a.defaultCandidates = candidates
	a.rankingMu.Unlock()
}

func (a *app) loadRanking() error {
	data, err := os.ReadFile(a.rankingPath)
	if err != nil {
		return err
	}
	var ranking savedRanking
	if err := json.Unmarshal(data, &ranking); err != nil {
		return err
	}
	if len(ranking.Models) == 0 {
		return errors.New("saved ranking has no models")
	}
	a.applySavedMaxTokens(ranking.MaxTokens)
	a.setDefaultCandidates(ranking.Models)
	log.Printf("ranking: loaded %d models with %d token limits", len(ranking.Models), len(ranking.MaxTokens))
	return nil
}

func (a *app) loadOrCreateRanking(ctx context.Context) error {
	if err := a.loadRanking(); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log.Printf("ranking: no saved ranking; generating one")
	_, err := a.rerank(ctx)
	return err
}

func saveRanking(path string, ranking savedRanking) error {
	data, err := json.MarshalIndent(ranking, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
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
