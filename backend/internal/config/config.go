// Package config is the only package that reads the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

type Config struct {
	Addr     string
	LogLevel slog.Level
}

// Load reads the environment and returns one error listing every invalid variable.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Addr: envOr("ADDR", ":8080"),
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	return cfg, errors.Join(errs...)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
