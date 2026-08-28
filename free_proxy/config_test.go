package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddOnOptionsAndEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(path, []byte(`{"openrouter_api_key":"from-addon","proxy_api_key":"addon-proxy","default_model":"opencode/big-pickle","listen_addr":"0.0.0.0:8080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPTIONS_PATH", path)
	t.Setenv("OPENROUTER_API_KEY", "from-env")
	options, err := loadAddOnOptions()
	if err != nil {
		t.Fatal(err)
	}
	if got := envOrOption("OPENROUTER_API_KEY", options.OpenRouterAPIKey, ""); got != "from-env" {
		t.Fatalf("OpenRouter key = %q", got)
	}
	if got := envOrOption("PROXY_API_KEY", options.ProxyAPIKey, ""); got != "addon-proxy" {
		t.Fatalf("proxy key = %q", got)
	}
}
