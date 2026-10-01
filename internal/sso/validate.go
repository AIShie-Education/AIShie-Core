package sso

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
)

// A provider's settings, each held to what a sign-in can use and the
// database keeps (migration 0022). Every refusal is invalid_argument, names
// the field in details.field, and says what is wrong without repeating a
// secret.

var idRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Bounds of the settings, in characters for the name on the button, and in
// bytes for the rest, which are ASCII but for an issuer's host in a script
// of its own.
const (
	MaxDisplayName  = 64
	maxIssuer       = 500
	maxClientID     = 500
	maxClientSecret = 500
	maxScopes       = 20
	maxScope        = 100
	maxClaim        = 200
	maxDomains      = 50
	maxDomain       = 253
)

func invalid(field, format string, args ...any) *apperr.Error {
	return apperr.Invalid(field+": "+format, args...).With("field", field)
}

// CheckID holds a provider's id to its shape: 1 to 64 lower-case letters,
// digits and hyphens, beginning and ending with a letter or a digit. It is
// what credential.provider records and what a sign-in starts with, and it
// never changes.
func CheckID(id string) error {
	if !idRE.MatchString(id) {
		return invalid("id", "1 to 64 lower-case letters, digits and hyphens, beginning and ending with a letter or a digit, such as polyu-adfs")
	}
	return nil
}

// CheckDisplayName is the name on the sign-in button, trimmed: 1 to 64
// characters of UTF-8, every one of them drawn, as OIDC_DISPLAY_NAME is
// (no control character, and none that draws nothing or turns the text
// around).
func CheckDisplayName(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch n := utf8.RuneCountInString(v); {
	case !utf8.ValidString(v):
		return "", invalid("display_name", "is not UTF-8")
	case n == 0:
		return "", invalid("display_name", "is required: the sign-in button shows it")
	case n > MaxDisplayName:
		return "", invalid("display_name", "is %d characters long; at most %d", n, MaxDisplayName)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return "", invalid("display_name", "has %U in it, which is not a printable character", r)
		}
	}
	return v, nil
}

// CheckIssuer is the provider's issuer, trimmed, written exactly as the
// provider writes it in its discovery document: an https URL with a host,
// and no user, query or fragment. Unless private (SSO_ALLOW_PRIVATE_ISSUERS),
// its host is no address that is not Public, nor localhost: the server would
// refuse to fetch from it (issuer_address_not_allowed). http is taken only
// for this machine (localhost, 127.0.0.0/8, ::1), and so only when private,
// for development and tests: anywhere else the client secret would cross
// the network in the clear.
func CheckIssuer(v string, private bool) (string, error) {
	v = strings.TrimSpace(v)
	u, err := url.Parse(v)
	switch {
	case v == "":
		return "", invalid("issuer", "is required, such as https://adfs.example.edu/adfs")
	case len(v) > maxIssuer:
		return "", invalid("issuer", "is %d bytes long; at most %d", len(v), maxIssuer)
	case err != nil || u.Opaque != "" || u.Hostname() == "" || strings.ContainsAny(v, " \t\r\n"):
		return "", invalid("issuer", "is not an absolute URL with a host, such as https://adfs.example.edu/adfs")
	case u.Scheme == "http" && !Loopback(u.Hostname()):
		return "", errIssuerHTTP(private)
	case u.Scheme != "https" && u.Scheme != "http":
		return "", invalid("issuer", "is not an https URL")
	case u.User != nil:
		return "", invalid("issuer", "carries a user name or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(v, "#"):
		return "", invalid("issuer", "has a query or a fragment, which an issuer never has")
	case !private && !publicHost(u.Hostname()):
		return "", errIssuerNotPublic("issuer")
	}
	return v, nil
}

// errIssuerHTTP refuses an http issuer elsewhere than on this machine,
// which is taken only when private: without it, it says nothing of this
// machine, which is refused for its address.
func errIssuerHTTP(private bool) *apperr.Error {
	if private {
		return invalid("issuer", "is http: an identity provider is reached over https, but on this machine")
	}
	return invalid("issuer", "is http: an identity provider is reached over https")
}

// errIssuerNotPublic refuses field for naming this machine, or an address
// that is not Public, as an issuer, without SSO_ALLOW_PRIVATE_ISSUERS.
func errIssuerNotPublic(field string) *apperr.Error {
	return invalid(field, "is %s", notPublicWhy).With("reason", ReasonAddressNotAllowed)
}

// Loopback reports whether host is this machine.
func Loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckClientID is the client id the provider gave this site, trimmed.
func CheckClientID(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", invalid("client_id", "is required: the provider gives it when the site is registered there")
	case len(v) > maxClientID:
		return "", invalid("client_id", "is %d bytes long; at most %d", len(v), maxClientID)
	case !visible(v, true):
		return "", invalid("client_id", "has a character that is not printable ASCII")
	}
	return v, nil
}

