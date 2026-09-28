// Package httpapi is the REST adapter. It holds no business logic and no
// list of endpoints: every route is generated from the tool registry, and
// every call goes through the pipeline exactly as an MCP call does. What is
// here is transport — finding the arguments in a request, establishing who is
// calling, and turning an outcome into a status code.
//
// A call that is attempted is recorded, and its answer names the action in a
// top-level action_id:
//
//	executed → 200    proposed → 202    denied → 403
//	failed   → its error's own status: 400, 403, 404, 409 or 422
//	replayed → as the action stands now, with Idempotency-Replayed: true;
//	           a proposal since rejected is 409, one cancelled 422
//
// An answer with no top-level action_id records nothing, whatever its
// status: among them every 401 and 429, a 400 or 404 from before the tool
// runs, a 403 from the cross-origin guard, a 405, a 500, and every read. A
// key used for another call is a 409 idempotency_conflict naming the earlier
// action in error.details.action_id, and records nothing either.
package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/signing"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderReplayed       = "Idempotency-Replayed"
	SessionCookie        = "ais_session"

	maxBodyBytes = 1 << 20
)

type Deps struct {
	Pool *pgxpool.Pool
	// LatestSchema is the highest migration version the binary carries.
	LatestSchema uint
	Pipeline     *pipeline.Pipeline
	Auth         *auth.Authenticator
	Log          *slog.Logger

	// TrustedOrigins are the origins of the web front end. They may call
	// with cookies (CORS) and are exempt from the cross-origin check.
	TrustedOrigins []string
	// TrustedProxies are the CIDRs of the reverse proxies in front of this
	// server; see clientAddr.
	TrustedProxies []string
	// InsecureCookies drops the Secure attribute, for http://localhost.
	InsecureCookies bool
	// CookieSameSite is the session cookie's SameSite; zero means Lax.
	CookieSameSite http.SameSite

	// BodyTimeout bounds reading one request's body; TransferTimeout bounds
	// a file upload or download. Zero means the defaults below. Whoever
	// holds a connection open must not hold it for ever.
	BodyTimeout, TransferTimeout time.Duration

	// Blob is the file store. When it keeps files on this server's own disk,
	// the server also serves its upload and download URLs.
	Blob           blob.Store
	MaxUploadBytes int64

	// SSO is the identity provider, when single sign-on is configured; Signer
	// signs the short-lived state cookie a sign-in carries. SSOLabel is the
	// provider's name as the front end's sign-in button shows it
	// (OIDC_DISPLAY_NAME); empty leaves the button to the front end's words.
	SSO      auth.IdentityProvider
	Signer   *signing.Signer
	SSOLabel string

	// Assertions vouch for the person signed in here to a service that
	// hosts agents, and publish the key that checks them. Nil means this
	// server has no key and makes none.
	Assertions *auth.Asserter

	// Calls bounds how fast one actor may call; SignIns bounds sign-in
	// attempts per email, and per address those that fail. Nil means no
	// limit.
	Calls   *ratelimit.Limiter
	SignIns *ratelimit.Limiter

	// MCP is the agents' door, mounted at /mcp beside the REST routes and
	// behind notRebound, but not behind their cross-origin guard (below). It
	// does its own authentication, with the same authenticator.
	MCP http.Handler
}

// MCPPath is where agents connect.
const MCPPath = "/mcp"

