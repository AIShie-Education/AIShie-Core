package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/memory"
)

func TestFromEnv(t *testing.T) {
	t.Run("defaults work for local development", func(t *testing.T) {
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.DatabaseURL == "" || c.HTTPAddr != ":8080" || c.ProposalTTL != 14*24*time.Hour || c.SessionTTL != 12*time.Hour || c.InsecureCookies ||
			!c.AgentSelfService || c.AgentMaxPerOwner != 5 || !c.JoinLinkRegistration || c.JoinRegistrationsPerMinute != 60 ||
			c.SignInsPerMinute != 10 || c.LongPollWaiters != 1000 || c.LongPollWaitersPerActor != 16 {
			t.Fatalf("defaults: %+v", c)
		}
		// A message carries ten files of 50 MiB, a conversation 500 MiB.
		if c.AttachmentMaxBytes != 50<<20 || c.AttachmentMaxPerMessage != 10 || c.AttachmentMaxConversationBytes != 500<<20 {
			t.Fatalf("attachments' defaults: %d %d %d", c.AttachmentMaxBytes, c.AttachmentMaxPerMessage, c.AttachmentMaxConversationBytes)
		}
		// An export holds 100,000 messages and 256 MiB of their text, and
		// is kept a day.
		if c.ExportMaxMessages != 100000 || c.ExportMaxBytes != 256<<20 || c.ExportTTL != 24*time.Hour {
			t.Fatalf("exports' defaults: %d %d %s", c.ExportMaxMessages, c.ExportMaxBytes, c.ExportTTL)
		}
		// Memory stays off until what forgets it on time is in place; its
		// limits are there whether or not.
		if c.Memory != (memory.Config{}).WithDefaults() || c.Memory.Enabled || c.Memory.MaxOwner != 200 || c.Memory.MaxAsker != 50 ||
			c.Memory.MaxShared != 200 || c.Memory.MaxProposed != 50 || c.Memory.MaxPerAgent != 5000 ||
			c.Memory.WritesPerHour != 60 || c.Memory.WritesPerDay != 300 {
			t.Fatalf("memory's defaults: %+v", c.Memory)
		}
	})
	t.Run("everything can be set", func(t *testing.T) {
		t.Setenv("PROPOSAL_TTL", "48h")
		t.Setenv("SESSION_TTL", "30m")
		t.Setenv("TRUSTED_ORIGINS", "https://lms.example.edu, http://localhost:5173 ,")
		t.Setenv("INSECURE_COOKIES", "true")
		t.Setenv("AGENT_SELF_SERVICE", "off")
		t.Setenv("AGENT_MAX_PER_OWNER", "2")
		t.Setenv("JOIN_LINK_REGISTRATION", "off")
		t.Setenv("JOIN_REGISTRATIONS_PER_MINUTE", "5")
		t.Setenv("MEMORY", "on")
		t.Setenv("MEMORY_MAX_OWNER", "10")
		t.Setenv("MEMORY_MAX_ASKER", "11")
		t.Setenv("MEMORY_MAX_SHARED", "12")
		t.Setenv("MEMORY_MAX_PROPOSED", "13")
		t.Setenv("MEMORY_MAX_PER_AGENT", "14")
		t.Setenv("MEMORY_WRITES_PER_HOUR", "15")
		t.Setenv("MEMORY_WRITES_PER_DAY", "16")
		t.Setenv("LONG_POLL_WAITERS", "0")
		t.Setenv("LONG_POLL_WAITERS_PER_ACTOR", "4")
		t.Setenv("ATTACHMENT_MAX_BYTES", "1048576")
		t.Setenv("ATTACHMENT_MAX_PER_MESSAGE", "3")
		t.Setenv("ATTACHMENT_MAX_CONVERSATION_BYTES", "10485760")
		t.Setenv("EXPORT_MAX_MESSAGES", "5000")
		t.Setenv("EXPORT_MAX_BYTES", "1048576")
		t.Setenv("EXPORT_TTL", "2h")
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.ProposalTTL != 48*time.Hour || c.SessionTTL != 30*time.Minute || !c.InsecureCookies ||
			c.AgentSelfService || c.AgentMaxPerOwner != 2 || c.JoinLinkRegistration || c.JoinRegistrationsPerMinute != 5 ||
			len(c.TrustedOrigins) != 2 || c.TrustedOrigins[1] != "http://localhost:5173" ||
			c.LongPollWaiters != 0 || c.LongPollWaitersPerActor != 4 {
			t.Fatalf("%+v", c)
		}
		if want := (memory.Config{Enabled: true, MaxOwner: 10, MaxAsker: 11, MaxShared: 12, MaxProposed: 13, MaxPerAgent: 14,
			WritesPerHour: 15, WritesPerDay: 16}); c.Memory != want {
			t.Fatalf("memory: %+v, want %+v", c.Memory, want)
		}
		if c.AttachmentMaxBytes != 1<<20 || c.AttachmentMaxPerMessage != 3 || c.AttachmentMaxConversationBytes != 10<<20 {
			t.Fatalf("attachments: %d %d %d", c.AttachmentMaxBytes, c.AttachmentMaxPerMessage, c.AttachmentMaxConversationBytes)
		}
		if c.ExportMaxMessages != 5000 || c.ExportMaxBytes != 1<<20 || c.ExportTTL != 2*time.Hour {
			t.Fatalf("exports: %d %d %s", c.ExportMaxMessages, c.ExportMaxBytes, c.ExportTTL)
		}
	})
	t.Run("a file a message carries is never larger than an upload", func(t *testing.T) {
		t.Setenv("MAX_UPLOAD_BYTES", "1000")
		t.Setenv("ATTACHMENT_MAX_BYTES", "2000")
		c, err := FromEnv()
		if err != nil || c.AttachmentMaxBytes != 1000 {
			t.Fatalf("ATTACHMENT_MAX_BYTES above MAX_UPLOAD_BYTES comes to %d (%v), want 1000", c.AttachmentMaxBytes, err)
		}
	})
	t.Run("S3 names the bucket as S3_BUCKET_LOOKUP says", func(t *testing.T) {
		t.Setenv("BLOB_STORE", "s3")
		t.Setenv("S3_ENDPOINT", "objects.example.edu")
		t.Setenv("S3_BUCKET", "aishie")
		t.Setenv("SIGNING_KEY", strings.Repeat("k", 32))
		// By default as the S3 client judges by the endpoint, as it always
		// has.
		c, err := FromEnv()
		if err != nil || c.S3.BucketLookup != "auto" || c.S3.Region != "us-east-1" || !c.S3.UseSSL {
			t.Fatalf("S3 by default: %+v %v", c.S3, err)
		}
		for _, lookup := range []string{"auto", "path", "dns"} {
			t.Setenv("S3_BUCKET_LOOKUP", lookup)
			if c, err := FromEnv(); err != nil || c.S3.BucketLookup != lookup {
				t.Fatalf("S3_BUCKET_LOOKUP=%s comes to %q (%v)", lookup, c.S3.BucketLookup, err)
			}
		}
		for _, bad := range []string{"virtual", "DNS", "path ", "host"} {
			t.Setenv("S3_BUCKET_LOOKUP", bad)
			if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "S3_BUCKET_LOOKUP") {
				t.Fatalf("S3_BUCKET_LOOKUP=%q: %v", bad, err)
			}
		}
	})
	for key, bad := range map[string]string{"PROPOSAL_TTL": "two weeks", "SESSION_TTL": "-1h", "INSECURE_COOKIES": "maybe",
		"AGENT_SELF_SERVICE": "yes", "AGENT_MAX_PER_OWNER": "0", "MEMORY": "true", "MEMORY_MAX_ASKER": "0",
		"MEMORY_WRITES_PER_DAY": "many", "MEMORY_MAX_PER_AGENT": "-5", "JOIN_LINK_REGISTRATION": "no",
		"JOIN_REGISTRATIONS_PER_MINUTE": "-1", "LONG_POLL_WAITERS": "lots", "LONG_POLL_WAITERS_PER_ACTOR": "-1",
		"ATTACHMENT_MAX_BYTES": "0", "ATTACHMENT_MAX_PER_MESSAGE": "101", "ATTACHMENT_MAX_CONVERSATION_BYTES": "lots",
		"EXPORT_MAX_MESSAGES": "0", "EXPORT_MAX_BYTES": "-1", "EXPORT_TTL": "10m"} {
		t.Run("rejects "+key+"="+bad, func(t *testing.T) {
			t.Setenv(key, bad)
			if _, err := FromEnv(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// OIDC_DISPLAY_NAME is shown on the front end's sign-in button as it is, so
// it is taken only if it is short and every character of it is drawn. A bad
// one is refused whether single sign-on is on or not.
func TestOIDCDisplayName(t *testing.T) {
	onOff := map[bool]string{false: "off", true: "on"}
	sso := func(t *testing.T, on bool) {
		if on {
			t.Setenv("OIDC_ISSUER", "https://adfs.example.edu/adfs")
			t.Setenv("OIDC_CLIENT_ID", "aishie")
			t.Setenv("SIGNING_KEY", "an installation's signing key, 32+ characters long")
		}
	}
	for _, on := range []bool{false, true} {
		for _, tc := range []struct{ name, value, want string }{
			{"no name", "", ""},
			{"a name", "PolyU NetID", "PolyU NetID"},
			{"a name in any script", "理大 NetID", "理大 NetID"},
			{"a name without the white space around it", "  PolyU NetID \t", "PolyU NetID"},
			{"white space alone as no name", "   ", ""},
			{"64 characters", strings.Repeat("理", 64), strings.Repeat("理", 64)},
		} {
			t.Run(fmt.Sprintf("takes %s, single sign-on %s", tc.name, onOff[on]), func(t *testing.T) {
				sso(t, on)
				t.Setenv("OIDC_DISPLAY_NAME", tc.value)
				c, err := FromEnv()
				if err != nil || c.OIDC.DisplayName != tc.want || c.OIDC.Enabled() != on {
					t.Fatalf("%v %q", err, c.OIDC.DisplayName)
				}
			})
		}
		for _, tc := range []struct{ name, value, why string }{
			{"65 characters", strings.Repeat("a", 65), "65 characters long; at most 64"},
			{"a newline", "PolyU\nNetID", "U+000A"},
			{"a tab", "PolyU\tNetID", "U+0009"},
			{"a terminal escape", "PolyU \x1b[31mNetID", "U+001B"},
			{"a delete", "PolyU\x7fNetID", "U+007F"},
			{"a zero-width space", "Poly\u200bU NetID", "U+200B"},
			{"a right-to-left override", "\u202eDIteN UyloP", "U+202E"},
			{"a non-breaking space", "PolyU\u00a0NetID", "U+00A0"},
			{"bytes that are not UTF-8", "PolyU \xff", "not UTF-8"},
		} {
			t.Run(fmt.Sprintf("refuses %s, single sign-on %s", tc.name, onOff[on]), func(t *testing.T) {
				sso(t, on)
				t.Setenv("OIDC_DISPLAY_NAME", tc.value)
				if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "OIDC_DISPLAY_NAME") || !strings.Contains(err.Error(), tc.why) {
					t.Fatalf("accepted, or refused for another reason: %v", err)
				}
			})
		}
	}
}

func TestAssertionSettings(t *testing.T) {
	const signingKey = "an installation's signing key, 32+ characters long"
	seed := bytes.Repeat([]byte{7}, 32)
	t.Setenv("PUBLIC_URL", "https://test.aishie.app")
	t.Run("off by default, with a five-minute lifetime", func(t *testing.T) {
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.RuntimeAudiences != nil || c.AssertionKey != nil || c.AssertionTTL != 5*time.Minute {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("everything can be set", func(t *testing.T) {
		t.Setenv("RUNTIME_AUDIENCES", " https://test.aishie.app/runtime, http://localhost:9091/runtime ,")
		t.Setenv("ASSERTION_KEY", base64.StdEncoding.EncodeToString(seed))
		t.Setenv("ASSERTION_TTL", "15m")
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(c.RuntimeAudiences, []string{"https://test.aishie.app/runtime", "http://localhost:9091/runtime"}) ||
			!bytes.Equal(c.AssertionKey, seed) || c.AssertionTTL != 15*time.Minute {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("a key in base64 of any common kind", func(t *testing.T) {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			t.Setenv("ASSERTION_KEY", enc.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)))
			if c, err := FromEnv(); err != nil || !bytes.Equal(c.AssertionKey, bytes.Repeat([]byte{0xfb}, 32)) {
				t.Fatalf("%v %x", err, c.AssertionKey)
			}
		}
	})
	// Audiences need a key that is the same on every instance and after a
	// restart: either will do.
	t.Run("audiences need a key", func(t *testing.T) {
		t.Setenv("RUNTIME_AUDIENCES", "https://test.aishie.app/runtime")
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "ASSERTION_KEY or SIGNING_KEY") {
			t.Fatalf("with neither key: %v", err)
		}
		t.Setenv("SIGNING_KEY", signingKey)
		if _, err := FromEnv(); err != nil {
			t.Fatalf("with SIGNING_KEY: %v", err)
		}
		t.Setenv("SIGNING_KEY", "")
		t.Setenv("ASSERTION_KEY", base64.StdEncoding.EncodeToString(seed))
		if _, err := FromEnv(); err != nil {
			t.Fatalf("with ASSERTION_KEY: %v", err)
		}
	})
	// The issuer is PUBLIC_URL; its localhost default is no runtime's.
	t.Run("audiences need PUBLIC_URL", func(t *testing.T) {
		t.Setenv("SIGNING_KEY", signingKey)
		t.Setenv("RUNTIME_AUDIENCES", "https://test.aishie.app/runtime")
		t.Setenv("PUBLIC_URL", "")
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "RUNTIME_AUDIENCES needs PUBLIC_URL") {
			t.Fatalf("without PUBLIC_URL: %v", err)
		}
		t.Setenv("RUNTIME_AUDIENCES", "")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("without audiences, PUBLIC_URL may be left to its default: %v", err)
		}
	})
	for _, bad := range []string{"test.aishie.app/runtime", "/runtime", "ftp://test.aishie.app/runtime", "mailto:ops@aishie.app",
		"https://", "https:///runtime", "https://:443/runtime", "https://ops:secret@test.aishie.app/runtime",
		"https://test.aishie.app/runtime?tenant=1", "https://test.aishie.app/runtime?", "https://test.aishie.app/runtime#top",
		"https://test.aishie.app/runtime#", "HTTPS://test.aishie.app/runtime", "https://test.aishie.app/run time", "https://test.aishie.app/%zz",
		// The same place written another way, or a way out of the path.
		"https://Test.aishie.app/runtime", "https://[FE80::1]/runtime", "https://test.aishie.app:443/runtime", "http://test.aishie.app:80/runtime",
		"https://test.aishie.app:/runtime", "https://test.aishie.app:0443/runtime", "https://test.aishie.app:99999/runtime",
		"https://test.aishie.app:0/runtime", "https://[::1]:/runtime",
		"https://test.aishie.app/%72untime", "https://test.aishie.app/a%2Fb", "https://test.aishie.app/a%2fb", "https://test.aishie.app/%2E%2E/admin",
		"https://test.aishie.app/runtime/../admin", "https://test.aishie.app/runtime/..", "https://test.aishie.app/./runtime",
		"https://test.aishie.app/runtime/.", "https://test.aishie.app//runtime", "https://test.aishie.app/runtime//api",
		"https://test.aishie.app/runtime\\..\\admin", "https://тест.example/runtime", "https://test.aishie.app/run!time", "https://test.aishie.app/run%21time",
		"https://test.aishie.app/(runtime)", "https://test.aishie.app/runtime*"} {
		t.Run("rejects the audience "+bad, func(t *testing.T) {
			t.Setenv("SIGNING_KEY", signingKey)
			t.Setenv("RUNTIME_AUDIENCES", "https://test.aishie.app/runtime,"+bad)
			if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "RUNTIME_AUDIENCES") {
				t.Fatalf("accepted, or refused for another reason: %v", err)
			}
		})
	}
	// Written the one way, each is taken as it is.
	for _, good := range []string{"https://test.aishie.app/runtime", "https://test.aishie.app/runtime/", "https://test.aishie.app",
		"https://test.aishie.app/", "http://localhost:9091/runtime", "https://[::1]:9091/runtime", "https://[::1]/runtime",
		"https://test.aishie.app:8443/runtime", "http://test.aishie.app:443/runtime", "https://test.aishie.app/run%20time/v1"} {
		t.Run("takes the audience "+good, func(t *testing.T) {
			t.Setenv("SIGNING_KEY", signingKey)
			t.Setenv("RUNTIME_AUDIENCES", good)
			if c, err := FromEnv(); err != nil || !slices.Equal(c.RuntimeAudiences, []string{good}) {
				t.Fatalf("%v %q", err, c.RuntimeAudiences)
			}
		})
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString(seed[:16]), base64.StdEncoding.EncodeToString(append(seed, 1))} {
		t.Run("rejects the key "+bad, func(t *testing.T) {
			t.Setenv("ASSERTION_KEY", bad)
			_, err := FromEnv()
			if err == nil || !strings.Contains(err.Error(), "ASSERTION_KEY") {
				t.Fatalf("accepted, or refused for another reason: %v", err)
			}
			// The key is a secret: a refusal names the setting, not its value.
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("the refusal repeats the key: %v", err)
			}
		})
	}
	for _, bad := range []string{"59s", "15m1s", "1h", "0", "-5m", "five minutes"} {
		t.Run("rejects ASSERTION_TTL="+bad, func(t *testing.T) {
			t.Setenv("ASSERTION_TTL", bad)
			if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "ASSERTION_TTL") {
				t.Fatalf("accepted, or refused for another reason: %v", err)
			}
		})
	}
	for _, good := range []string{"1m", "15m", "90s"} {
		t.Setenv("ASSERTION_TTL", good)
		if _, err := FromEnv(); err != nil {
			t.Errorf("ASSERTION_TTL=%s: %v", good, err)
		}
	}
}

