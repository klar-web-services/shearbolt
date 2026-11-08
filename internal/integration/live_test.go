//go:build integration

package integration

import (
	"testing"
	"time"
)

// The one test on today's database: ad hoc flags, default policy (48-hour age limit).
// lodash 4.17.20 must stay vulnerable; if this fails, check whether an advisory was withdrawn
// before suspecting Shearbolt.
func TestLiveDatabaseSmoke(t *testing.T) {
	code, r := liveScan(t, "--source", fixture("projects", "npm-vulnerable"))
	if code != 1 || !blocked(r, "", "lodash", "4.17.20") {
		t.Fatalf("exit = %d, errors = %+v, findings = %+v", code, r.Errors, r.Findings)
	}
	if r.DBArchive != "" {
		t.Fatalf("dbArchive = %q, want empty for the live database", r.DBArchive)
	}
	if r.Database == nil || time.Since(r.Database.Built) > 48*time.Hour {
		t.Fatalf("database = %+v, want one built within 48 hours", r.Database)
	}
}
