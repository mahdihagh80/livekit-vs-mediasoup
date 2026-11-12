package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is mediasoup/signaling.yaml. No environment variables are read.
type Config struct {
	Listen             string `yaml:"listen"`
	MetricsListen      string `yaml:"metrics_listen"`
	EventsListen       string `yaml:"events_listen"` // loopback: media service events + health probe
	MediaURL           string `yaml:"media_url"`
	ConsumeConcurrency int    `yaml:"consume_concurrency"`
}

func defaultConfigPath() string {
	// The binary lives in mediasoup/signaling/, signaling.yaml beside that directory.
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "signaling.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "signaling.yaml"
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w (pass -config mediasoup/signaling.yaml)", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8080"
	}
	if c.MetricsListen == "" {
		c.MetricsListen = "0.0.0.0:9311"
	}
	if c.EventsListen == "" {
		c.EventsListen = "127.0.0.1:8081"
	}
	if c.MediaURL == "" {
		c.MediaURL = "http://127.0.0.1:3100"
	}
	if c.ConsumeConcurrency <= 0 {
		c.ConsumeConcurrency = 512
	}
	return &c, nil
}
