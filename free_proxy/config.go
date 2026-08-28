package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type addOnOptions struct {
	OpenRouterAPIKey string `json:"openrouter_api_key"`
	ProxyAPIKey      string `json:"proxy_api_key"`
	DefaultModel     string `json:"default_model"`
	ListenAddr       string `json:"listen_addr"`
}

func loadAddOnOptions() (addOnOptions, error) {
	path := envOr("OPTIONS_PATH", "/data/options.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return addOnOptions{}, nil
	}
	if err != nil {
		return addOnOptions{}, err
	}
	var options addOnOptions
	if err := json.Unmarshal(data, &options); err != nil {
		return addOnOptions{}, err
	}
	return options, nil
}

func envOrOption(name, option, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	if value := strings.TrimSpace(option); value != "" {
		return value
	}
	return fallback
}
