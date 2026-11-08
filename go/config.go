package main

import (
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type config struct {
	// Site is the fallback portal for a login request that names no entry.
	Site string `yaml:"site"`
	// MaxConcurrency bounds simultaneous requests per credential.
	MaxConcurrency int `yaml:"max_concurrency"`
	// ClientVersion is the TRAE SOLO build the model gateway is told it serves.
	ClientVersion string `yaml:"client_version"`
	// MaxTokens is used when the client names no output budget.
	MaxTokens int `yaml:"max_tokens"`
}

var configMu sync.RWMutex
var currentConfig = defaults()

func defaults() config {
	return config{Site: siteTrae, MaxConcurrency: 3, ClientVersion: defaultClientVersion, MaxTokens: 32000}
}

func getConfig() config { configMu.RLock(); defer configMu.RUnlock(); return currentConfig }

func configure(raw []byte) error {
	c := defaults()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("invalid plugin configuration")
		}
	}
	c.Site = strings.ToLower(strings.TrimSpace(c.Site))
	if c.Site == "" {
		c.Site = siteTrae
	}
	if c.Site != siteTrae && c.Site != siteTraeCN {
		return fmt.Errorf("invalid trae plugin configuration: site must be trae or trae-cn")
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 100 {
		return fmt.Errorf("invalid trae plugin configuration: max_concurrency must be 1-100")
	}
	c.ClientVersion = strings.TrimSpace(c.ClientVersion)
	if c.ClientVersion == "" || len(c.ClientVersion) > 32 || strings.ContainsAny(c.ClientVersion, "\r\n\t ") {
		return fmt.Errorf("invalid trae plugin configuration: client_version must be a short version string")
	}
	if c.MaxTokens < 1 || c.MaxTokens > 1_000_000 {
		return fmt.Errorf("invalid trae plugin configuration: max_tokens must be 1-1000000")
	}
	configMu.Lock()
	currentConfig = c
	configMu.Unlock()
	return nil
}
