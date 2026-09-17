// Package config reads the process environment into a struct. There is no
// config file: every setting is an environment variable with a default that
// works for local development.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// ProposalTTL is how long a confirm_required proposal may wait for a
	// decision before it is cancelled. Zero disables expiry.
	ProposalTTL time.Duration
	// SessionTTL is how long a browser login lasts.
	SessionTTL time.Duration
	// TrustedOrigins are the web front end's origins, comma separated:
	// https://lms.example.edu. They may call with cookies from a browser.
	TrustedOrigins []string
	// InsecureCookies drops the Secure attribute from the session cookie, for
	// development over http://localhost. Never set it in production.
	InsecureCookies bool
}

func FromEnv() (Config, error) {
	c := Config{
		DatabaseURL:   env("DATABASE_URL", "postgres:///aishiteru"),
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		ShutdownGrace: 15 * time.Second,
	}
	c.ProposalTTL = 14 * 24 * time.Hour
	c.SessionTTL = 12 * time.Hour
	for key, dst := range map[string]*time.Duration{
		"SHUTDOWN_GRACE": &c.ShutdownGrace, "PROPOSAL_TTL": &c.ProposalTTL, "SESSION_TTL": &c.SessionTTL,
	} {
		if v := os.Getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return Config{}, fmt.Errorf("%s: %q is not a duration such as 12h", key, v)
			}
			*dst = d
		}
	}
	for _, o := range strings.Split(os.Getenv("TRUSTED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.TrustedOrigins = append(c.TrustedOrigins, o)
		}
	}
	if v := os.Getenv("INSECURE_COOKIES"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("INSECURE_COOKIES: %q is not true or false", v)
		}
		c.InsecureCookies = b
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
