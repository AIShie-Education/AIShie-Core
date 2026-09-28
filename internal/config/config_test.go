package config

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	t.Run("defaults work for local development", func(t *testing.T) {
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.DatabaseURL == "" || c.HTTPAddr != ":8080" || c.ProposalTTL != 14*24*time.Hour || c.SessionTTL != 12*time.Hour || c.InsecureCookies ||
			!c.AgentSelfService || c.AgentMaxPerOwner != 5 {
			t.Fatalf("defaults: %+v", c)
		}
	})
	t.Run("everything can be set", func(t *testing.T) {
		t.Setenv("PROPOSAL_TTL", "48h")
		t.Setenv("SESSION_TTL", "30m")
		t.Setenv("TRUSTED_ORIGINS", "https://lms.example.edu, http://localhost:5173 ,")
		t.Setenv("INSECURE_COOKIES", "true")
		t.Setenv("AGENT_SELF_SERVICE", "off")
		t.Setenv("AGENT_MAX_PER_OWNER", "2")
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.ProposalTTL != 48*time.Hour || c.SessionTTL != 30*time.Minute || !c.InsecureCookies ||
			c.AgentSelfService || c.AgentMaxPerOwner != 2 ||
			len(c.TrustedOrigins) != 2 || c.TrustedOrigins[1] != "http://localhost:5173" {
			t.Fatalf("%+v", c)
		}
	})
	for key, bad := range map[string]string{"PROPOSAL_TTL": "two weeks", "SESSION_TTL": "-1h", "INSECURE_COOKIES": "maybe",
		"AGENT_SELF_SERVICE": "yes", "AGENT_MAX_PER_OWNER": "0"} {
		t.Run("rejects "+key+"="+bad, func(t *testing.T) {
			t.Setenv(key, bad)
			if _, err := FromEnv(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestAssertionSettings(t *testing.T) {
	const signingKey = "an installation's signing key, 32+ characters long"
	seed := bytes.Repeat([]byte{7}, 32)
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
	for _, bad := range []string{"test.aishie.app/runtime", "/runtime", "ftp://test.aishie.app/runtime", "mailto:ops@aishie.app",
		"https://", "https:///runtime", "https://:443/runtime", "https://ops:secret@test.aishie.app/runtime",
		"https://test.aishie.app/runtime?tenant=1", "https://test.aishie.app/runtime?", "https://test.aishie.app/runtime#top",
		"https://test.aishie.app/runtime#", "HTTPS://test.aishie.app/runtime", "https://test.aishie.app/run time", "https://test.aishie.app/%zz"} {
		t.Run("rejects the audience "+bad, func(t *testing.T) {
			t.Setenv("SIGNING_KEY", signingKey)
			t.Setenv("RUNTIME_AUDIENCES", "https://test.aishie.app/runtime,"+bad)
			if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "RUNTIME_AUDIENCES") {
				t.Fatalf("accepted, or refused for another reason: %v", err)
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
