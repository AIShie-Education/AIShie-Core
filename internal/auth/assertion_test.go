package auth_test

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/testdb"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
)

// testSigningKey stands for an installation's SIGNING_KEY.
const testSigningKey = "an installation's signing key, 32+ characters long"

// The key derived from testSigningKey, worked out apart from this code:
// HKDF-SHA256 with Python's hmac module, the Ed25519 public key with
// OpenSSL, and the RFC 7638 thumbprint by hand. It is the same in every
// release, or an upgrade would change the key every runtime has cached.
const (
	pinnedX   = "F41qp5rATKmPEA5gp2HRkYgYhWAGK0t5gQHbUTQjQIQ"
	pinnedKid = "voqq_vnOFJYWvpqQXQrwg4d1x8BfU38XrcfqxGSBfrQ"
)

const (
	runtimeAud = "https://lms.example.edu/runtime"
	issuer     = "https://lms.example.edu"
)

func TestTheAssertionKey(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	fromSeed, err := auth.AssertionKey(seed, testSigningKey)
	if err != nil || !fromSeed.Equal(ed25519.NewKeyFromSeed(seed)) {
		t.Fatalf("ASSERTION_KEY is not the key it is the seed of: %v", err)
	}

	derived, err := auth.AssertionKey(nil, testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	// HKDF-SHA256 (RFC 5869) with no salt, worked out here with HMAC alone.
	prk := hmacSHA256(make([]byte, sha256.Size), []byte(testSigningKey))
	okm := hmacSHA256(prk, append([]byte("aishiteru/runtime-assertion/v1"), 1))
	if !derived.Equal(ed25519.NewKeyFromSeed(okm)) {
		t.Fatal("the derived key is not HKDF-SHA256 of SIGNING_KEY with the documented info")
	}
	pub := derived.Public().(ed25519.PublicKey)
	if x := base64.RawURLEncoding.EncodeToString(pub); x != pinnedX {
		t.Fatalf("the key derived from a SIGNING_KEY changed: %s, want %s", x, pinnedX)
	}

	// The same after a restart; another with another SIGNING_KEY, and with
	// an ASSERTION_KEY, whatever SIGNING_KEY says.
	again, _ := auth.AssertionKey(nil, testSigningKey)
	other, _ := auth.AssertionKey(nil, testSigningKey+"!")
	if !again.Equal(derived) || other.Equal(derived) || fromSeed.Equal(derived) {
		t.Fatal("the derived key is not a function of SIGNING_KEY alone, or ASSERTION_KEY does not take its place")
	}

	for name, c := range map[string]struct {
		seed       []byte
		signingKey string
	}{
		"a seed too short":        {seed[:31], testSigningKey},
		"a seed too long":         {append(bytes.Clone(seed), 0), testSigningKey},
		"a signing key too short": {nil, strings.Repeat("k", 31)},
	} {
		if _, err := auth.AssertionKey(c.seed, c.signingKey); err == nil || errors.Is(err, auth.ErrNoAssertionKey) {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
	if _, err := auth.AssertionKey(nil, ""); !errors.Is(err, auth.ErrNoAssertionKey) {
		t.Errorf("no key at all: %v", err)
	}
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func TestAnAsserterNeedsWhatItSigns(t *testing.T) {
	key, _ := auth.AssertionKey(nil, testSigningKey)
	good := auth.AssertionConfig{Issuer: issuer, Audiences: []string{runtimeAud}, TTL: time.Minute, Key: key}
	for name, adjust := range map[string]func(*auth.AssertionConfig){
		"no key":            func(c *auth.AssertionConfig) { c.Key = nil },
		"half a key":        func(c *auth.AssertionConfig) { c.Key = c.Key[:32] },
		"no issuer":         func(c *auth.AssertionConfig) { c.Issuer = "" },
		"no lifetime":       func(c *auth.AssertionConfig) { c.TTL = 0 },
		"an empty audience": func(c *auth.AssertionConfig) { c.Audiences = []string{runtimeAud, ""} },
	} {
		c := good
		adjust(&c)
		if _, err := auth.NewAsserter(nil, c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	a, err := auth.NewAsserter(nil, auth.AssertionConfig{Issuer: issuer, TTL: time.Minute, Key: key})
	if err != nil || a.Issues() {
		t.Fatalf("with no audience it publishes its key and makes no assertion: %v", err)
	}
	if a.KeyID() != pinnedKid {
		t.Fatalf("kid %s, want the key's RFC 7638 thumbprint %s", a.KeyID(), pinnedKid)
	}
}

// people is a database with a person, an administrator, an agent and a
// suspended person in it.
type people struct {
	pool                     *pgxpool.Pool
	sato, admin, agent, gone uuid.UUID
}

func newPeople(t *testing.T) people {
	t.Helper()
	pool := testdb.New(t)
	q := dbq.New(pool)
	add := func(kind, name string, email, role *string) uuid.UUID {
		t.Helper()
		id := ids.New()
		if err := q.InsertActor(t.Context(), dbq.InsertActorParams{ID: id, Kind: kind, DisplayName: name, Email: email,
			PlatformRole: role, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	email, admin := "sato@example.edu", "admin"
	p := people{pool: pool}
	p.sato = add("human", "Sato <Instructor> & co", &email, nil)
	p.admin = add("human", "Ada", nil, &admin)
	p.agent = add("agent", "grader-v2", nil, nil)
	p.gone = add("human", "Gone", nil, nil)
	if _, err := pool.Exec(t.Context(), `UPDATE actor SET status = 'suspended' WHERE id = $1`, p.gone); err != nil {
		t.Fatal(err)
	}
	return p
}

// token is what actor presents: an agent's API token, which lasts until
// expires or for ever; a person's session, as signing in makes one, which
// lasts until expires or for as long as a session does. A person holds no
// API token.
func (p people) token(t *testing.T, actor uuid.UUID, expires *time.Time) string {
	t.Helper()
	q := dbq.New(p.pool)
	a, err := q.GetActor(t.Context(), actor)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind == "agent" {
		tok, _, err := auth.IssueToken(t.Context(), q, actor, nil, "test", expires, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok.Full
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if expires == nil {
		e := time.Now().Add(auth.DefaultSessionTTL)
		expires = &e
	}
	label := "password login"
	if err := q.InsertCredential(t.Context(), dbq.InsertCredentialParams{ID: ids.New(), ActorID: actor, Kind: auth.KindSession,
		SecretHash: &tok.Hash, TokenPrefix: &tok.Prefix, Label: &label, ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return tok.Full
}

// verified checks an assertion as a runtime would, with go-jose rather
// than anything of ours, against the key set as it is published, and
// returns its header and every claim it makes.
func verified(t *testing.T, keySet any, assertion string, now time.Time) (jose.Header, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(keySet)
	if err != nil {
		t.Fatal(err)
	}
	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &jwks); err != nil {
		t.Fatalf("the key set does not parse as a JWKS: %v", err)
	}
	tok, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		t.Fatalf("not an EdDSA JWT: %v", err)
	}
	h := tok.Headers[0]
	keys := jwks.Key(h.KeyID)
	if len(keys) != 1 {
		t.Fatalf("the key set has no key %q", h.KeyID)
	}
	var std jwt.Claims
	var all map[string]any
	if err := tok.Claims(keys[0].Key, &std, &all); err != nil {
		t.Fatalf("the signature does not check: %v", err)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{runtimeAud}, Time: now}, 0); err != nil {
		t.Fatalf("the claims do not validate: %v", err)
	}
	return h, all
}

func TestAnAssertionSaysWhoIsSignedIn(t *testing.T) {
	p := newPeople(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 28, 10, 0, 0, 500_000_000, time.UTC)
	clock := func() time.Time { return now }
	authn := auth.NewAuthenticator(p.pool, time.Hour)
	authn.SetClock(clock)
	key, _ := auth.AssertionKey(nil, testSigningKey)
	a, err := auth.NewAsserter(p.pool, auth.AssertionConfig{Issuer: issuer, Audiences: []string{"http://localhost:9091/runtime", runtimeAud},
		TTL: 5 * time.Minute, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	a.SetClock(clock)
	principal := func(token string) auth.Principal {
		t.Helper()
		pr, err := authn.Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}

	// Sato, signed in with a password: a session.
	sess, err := authn.StartSession(ctx, p.sato, "password login")
	if err != nil {
		t.Fatal(err)
	}
	sp := principal(sess.Token)
	got, err := a.Assert(ctx, sp, runtimeAud)
	if err != nil {
		t.Fatal(err)
	}
	h, claims := verified(t, a.KeySet(), got.Token, now)
	if h.Algorithm != "EdDSA" || h.KeyID != pinnedKid || h.ExtraHeaders["typ"] != "JWT" {
		t.Fatalf("header: %+v", h)
	}
	compact(t, got.Token, key.Public().(ed25519.PublicKey))
	jti, _ := claims["jti"].(string)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`).MatchString(jti) {
		t.Fatalf("jti %q is not 128 random bits", jti)
	}
	delete(claims, "jti")
	iat, exp := float64(now.Unix()), float64(now.Unix()+300)
	want := map[string]any{"iss": issuer, "aud": runtimeAud, "sub": p.sato.String(), "iat": iat, "nbf": iat, "exp": exp,
		"kind": "human", "name": "Sato <Instructor> & co", "email": "sato@example.edu", "sid": sp.CredentialID.String()}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("claims:\n got %v\nwant %v", claims, want)
	}
	if !got.ExpiresAt.Equal(time.Unix(now.Unix()+300, 0)) || got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("expires_at %s, want five minutes on", got.ExpiresAt)
	}
	if strings.Contains(got.Token, sess.Token) || strings.Contains(string(mustDecode(t, got.Token)), sess.Token[17:]) {
		t.Fatal("the assertion carries the session")
	}
	// Another is another: its own jti.
	if again, _ := a.Assert(ctx, sp, runtimeAud); again.Token == got.Token {
		t.Fatal("two assertions are the same token")
	}

	// An administrator, signed in for longer than an assertion lasts: the
	// role, and no email, since there is none; the session is the sid.
	ap := principal(p.token(t, p.admin, nil))
	got, err = a.Assert(ctx, ap, runtimeAud)
	if err != nil {
		t.Fatal(err)
	}
	_, claims = verified(t, a.KeySet(), got.Token, now)
	if claims["platform_role"] != "admin" || claims["name"] != "Ada" || claims["sid"] != ap.CredentialID.String() || claims["exp"] != exp {
		t.Fatalf("the administrator's claims: %v", claims)
	}
	if _, ok := claims["email"]; ok {
		t.Fatalf("an email for someone who has none: %v", claims)
	}
	if _, ok := claims["login_id"]; ok {
		t.Fatalf("a login ID for someone who has none: %v", claims)
	}
	// A person's login ID, their staff or student number, as their email is.
	if _, err := p.pool.Exec(ctx, `UPDATE actor SET login_id = 'T19880042' WHERE id = $1`, p.admin); err != nil {
		t.Fatal(err)
	}
	if got, err = a.Assert(ctx, ap, runtimeAud); err != nil {
		t.Fatal(err)
	}
	if _, claims = verified(t, a.KeySet(), got.Token, now); claims["login_id"] != "T19880042" {
		t.Fatalf("the login ID: %v", claims)
	}

	// No longer than the credential, to the second, rounded down.
	ends := now.Add(90*time.Second + 700*time.Millisecond)
	got, err = a.Assert(ctx, principal(p.token(t, p.sato, &ends)), runtimeAud)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(time.Unix(ends.Unix(), 0)) || got.ExpiresAt.After(ends) {
		t.Fatalf("with a token that ends at %s: expires_at %s", ends, got.ExpiresAt)
	}
	if _, claims = verified(t, a.KeySet(), got.Token, now); claims["exp"] != float64(ends.Unix()) {
		t.Fatalf("exp %v, want %d", claims["exp"], ends.Unix())
	}
	// A credential that ends within the second is as good as gone.
	ends = now.Add(300 * time.Millisecond)
	if _, err := a.Assert(ctx, principal(p.token(t, p.sato, &ends)), runtimeAud); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("a credential about to end: %v", err)
	}

	// Refused.
	for name, c := range map[string]struct {
		p        auth.Principal
		audience string
		want     apperr.Code
	}{
		"an audience not listed":          {sp, "https://evil.example/runtime", apperr.InvalidArgument},
		"no audience":                     {sp, "", apperr.InvalidArgument},
		"a listed audience with a slash":  {sp, runtimeAud + "/", apperr.InvalidArgument},
		"a listed audience in upper case": {sp, strings.ToUpper(runtimeAud), apperr.InvalidArgument},
		"a suspended person":              {principal(p.token(t, p.gone, nil)), runtimeAud, apperr.Forbidden},
		"an agent":                        {principal(p.token(t, p.agent, nil)), runtimeAud, apperr.Forbidden},
		"nobody":                          {auth.Principal{ActorID: uuid.New(), CredentialID: uuid.New()}, runtimeAud, apperr.Unauthenticated},
	} {
		if got, err := a.Assert(ctx, c.p, c.audience); !apperr.Is(err, c.want) || got.Token != "" {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	// A person whose password someone else set gets none until they have
	// set their own, whatever they come with.
	if _, err := auth.SetTemporaryPassword(ctx, dbq.New(p.pool), p.admin, p.sato, "abcd-efgh-jkmn-pqrs", "set by Sato", now); err != nil {
		t.Fatal(err)
	}
	_, err = a.Assert(ctx, ap, runtimeAud)
	if e, ok := apperr.As(err); !ok || e.Code != apperr.Forbidden || e.Details["reason"] != "password_change_required" {
		t.Fatalf("a temporary password: %v", err)
	}
	if err := auth.SetPassword(ctx, dbq.New(p.pool), p.admin, "adas own password", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Assert(ctx, ap, runtimeAud); err != nil {
		t.Fatalf("once she has set her own: %v", err)
	}
	// Suspended after signing in: the session still authenticates, and gets
	// no assertion.
	if _, err := p.pool.Exec(ctx, `UPDATE actor SET status = 'suspended' WHERE id = $1`, p.sato); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Assert(ctx, principal(sess.Token), runtimeAud); !apperr.Is(err, apperr.Forbidden) {
		t.Fatalf("suspended since signing in: %v", err)
	}
}

// What a runtime checks is checked: a signature changed, a claim changed,
// or a key not published here is refused, and an assertion is no credential
// here, whatever it carries.
func TestAnAssertionIsCheckedAndIsNoCredential(t *testing.T) {
	p := newPeople(t)
	ctx := t.Context()
	authn := auth.NewAuthenticator(p.pool, time.Hour)
	key, _ := auth.AssertionKey(nil, testSigningKey)
	cfg := auth.AssertionConfig{Issuer: issuer, Audiences: []string{runtimeAud}, TTL: 5 * time.Minute, Key: key}
	a, err := auth.NewAsserter(p.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	token := p.token(t, p.sato, nil)
	pr, err := authn.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Assert(ctx, pr, runtimeAud)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	verified(t, a.KeySet(), got.Token, now)

	// The server restarted with the same SIGNING_KEY publishes the same key,
	// and what it signed before still checks.
	restarted, err := auth.NewAsserter(p.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restarted.KeySet(), a.KeySet()) {
		t.Fatal("the key set changed across a restart")
	}
	verified(t, restarted.KeySet(), got.Token, now)
	// With an ASSERTION_KEY it is another key, under another kid.
	cfg.Key, _ = auth.AssertionKey(bytes.Repeat([]byte{9}, 32), testSigningKey)
	own, err := auth.NewAsserter(p.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if own.KeyID() == a.KeyID() || reflect.DeepEqual(own.KeySet(), a.KeySet()) {
		t.Fatal("ASSERTION_KEY did not take the derived key's place")
	}
	forged, _ := own.Assert(ctx, pr, runtimeAud)

	parts := strings.Split(got.Token, ".")
	claims := mustDecode(t, got.Token)
	promoted := bytes.Replace(claims, []byte(`"kind":"human"`), []byte(`"kind":"human","platform_role":"root"`), 1)
	published := ed25519.PublicKey(nil)
	if k, err := base64.RawURLEncoding.DecodeString(a.KeySet().Keys[0].X); err == nil {
		published = k
	}
	for name, bad := range map[string]string{
		"a signature with a bit flipped": testkit.Forged(t, got.Token),
		"a claim added":                  parts[0] + "." + base64.RawURLEncoding.EncodeToString(promoted) + "." + parts[2],
		"signed with a key not published here, under this key's id": swapKid(t, forged.Token, a.KeyID()),
	} {
		tok, err := jwt.ParseSigned(bad, []jose.SignatureAlgorithm{jose.EdDSA})
		if err != nil {
			t.Fatalf("%s: does not parse, so proves nothing: %v", name, err)
		}
		var c map[string]any
		if err := tok.Claims(published, &c); err == nil {
			t.Errorf("%s: it checks", name)
		}
	}

	// Core itself takes none of it, however it is presented.
	for name, presented := range map[string]string{
		"the assertion": got.Token, "its signature": parts[2], "Bearer and the assertion": "Bearer " + got.Token,
		"ais_ and the assertion": "ais_" + got.Token,
	} {
		if _, err := authn.Authenticate(ctx, presented); !apperr.Is(err, apperr.Unauthenticated) {
			t.Errorf("Authenticate(%s): %v, want unauthenticated", name, err)
		}
	}
	if _, err := authn.AcceptInvite(ctx, got.Token, "a long enough password"); !apperr.Is(err, apperr.Unauthenticated) {
		t.Errorf("the assertion as an invitation: %v", err)
	}
}

// swapKid gives a token another key id in its header, and so another
// signing input: what is left is a token whose signature is by a key that
// is not the one its header names.
func swapKid(t *testing.T, token, kid string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(h, &hdr); err != nil {
		t.Fatal(err)
	}
	hdr["kid"] = kid
	h, _ = json.Marshal(hdr)
	return base64.RawURLEncoding.EncodeToString(h) + "." + parts[1] + "." + parts[2]
}

// compact checks by hand, rather than with a library that might forgive
// what it reads, that an assertion is a JWS in compact serialisation as
// RFC 7515 and RFC 7519 write one: three parts of base64url with no padding;
// a header of alg EdDSA, typ JWT and the key's thumbprint as kid, and
// nothing else; claims whose times are whole numbers of seconds; and an
// Ed25519 signature by pub over exactly the first two parts and the dot
// between them.
func compact(t *testing.T, token string, pub ed25519.PublicKey) {
	t.Helper()
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$`).MatchString(token) {
		t.Fatalf("not three parts of unpadded base64url, the last a 64-byte signature: %q", token)
	}
	parts := strings.Split(token, ".")
	enc := base64.RawURLEncoding.Strict()
	decode := func(part string) map[string]any {
		t.Helper()
		b, err := enc.DecodeString(part)
		if err != nil {
			t.Fatalf("%q is not strict base64url: %v", part, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var v map[string]any
		if err := dec.Decode(&v); err != nil || dec.More() {
			t.Fatalf("%s is not one JSON object: %v", b, err)
		}
		return v
	}
	thumb := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	if h := decode(parts[0]); !reflect.DeepEqual(h, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": base64.RawURLEncoding.EncodeToString(thumb[:])}) {
		t.Fatalf("header %v", h)
	}
	claims := decode(parts[1])
	for _, name := range []string{"iat", "nbf", "exp"} {
		n, ok := claims[name].(json.Number)
		if _, err := n.Int64(); !ok || err != nil || strings.ContainsAny(n.String(), ".eE+-") {
			t.Fatalf("%s is %v, not a whole number of seconds", name, claims[name])
		}
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatalf("the signature is not Ed25519 over the header and the claims: %v", err)
	}
}

// mustDecode is an assertion's claims, as the JSON they were signed as.
func mustDecode(t *testing.T, token string) []byte {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", token)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The published key set is exactly one Ed25519 key, in the members RFC 8037
// names, with the id every JOSE library would work out for it.
func TestTheKeySetIsOneEd25519Key(t *testing.T) {
	key, _ := auth.AssertionKey(nil, testSigningKey)
	a, err := auth.NewAsserter(nil, auth.AssertionConfig{Issuer: issuer, TTL: time.Minute, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a.KeySet())
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "x": pinnedX, "kid": pinnedKid, "alg": "EdDSA", "use": "sig"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("key set:\n got %s\nwant %v", raw, want)
	}
	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("go-jose reads it as %+v: %v", jwks, err)
	}
	k := jwks.Keys[0]
	if pub, ok := k.Key.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) || !k.Valid() || !k.IsPublic() {
		t.Fatalf("go-jose's key: %+v", k)
	}
	if tp, err := k.Thumbprint(crypto.SHA256); err != nil || base64.RawURLEncoding.EncodeToString(tp) != a.KeyID() {
		t.Fatalf("go-jose's thumbprint: %x %v, kid %s", tp, err, a.KeyID())
	}
}
