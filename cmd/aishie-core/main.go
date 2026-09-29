// Command aishie-core is the AIshiteru Core server and its operator tooling.
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
	"github.com/AIShie-Education/AIShie-Core/internal/signing"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/version"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

const usage = `aishie-core — AIshiteru Core

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
                                     issue an API token for an agent; a person holds none
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
  MAX_UPLOAD_BYTES  default 52428800 (50 MiB)
  AGENT_SELF_SERVICE   on (default) or off; whether people may register agents of their own
  AGENT_MAX_PER_OWNER  default 5; the agents one person may have that are not suspended
  S3_ENDPOINT, S3_BUCKET, S3_REGION, S3_ACCESS_KEY, S3_SECRET_KEY, S3_USE_SSL
  OIDC_ISSUER       turns single sign-on on; for ADFS, https://<host>/adfs
  OIDC_CLIENT_ID, OIDC_CLIENT_SECRET
  OIDC_PROVIDER_NAME   default polyu-adfs; what actor.link_sso calls the provider
  OIDC_SUBJECT_CLAIM   default upn; the claim an account is known by
  OIDC_SCOPES          default "openid profile email"
  OIDC_DISPLAY_NAME    the provider's name on the front end's sign-in button, such as
                       "PolyU NetID", at most 64 printable characters; unset, the front end
                       uses words of its own. GET /v1/auth/methods tells the front end this,
                       and whether single sign-on is on.
                       Register <PUBLIC_URL>/v1/auth/sso/callback with the provider.
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
	tools.RegisterAll(reg, tools.Deps{Pipeline: pl, Blob: store, Uploads: signer, MaxUploadBytes: cfg.MaxUploadBytes,
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
			runner := jobs.New(pool, pl, system, jobs.Config{Interval: cfg.JobsInterval, Blob: store}, log)
			go func() { defer close(jobsDone); runner.Run(ctx) }()
		}
	}

	authn := auth.NewAuthenticator(pool, cfg.SessionTTL)
	calls := ratelimit.New(cfg.CallsPerMinute, cfg.CallsBurst)
	// Single sign-on is discovered at start-up. If the provider cannot be
	// reached the server does not start: better that than a sign-in page that
	// fails for everyone with nothing in the log to say why.
	var sso auth.IdentityProvider
	if cfg.OIDC.Enabled() {
		discover, cancel := context.WithTimeout(ctx, 20*time.Second)
		sso, err = auth.NewOIDC(discover, auth.OIDCConfig{Name: cfg.OIDC.ProviderName, Issuer: cfg.OIDC.Issuer,
			ClientID: cfg.OIDC.ClientID, ClientSecret: cfg.OIDC.ClientSecret, SubjectClaim: cfg.OIDC.SubjectClaim,
			Scopes: cfg.OIDC.Scopes, RedirectURL: strings.TrimRight(cfg.PublicURL, "/") + httpapi.SSOCallbackPath})
		cancel()
		if err != nil {
			return err
		}
	}
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
			SSO: sso, Signer: signatures, SSOLabel: cfg.OIDC.DisplayName, Assertions: asserter,
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
			AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, UseSSL: cfg.S3.UseSSL})
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

// token issues an API token for an agent from the command line: a newly
// registered agent never signs in to ask for its first, so someone with
// access to the server gives it one. A person is refused: people sign in,
// and hold no API token. This is an operator's act outside the tool layer
// and writes no action row; whoever can run it can already write to the
// database.
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
