// Package config reads the process environment into a struct. There is no
// config file: every setting is an environment variable with a default that
// works for local development.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Core/internal/memory"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
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
	// SignInsPerMinute bounds sign-in attempts per email or login ID, and per address
	// those that fail, registrations through a join link among them;
	// JoinRegistrationsPerMinute bounds registrations through one join link.
	// Zero turns a limit off.
	CallsPerMinute, CallsBurst, SignInsPerMinute, JoinRegistrationsPerMinute int

	// LongPollWaiters bounds the calls waiting for news at once (wait_s) in
	// this instance, and LongPollWaitersPerActor those of one actor. A call
	// past either answers at once. Zero lets none wait.
	LongPollWaiters, LongPollWaitersPerActor int

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

	// The files messages of conversations carry (docs/schema.md §2.8,
	// Attachments): the largest one (ATTACHMENT_MAX_BYTES, 50 MiB, and
	// never more than MaxUploadBytes), how many one message carries
	// (ATTACHMENT_MAX_PER_MESSAGE, 10) and how much one conversation holds
	// in all (ATTACHMENT_MAX_CONVERSATION_BYTES, 500 MiB).
	AttachmentMaxBytes, AttachmentMaxConversationBytes int64
	AttachmentMaxPerMessage                            int
	// DocumentMaxFilesPerVersion and DocumentMaxVersionBytes bound the files
	// one version of a document holds (docs/schema.md §2.4, Files of a
	// version): how many (DOCUMENT_MAX_FILES_PER_VERSION, 20) and how much
	// in all (DOCUMENT_MAX_VERSION_BYTES, 200 MiB). Each file is held to
	// MaxUploadBytes as ever.
	DocumentMaxFilesPerVersion int
	DocumentMaxVersionBytes    int64
	// RenditionMaxBytes bounds the PDF an Office file is converted into
	// (docs/schema.md §2.4, Renditions; RENDITION_MAX_BYTES, 100 MiB): a
	// larger one the agent runtime uploads is refused, and it says the
	// rendition was skipped, too_large.
	RenditionMaxBytes int64
	// The exports of conversations administrators make for audit
	// (docs/schema.md §2.8, Exporting conversations for audit): how many
	// messages one holds at most (EXPORT_MAX_MESSAGES, 100,000, answers and
	// questions proposed and never posted counted with them), how much of
	// their text (EXPORT_MAX_BYTES, 256 MiB), and how long its files are
	// kept (EXPORT_TTL, 24 hours, from 15 minutes to 7 days).
	ExportMaxMessages int
	ExportMaxBytes    int64
	ExportTTL         time.Duration

	// OIDC is the identity provider the server's operator sets: single
	// sign-on through it is off unless OIDC_ISSUER is set. Administrators
	// add others from the front end (sso.create), kept in the database.
	OIDC OIDC
	// SecretsKey seals, and SecretsKeysPrevious open as well, the secrets
	// Core keeps and must read back: an identity provider's client secret
	// (SECRETS_KEY, SECRETS_KEY_PREVIOUS, 32 bytes each in base64; package
	// secrets). Without it, no provider is added from the front end.
	SecretsKey          []byte
	SecretsKeysPrevious [][]byte
	// JoinLinkRegistration lets someone with no account register through a
	// course's join link (JOIN_LINK_REGISTRATION, on unless off): the one
	// way a person registers on their own, a stopgap until single sign-on
	// covers everyone. Off, people sign in — by single sign-on, say — and
	// then join through the link; nobody registers through one.
	JoinLinkRegistration bool

	// RuntimeAudiences are the services that host agents (runtimes) this
	// server vouches for its signed-in people to, each named by the absolute
	// URL it knows itself by: https://lms.example.edu/runtime. A person asks
	// for an assertion for one of them at POST /v1/auth/assertion. Empty
	// means no assertion is made.
	RuntimeAudiences []string
	// AssertionKey is the Ed25519 seed assertions are signed with
	// (ASSERTION_KEY, 32 bytes in base64). Empty means a key derived from
	// SigningKey, which then must be set if RuntimeAudiences is.
	AssertionKey []byte
	// AssertionTTL is how long an assertion lasts at most, from
	// MinAssertionTTL to MaxAssertionTTL: a sign-out or a suspension reaches
	// a runtime no later than that.
	AssertionTTL time.Duration

	// AgentSelfService lets people register agents of their own
	// (agent.create). Off, only administrators register agents; agents
	// already registered, and what their owners do with them, are left as
	// they are. AgentMaxPerOwner bounds the agents one person may have that
	// are not suspended.
	AgentSelfService bool
	AgentMaxPerOwner int

	// Memory is agents' memory, kept in Core (docs/schema.md §2.9): whether
	// this installation keeps it at all (MEMORY, off unless on), how much an
	// agent may keep (MEMORY_MAX_OWNER, _ASKER, _SHARED, _PROPOSED,
	// _PER_AGENT) and how fast it may write (MEMORY_WRITES_PER_HOUR,
	// _PER_DAY). It stays off until what forgets it on time, when seats and
	// courses end, is in place.
	Memory memory.Config
}

