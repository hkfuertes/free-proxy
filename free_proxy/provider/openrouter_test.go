package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterRejectsModelWhenItsPriceChanges(t *testing.T) {
	catalogCalls := 0
	completionCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			catalogCalls++
			w.Header().Set("Content-Type", "application/json")
			if catalogCalls == 1 {
				fmt.Fprint(w, `{"data":[{"id":"free/model:free","pricing":{"prompt":"0","completion":"0"}}]}`)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"free/model:free","pricing":{"prompt":"0.01","completion":"0.01"}}]}`)
		case "/v1/chat/completions":
			completionCalls++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	provider, err := NewOpenRouter(OpenRouterConfig{APIKey: "token", BaseURL: upstream.URL + "/v1", Client: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), Request{Model: "free/model:free", Payload: map[string]json.RawMessage{}})
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}
	if completionCalls != 0 || catalogCalls != 2 {
		t.Fatalf("catalog calls=%d completion calls=%d", catalogCalls, completionCalls)
	}
}

func TestDiscoverFreeModelsHandlesPricingOverrides(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"free/with-overrides","pricing":{"prompt":"0","completion":"0","overrides":[{"min_prompt_tokens":42,"prompt":"0","completion":"0"}]}},
			{"id":"paid/with-overrides","pricing":{"prompt":"0","completion":"0","overrides":[{"utc_days":["saturday"],"prompt":"0.01","completion":"0"}]}}
		]}`)
	}))
	defer upstream.Close()

	models, err := discoverFreeModels(context.Background(), upstream.Client(), upstream.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "free/with-overrides" {
		t.Fatalf("models = %#v", models)
	}
}