// NewHandler builds the whole HTTP surface.
func NewHandler(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	s := &server{Deps: d}
	if s.BodyTimeout <= 0 {
		s.BodyTimeout = DefaultBodyTimeout
	}
	if s.TransferTimeout <= 0 {
		s.TransferTimeout = DefaultTransferTimeout
	}
	if s.CookieSameSite == 0 {
		s.CookieSameSite = http.SameSiteLaxMode
	}
	for _, cidr := range d.TrustedProxies {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("httpapi: trusted proxy " + cidr + ": " + err.Error())
		}
		s.proxies = append(s.proxies, ipnet)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)

	if d.Pipeline != nil {
		mux.HandleFunc("GET "+MethodsPath, s.methods)
		mux.HandleFunc("POST /v1/auth/login", s.login)
		mux.HandleFunc("POST /v1/auth/invite", s.acceptInvite)
		if d.SSO != nil {
			if s.Signer == nil {
				panic("httpapi: single sign-on needs a Signer for its state cookie")
			}
			mux.HandleFunc("GET "+ssoStartPath, s.ssoStart)
			mux.HandleFunc("GET "+ssoReturnPath, s.ssoCallback)
		}
		mux.Handle("POST /v1/auth/logout", s.authenticated(s.logout))
		mux.Handle("POST "+AssertionPath, s.assertions(s.authenticated(s.assert)))
		mux.HandleFunc("GET "+KeysPath, s.keys)
		mux.HandleFunc("GET /v1/tools", s.listTools)
		mux.Handle("POST /v1/tools/{tool_name}", s.authenticated(s.callByName))
		for _, t := range d.Pipeline.Registry().Exposed() {
			if t.HTTP.Pattern != "" {
				mux.Handle(t.HTTP.Method+" "+t.HTTP.Pattern, s.authenticated(s.callTool(t)))
			}
		}
	}
	if d.MCP != nil {
		mux.Handle(MCPPath, s.notRebound(d.MCP))
	}
	if local, ok := d.Blob.(blob.Local); ok {
		mux.HandleFunc("PUT "+blob.BlobPath+"{token}", s.blobPut(local))
		mux.HandleFunc("GET "+blob.BlobPath+"{token}", s.blobGet(local))
	}

	// A browser sends cookies along with requests a hostile page makes it
	// send. Unsafe methods from another origin are refused unless the origin
	// is ours. Clients that are not browsers send neither Origin nor
	// Sec-Fetch-Site and are unaffected.
	guard := http.NewCrossOriginProtection()
	guard.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, apperr.Forbid("cross-origin requests are accepted only from this installation's own front end"))
	}))
	for _, o := range d.TrustedOrigins {
		if err := guard.AddTrustedOrigin(o); err != nil {
			panic("httpapi: trusted origin " + o + ": " + err.Error())
		}
	}
	// The agents' door takes a bearer token and never a cookie: a page on
	// another site has no credential of anyone's to bring there, so the
	// guard would protect nothing on it. And agents that run in a browser or
	// an app send an Origin of their own (Claude's custom connectors, from
	// claude.ai), which the guard refused.
	guard.AddInsecureBypassPattern(MCPPath)
	return s.logged(s.recovered(s.cors(guard.Handler(s.routed(mux)))))
}

// Deadlines, per request. The server's ReadHeaderTimeout bounds the headers;
// these bound the rest, so that a client trickling a body a byte at a time,
// or never reading its response, holds a connection for minutes, not for
// ever. A file transfer gets longer; everything else this API reads is under
// a megabyte.
const (
	DefaultBodyTimeout     = 30 * time.Second
	DefaultTransferTimeout = 10 * time.Minute
)

// maxTidied is the longest path, query included, the mux is left to tidy.
// The paths of our routes are shorter, but for a blob URL's, which is never
// untidy.
const maxTidied = 256

// routed answers for the routes the mux does not have. The mux's own 404 and
// 405 are plain text; everything else this API says, it says in JSON, and a
// client should not need a second parser for a mistyped path.
//
// The mux tidies a path by redirecting to the tidy one, which it repeats
// twice, escaped, in Location and in the page, and the query with it as it
// was sent: a byte sent could come back as six, before anyone is
// authenticated. So a long path it would tidy, or a short one with a long
// query, is answered as no route, whether or not the tidy one would match
// one.
func (s *server) routed(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.EscapedPath(); len(p)+len(r.URL.RawQuery) > maxTidied && path.Clean(p) != strings.TrimSuffix(p, "/") {
			s.writeError(w, r, apperr.Missing("no such route; GET /v1/tools lists what there is"))
			return
		}
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// No route. Ask the mux's own handler whether that is a 404 or a 405
		// (and which methods it would have taken), and say so in JSON.
		probe := &probeWriter{header: http.Header{}}
		h.ServeHTTP(probe, r)
		switch {
		case probe.status == http.StatusMethodNotAllowed:
			w.Header().Set("Allow", probe.header.Get("Allow"))
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: apperr.New("method_not_allowed",
				"%s is not something %s takes; it takes %s", r.Method, r.URL.Path, probe.header.Get("Allow"))})
		case probe.status >= 300 && probe.status < 400:
			// The mux tidying a path: /v1//tools → /v1/tools.
			h.ServeHTTP(w, r)
		default:
			s.writeError(w, r, apperr.Missing("no such route; GET /v1/tools lists what there is"))
		}
	})
}

