package main

import (
	"encoding/json"
	"fmt"
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
			fmt.Fprint(w, `{"opencode":{"models":{"big-pickle":{"cost":{"input":0,"output":0}},"paid-model":{"cost":{"input":1,"output":1}}}}}`)
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
				{"id":"free/model:free","pricing":{"prompt":"0","completion":"0"}},
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
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data) != 2 || response.Data[0].ID != "opencode/big-pickle" || response.Data[1].ID != "openrouter/free/model:free" || response.Data[1].OwnedBy != "openrouter" {
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
