package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free-proxy/provider"
)

func TestProvidersShareFreeModelListAndRouteRequests(t *testing.T) {
	openCodeCalls := 0
	openRouterCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"opencode":{"models":{"big-pickle":{"cost":{"input":0,"output":0},"limit":{"output":128}},"paid-model":{"cost":{"input":1,"output":1}}}}}`)
		case "/zen/v1/chat/completions":
			openCodeCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer public" {
				t.Errorf("OpenCode authorization = %q", got)
			}
			if got := r.Header.Get("User-Agent"); got != "opencode/test" {
				t.Errorf("OpenCode user-agent = %q", got)
			}
			for _, header := range []string{"X-OpenCode-Project", "X-OpenCode-Session", "X-OpenCode-Request", "X-OpenCode-Client"} {
				if r.Header.Get(header) == "" {
					t.Errorf("missing %s", header)
				}
			}
			var input map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			var model string
			var stream bool
			var options map[string]bool
			_ = json.Unmarshal(input["model"], &model)
			_ = json.Unmarshal(input["stream"], &stream)
			_ = json.Unmarshal(input["stream_options"], &options)
			if model != "big-pickle" || !stream || !options["include_usage"] {
				t.Errorf("unexpected OpenCode payload: model=%q stream=%v options=%v", model, stream, options)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "/router/v1/models":
			if got := r.Header.Get("Authorization"); got != "Bearer router-key" {
				t.Errorf("OpenRouter catalog authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[
				{"id":"free/model:free","pricing":{"prompt":"0","completion":"0"},"top_provider":{"max_completion_tokens":256}},
				{"id":"request-fee","pricing":{"prompt":"0","completion":"0","request":"0.01"}},
				{"id":"paid/model","pricing":{"prompt":"0.1","completion":"0.2"}}
			]}`)
		case "/router/v1/chat/completions":
			openRouterCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer router-key" {
				t.Errorf("OpenRouter authorization = %q", got)
			}
			var input map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			var model string
			var stream bool
			_ = json.Unmarshal(input["model"], &model)
			_ = json.Unmarshal(input["stream"], &stream)
			if model != "free/model:free" || stream {
				t.Errorf("unexpected OpenRouter payload: model=%q stream=%v", model, stream)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"router-1","object":"chat.completion","model":"free/model:free","choices":[{"index":0,"message":{"role":"assistant","content":"Router hello"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	openCode, err := provider.NewOpenCode(provider.OpenCodeConfig{
		APIKey:     "public",
		BaseURL:    upstream.URL + "/zen/v1",
		CatalogURL: upstream.URL + "/catalog",
		UserAgent:  "opencode/test",
		Client:     upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	openRouter, err := provider.NewOpenRouter(provider.OpenRouterConfig{
		APIKey:  "router-key",
		BaseURL: upstream.URL + "/router/v1",
		Client:  upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp([]provider.Provider{openCode, openRouter}, "opencode/big-pickle", "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("lists only free models from both providers", func(t *testing.T) {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var response struct {
			Data []struct {
				ID        string `json:"id"`
				OwnedBy   string `json:"owned_by"`
				MaxTokens int    `json:"max_tokens"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data) != 3 || response.Data[0].ID != defaultModelID || response.Data[0].OwnedBy != "free-proxy" || response.Data[1].ID != "opencode/big-pickle" || response.Data[1].MaxTokens != 128 || response.Data[2].ID != "openrouter/free/model:free" || response.Data[2].OwnedBy != "openrouter" || response.Data[2].MaxTokens != 256 {
			t.Fatalf("unexpected models: %s", rec.Body.String())
		}
	})

	t.Run("aggregates OpenCode SSE for a non-streaming client", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode/big-pickle","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hello world") {
			t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("forwards OpenRouter's normal OpenAI response", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openrouter/free/model:free","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Router hello") {
			t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects non-free OpenRouter models before sending", func(t *testing.T) {
		before := openRouterCalls
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openrouter/paid/model","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || openRouterCalls != before {
			t.Fatalf("status=%d OpenRouter calls=%d want=%d", rec.Code, openRouterCalls, before)
		}
	})

	if openCodeCalls != 1 || openRouterCalls != 1 {
		t.Fatalf("unexpected upstream calls: OpenCode=%d OpenRouter=%d", openCodeCalls, openRouterCalls)
	}
}

type testProvider struct {
	id       string
	models   []provider.Model
	complete func(provider.Request) (provider.Response, error)
}

func (p testProvider) ID() string               { return p.id }
func (p testProvider) Models() []provider.Model { return p.models }
func (p testProvider) Complete(_ context.Context, request provider.Request) (provider.Response, error) {
	return p.complete(request)
}

func testResponse(status int, body string) provider.Response {
	return provider.Response{HTTP: &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
}

func TestDefaultModelFallsBack(t *testing.T) {
	for _, test := range []struct {
		name  string
		first func() (provider.Response, error)
	}{
		{
			name: "rate limit",
			first: func() (provider.Response, error) {
				return testResponse(http.StatusTooManyRequests, `{"error":"rate limited"}`), nil
			},
		},
		{
			name: "model is no longer free",
			first: func() (provider.Response, error) {
				return provider.Response{}, &provider.RequestError{Message: "model is no longer free", ModelUnavailable: true}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			first := testProvider{
				id:     "first",
				models: []provider.Model{{ID: "best"}},
				complete: func(request provider.Request) (provider.Response, error) {
					calls = append(calls, "first/"+request.Model)
					return test.first()
				},
			}
			second := testProvider{
				id:     "second",
				models: []provider.Model{{ID: "fallback"}},
				complete: func(request provider.Request) (provider.Response, error) {
					calls = append(calls, "second/"+request.Model)
					return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"fallback"}}]}`), nil
				},
			}
			app, err := newApp([]provider.Provider{first, second}, defaultModelID, "")
			if err != nil {
				t.Fatal(err)
			}

			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fallback") {
				t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
			}
			if got := strings.Join(calls, ","); got != "first/best,second/fallback" {
				t.Fatalf("calls = %q", got)
			}
		})
	}
}

func TestRerankSavesAndUsesRanking(t *testing.T) {
	calls := 0
	ranker := testProvider{
		id:     "ranker",
		models: []provider.Model{{ID: "best", MaxTokens: 256}, {ID: "fallback", MaxTokens: 128}},
		complete: func(request provider.Request) (provider.Response, error) {
			calls++
			var messages []struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(request.Payload["messages"], &messages); err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content, "ranker/best") || !strings.Contains(messages[0].Content, "ranker/fallback") {
				t.Errorf("unexpected ranking prompt: %s", request.Payload["messages"])
			}
			return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"[\"ranker/fallback\",\"ranker/best\"]"}}]}`), nil
		},
	}
	app, err := newApp([]provider.Provider{ranker}, defaultModelID, "key")
	if err != nil {
		t.Fatal(err)
	}
	app.rankingPath = t.TempDir() + "/ranking.json"
	if err := app.loadOrCreateRanking(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || app.defaultCandidateIDs()[0] != "ranker/fallback" {
		t.Fatalf("initial calls=%d ranking=%v", calls, app.defaultCandidateIDs())
	}

	ui := httptest.NewRecorder()
	uiRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	uiRequest.RemoteAddr = "172.30.32.2:1234"
	app.ServeHTTP(ui, uiRequest)
	if ui.Code != http.StatusOK || !strings.Contains(ui.Body.String(), "Rerank free models") || !strings.Contains(ui.Body.String(), "/v1/models") || !strings.Contains(ui.Body.String(), "@tailwindcss/browser@4") {
		t.Fatalf("unexpected ingress UI: %d %s", ui.Code, ui.Body.String())
	}
	models := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.RemoteAddr = "172.30.32.2:1234"
	app.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK {
		t.Fatalf("unexpected ingress models response: %d %s", models.Code, models.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rerank", nil)
	req.Header.Set("Authorization", "Bearer key")
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected rerank response: %d %s", rec.Code, rec.Body.String())
	}
	var ranking savedRanking
	if err := json.Unmarshal(rec.Body.Bytes(), &ranking); err != nil || ranking.RankedBy != "ranker/fallback" || ranking.MaxTokens["ranker/fallback"] != 128 || ranking.MaxTokens["ranker/best"] != 256 {
		t.Fatalf("unexpected rerank result: %s", rec.Body.String())
	}
	if calls != 2 || app.defaultCandidateIDs()[0] != "ranker/fallback" {
		t.Fatalf("calls=%d ranking=%v", calls, app.defaultCandidateIDs())
	}

	reloadedSource := testProvider{id: "ranker", models: []provider.Model{{ID: "best"}, {ID: "fallback"}}}
	reloaded, err := newApp([]provider.Provider{reloadedSource}, defaultModelID, "key")
	if err != nil {
		t.Fatal(err)
	}
	reloaded.rankingPath = app.rankingPath
	if err := reloaded.loadRanking(); err != nil {
		t.Fatal(err)
	}
	if reloaded.defaultCandidateIDs()[0] != "ranker/fallback" || reloaded.models["ranker/fallback"].maxTokens != 128 || reloaded.models["ranker/best"].maxTokens != 256 {
		t.Fatalf("reloaded ranking=%v limits=%v", reloaded.defaultCandidateIDs(), reloaded.rankingMaxTokens(reloaded.concreteModelIDs()))
	}
}

func TestDefaultModelClampsMaxTokens(t *testing.T) {
	limited := testProvider{
		id:     "limited",
		models: []provider.Model{{ID: "small", MaxTokens: 128}},
		complete: func(request provider.Request) (provider.Response, error) {
			for field, want := range map[string]int{"max_tokens": 128, "max_completion_tokens": 128} {
				var got int
				if err := json.Unmarshal(request.Payload[field], &got); err != nil || got != want {
					t.Errorf("%s = %s, want %d", field, request.Payload[field], want)
				}
			}
			return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`), nil
		},
	}
	app, err := newApp([]provider.Provider{limited}, defaultModelID, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"default","max_tokens":150,"max_completion_tokens":160,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestThinkingPolicyDisablesAndSkipsRequiredModels(t *testing.T) {
	var calls []provider.Request
	source := testProvider{
		id: "test",
		models: []provider.Model{
			{ID: "required", ThinkingRequired: true},
			{ID: "allowed"},
		},
		complete: func(request provider.Request) (provider.Response, error) {
			calls = append(calls, request)
			return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`), nil
		},
	}
	app, err := newApp([]provider.Provider{source}, defaultModelID, "")
	if err != nil {
		t.Fatal(err)
	}
	app.defaultCandidates = []string{"test/required", "test/allowed"}

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"default","include_reasoning":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK || len(calls) != 1 || calls[0].Model != "allowed" {
		t.Fatalf("default response=%d calls=%#v", rec.Code, calls)
	}
	var reasoning map[string]bool
	if err := json.Unmarshal(calls[0].Payload["reasoning"], &reasoning); err != nil {
		t.Fatalf("thinking payload=%v", calls[0].Payload)
	}
	enabled, ok := reasoning["enabled"]
	if !ok || enabled || calls[0].Payload["include_reasoning"] != nil || calls[0].Payload["reasoning_effort"] != nil {
		t.Fatalf("thinking payload=%v", calls[0].Payload)
	}

	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/required","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest || len(calls) != 1 {
		t.Fatalf("required response=%d calls=%#v", rec.Code, calls)
	}

	app.thinking = true
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/required","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK || len(calls) != 2 || calls[1].Payload["reasoning"] != nil {
		t.Fatalf("enabled response=%d calls=%#v", rec.Code, calls)
	}
}