// probeWriter takes a response and keeps only its status and headers.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

type server struct {
	Deps
	proxies []*net.IPNet
}

// ---------------------------------------------------------------------------
// Calling tools
// ---------------------------------------------------------------------------

type callerKey struct{}

func (s *server) callTool(t tool.Tool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// One key. Given twice, which of the two a retry carries would decide
		// whether it is the same call, so neither is taken, as a key named
		// twice in a body is not.
		if len(r.Header.Values(HeaderIdempotencyKey)) > 1 {
			s.writeError(w, r, apperr.Invalid("%s is given more than once", HeaderIdempotencyKey))
			return
		}
		args, err := buildArgs(t, r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		p := r.Context().Value(callerKey{}).(auth.Principal)
		out, err := s.Pipeline.Invoke(r.Context(), pipeline.Caller{ActorID: p.ActorID}, t.Name, args, r.Header.Get(HeaderIdempotencyKey))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeOutcome(w, out)
	}
}

// callByName is the generic entry point: POST /v1/tools/grade.submit with the
// arguments as the body. It is what a client that discovered the catalogue at
// GET /v1/tools uses, and it is the same call as the tool's own route.
func (s *server) callByName(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Pipeline.Registry().Get(r.PathValue("tool_name"))
	if !ok || t.Internal {
		s.writeError(w, r, apperr.Missing("there is no tool named %q", r.PathValue("tool_name")))
		return
	}
	t.HTTP = tool.Route{Method: http.MethodPost} // arguments come from the body alone
	s.callTool(t)(w, r)
}

type toolInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Kind         string `json:"kind"`
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	InputSchema  any    `json:"input_schema"`
	OutputSchema any    `json:"output_schema"`
}

