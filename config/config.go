package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	ProviderTypeOpenAI    = "openai"
	ProviderTypeAnthropic = "anthropic"

	DefaultMaxTokens     = 32000
	MaxAllowedTokens     = 4096
	DefaultMaxIterations = 10
	configDirName        = ".tachi"
	configFileName       = "config.yaml"
)

type ProviderConfig struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type WebSearchConfig struct {
	Type string `yaml:"type"` // brave, serper, serpapi
	Key  string `yaml:"key"`
}

type Config struct {
	Provider       string           `yaml:"provider"`
	MaxTokens      int              `yaml:"max_tokens"`
	MaxIterations  int              `yaml:"max_iterations"`
	ThinkingBudget int64            `yaml:"thinking_budget"`
	Providers      []ProviderConfig `yaml:"providers"`
	WebSearch      WebSearchConfig  `yaml:"web_search"`
}

func DefaultConfig() *Config {
	return &Config{
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: DefaultMaxIterations,
		WebSearch: WebSearchConfig{
			Type: "brave",
		},
	}
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDirName), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	// Set default web search type if not specified
	if cfg.WebSearch.Type == "" {
		cfg.WebSearch.Type = "brave"
	}
	return cfg, nil
}

func Save(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, configFileName)
	return os.WriteFile(path, data, 0600)
}

func Init() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}

	dir, err := configDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	cfg := &Config{
		Provider:      "minimax-anthropic",
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: DefaultMaxIterations,
		Providers: []ProviderConfig{
			{
				Name:    "minimax-anthropic",
				Type:    ProviderTypeAnthropic,
				Model:   "MiniMax-M2.7",
				BaseURL: "https://api.minimaxi.com/anthropic",
				APIKey:  "<your-api-key>",
			},
			{
				Name:    "minimax-openai",
				Type:    ProviderTypeOpenAI,
				Model:   "MiniMax-M2.7",
				BaseURL: "https://api.minimaxi.com/v1",
				APIKey:  "<your-api-key>",
			},
		},
		WebSearch: WebSearchConfig{
			Type: "brave",
			Key:  "<your-web-search-api-key>",
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return path, fmt.Errorf("config file already exists: %s", path)
		}
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Config) FindProvider(name string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}
