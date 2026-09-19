package main

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gablooge/sluiceway/internal/testdb"
)

// runMigrate runs "sluiceway migrate" the way main does, against databaseURL, with text logs so the
// count can be read back. The deadline bounds a command that wrongly waits.
func runMigrate(t *testing.T, databaseURL string) (code int, printed string) {
	t.Helper()
	getenv := func(k string) string {
		switch k {
		case "SLUICEWAY_DATABASE_URL":
			return databaseURL
		case "SLUICEWAY_LOG_FORMAT":
			return "text"
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var out, errOut bytes.Buffer
	code = run(ctx, []string{"migrate"}, getenv, &out, &errOut)
	if ctx.Err() != nil {
		t.Fatal("migrate was still going at the deadline")
	}
	return code, out.String() + errOut.String()
}

func TestMigrateAppliesOnceAndThenNothing(t *testing.T) {
	tdb := testdb.New(t)

	code, printed := runMigrate(t, tdb.URL)
	if code != 0 {
		t.Fatalf("first migrate: exit = %d, output: %s", code, printed)
	}
	if !strings.Contains(printed, `msg="migrations applied"`) || strings.Contains(printed, "count=0") {
		t.Errorf("first migrate does not report what it applied: %s", printed)
	}

	code, printed = runMigrate(t, tdb.URL)
	if code != 0 {
		t.Fatalf("second migrate: exit = %d, output: %s", code, printed)
	}
	if !strings.Contains(printed, `msg="migrations applied"`) || !strings.Contains(printed, "count=0") {
		t.Errorf("second migrate does not report that it applied nothing: %s", printed)
	}
}

func TestMigrateRefusesASuperuserAndSaysWhy(t *testing.T) {
	tdb := testdb.New(t)

	code, printed := runMigrate(t, tdb.AdminURL)
	if code != 1 {
		t.Errorf("migrate as a superuser: exit = %d, want 1", code)
	}
	if !strings.Contains(printed, "unsafe database role") || !strings.Contains(printed, "SUPERUSER") {
		t.Errorf("the refusal does not say why: %s", printed)
	}
	// Refused means nothing was applied: the application role still finds every migration pending.
	code, printed = runMigrate(t, tdb.URL)
	if code != 0 || strings.Contains(printed, "count=0") {
		t.Errorf("after the refusal, migrate as the application role: exit = %d, output: %s", code, printed)
	}
}

// What store.Open returns is what this command logs, so the rule that no part of the database URL
// reaches the log is checked here as well, where the log is.
func TestMigrateNeverLogsAPartOfTheDatabaseURL(t *testing.T) {
	tdb := testdb.New(t)
	target, err := url.Parse(tdb.URL)
	if err != nil {
		t.Fatal("the test database URL does not parse")
	}

	unreachable := "postgres://leakuser:hunter2@leakhost.invalid:5432/leakdb?sslmode=disable&connect_timeout=5"
	wrongLogin := *target
	wrongLogin.User = url.UserPassword("leakuser", "hunter2")
	wrongLogin.Path = "/leakdb"

	for name, databaseURL := range map[string]string{
		"host that does not resolve":   unreachable,
		"wrong login on a real server": wrongLogin.String(),
	} {
		t.Run(name, func(t *testing.T) {
			code, printed := runMigrate(t, databaseURL)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(printed, "SLUICEWAY_DATABASE_URL") {
				t.Errorf("the log does not name the variable: %s", printed)
			}
			for _, frag := range []string{"leakuser", "hunter2", "leakhost", "leakdb", target.Port()} {
				if strings.Contains(printed, frag) {
					t.Errorf("the log leaks %q: %s", frag, printed)
				}
			}
		})
	}
}