// OIDC describes the identity provider. The defaults are for ADFS: an account
// is known by its UPN. ProviderName (OIDC_PROVIDER_NAME) is what the provider
// is recorded as, which an installation names for itself, such as
// "school-adfs"; unset, it is DefaultOIDCProviderName.
type OIDC struct {
	ProviderName, Issuer, ClientID, ClientSecret, SubjectClaim string
	Scopes                                                     []string
	// DisplayName is the provider's name as the front end's sign-in button
	// shows it, such as "School NetID" (OIDC_DISPLAY_NAME). Empty leaves the
	// button to the front end's own words.
	DisplayName string
}

// DefaultOIDCProviderName is OIDC_PROVIDER_NAME when it is unset. It names the
// identity provider Core was first written against, a real university's, and
// it stays: an installation that never set OIDC_PROVIDER_NAME has its people's
// identities linked under it (credential.provider), and under another
// default none of them could sign in. A new installation names its provider
// itself, before anyone is linked (README.md, Single sign-on).
const DefaultOIDCProviderName = "polyu-adfs"

func (o OIDC) Enabled() bool { return o.Issuer != "" }

type S3 struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey string
	UseSSL                                         bool
	// BucketLookup is how a request names the bucket (S3_BUCKET_LOOKUP):
	// "auto", the default, as the S3 client judges by the endpoint; "path",
	// after the endpoint; or "dns", in the host name (virtual-hosted style).
	BucketLookup string
}

// The bounds of ASSERTION_TTL, and its default. Shorter than a minute, a
// front end would spend its calls asking again; longer than a quarter of an
// hour, a sign-out would take too long to reach a runtime.
const (
	MinAssertionTTL     = time.Minute
	MaxAssertionTTL     = 15 * time.Minute
	DefaultAssertionTTL = 5 * time.Minute
)