func (s *server) listTools(w http.ResponseWriter, _ *http.Request) {
	var out struct {
		Tools []toolInfo `json:"tools"`
	}
	for _, t := range s.Pipeline.Registry().Exposed() {
		kind := "read"
		if t.Kind == tool.Write {
			kind = "write"
		}
		out.Tools = append(out.Tools, toolInfo{
			Name: t.Name, Description: t.Description, Kind: kind,
			Method: t.HTTP.Method, Path: t.HTTP.Pattern,
			InputSchema: t.InputSchema, OutputSchema: t.OutputSchema,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Who is calling
// ---------------------------------------------------------------------------

func (s *server) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := bearer(r)
		if presented == "" {
			if c, err := r.Cookie(SessionCookie); err == nil {
				presented = c.Value
			}
		}
		p, err := s.Auth.Authenticate(r.Context(), presented)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		// After authentication, so that the limit is the actor's and nobody
		// can spend it for them; before the pipeline, so that a call refused
		// here was never attempted and leaves no row.
		if ok, wait := s.Calls.Allow(p.ActorID.String()); !ok {
			s.tooMany(w, r, wait)
			return
		}
		noteActor(r, p.ActorID.String())
		next(w, r.WithContext(contextWith(r.Context(), p)))
	})
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

type loginIn struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginOut struct {
	ActorID   string    `json:"actor_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// login is not a tool: there is no actor yet to call one as. It sets the
// session cookie; the session itself is a credential row like any other and
// shows up in credential.list.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in loginIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&in); err != nil {
		s.writeError(w, r, apperr.Invalid("the body must be JSON with email and password"))
		return
	}
	// Guessing is limited twice over: by where it comes from, and by whose
	// account it is aimed at. Each attempt costs a 64 MiB argon2 hash, so
	// this protects the server as much as the password.
	//
	// The address is asked first, so that an attempt it refuses touches
	// nothing under the email: it neither spends the allowance of the account
	// it was aimed at nor leaves a bucket behind for ten minutes.
	var keys []string
	addr, known := s.clientAddr(r)
	if known {
		keys = append(keys, addrKey(addr))
	}
	keys = append(keys, emailKey(in.Email))
	for _, key := range keys {
		if ok, wait := s.SignIns.Allow(key); !ok {
			s.tooMany(w, r, wait)
			return
		}
	}
	sess, err := s.Auth.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// A sign-in that succeeds was no guess, and its address has it back. One
	// address may be a whole lecture hall (an IPv4 address behind a NAT, or
	// a campus LAN's /64), and the students in it signing in must not use up
	// what it is allowed and lock out the next one with the right password.
	// It is taken first all the same, so that no more attempts from one
	// address are hashed at once than it is allowed. The account keeps it
	// spent: nobody signs in to one account that often.
	if known {
		s.SignIns.Refund(addrKey(addr))
	}
	http.SetCookie(w, s.sessionCookie(sess.Token, sess.ExpiresAt))
	writeJSON(w, http.StatusOK, loginOut{ActorID: sess.ActorID.String(), ExpiresAt: sess.ExpiresAt})
}

type acceptInviteIn struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type acceptInviteOut struct {
	ActorID   string    `json:"actor_id"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
}

// acceptInvite takes up an invitation (actor.invite): the person chooses
// their password and is signed in, as by login. It is not a tool, for the
// same reason login is not. Attempts count against the address's sign-in
// allowance, and one that succeeds has it back, as with login; there is no
// account to key a second limit on until the invitation is found good, and
// a wrong one costs no hash.
func (s *server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var in acceptInviteIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&in); err != nil {
		s.writeError(w, r, apperr.Invalid("the body must be JSON with token and password"))
		return
	}
	addr, known := s.clientAddr(r)
	if known {
		if ok, wait := s.SignIns.Allow(addrKey(addr)); !ok {
			s.tooMany(w, r, wait)
			return
		}
	}
	acc, err := s.Auth.AcceptInvite(r.Context(), in.Token, in.Password)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if known {
		s.SignIns.Refund(addrKey(addr))
	}
	http.SetCookie(w, s.sessionCookie(acc.Token, acc.ExpiresAt))
	writeJSON(w, http.StatusOK, acceptInviteOut{ActorID: acc.ActorID.String(), Email: acc.Email, ExpiresAt: acc.ExpiresAt})
}

// addrKey is the sign-in limit's key for an address. An IPv6 address is
// keyed by its /64, the least one network is given: whoever holds it may
// send from any address in it, and keyed alone, each would be a fresh
// bucket. A /64 may as well be a LAN of many people, as an IPv4 address
// behind a NAT may be; they share only their guesses (see login). The zone
// a link-local peer comes with names our interface, not the peer, and is
// no part of the key. An IPv4 address written as IPv6 (::ffff:203.0.113.7),
// or shown to an IPv6-only server through NAT64 (64:ff9b::203.0.113.7), is
// that IPv4 address.
func addrKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return "addr:" + addr
	}
	ip = ip.WithZone("").Unmap()
	if nat64.Contains(ip) {
		a := ip.As16()
		ip = netip.AddrFrom4([4]byte(a[12:]))
	}
	if ip.Is4() {
		return "addr:" + ip.String()
	}
	return "addr:" + netip.PrefixFrom(ip, 64).Masked().String()
}

// nat64 is the well-known prefix (RFC 6052) under which NAT64 and SIIT show
// an IPv4 client to an IPv6-only server, with its address in the last 32
// bits. Keyed by its /64, every IPv4 client would share one bucket.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// emailKey is the sign-in limit's key for an email: one key for an account
// however its email is typed, and a small one however long the body made it.
func emailKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "email:" + hex.EncodeToString(sum[:])
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(callerKey{}).(auth.Principal)
	if err := s.Auth.Logout(r.Context(), p); err != nil {
		s.writeError(w, r, err)
		return
	}
	http.SetCookie(w, s.sessionCookie("", time.Time{}))
	w.WriteHeader(http.StatusNoContent)
}

