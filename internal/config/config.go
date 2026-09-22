// Package config reads the process environment into a struct. There is no
// config file: every setting is an environment variable with a default that
// works for local development.
package config

import (
	"fmt"
	"net"
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
	// CookieSameSite is "lax" (the default) or "none". Lax keeps the session
	// cookie off cross-site requests, which is right when the front end is
	// same-site with this server. A front end on another site needs "none",
	// which needs Secure.
	CookieSameSite string
	// TrustedProxies are the address ranges (CIDRs) of the reverse proxies in
	// front of this server. A request from one of them is attributed to the
	// client named in X-Forwarded-For; from anywhere else that header is
	// ignored, since anyone can send it. Empty means the server is reached
	// directly.
	TrustedProxies []string

	// CallsPerMinute and CallsBurst bound one actor's calls, per instance;
	// SignInsPerMinute bounds sign-in attempts per address and per email.
	// Zero turns a limit off.
	CallsPerMinute, CallsBurst, SignInsPerMinute int

	// Jobs turns the background sweeps on. Every instance may leave it on: only
	// one sweeps at a time. JobsInterval is how often a sweep is attempted.
	Jobs         bool
	JobsInterval time.Duration

	// BlobStore is where files live: "fs" (this server's disk), "s3", or
	// "none" (documents hold text only).
	BlobStore string
	// BlobFSRoot is the directory for BlobStore = fs.
	BlobFSRoot string
	// PublicURL is how clients reach this server. The filesystem store builds
	// its upload and download URLs from it.
	PublicURL string
	// SigningKey signs upload tokens, filesystem-store URLs and the
	// single-sign-on state cookie. It must be the same on every instance and
	// across restarts; at least 32 characters. Empty means a random key per
	// process, which is fine for one developer.
	SigningKey     string
	MaxUploadBytes int64
	S3             S3

	// OIDC is single sign-on. It is off unless OIDC_ISSUER is set.
	OIDC OIDC
}

// OIDC describes the identity provider. The defaults are PolyU's ADFS, which
// is what docs/schema.md was written against: the provider is recorded as
// "polyu-adfs" and an account is known by its UPN.
type OIDC struct {
	ProviderName, Issuer, ClientID, ClientSecret, SubjectClaim string
	Scopes                                                     []string
}

func (o OIDC) Enabled() bool { return o.Issuer != "" }

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
	c.Jobs, c.JobsInterval = true, time.Minute
	for key, dst := range map[string]*time.Duration{
		"SHUTDOWN_GRACE": &c.ShutdownGrace, "PROPOSAL_TTL": &c.ProposalTTL, "SESSION_TTL": &c.SessionTTL,
		"JOBS_INTERVAL": &c.JobsInterval,
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
	for _, r := range strings.Split(os.Getenv("TRUSTED_PROXIES"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			if _, _, err := net.ParseCIDR(r); err != nil {
				return Config{}, fmt.Errorf("TRUSTED_PROXIES: %q is not a CIDR such as 10.0.0.0/8", r)
			}
			c.TrustedProxies = append(c.TrustedProxies, r)
		}
	}
	c.CookieSameSite = env("COOKIE_SAMESITE", "lax")
	c.CallsPerMinute, c.CallsBurst, c.SignInsPerMinute = 600, 100, 10
	for key, dst := range map[string]*int{"RATE_LIMIT_PER_MINUTE": &c.CallsPerMinute, "RATE_LIMIT_BURST": &c.CallsBurst,
		"SIGN_IN_ATTEMPTS_PER_MINUTE": &c.SignInsPerMinute} {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("%s: %q is not a number, zero or more", key, v)
			}
			*dst = n
		}
	}
	c.BlobStore = env("BLOB_STORE", "fs")
	c.BlobFSRoot = env("BLOB_FS_ROOT", "var/blobs")
	c.PublicURL = env("PUBLIC_URL", "http://localhost"+portOf(c.HTTPAddr))
	c.SigningKey = os.Getenv("SIGNING_KEY")
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
		if c.SigningKey == "" {
			return Config{}, fmt.Errorf("BLOB_STORE=s3 needs SIGNING_KEY: upload tokens must verify on every instance")
		}
	default:
		return Config{}, fmt.Errorf("BLOB_STORE: %q is not fs, s3 or none", c.BlobStore)
	}
	for key, dst := range map[string]*bool{"INSECURE_COOKIES": &c.InsecureCookies, "JOBS": &c.Jobs} {
		if v := os.Getenv(key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %q is not true or false", key, v)
			}
			*dst = b
		}
	}
	c.OIDC = OIDC{ProviderName: env("OIDC_PROVIDER_NAME", "polyu-adfs"), Issuer: os.Getenv("OIDC_ISSUER"),
		ClientID: os.Getenv("OIDC_CLIENT_ID"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		SubjectClaim: env("OIDC_SUBJECT_CLAIM", "upn"), Scopes: strings.Fields(env("OIDC_SCOPES", "openid profile email"))}
	if c.OIDC.Enabled() {
		if c.OIDC.ClientID == "" {
			return Config{}, fmt.Errorf("OIDC_ISSUER is set, so OIDC_CLIENT_ID is needed too")
		}
		// The browser may come back to a different instance from the one that
		// sent it away, or to this one after a restart.
		if c.SigningKey == "" {
			return Config{}, fmt.Errorf("single sign-on needs SIGNING_KEY: the sign-in state must verify on every instance")
		}
	}
	switch c.CookieSameSite {
	case "lax":
	case "none":
		if c.InsecureCookies {
			return Config{}, fmt.Errorf("COOKIE_SAMESITE=none needs Secure cookies; it cannot go with INSECURE_COOKIES")
		}
	default:
		return Config{}, fmt.Errorf("COOKIE_SAMESITE: %q is not lax or none", c.CookieSameSite)
	}
	if c.JobsInterval < time.Second {
		return Config{}, fmt.Errorf("JOBS_INTERVAL: %s is too often; at least 1s", c.JobsInterval)
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