func FromEnv() (Config, error) {
	c := Config{
		DatabaseURL:   env("DATABASE_URL", "postgres:///aishie"),
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
	c.CallsPerMinute, c.CallsBurst, c.SignInsPerMinute, c.JoinRegistrationsPerMinute = 600, 100, 10, 60
	c.LongPollWaiters, c.LongPollWaitersPerActor = wake.DefaultMaxWaiters, wake.DefaultMaxPerActor
	for key, dst := range map[string]*int{"RATE_LIMIT_PER_MINUTE": &c.CallsPerMinute, "RATE_LIMIT_BURST": &c.CallsBurst,
		"SIGN_IN_ATTEMPTS_PER_MINUTE": &c.SignInsPerMinute, "JOIN_REGISTRATIONS_PER_MINUTE": &c.JoinRegistrationsPerMinute,
		"LONG_POLL_WAITERS": &c.LongPollWaiters, "LONG_POLL_WAITERS_PER_ACTOR": &c.LongPollWaitersPerActor} {
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
	if err := c.readDocumentLimits(); err != nil {
		return Config{}, err
	}
	c.RenditionMaxBytes = 100 << 20
	if v := os.Getenv("RENDITION_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("RENDITION_MAX_BYTES: %q is not a positive number of bytes", v)
		}
		c.RenditionMaxBytes = n
	}
	if err := c.readAttachments(); err != nil {
		return Config{}, err
	}
	if err := c.readExports(); err != nil {
		return Config{}, err
	}
	switch c.BlobStore {
	case "fs", "none":
	case "s3":
		c.S3 = S3{Endpoint: os.Getenv("S3_ENDPOINT"), Bucket: os.Getenv("S3_BUCKET"), Region: env("S3_REGION", "us-east-1"),
			AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), UseSSL: env("S3_USE_SSL", "true") != "false",
			BucketLookup: env("S3_BUCKET_LOOKUP", "auto")}
		if c.S3.Endpoint == "" || c.S3.Bucket == "" {
			return Config{}, fmt.Errorf("BLOB_STORE=s3 needs S3_ENDPOINT and S3_BUCKET")
		}
		switch c.S3.BucketLookup {
		case "auto", "path", "dns":
		default:
			return Config{}, fmt.Errorf("S3_BUCKET_LOOKUP: %q is not auto, path or dns", c.S3.BucketLookup)
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
	c.JoinLinkRegistration = true
	switch v := os.Getenv("JOIN_LINK_REGISTRATION"); v {
	case "", "on":
	case "off":
		c.JoinLinkRegistration = false
	default:
		return Config{}, fmt.Errorf("JOIN_LINK_REGISTRATION: %q is not on or off", v)
	}
	c.AgentSelfService, c.AgentMaxPerOwner = true, 5
	switch v := os.Getenv("AGENT_SELF_SERVICE"); v {
	case "", "on":
	case "off":
		c.AgentSelfService = false
	default:
		return Config{}, fmt.Errorf("AGENT_SELF_SERVICE: %q is not on or off", v)
	}
	if v := os.Getenv("AGENT_MAX_PER_OWNER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("AGENT_MAX_PER_OWNER: %q is not a number, one or more", v)
		}
		c.AgentMaxPerOwner = n
	}
	if err := c.readMemory(); err != nil {
		return Config{}, err
	}
	c.OIDC = OIDC{ProviderName: env("OIDC_PROVIDER_NAME", DefaultOIDCProviderName), Issuer: os.Getenv("OIDC_ISSUER"),
		ClientID: os.Getenv("OIDC_CLIENT_ID"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		SubjectClaim: env("OIDC_SUBJECT_CLAIM", "upn"), Scopes: strings.Fields(env("OIDC_SCOPES", "openid profile email"))}
	name, err := displayName(os.Getenv("OIDC_DISPLAY_NAME"))
	if err != nil {
		return Config{}, fmt.Errorf("OIDC_DISPLAY_NAME: %w", err)
	}
	c.OIDC.DisplayName = name
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
	if err := c.readAssertions(); err != nil {
		return Config{}, err
	}
	if err := c.readSecrets(); err != nil {
		return Config{}, err
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

// MaxAttachmentsPerMessage is the most ATTACHMENT_MAX_PER_MESSAGE may be:
// each file is named by an upload token in the request that writes the
// message, which is at most 1 MiB.
const MaxAttachmentsPerMessage = 100

// readAttachments reads the limits on the files messages of conversations
// carry, each a whole number, one or more, and one not set its default. A
// file is never larger than MAX_UPLOAD_BYTES, whatever ATTACHMENT_MAX_BYTES
// says: that is what this server's own disk takes as it arrives.
func (c *Config) readAttachments() error {
	c.AttachmentMaxBytes, c.AttachmentMaxPerMessage, c.AttachmentMaxConversationBytes = 50<<20, 10, 500<<20
	for key, dst := range map[string]*int64{"ATTACHMENT_MAX_BYTES": &c.AttachmentMaxBytes,
		"ATTACHMENT_MAX_CONVERSATION_BYTES": &c.AttachmentMaxConversationBytes} {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s: %q is not a positive number of bytes", key, v)
			}
			*dst = n
		}
	}
	if v := os.Getenv("ATTACHMENT_MAX_PER_MESSAGE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxAttachmentsPerMessage {
			return fmt.Errorf("ATTACHMENT_MAX_PER_MESSAGE: %q is not a number from 1 to %d", v, MaxAttachmentsPerMessage)
		}
		c.AttachmentMaxPerMessage = n
	}
	c.AttachmentMaxBytes = min(c.AttachmentMaxBytes, c.MaxUploadBytes)
	return nil
}

// MaxDocumentFilesPerVersion is the most DOCUMENT_MAX_FILES_PER_VERSION may
// be, and what the database holds a version to (document_version_file).
const MaxDocumentFilesPerVersion = 100

// readDocumentLimits reads the limits on the files one version of a
// document holds, each a whole number, one or more, and one not set its
// default.
func (c *Config) readDocumentLimits() error {
	c.DocumentMaxFilesPerVersion, c.DocumentMaxVersionBytes = 20, 200<<20
	if v := os.Getenv("DOCUMENT_MAX_FILES_PER_VERSION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxDocumentFilesPerVersion {
			return fmt.Errorf("DOCUMENT_MAX_FILES_PER_VERSION: %q is not a number from 1 to %d", v, MaxDocumentFilesPerVersion)
		}
		c.DocumentMaxFilesPerVersion = n
	}
	if v := os.Getenv("DOCUMENT_MAX_VERSION_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("DOCUMENT_MAX_VERSION_BYTES: %q is not a positive number of bytes", v)
		}
		c.DocumentMaxVersionBytes = n
	}
	return nil
}