// CheckClientSecret is the client secret, trimmed: printable ASCII, as
// OAuth 2.0 defines one (RFC 6749, VSCHAR). What is wrong is said without
// repeating it.
func CheckClientSecret(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", invalid("client_secret", "is required: the provider gives it with the client id")
	case len(v) > maxClientSecret:
		return "", invalid("client_secret", "is %d bytes long; at most %d", len(v), maxClientSecret)
	case !visible(v, true):
		return "", invalid("client_secret", "has a character that is not printable ASCII, which a client secret never has")
	}
	return v, nil
}

// visible reports whether s is printable ASCII: with spaces when spaces is
// true (VSCHAR), without them otherwise (a scope's or a claim's name).
func visible(s string, spaces bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > '~' || c < ' ' || (c == ' ' && !spaces) {
			return false
		}
	}
	return true
}

// DefaultScopes are what a sign-in asks for when nothing else is said.
var DefaultScopes = []string{"openid", "profile", "email"}

// CheckScopes are the scopes a sign-in asks for: openid among them, each a
// scope token (RFC 6749: printable ASCII but a space, " and \), at most 20,
// each once, in the order given. None given is DefaultScopes.
func CheckScopes(v []string) ([]string, error) {
	if len(v) == 0 {
		return slices.Clone(DefaultScopes), nil
	}
	var out []string
	for _, s := range v {
		s = strings.TrimSpace(s)
		if s == "" || len(s) > maxScope || !visible(s, false) || strings.ContainsAny(s, `"\`) {
			return nil, invalid("scopes", "%q is not a scope: printable ASCII with no space, quote or backslash, at most %d bytes", apperr.Clip(s), maxScope)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	switch {
	case !slices.Contains(out, "openid"):
		return nil, invalid("scopes", "must include openid: a sign-in is OpenID Connect")
	case len(out) > maxScopes:
		return nil, invalid("scopes", "are %d; at most %d", len(out), maxScopes)
	}
	return out, nil
}

// CheckClaim is the name of a claim of the provider's id_token, trimmed:
// printable ASCII with no space, at most 200 bytes. A claim may be named by a
// URI, as ADFS names some.
func CheckClaim(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxClaim || !visible(v, false) {
		return "", invalid(field, "is not a claim's name: printable ASCII with no space, 1 to %d bytes, such as sub, upn or email", maxClaim)
	}
	return v, nil
}

var domainLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckDomains are the domains an email may be linked from: each a domain
// name (polyu.edu.hk), lower-cased, without an @ before it, at least two
// labels long, each once, at most 50. An email is of a domain when what
// follows its @ is that domain exactly: a subdomain is a domain of its own.
func CheckDomains(v []string) ([]string, error) {
	out := []string{}
	for _, d := range v {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		labels := strings.Split(d, ".")
		ok := len(d) <= maxDomain && len(labels) >= 2
		for _, l := range labels {
			ok = ok && domainLabelRE.MatchString(l)
		}
		if !ok {
			return nil, invalid("allowed_email_domains", "%q is not a domain name such as polyu.edu.hk", apperr.Clip(d))
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if len(out) > maxDomains {
		return nil, invalid("allowed_email_domains", "are %d; at most %d", len(out), maxDomains)
	}
	return out, nil
}

// EmailDomain is what follows an email's @, lower-cased, or "" for what is
// not an email.
func EmailDomain(email string) string {
	i := strings.LastIndex(email, "@")
	if i <= 0 || i == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[i+1:])
}

// InDomains reports whether email is of one of domains.
func InDomains(email string, domains []string) bool {
	d := EmailDomain(email)
	return d != "" && slices.Contains(domains, d)
}

// CheckPosition is a provider's place on the sign-in page: 0 to 10000.
func CheckPosition(p int) error {
	if p < 0 || p > 10000 {
		return invalid("position", "%d is not from 0 to 10000", p)
	}
	return nil
}

// describe names what is wrong with a setting for a message, as the checks
// above would.
func describe(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Message
	}
	return fmt.Sprint(err)
}
