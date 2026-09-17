// Package config reads the process environment into a struct. There is no
// config file: every setting is an environment variable with a default that
// works for local development.
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	// DatabaseURL is a libpq-style URL. The default reaches a local server
	// over its unix socket as the current OS user.
	DatabaseURL string
	// HTTPAddr is the listen address for REST and MCP.
	HTTPAddr string
	// ShutdownGrace bounds how long in-flight requests get on SIGTERM.
	ShutdownGrace time.Duration
}

func FromEnv() (Config, error) {
	c := Config{
		DatabaseURL:   env("DATABASE_URL", "postgres:///aishiteru"),
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		ShutdownGrace: 15 * time.Second,
	}
	if v := os.Getenv("SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("SHUTDOWN_GRACE: %w", err)
		}
		c.ShutdownGrace = d
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