// The bounds of EXPORT_TTL. An export's files are kept at least as long as
// a URL to download them lasts, and never more than a week: they are
// personal data, made to be taken away, not kept here.
const (
	MinExportTTL = 15 * time.Minute
	MaxExportTTL = 7 * 24 * time.Hour
)

// readExports reads the limits on an export of conversations, each a whole
// number, one or more, and how long its files are kept; one not set is its
// default.
func (c *Config) readExports() error {
	c.ExportMaxMessages, c.ExportMaxBytes, c.ExportTTL = 100000, 256<<20, 24*time.Hour
	if v := os.Getenv("EXPORT_MAX_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("EXPORT_MAX_MESSAGES: %q is not a number, one or more", v)
		}
		c.ExportMaxMessages = n
	}
	if v := os.Getenv("EXPORT_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("EXPORT_MAX_BYTES: %q is not a positive number of bytes", v)
		}
		c.ExportMaxBytes = n
	}
	if v := os.Getenv("EXPORT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < MinExportTTL || d > MaxExportTTL {
			return fmt.Errorf("EXPORT_TTL: %q is not a duration from %s to %s, such as 24h", v, MinExportTTL, MaxExportTTL)
		}
		c.ExportTTL = d
	}
	return nil
}

// readMemory reads MEMORY and the limits of agents' memory. A limit is a
// whole number, one or more; one not set is its default.
func (c *Config) readMemory() error {
	switch v := os.Getenv("MEMORY"); v {
	case "", "off":
	case "on":
		c.Memory.Enabled = true
	default:
		return fmt.Errorf("MEMORY: %q is not on or off", v)
	}
	for key, dst := range map[string]*int{
		"MEMORY_MAX_OWNER": &c.Memory.MaxOwner, "MEMORY_MAX_ASKER": &c.Memory.MaxAsker, "MEMORY_MAX_SHARED": &c.Memory.MaxShared,
		"MEMORY_MAX_PROPOSED": &c.Memory.MaxProposed, "MEMORY_MAX_PER_AGENT": &c.Memory.MaxPerAgent,
		"MEMORY_WRITES_PER_HOUR": &c.Memory.WritesPerHour, "MEMORY_WRITES_PER_DAY": &c.Memory.WritesPerDay,
	} {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fmt.Errorf("%s: %q is not a number, one or more", key, v)
			}
			*dst = n
		}
	}
	c.Memory = c.Memory.WithDefaults()
	return nil
}

