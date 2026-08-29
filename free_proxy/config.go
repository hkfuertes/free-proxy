package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type addOnOptions struct {
	OpenRouterAPIKey string `json:"openrouter_api_key"`
	ProxyAPIKey      string `json:"proxy_api_key"`
	DefaultModel     string `json:"default_model"`
	EnableThinking   bool   `json:"enable_thinking"`
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

func boolEnvOrOption(name string, option, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		if option {
			return true, nil
		}
		return fallback, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return enabled, nil
}