// cookie is the one place cookies are made. HttpOnly keeps them from scripts;
// SameSite=Lax keeps them off cross-site subrequests, which is right when the
// front end is same-site with this server, and is None (with Secure) when it
// is not — the cross-origin guard is what stands against CSRF then; Secure
// keeps them off plain HTTP. Secure is on unless INSECURE_COOKIES says
// otherwise, which exists only so that a developer can sign in at
// http://localhost. No value means "forget it".
func (s *server) cookie(name, value, path string, expires time.Time) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // Secure is configurable on purpose; see above
		Name: name, Value: value, Path: path, Expires: expires,
		HttpOnly: true, Secure: !s.InsecureCookies, SameSite: s.CookieSameSite,
	}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

func (s *server) sessionCookie(value string, expires time.Time) *http.Cookie {
	return s.cookie(SessionCookie, value, "/", expires)
}

// cors lets the trusted front-end origins call with credentials. Any other
// origin gets no CORS headers, and the browser keeps the response from it.
func (s *server) cors(next http.Handler) http.Handler {
	trusted := map[string]bool{}
	for _, o := range s.TrustedOrigins {
		trusted[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MCPPath {
			mcpCORS(w, r, next)
			return
		}
		if origin := r.Header.Get("Origin"); trusted[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", HeaderReplayed)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+HeaderIdempotencyKey)
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// mcpCORS lets an agent harness that runs in a browser, or in an app's web
// view, use /mcp from any origin: it answers the browser's preflight itself,
// before the bearer check (a preflight carries no Authorization header), and
// lets the page read the answers and the headers MCP sets. It allows no
// credentials, and needs none: /mcp takes a bearer token and never a cookie,
// so a page gets nothing from it without a token it already holds.
func mcpCORS(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Header.Get("Origin") == "" {
		next.ServeHTTP(w, r)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, Mcp-Protocol-Version, WWW-Authenticate")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	next.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOutcome(w http.ResponseWriter, out pipeline.Outcome) {
	if out.Replayed {
		w.Header().Set(HeaderReplayed, "true")
	}
	writeJSON(w, outcomeStatus(out), out)
}

func outcomeStatus(out pipeline.Outcome) int {
	switch out.Status {
	case domain.StatusExecuted:
		return http.StatusOK
	case domain.StatusProposed:
		return http.StatusAccepted
	case domain.StatusDenied:
		return http.StatusForbidden
	default: // failed; or, replaying an old proposal, rejected or cancelled
		if out.Error != nil {
			return codeStatus(out.Error.Code)
		}
		return http.StatusConflict
	}
}

func codeStatus(c apperr.Code) int {
	switch c {
	case apperr.RateLimited:
		return http.StatusTooManyRequests
	case apperr.InvalidArgument:
		return http.StatusBadRequest
	case apperr.Unauthenticated:
		return http.StatusUnauthorized
	case apperr.Forbidden:
		return http.StatusForbidden
	case apperr.NotFound:
		return http.StatusNotFound
	case apperr.Conflict, apperr.IdempotencyConflict:
		return http.StatusConflict
	case apperr.FailedPrecondition:
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

type errorBody struct {
	Error *apperr.Error `json:"error"`
}

// writeError answers a call that was never attempted. Anything that is not an
// apperr is ours: it is logged in full and the caller is told nothing of it.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = apperr.Invalid("the request body is larger than %d bytes", maxBodyBytes)
	}
	e, ok := apperr.As(err)
	if !ok {
		s.Log.Error("internal error", "method", r.Method, "path", safePath(r.URL.Path), "err", err)
		e = &apperr.Error{Code: "internal", Message: "something went wrong on our side; the call can be retried with the same idempotency key"}
	}
	if e.Code == apperr.Unauthenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="aishiteru"`)
	}
	writeJSON(w, codeStatus(e.Code), errorBody{Error: e})
}