// readSecrets reads SECRETS_KEY and SECRETS_KEY_PREVIOUS. Each key is 32
// bytes in base64. An old key is of no use without the one that took its
// place, and the key is refused without SIGNING_KEY: what it keeps is
// identity providers' secrets, and a sign-in through one must verify on
// every instance and after a restart, as the operator's provider's does.
func (c *Config) readSecrets() error {
	if v := os.Getenv("SECRETS_KEY"); v != "" {
		key, err := secrets.ParseKey(v)
		if err != nil {
			return fmt.Errorf("SECRETS_KEY %w", err)
		}
		c.SecretsKey = key
	}
	for i, v := range strings.Split(os.Getenv("SECRETS_KEY_PREVIOUS"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		key, err := secrets.ParseKey(v)
		if err != nil {
			return fmt.Errorf("SECRETS_KEY_PREVIOUS: the key at position %d %w", i+1, err)
		}
		c.SecretsKeysPrevious = append(c.SecretsKeysPrevious, key)
	}
	switch {
	case len(c.SecretsKeysPrevious) > 0 && c.SecretsKey == nil:
		return errors.New("SECRETS_KEY_PREVIOUS needs SECRETS_KEY: the old keys open what they sealed, and SECRETS_KEY seals from now on")
	case c.SecretsKey != nil && c.SigningKey == "":
		return errors.New("SECRETS_KEY needs SIGNING_KEY: a sign-in through an identity provider it keeps must verify on every instance and after a restart")
	}
	return nil
}

// MaxDisplayName is the most characters OIDC_DISPLAY_NAME may have: it is a
// button's label, not a sentence.
const MaxDisplayName = 64

// displayName checks OIDC_DISPLAY_NAME, which the front end shows on its
// sign-in button as it is. White space around it is dropped, and a name
// that is nothing else is none. The rest must be UTF-8, at most
// MaxDisplayName characters, and printable to the last one: a control
// character, or one that draws nothing or turns what follows around, such
// as a zero-width space or a right-to-left override, would have the button
// say something other than what the operator reads in the env file. It is
// checked whether single sign-on is on or not, so that a bad name is found
// when it is set and not only once OIDC_ISSUER is.
func displayName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) {
		return "", errors.New("is not UTF-8")
	}
	if n := utf8.RuneCountInString(v); n > MaxDisplayName {
		return "", fmt.Errorf("is %d characters long; at most %d", n, MaxDisplayName)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("has %U in it, which is not a printable character", r)
		}
	}
	return v, nil
}

// readAssertions reads RUNTIME_AUDIENCES, ASSERTION_KEY and ASSERTION_TTL.
// An audience is refused unless it is an absolute http or https URL with
// nothing in it but a host, a port and a path, since a runtime compares it
// exactly and a mistyped one would make assertions nobody takes. Audiences
// with no key to sign for them are refused, as single sign-on is without
// SIGNING_KEY: with a key made up at each start, what one instance signs
// would not check against what another publishes, nor survive a restart.
// So are audiences without PUBLIC_URL, which is the assertions' issuer: the
// default, a localhost URL, would be refused by every runtime, and only then.
func (c *Config) readAssertions() error {
	for _, a := range strings.Split(os.Getenv("RUNTIME_AUDIENCES"), ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if err := checkAudience(a); err != nil {
			return fmt.Errorf("RUNTIME_AUDIENCES: %q %w", a, err)
		}
		c.RuntimeAudiences = append(c.RuntimeAudiences, a)
	}
	if v := os.Getenv("ASSERTION_KEY"); v != "" {
		seed, err := decodeSeed(v)
		if err != nil {
			return fmt.Errorf("ASSERTION_KEY %w", err)
		}
		c.AssertionKey = seed
	}
	c.AssertionTTL = DefaultAssertionTTL
	if v := os.Getenv("ASSERTION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < MinAssertionTTL || d > MaxAssertionTTL {
			return fmt.Errorf("ASSERTION_TTL: %q is not a duration from %s to %s", v, MinAssertionTTL, MaxAssertionTTL)
		}
		c.AssertionTTL = d
	}
	if len(c.RuntimeAudiences) > 0 && c.AssertionKey == nil && c.SigningKey == "" {
		return fmt.Errorf("RUNTIME_AUDIENCES needs ASSERTION_KEY or SIGNING_KEY: assertions must check against the same key on every instance and after a restart")
	}
	if len(c.RuntimeAudiences) > 0 && os.Getenv("PUBLIC_URL") == "" {
		return fmt.Errorf("RUNTIME_AUDIENCES needs PUBLIC_URL: it is the issuer a runtime takes assertions from, and the default, %s, is none a runtime would name", c.PublicURL)
	}
	return nil
}

