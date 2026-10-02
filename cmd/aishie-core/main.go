// Command aishie-core is the AIshie Core server and its operator tooling.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/config"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/jobs"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
	"github.com/AIShie-Education/AIShie-Core/internal/signing"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/version"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

const usage = `aishie-core — AIshie Core

Usage:
  aishie-core serve                  run the HTTP server
  aishie-core migrate up             apply every pending migration
  aishie-core migrate down --yes     revert the last migration (DESTROYS DATA)
  aishie-core migrate down --all --yes
                                     revert every migration (DESTROYS ALL DATA)
  aishie-core migrate version        print the applied and the embedded version
  aishie-core migrate force N        record version N (0 for none) without running anything
  aishie-core seed                   insert the built-in permission presets
  aishie-core bootstrap --name N --email E|--login-id L --password-stdin
                                     create the root actor, once, with the password read from
                                     standard input, to sign in with; prints no token
  aishie-core token issue --actor ID|EMAIL --label L [--days N]
                                     issue an API token for an mcp agent; a person holds none, and
                                     a runtime agent none but the one its runtime is issued
  aishie-core service issue SCOPE --label L [--days N] [--replace]
                                     issue a credential for a site service, agent_runtime (the site's
                                     agent runtime) or document_text (its transcriber), and print it,
                                     once, on standard output; --replace revokes its others
  aishie-core secrets rewrap         seal again, under SECRETS_KEY, every secret an older key
                                     (SECRETS_KEY_PREVIOUS) sealed: the last step of a rotation
  aishie-core version                print build information

Environment:
  DATABASE_URL      default postgres:///aishie (local unix socket)
  HTTP_ADDR         default :8080
  SHUTDOWN_GRACE    default 15s
  PROPOSAL_TTL      default 336h (14 days); 0 disables expiry
  RATE_LIMIT_PER_MINUTE        default 600 calls per actor per instance; 0 for no limit
  RATE_LIMIT_BURST             default 100
  LONG_POLL_WAITERS            default 1000; the calls that may wait for news at once (wait_s) per instance;
                               0 lets none wait, and each answers at once
  LONG_POLL_WAITERS_PER_ACTOR  default 16; of them, one actor's
  SIGN_IN_ATTEMPTS_PER_MINUTE  default 10, per address (an IPv6 /64 counts as one) and per email or login ID;
                               a sign-in that succeeds is not counted against its address;
                               registrations through a join link count with an address's sign-ins
  JOIN_REGISTRATIONS_PER_MINUTE  default 60, per join link; 0 for no limit
  JOBS              default true; background sweeps (only one instance sweeps at a time)
  JOBS_INTERVAL     default 1m
  SESSION_TTL       default 12h
  TRUSTED_ORIGINS   the web front end's origins, comma separated
  TRUSTED_PROXIES   CIDRs of the reverse proxies in front of this server, comma separated;
                    a request from one is attributed to the client in X-Forwarded-For
  COOKIE_SAMESITE   lax (default) when the front end is same-site with this server; none otherwise
  INSECURE_COOKIES  true for development over http://localhost only
  BLOB_STORE        fs (default), s3 or none
  BLOB_FS_ROOT      default var/blobs
  PUBLIC_URL        how clients reach this server; default http://localhost:8080
  SIGNING_KEY       32+ characters; required with s3, with single sign-on, and for more than one instance
  MAX_UPLOAD_BYTES  default 52428800 (50 MiB); the largest file
  DOCUMENT_MAX_FILES_PER_VERSION  default 20, at most 100; the files one version of a document holds
  DOCUMENT_MAX_VERSION_BYTES      default 209715200 (200 MiB); the files of one version, in all
  ATTACHMENT_MAX_BYTES     default 52428800 (50 MiB); the largest file a message of a conversation
                           carries, never more than MAX_UPLOAD_BYTES
  ATTACHMENT_MAX_PER_MESSAGE         default 10, at most 100; the files one message carries
  ATTACHMENT_MAX_CONVERSATION_BYTES  default 524288000 (500 MiB); the files of one conversation, in all
  EXPORT_MAX_MESSAGES  default 100000; the messages one export of conversations holds, answers and
                       questions proposed and never posted counted with them (conversation.export)
  EXPORT_MAX_BYTES     default 268435456 (256 MiB); the text of those messages, in all
  EXPORT_TTL           default 24h, from 15m to 168h; how long an export's files are kept, under
                       exports/ in the file store, before the sweep removes them
  RENDITION_MAX_BYTES  default 104857600 (100 MiB); the largest PDF an Office file is converted into
                       by the agent runtime (agent_runtime.rendition_complete)
  AGENT_SELF_SERVICE   on (default) or off; whether people may register agents of their own
  AGENT_MAX_PER_OWNER  default 5; the agents one person may have that are not suspended
  S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY, S3_USE_SSL
  S3_REGION         default us-east-1; the region requests are signed for and, with AWS, sent to
  S3_BUCKET_LOOKUP  auto (default), path or dns; how a request names the bucket: path after the endpoint
                    (endpoint/bucket), dns in the host name (bucket.endpoint, virtual-hosted style, for a
                    service that takes nothing else); auto is dns for AWS, Google and Aliyun, path otherwise
  OIDC_ISSUER       the operator's identity provider, for single sign-on; for ADFS,
                    https://<host>/adfs. Administrators add others from the front end
                    (sso.create), which need SECRETS_KEY; this one is read-only to them,
                    and wins over one of theirs with its name
  OIDC_CLIENT_ID, OIDC_CLIENT_SECRET
  OIDC_PROVIDER_NAME   what actor.link_sso calls the provider, such as school-adfs, and what every
                       identity linked at it is recorded under: set it before anyone is linked,
                       and never change it. Unset, it is example-adfs, a placeholder to replace
                       before anyone is linked
  OIDC_SUBJECT_CLAIM   default upn; the claim an account is known by
  OIDC_SCOPES          default "openid profile email"
  OIDC_DISPLAY_NAME    the provider's name on the front end's sign-in button, such as
                       "School NetID", at most 64 printable characters; unset, the front end
                       uses words of its own. GET /v1/auth/methods tells the front end this,
                       and which providers a person may sign in through.
                       Register <PUBLIC_URL>/v1/auth/sso/callback with every provider.
  SECRETS_KEY          base64 of 32 random bytes (openssl rand -base64 32): seals the client
                       secrets of the identity providers administrators add; unset, none is
                       added. Needs SIGNING_KEY; the same on every instance; never lost
                       (what it sealed opens with nothing else)
  SECRETS_KEY_PREVIOUS older keys, comma separated, which open what they sealed and seal
                       nothing: set the new key in SECRETS_KEY and the old one here, run
                       aishie-core secrets rewrap, then remove the old one
  SSO_ALLOW_PRIVATE_ISSUERS  false (default) or true; whether the identity providers administrators
                       add may be on this machine (over http too) or on a private, link-local or
                       other address that is not public. False, the server fetches nothing of
                       theirs from such an address, checked on the address it connects to
                       (issuer_address_not_allowed), and through no proxy (HTTPS_PROXY). For
                       development, tests, a provider on the site's own network, and a server
                       that reaches the internet only through a proxy, or through a DNS that
                       answers with 198.18. addresses (fake-IP). The operator's provider is
                       reached wherever it is
  JOIN_LINK_REGISTRATION  on (default) or off; whether someone with no account may register
                       through a course's join link. Off, people sign in (by single sign-on,
                       say) and then join; GET /v1/join/{token} says registration is false
  RUNTIME_AUDIENCES    the services that host agents a signed-in person may be vouched for to,
                       comma separated absolute URLs such as https://lms.example.edu/runtime;
                       POST /v1/auth/assertion makes the assertion, GET /v1/auth/keys checks it;
                       needs PUBLIC_URL, the assertions' issuer
  ASSERTION_KEY        base64 of a 32-byte Ed25519 seed; default derived from SIGNING_KEY,
                       one of which RUNTIME_AUDIENCES needs
  ASSERTION_TTL        default 5m, from 1m to 15m; never longer than the session asking
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aishie-core:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		return serve(cfg)
	case "migrate":
		return migrate(cfg, args[1:])
	case "seed":
		return seed(cfg)
	case "bootstrap":
		return bootstrap(cfg, args[1:], os.Stdin, os.Stderr)
	case "token":
		return token(cfg, args[1:], os.Stdout, os.Stderr)
	case "service":
		return service(cfg, args[1:], os.Stdout, os.Stderr)
	case "secrets":
		return secretsCmd(cfg, args[1:], os.Stdout)
	case "version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	latest, err := db.LatestEmbedded()
	if err != nil {
		return err
	}
	// serve never migrates on its own: a migration is somebody's decision.
	// But it does not start against a schema it cannot work with, either.
	switch have, dirty, err := db.SchemaVersion(ctx, pool); {
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	case dirty:
		return fmt.Errorf("the schema is dirty at version %d: a migration failed half-way; fix it by hand, then `aishie-core migrate force N`, N being the last migration fully applied (0 if none)", have)
	case have < latest:
		return fmt.Errorf("the schema is at version %d and this binary needs %d; run `aishie-core migrate up` first", have, latest)
	case have > latest:
		log.Warn("the schema is ahead of this binary; fine during a rolling deploy", "schema", have, "binary", latest)
	}

	signatures, err := signing.New(cfg.SigningKey)
	if err != nil {
		return fmt.Errorf("SIGNING_KEY: %w", err)
	}
	signer := blob.SignerFrom(signatures)
	store, err := openBlobStore(ctx, cfg, signer)
	if err != nil {
		return err
	}

	// Calls that wait for news (wait_s) wait in hub, which a connection of
	// its own, listening, keeps told of what every instance commits.
	hub := wake.NewHub(wake.Config{MaxWaiters: cfg.LongPollWaiters, MaxPerActor: cfg.LongPollWaitersPerActor})
	listenDone := make(chan struct{})
	if cfg.LongPollWaiters > 0 && cfg.LongPollWaitersPerActor > 0 {
		listener := wake.NewListener(pool.Config().ConnConfig, hub, log)
		go func() { defer close(listenDone); listener.Run(ctx) }()
	} else {
		close(listenDone)
	}

	reg := tool.NewRegistry()
	pl := pipeline.New(pool, reg, pipeline.Config{ProposalTTL: cfg.ProposalTTL, Secrets: signatures, Wake: hub})
	// Single sign-on's providers: the operator's is discovered now, and if
	// it cannot be reached the server does not start — better that than a
	// sign-in page that fails for everyone with nothing in the log to say
	// why. The site's are read at each sign-in.
	providers, err := ssoProviders(ctx, pool, cfg, log)
	if err != nil {
		return err
	}
	tools.RegisterAll(reg, tools.Deps{Pipeline: pl, Blob: store, Uploads: signer, MaxUploadBytes: cfg.MaxUploadBytes, SSO: providers,
		Attachments: tools.AttachmentLimits{MaxBytes: cfg.AttachmentMaxBytes, PerMessage: cfg.AttachmentMaxPerMessage,
			ConversationBytes: cfg.AttachmentMaxConversationBytes},
		Documents:               tools.DocumentLimits{FilesPerVersion: cfg.DocumentMaxFilesPerVersion, VersionBytes: cfg.DocumentMaxVersionBytes},
		Exports:                 tools.ExportLimits{MaxMessages: cfg.ExportMaxMessages, MaxBytes: cfg.ExportMaxBytes, TTL: cfg.ExportTTL},
		Renditions:              tools.RenditionLimits{MaxBytes: cfg.RenditionMaxBytes},
		DisableAgentSelfService: !cfg.AgentSelfService, MaxAgentsPerOwner: cfg.AgentMaxPerOwner, Memory: cfg.Memory})

	// The sweeps act as the system actor, which bootstrap creates. Before
	// bootstrap there is nothing to sweep and nobody to sweep as.
	jobsDone := make(chan struct{})
	close(jobsDone)
	if cfg.Jobs {
		if system, err := dbq.New(pool).GetSystemActor(ctx); err != nil {
			log.Warn("background jobs are off: there is no system actor yet; run `aishie-core bootstrap`, then restart")
		} else {
			jobsDone = make(chan struct{})
			runner := jobs.New(pool, pl, system, jobs.Config{Interval: cfg.JobsInterval, Blob: store, ExportTTL: cfg.ExportTTL}, log)
			go func() { defer close(jobsDone); runner.Run(ctx) }()
		}
	}

	authn := auth.NewAuthenticator(pool, cfg.SessionTTL)
	calls := ratelimit.New(cfg.CallsPerMinute, cfg.CallsBurst)
	asserter, err := newAsserter(pool, cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Pool: pool, LatestSchema: latest, Pipeline: pl, Log: log,
			Auth: authn, MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: pl, Auth: authn, Log: log, Calls: calls, Memory: cfg.Memory.Enabled}),
			Calls: calls, SignIns: ratelimit.New(cfg.SignInsPerMinute, cfg.SignInsPerMinute),
			Registrations: ratelimit.New(cfg.JoinRegistrationsPerMinute, cfg.JoinRegistrationsPerMinute), NoJoinRegistration: !cfg.JoinLinkRegistration,
			TrustedOrigins: cfg.TrustedOrigins, TrustedProxies: cfg.TrustedProxies,
			InsecureCookies: cfg.InsecureCookies, CookieSameSite: sameSite(cfg.CookieSameSite),
			Blob: store, MaxUploadBytes: cfg.MaxUploadBytes,
			SSO: providers, Signer: signatures, Assertions: asserter,
		}),
		// Headers within ten seconds, an idle keep-alive for two minutes; the
		// body and the response are bounded per request by the handler.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Shutting down, the server waits for every request in flight: those
	// waiting for news answer at once, with what they read.
	srv.RegisterOnShutdown(hub.Shutdown)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.HTTPAddr, "blob_store", cfg.BlobStore, "version", version.Version, "schema_latest", latest, "tools", len(reg.Exposed()),
		"memory", cfg.Memory.Enabled, "long_poll_waiters", cfg.LongPollWaiters, "long_poll_waiters_per_actor", cfg.LongPollWaitersPerActor)
	if asserter != nil {
		// The key's id is public, and says which key a runtime should find
		// at /v1/auth/keys.
		log.Info("assertions", "audiences", len(cfg.RuntimeAudiences), "kid", asserter.KeyID(), "ttl", cfg.AssertionTTL.String())
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", cfg.ShutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// Let a sweep in flight finish its current step and hand its lock back
	// before the pool closes underneath it; and the listener let go of its
	// connection.
	for _, done := range []chan struct{}{jobsDone, listenDone} {
		select {
		case <-done:
		case <-shutdownCtx.Done():
		}
	}
	return nil
}

// ssoProviders is single sign-on's providers: the one the operator sets in
// the environment (OIDC_ISSUER), discovered now, and those the site's
// administrators set up, read from the database at each sign-in, their
// client secrets opened with SECRETS_KEY and SECRETS_KEY_PREVIOUS.
func ssoProviders(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) (*sso.Registry, error) {
	keys, err := keyring(cfg)
	if err != nil {
		return nil, err
	}
	// The operator's provider is the operator's own setting, reached
	// wherever it is; the site's, which administrators set, are held to
	// public addresses unless SSO_ALLOW_PRIVATE_ISSUERS (sso.NewClient).
	client := &http.Client{Timeout: sso.DefaultTimeout}
	var op *sso.Operator
	if cfg.OIDC.Enabled() {
		discover, cancel := context.WithTimeout(ctx, 20*time.Second)
		idp, err := auth.NewOIDC(discover, auth.OIDCConfig{Name: cfg.OIDC.ProviderName, Issuer: cfg.OIDC.Issuer,
			ClientID: cfg.OIDC.ClientID, ClientSecret: cfg.OIDC.ClientSecret, SubjectClaim: cfg.OIDC.SubjectClaim,
			Scopes: cfg.OIDC.Scopes, RedirectURL: strings.TrimRight(cfg.PublicURL, "/") + httpapi.SSOCallbackPath, HTTPClient: client})
		cancel()
		if err != nil {
			return nil, err
		}
		op = &sso.Operator{ID: cfg.OIDC.ProviderName, DisplayName: cfg.OIDC.DisplayName, Issuer: cfg.OIDC.Issuer,
			ClientID: cfg.OIDC.ClientID, SecretHint: secrets.Hint(cfg.OIDC.ClientSecret), Scopes: cfg.OIDC.Scopes,
			SubjectClaim: cfg.OIDC.SubjectClaim, IdP: idp, Client: client}
	}
	if keys != nil {
		// The key's id is public, and says which key sealed what.
		log.Info("secrets key", "key_id", keys.KeyID(), "previous", len(cfg.SecretsKeysPrevious))
	}
	if cfg.SSOAllowPrivateIssuers {
		// Said once, so that it is not set unknowingly.
		log.Info("SSO_ALLOW_PRIVATE_ISSUERS: administrators may have this server reach identity providers on this machine " +
			"and on private and link-local addresses")
	}
	return sso.New(sso.Config{Pool: pool, Operator: op, Keys: keys, PublicURL: cfg.PublicURL, PrivateIssuers: cfg.SSOAllowPrivateIssuers,
		Log: log}), nil
}

// keyring is SECRETS_KEY with SECRETS_KEY_PREVIOUS, or nil without them.
func keyring(cfg config.Config) (*secrets.Keyring, error) {
	if cfg.SecretsKey == nil {
		return nil, nil
	}
	keys, err := secrets.NewKeyring(cfg.SecretsKey, cfg.SecretsKeysPrevious...)
	if err != nil {
		return nil, fmt.Errorf("SECRETS_KEY: %w", err)
	}
	return keys, nil
}

// newAsserter makes what vouches for a signed-in person to a service that
// hosts agents, or nil on a server with no key for it: no ASSERTION_KEY and
// no SIGNING_KEY, which config allows only while RUNTIME_AUDIENCES is empty.
// With a key and no audiences, the key is still published.
func newAsserter(pool *pgxpool.Pool, cfg config.Config) (*auth.Asserter, error) {
	key, err := auth.AssertionKey(cfg.AssertionKey, cfg.SigningKey)
	if errors.Is(err, auth.ErrNoAssertionKey) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return auth.NewAsserter(pool, auth.AssertionConfig{Issuer: strings.TrimRight(cfg.PublicURL, "/"),
		Audiences: cfg.RuntimeAudiences, TTL: cfg.AssertionTTL, Key: key})
}

func sameSite(mode string) http.SameSite {
	if mode == "none" {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

// openBlobStore returns the configured file store, or nil for "none".
func openBlobStore(ctx context.Context, cfg config.Config, signer *blob.Signer) (blob.Store, error) {
	switch cfg.BlobStore {
	case "fs":
		return blob.NewFSStore(cfg.BlobFSRoot, cfg.PublicURL, signer)
	case "s3":
		s, err := blob.NewS3Store(blob.S3Config{Endpoint: cfg.S3.Endpoint, Bucket: cfg.S3.Bucket, Region: cfg.S3.Region,
			AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, UseSSL: cfg.S3.UseSSL, BucketLookup: cfg.S3.BucketLookup})
		if err != nil {
			return nil, err
		}
		if _, err := s.Stat(ctx, "aishie-startup-probe"); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("the S3 bucket is not reachable: %w", err)
		}
		return s, nil
	}
	return nil, nil
}

func migrate(cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate: want up, down, version or force N")
	}
	m, err := db.NewMigrator(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	switch args[0] {
	case "up":
		if err := m.Up(); err != nil {
			return err
		}
	case "down":
		fs := flag.NewFlagSet("migrate down", flag.ContinueOnError)
		all := fs.Bool("all", false, "revert every migration")
		yes := fs.Bool("yes", false, "confirm that data will be destroyed")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*yes {
			return errors.New("migrate down destroys data; pass --yes to confirm")
		}
		if *all {
			err = m.Down()
		} else {
			err = m.Steps(-1)
		}
		if err != nil {
			return err
		}
	case "force":
		if len(args) != 2 {
			return errors.New("migrate force: want a version number")
		}
		v, err := strconv.Atoi(args[1])
		if err != nil || v < 0 {
			return fmt.Errorf("migrate force: %q is not a version number", args[1])
		}
		if err := m.Force(v); err != nil {
			return err
		}
	case "version":
		// falls through to the report below
	default:
		return fmt.Errorf("migrate: unknown subcommand %q", args[0])
	}

	v, dirty, err := m.Version()
	if err != nil {
		return err
	}
	latest, err := db.LatestEmbedded()
	if err != nil {
		return err
	}
	fmt.Println(schemaReport(v, latest, dirty))
	return nil
}

// schemaReport is the line every migrate subcommand ends with. `migrate up`
// leaves a schema that is ahead of the binary as it is, and says so here, so
// that a deploy's log does not read as if it had brought the schema to this
// binary's version.
func schemaReport(version, latest uint, dirty bool) string {
	s := fmt.Sprintf("schema version %d (embedded latest %d)", version, latest)
	switch {
	case dirty:
		s += " DIRTY — fix the database by hand, then `migrate force N`, N being the last migration fully applied (0 if none)"
	case version > latest:
		s += " AHEAD — migrated by a newer release, or by a migration since taken out; left as it is"
	}
	return s
}

func seed(cfg config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Seed(ctx, pool); err != nil {
		return err
	}
	fmt.Println("built-in presets seeded")
	return nil
}

// bootstrap creates the first person of an installation, root, and the
// system actor. It runs once. Root signs in like anyone else, with a
// password, read from standard input, and an email or a login ID, and is
// given no API token: people hold none. It prints nothing on standard
// output, and on standard error the two actors' ids and what root signs in
// with.
func bootstrap(cfg config.Config, args []string, stdin io.Reader, stderr io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "display name of the root actor (required)")
	email := fs.String("email", "", "root's email, to sign in with (this or --login-id is required)")
	loginID := fs.String("login-id", "", "root's login ID, such as a staff number, to sign in with")
	pwStdin := fs.Bool("password-stdin", false, "read root's password from standard input (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("bootstrap: unexpected argument %q", fs.Arg(0))
	case strings.TrimSpace(*name) == "":
		return errors.New("bootstrap: --name is required")
	case strings.TrimSpace(*email) == "" && strings.TrimSpace(*loginID) == "":
		return errors.New("bootstrap: --email or --login-id is required: root signs in with it and a password")
	case !*pwStdin:
		return errors.New("bootstrap: --password-stdin is required: root signs in with a password, and is given no API token")
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("bootstrap: read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	// An empty line is not taken as no password: root would be left with no
	// way to sign in, and bootstrap does not run a second time to put that
	// right.
	if password == "" {
		return errors.New("bootstrap: --password-stdin read an empty password; nothing was created")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: *name, Email: *email, LoginID: *loginID, Password: password})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	signIn := strings.TrimSpace(*email)
	if signIn == "" {
		signIn = strings.TrimSpace(*loginID)
	}
	_, err = fmt.Fprintf(stderr, "root actor   %s\nsystem actor %s\n\nSign in at your site with %s and that password.\n"+
		"No API token is made: people sign in, and API tokens are for agents.\n", res.RootID, res.SystemID, signIn)
	return err
}

// secretsCmd is `secrets rewrap`: every secret an older key sealed
// (SECRETS_KEY_PREVIOUS) is sealed again under SECRETS_KEY, as it was bound,
// written only over what was read, so that the older key can then go. It
// says how many were sealed again, how many were under SECRETS_KEY already,
// and which open with no key this server holds, and fails if any does not.
// It prints no secret. Like `token issue` it is an operator's act outside
// the tool layer and writes no action row: nothing a secret says changes.
func secretsCmd(cfg config.Config, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "rewrap" {
		return errors.New("secrets: want `secrets rewrap`")
	}
	keys, err := keyring(cfg)
	if err != nil {
		return err
	}
	if keys == nil {
		return errors.New("secrets rewrap: SECRETS_KEY is not set; it is the key everything is sealed again under")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := dbq.New(pool)
	rows, err := q.ListSealedSSOSecrets(ctx)
	if err != nil {
		return err
	}
	var rewrapped, current int
	var unopened []string
	for _, r := range rows {
		if keys.Current(r.ClientSecretSealed) {
			current++
			continue
		}
		sealed, err := keys.Rewrap(sso.SecretBinding(r.ID), r.ClientSecretSealed)
		if err != nil {
			unopened = append(unopened, r.ID)
			continue
		}
		n, err := q.RewrapSSOSecret(ctx, dbq.RewrapSSOSecretParams{ID: r.ID, Sealed: sealed, Was: r.ClientSecretSealed})
		if err != nil {
			return err
		}
		if n == 1 {
			rewrapped++
		} else {
			current++ // changed meanwhile, by a write that sealed it under SECRETS_KEY
		}
	}
	if _, err := fmt.Fprintf(stdout, "identity providers' client secrets under key %s: %d sealed again, %d already\n",
		keys.KeyID(), rewrapped, current); err != nil {
		return err
	}
	if len(unopened) > 0 {
		return fmt.Errorf("secrets rewrap: %d do not open with SECRETS_KEY or SECRETS_KEY_PREVIOUS: the client secrets of %s; "+
			"keep the key that sealed them in SECRETS_KEY_PREVIOUS, or give each provider its secret again (sso.update)",
			len(unopened), strings.Join(unopened, ", "))
	}
	return nil
}

// token issues an API token for an agent from the command line: a newly
// registered agent never signs in to ask for its first, so someone with
// access to the server gives it one. A person is refused: people sign in,
// and hold no API token; so is a runtime agent, whose one token is the one
// the site's agent runtime is issued (hosted_by_runtime). This is an
// operator's act outside the tool layer and writes no action row; whoever
// can run it can already write to the database.
func token(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "issue" {
		return errors.New("token: want `token issue --actor ID|EMAIL --label L`")
	}
	fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	who := fs.String("actor", "", "the agent's actor id or email (required)")
	label := fs.String("label", "", "what the token is for (required)")
	days := fs.Int("days", 0, "expire after this many days; 0 means never")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *who == "" || *label == "" {
		return errors.New("token issue: --actor and --label are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := dbq.New(pool)

	actorID, err := uuid.Parse(*who)
	if err != nil {
		// An email has an @, and a login ID none, as a sign-in tells them
		// apart. A login ID is a person's, and is found only to be refused.
		var found uuid.UUID
		if auth.IsEmail(*who) {
			var a dbq.GetActorByEmailRow
			a, err = q.GetActorByEmail(ctx, *who)
			found = a.ID
		} else {
			var a dbq.GetActorByLoginIDRow
			a, err = q.GetActorByLoginID(ctx, *who)
			found = a.ID
		}
		if err != nil {
			return fmt.Errorf("token issue: no actor with id, email or login ID %q", *who)
		}
		actorID = found
	} else if _, err := q.GetActor(ctx, actorID); err != nil {
		return fmt.Errorf("token issue: no actor %s", actorID)
	}
	now := time.Now()
	var expires *time.Time
	if *days > 0 {
		t := now.AddDate(0, 0, *days)
		expires = &t
	}
	tok, _, err := auth.IssueToken(ctx, q, actorID, nil, *label, expires, now)
	if err != nil {
		return fmt.Errorf("token issue: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "API token for agent %s, shown once:\n", actorID); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, tok.Full)
	return err
}

// service issues a site service a credential from the command line, as
// service.issue_credential does: the operator's, at setup, for the site's
// agent runtime (agent_runtime) or its transcriber (document_text), before
// anyone has signed in to issue one. The service is made the first time,
// by nobody Core knows, and the credential is issued by nobody. It prints
// the credential on standard output, once, alone on its line, for a set-up
// script to keep where the service reads it, and what it is on standard
// error. --replace revokes the service's other credentials in the same
// transaction, as a set-up run again does when it has lost the one it kept.
// Like `token issue` it is an operator's act outside the tool layer and
// writes no action row.
func service(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "issue" || strings.HasPrefix(args[1], "-") {
		return errors.New("service: want `service issue SCOPE --label L`, SCOPE agent_runtime or document_text")
	}
	scope := args[1]
	fs := flag.NewFlagSet("service issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	label := fs.String("label", "", "what the credential is for, as administrators see it listed (required)")
	days := fs.Int("days", 0, "expire after this many days, 1 to 3650; 0 means never")
	replace := fs.Bool("replace", false, "revoke the service's other credentials")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("service issue: unexpected argument %q", fs.Arg(0))
	case !tools.ServiceScope(scope):
		return fmt.Errorf("service issue: no site service %q: agent_runtime or document_text", scope)
	case strings.TrimSpace(*label) == "":
		return errors.New("service issue: --label is required")
	case *days < 0 || *days > 3650:
		return errors.New("service issue: --days is 1 to 3650, or 0 for never")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	now := time.Now()
	var expires *time.Time
	if *days > 0 {
		t := now.AddDate(0, 0, *days)
		expires = &t
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := tools.IssueServiceCredential(ctx, dbq.New(tx), tools.ServiceCredentialArgs{Scope: scope, Label: strings.TrimSpace(*label),
		ExpiresAt: expires, Replace: *replace, Now: now})
	if err != nil {
		return fmt.Errorf("service issue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "credential %s (%s) for the site service %s, %d other(s) revoked, shown once:\n",
		out.CredentialID, out.TokenPrefix, scope, len(out.Revoked)); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, out.Token)
	return err
}
