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

	// BlobStore is where files live: "fs" (this server's disk), "s3", or
	// "none" (documents hold text only).
	BlobStore string
	// BlobFSRoot is the directory for BlobStore = fs.
	BlobFSRoot string
	// PublicURL is how clients reach this server. The filesystem store builds
	// its upload and download URLs from it.
	PublicURL string
	// BlobSigningKey signs upload tokens and filesystem-store URLs. It must be
	// the same on every instance and across restarts; at least 32 characters.
	// Empty means a random key per process, which is fine for one developer.
	BlobSigningKey string
	MaxUploadBytes int64
	S3             S3
}

type S3 struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey string
	UseSSL                                         bool
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
	c.BlobStore = env("BLOB_STORE", "fs")
	c.BlobFSRoot = env("BLOB_FS_ROOT", "var/blobs")
	c.PublicURL = env("PUBLIC_URL", "http://localhost"+portOf(c.HTTPAddr))
	c.BlobSigningKey = os.Getenv("BLOB_SIGNING_KEY")
	c.MaxUploadBytes = 50 << 20
	if v := os.Getenv("MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES: %q is not a positive number of bytes", v)
		}
		c.MaxUploadBytes = n
	}
	switch c.BlobStore {
	case "fs", "none":
	case "s3":
		c.S3 = S3{Endpoint: os.Getenv("S3_ENDPOINT"), Bucket: os.Getenv("S3_BUCKET"), Region: env("S3_REGION", "us-east-1"),
			AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), UseSSL: env("S3_USE_SSL", "true") != "false"}
		if c.S3.Endpoint == "" || c.S3.Bucket == "" {
			return Config{}, fmt.Errorf("BLOB_STORE=s3 needs S3_ENDPOINT and S3_BUCKET")
		}
		if c.BlobSigningKey == "" {
			return Config{}, fmt.Errorf("BLOB_STORE=s3 needs BLOB_SIGNING_KEY: upload tokens must verify on every instance")
		}
	default:
		return Config{}, fmt.Errorf("BLOB_STORE: %q is not fs, s3 or none", c.BlobStore)
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

// portOf turns a listen address into the ":port" of a localhost URL.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ""
}
