package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spoloh/spoloh/internal/atomicfile"
)

// Load reads and validates a config file. A missing file returns an error for
// which os.IsNotExist reports true, so callers can seed a default.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

// Save validates then writes the config atomically. A power cut mid-write
// leaves the previous file intact, which matters on an appliance that loses
// mains power without warning; so does an invalid config, which is rejected
// before anything is written at all.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save an invalid config: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return atomicfile.Write(path, data, 0o600)
}
