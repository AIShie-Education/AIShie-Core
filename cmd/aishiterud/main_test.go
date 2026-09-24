package main

import (
	"strings"
	"testing"
)

// A rollback's `migrate up` changes nothing, and a revert that took a
// migration out looks the same; the deploy log is where either shows.
func TestSchemaReportSaysASchemaIsAhead(t *testing.T) {
	for _, c := range []struct {
		version, latest uint
		dirty           bool
		want            string
	}{
		{3, 3, false, "schema version 3 (embedded latest 3)"},
		{2, 3, false, "schema version 2 (embedded latest 3)"},
		{4, 3, false, "schema version 4 (embedded latest 3) AHEAD"},
		{4, 3, true, "schema version 4 (embedded latest 3) DIRTY"},
		{3, 3, true, "schema version 3 (embedded latest 3) DIRTY"},
	} {
		got := schemaReport(c.version, c.latest, c.dirty)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("schemaReport(%d, %d, %v) = %q, want it to start with %q", c.version, c.latest, c.dirty, got, c.want)
		}
		if !strings.Contains(c.want, "AHEAD") && strings.Contains(got, "AHEAD") {
			t.Errorf("schemaReport(%d, %d, %v) = %q, which is not ahead", c.version, c.latest, c.dirty, got)
		}
	}
}