// SECRETS_KEY seals identity providers' client secrets, and
// SECRETS_KEY_PREVIOUS opens what older keys sealed, for as long as a
// rotation takes. Each is 32 bytes in base64; a bad one is refused, saying
// where and never repeating it.
func TestSecretsKey(t *testing.T) {
	const signingKey = "an installation's signing key, 32+ characters long"
	current, older, oldest := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	b64 := base64.StdEncoding.EncodeToString
	t.Run("off by default", func(t *testing.T) {
		c, err := FromEnv()
		if err != nil || c.SecretsKey != nil || c.SecretsKeysPrevious != nil {
			t.Fatalf("%v %+v", err, c)
		}
	})
	t.Run("a key and the keys before it", func(t *testing.T) {
		t.Setenv("SIGNING_KEY", signingKey)
		t.Setenv("SECRETS_KEY", b64(current))
		t.Setenv("SECRETS_KEY_PREVIOUS", " "+base64.RawURLEncoding.EncodeToString(older)+", "+b64(oldest)+" ,")
		c, err := FromEnv()
		if err != nil || !bytes.Equal(c.SecretsKey, current) || len(c.SecretsKeysPrevious) != 2 ||
			!bytes.Equal(c.SecretsKeysPrevious[0], older) || !bytes.Equal(c.SecretsKeysPrevious[1], oldest) {
			t.Fatalf("%v %+v", err, c)
		}
	})
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a short key":             {map[string]string{"SIGNING_KEY": signingKey, "SECRETS_KEY": b64(current[:16])}, "SECRETS_KEY is 16 bytes, not 32"},
		"a key in hex":            {map[string]string{"SIGNING_KEY": signingKey, "SECRETS_KEY": strings.Repeat("ab", 32)}, "SECRETS_KEY is 48 bytes, not 32"},
		"a key that is no base64": {map[string]string{"SIGNING_KEY": signingKey, "SECRETS_KEY": "not a key at all!"}, "SECRETS_KEY is not base64"},
		"a bad old key": {map[string]string{"SIGNING_KEY": signingKey, "SECRETS_KEY": b64(current),
			"SECRETS_KEY_PREVIOUS": b64(older) + "," + b64(oldest[:31])}, "SECRETS_KEY_PREVIOUS: the key at position 2 is 31 bytes"},
		"old keys with no key":      {map[string]string{"SIGNING_KEY": signingKey, "SECRETS_KEY_PREVIOUS": b64(older)}, "SECRETS_KEY_PREVIOUS needs SECRETS_KEY"},
		"a key with no SIGNING_KEY": {map[string]string{"SECRETS_KEY": b64(current)}, "SECRETS_KEY needs SIGNING_KEY"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := FromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("accepted, or refused for another reason: %v", err)
			}
			if v := tc.env["SECRETS_KEY"]; v != "" && strings.Contains(err.Error(), v) {
				t.Fatalf("the refusal repeats the key: %v", err)
			}
		})
	}
}