// checkAudience says what is wrong with an audience, if anything.
//
// A runtime compares the audience byte for byte, so it must be written the
// one way a URL is written: a lower-case scheme and host, no port that is
// the scheme's own or not a plain number, and a path escaped as Go escapes
// it, with nothing percent-encoded that need not be and none of ! ' ( ) *,
// which URLs write both ways. Its path has no "." or ".." segment and no
// empty one but a final "/": such a URL names the same place as another
// written differently, or, after a proxy tidies it, a place outside the
// runtime's path altogether.
func checkAudience(a string) error {
	u, err := url.Parse(a)
	switch {
	case err != nil:
		return errors.New("is not a URL")
	case u.Scheme != "https" && u.Scheme != "http":
		return errors.New("is not an absolute http or https URL, such as https://lms.example.edu/runtime")
	case u.Opaque != "" || u.Hostname() == "":
		return errors.New("names no host")
	case u.User != nil:
		return errors.New("carries a user name or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(a, "#"):
		return errors.New("has a query or a fragment")
	case u.Host != strings.ToLower(u.Host):
		return errors.New("has upper case in its host; a host is written in lower case")
	case !canonicalPort(u):
		return errors.New("names a port that is empty, not a plain number from 1 to 65535, or the scheme's own")
	case strings.ContainsAny(u.Path, "!'()*"):
		return errors.New("has one of ! ' ( ) * in its path, which URLs write both escaped and not")
	case u.RawPath != "":
		return errors.New("escapes its path otherwise than plainly: an escape that is not needed or not in upper case, or an escaped /")
	case !plainPath(u.Path):
		return errors.New(`has a ".", ".." or empty segment in its path`)
	case u.String() != a:
		return errors.New("is not written the way a URL is, lower-case scheme and path escaped")
	}
	return nil
}

// canonicalPort reports whether a URL's port, if it names one, is written as
// a URL's port is: a number with no leading zero, from 1 to 65535, and not
// the one its scheme implies.
func canonicalPort(u *url.URL) bool {
	i := strings.LastIndex(u.Host, ":")
	if i < 0 || strings.HasSuffix(u.Host, "]") {
		return true // no port at all: a name, or an IPv6 address in brackets
	}
	p := u.Host[i+1:]
	n, err := strconv.Atoi(p)
	switch {
	case err != nil || strconv.Itoa(n) != p || n < 1 || n > 65535:
		return false
	case u.Scheme == "https" && n == 443, u.Scheme == "http" && n == 80:
		return false
	}
	return true
}

// plainPath reports whether a URL path has no "." or ".." segment and no
// empty segment but the last, which a final "/" leaves.
func plainPath(p string) bool {
	if p == "" {
		return true
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i < len(segs)-1) {
			return false
		}
	}
	return true
}

// decodeSeed takes an Ed25519 seed in base64, padded or not, standard or
// URL-safe.
func decodeSeed(v string) ([]byte, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "=")
	seed, err := base64.RawStdEncoding.DecodeString(v)
	if err != nil {
		seed, err = base64.RawURLEncoding.DecodeString(v)
	}
	if err != nil {
		return nil, errors.New("is not base64")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("is %d bytes, not the %d of an Ed25519 seed (openssl rand -base64 32 makes one)", len(seed), ed25519.SeedSize)
	}
	return seed, nil
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
