package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultLocalLabel = "Windows"
	defaultLocalColor = "#3b82f6"
	defaultHostColor  = "#f97316"
)

var colorPattern = regexp.MustCompile(`^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

var hostIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// HostConfig describes a remote machine reachable over SSH.
type HostConfig struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	SSH        string `json:"ssh"`
	DefaultCwd string `json:"default_cwd"`
	Color      string `json:"color"`
}

// AppConfig holds user-configurable settings loaded from config.json.
type AppConfig struct {
	DefaultCwd string       `json:"default_cwd"`
	LocalLabel string       `json:"local_label"`
	LocalColor string       `json:"local_color"`
	Hosts      []HostConfig `json:"hosts"`
}

// parseConfig decodes config JSON and applies defaults. Invalid or duplicate
// hosts are dropped and described in the returned warnings.
func parseConfig(data []byte, home string) (AppConfig, []string, error) {
	var cfg AppConfig
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return AppConfig{}, nil, fmt.Errorf("parse config.json: %w", err)
		}
	}
	if cfg.DefaultCwd == "" {
		cfg.DefaultCwd = home
	}
	if cfg.LocalLabel == "" {
		cfg.LocalLabel = defaultLocalLabel
	}
	if cfg.LocalColor == "" {
		cfg.LocalColor = defaultLocalColor
	}

	var warnings []string
	if !colorPattern.MatchString(cfg.LocalColor) {
		warnings = append(warnings, fmt.Sprintf("local_color %q invalid, using default", cfg.LocalColor))
		cfg.LocalColor = defaultLocalColor
	}
	seen := map[string]bool{}
	hosts := make([]HostConfig, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		if !hostIDPattern.MatchString(h.ID) {
			warnings = append(warnings, fmt.Sprintf("host %q skipped: id must match [a-z0-9_-]+", h.ID))
			continue
		}
		if seen[h.ID] {
			warnings = append(warnings, fmt.Sprintf("host %q skipped: duplicate id", h.ID))
			continue
		}
		seen[h.ID] = true
		if h.SSH == "" {
			h.SSH = h.ID
		}
		if strings.HasPrefix(h.SSH, "-") || strings.ContainsAny(h.SSH, " \t\r\n") {
			warnings = append(warnings, fmt.Sprintf("host %q skipped: invalid ssh value", h.ID))
			continue
		}
		if h.Label == "" {
			h.Label = h.ID
		}
		if !colorPattern.MatchString(h.Color) {
			if h.Color != "" {
				warnings = append(warnings, fmt.Sprintf("host %q: invalid color %q, using default", h.ID, h.Color))
			}
			h.Color = defaultHostColor
		}
		if h.DefaultCwd == "" {
			h.DefaultCwd = "~"
		}
		hosts = append(hosts, h)
	}
	cfg.Hosts = hosts
	return cfg, warnings, nil
}

// loadConfig reads config.json next to the executable, falling back to defaults.
func loadConfig() AppConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	var data []byte
	if exePath, err := os.Executable(); err == nil {
		configPath := filepath.Join(filepath.Dir(exePath), "config.json")
		raw, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			data = raw
		case !errors.Is(err, os.ErrNotExist):
			log.Printf("config: read %s: %v", configPath, err)
		}
	}

	cfg, warnings, err := parseConfig(data, home)
	if err != nil {
		log.Printf("config: %v (using defaults)", err)
		cfg, _, _ = parseConfig(nil, home)
	}
	for _, w := range warnings {
		log.Printf("config: %s", w)
	}
	return cfg
}
