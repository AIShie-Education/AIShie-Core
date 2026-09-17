package config

import (
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	t.Run("defaults work for local development", func(t *testing.T) {
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.DatabaseURL == "" || c.HTTPAddr != ":8080" || c.ProposalTTL != 14*24*time.Hour || c.SessionTTL != 12*time.Hour || c.InsecureCookies {
			t.Fatalf("defaults: %+v", c)
		}
	})
	t.Run("everything can be set", func(t *testing.T) {
		t.Setenv("PROPOSAL_TTL", "48h")
		t.Setenv("SESSION_TTL", "30m")
		t.Setenv("TRUSTED_ORIGINS", "https://lms.example.edu, http://localhost:5173 ,")
		t.Setenv("INSECURE_COOKIES", "true")
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.ProposalTTL != 48*time.Hour || c.SessionTTL != 30*time.Minute || !c.InsecureCookies ||
			len(c.TrustedOrigins) != 2 || c.TrustedOrigins[1] != "http://localhost:5173" {
			t.Fatalf("%+v", c)
		}
	})
	for key, bad := range map[string]string{"PROPOSAL_TTL": "two weeks", "SESSION_TTL": "-1h", "INSECURE_COOKIES": "maybe"} {
		t.Run("rejects "+key+"="+bad, func(t *testing.T) {
			t.Setenv(key, bad)
			if _, err := FromEnv(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
