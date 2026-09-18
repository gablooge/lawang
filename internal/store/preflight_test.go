package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/testdb"
)

// The tests in this file change roles and memberships, which are cluster-wide. Each takes a
// Postgres of its own from testdb.NewRawCluster, so nothing it breaks, and nothing a failed
// cleanup leaves behind, can reach another test. Do not move them onto the shared container, and
// do not make them parallel: the cases inside one test share that test's cluster.

// TestOpenRefusesUnsafeHelperRoles breaks one thing at a time about the helper roles and expects
// the preflight to refuse the start. What it must catch is the effective state, however it came
// about: a plain transaction of the application role must never hold a helper role's privileges.
func TestOpenRefusesUnsafeHelperRoles(t *testing.T) {
	tdb := testdb.NewRawCluster(t)
	testdb.Bootstrap(t, tdb.AdminURL)
	// A second administrator, so that memberships can be granted by someone other than the role
	// that ran the bootstrap. Postgres keeps one membership row per grantor.
	testdb.Exec(t, tdb.AdminURL, `
		CREATE ROLE second_admin NOLOGIN;
		GRANT sluiceway_resolver, sluiceway_worker TO second_admin WITH ADMIN OPTION`)

	cases := []struct {
		name    string
		breakIt string
		restore string
		// inherits is what pg_has_role(helper, 'USAGE') must report for the application role
		// while broken, to prove the case builds the state it claims to.
		helper   store.Role
		inherits bool
		// escalate, if set, is run as the APPLICATION role once Open has given its verdict, and
		// must leave it inheriting the helper role. It proves the refused state is one the
		// application role could turn into inheritance by itself, with no administrator involved.
		escalate string
		wantErr  error
	}{
		{
			name:    "helper role is BYPASSRLS",
			breakIt: "ALTER ROLE sluiceway_worker BYPASSRLS",
			restore: "ALTER ROLE sluiceway_worker NOBYPASSRLS",
			helper:  store.RoleWorker,
			wantErr: store.ErrUnsafeRole,
		},
		{
			name:    "helper role is SUPERUSER",
			breakIt: "ALTER ROLE sluiceway_resolver SUPERUSER",
			restore: "ALTER ROLE sluiceway_resolver NOSUPERUSER",
			helper:  store.RoleResolver,
			wantErr: store.ErrUnsafeRole,
		},
		{
			name:     "membership is INHERIT TRUE",
			breakIt:  "GRANT sluiceway_worker TO sluiceway WITH INHERIT TRUE",
			restore:  "GRANT sluiceway_worker TO sluiceway WITH INHERIT FALSE",
			helper:   store.RoleWorker,
			inherits: true,
			wantErr:  store.ErrUnsafeRole,
		},
		{
			name: "helper role is inherited through an intermediate role",
			breakIt: `CREATE ROLE ops NOLOGIN;
				GRANT sluiceway_worker TO ops;
				GRANT ops TO sluiceway`,
			restore:  "DROP ROLE ops",
			helper:   store.RoleWorker,
			inherits: true,
			wantErr:  store.ErrUnsafeRole,
		},
		{
			name:     "a second grantor adds an INHERIT TRUE membership beside the correct one",
			breakIt:  "GRANT sluiceway_resolver TO sluiceway WITH INHERIT TRUE GRANTED BY second_admin",
			restore:  "REVOKE sluiceway_resolver FROM sluiceway GRANTED BY second_admin",
			helper:   store.RoleResolver,
			inherits: true,
			wantErr:  store.ErrUnsafeRole,
		},
		{
			name:    "membership is SET FALSE",
			breakIt: "GRANT sluiceway_resolver TO sluiceway WITH SET FALSE",
			restore: "GRANT sluiceway_resolver TO sluiceway WITH SET TRUE",
			helper:  store.RoleResolver,
			wantErr: store.ErrUnsafeRole,
		},
		{
			name:    "membership is missing",
			breakIt: "REVOKE sluiceway_worker FROM sluiceway",
			restore: "GRANT sluiceway_worker TO sluiceway WITH INHERIT FALSE, SET TRUE",
			helper:  store.RoleWorker,
			wantErr: store.ErrUnsafeRole,
		},
		{
			// For the same grantor this only adds ADMIN to the correct row: USAGE stays false and
			// SET stays true. But a role that administers a helper role can grant it to itself
			// again WITH INHERIT TRUE, after the preflight has run.
			name:     "membership carries ADMIN OPTION",
			breakIt:  "GRANT sluiceway_worker TO sluiceway WITH ADMIN OPTION",
			restore:  "REVOKE ADMIN OPTION FOR sluiceway_worker FROM sluiceway CASCADE",
			helper:   store.RoleWorker,
			escalate: "GRANT sluiceway_worker TO sluiceway WITH INHERIT TRUE",
			wantErr:  store.ErrUnsafeRole,
		},
		{
			// The same through a role the login does not even inherit: it can SET ROLE to it.
			name: "ADMIN OPTION is held through an intermediate role that is not inherited",
			breakIt: `CREATE ROLE ops NOLOGIN;
				GRANT sluiceway_resolver TO ops WITH ADMIN OPTION;
				GRANT ops TO sluiceway WITH INHERIT FALSE, SET TRUE`,
			restore: `REVOKE sluiceway_resolver FROM ops CASCADE;
				DROP ROLE ops`,
			helper: store.RoleResolver,
			escalate: `SET ROLE ops;
				GRANT sluiceway_resolver TO sluiceway WITH INHERIT TRUE`,
			wantErr: store.ErrUnsafeRole,
		},
		{
			// Not a broken setup at all: two administrators have each run the bootstrap grants, so
			// there are two correct membership rows per helper role. It must start.
			name: "two grantors, every membership correct",
			breakIt: `GRANT sluiceway_resolver TO sluiceway WITH INHERIT FALSE, SET TRUE GRANTED BY second_admin;
				GRANT sluiceway_worker TO sluiceway WITH INHERIT FALSE, SET TRUE GRANTED BY second_admin`,
			restore: `REVOKE sluiceway_resolver FROM sluiceway GRANTED BY second_admin;
				REVOKE sluiceway_worker FROM sluiceway GRANTED BY second_admin`,
			helper:  store.RoleWorker,
			wantErr: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The state every case starts from is a correct one. This also proves the previous
			// case's restore worked, so a refusal below is caused by this case and nothing else.
			mustOpen(t, tdb.URL).Close()

			// Registered before breaking anything, so a break that fails halfway is still undone.
			t.Cleanup(func() { testdb.Exec(t, tdb.AdminURL, tc.restore) })
			testdb.Exec(t, tdb.AdminURL, tc.breakIt)

			if got := inheritsHelper(t, tdb.URL, tc.helper); got != tc.inherits {
				t.Fatalf("while broken, pg_has_role(%s, 'USAGE') = %v, want %v: the case does not build the state it names",
					tc.helper, got, tc.inherits)
			}

			db, err := store.Open(testCtx(t), tdb.URL)
			if db != nil {
				db.Close()
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Open: %v, want a correct setup to be accepted", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Open: err = %v, want %v", err, tc.wantErr)
			}
			if tc.escalate != "" {
				testdb.Exec(t, tdb.URL, tc.escalate)
				if !inheritsHelper(t, tdb.URL, tc.helper) {
					t.Fatalf("the application role could not make itself inherit %s: the case does not build the state it names", tc.helper)
				}
			}
		})
	}

	// And the cluster is left correct.
	mustOpen(t, tdb.URL).Close()
}

// TestBootstrapRunsAsACreateroleAdmin runs the bootstrap the way managed Postgres makes you run
// it: as a non-superuser administrator that has CREATEROLE, and CREATE on the database. It needs
// a cluster of its own because the roles must not exist yet: a CREATEROLE role may only grant
// roles it created, or was given ADMIN OPTION on.
func TestBootstrapRunsAsACreateroleAdmin(t *testing.T) {
	tdb := testdb.NewRawCluster(t)
	ctx := testCtx(t)

	testdb.Exec(t, tdb.AdminURL, "CREATE ROLE dba LOGIN CREATEROLE NOSUPERUSER PASSWORD 'dba'")
	dbaURL := testdb.As(t, tdb.AdminURL, "dba", "dba")

	// Without CREATE on the database the script fails at its last step, the schema. It must leave
	// nothing behind, least of all an application role holding its memberships in a database
	// that has no schema for it.
	var denied *pgconn.PgError
	if err := testdb.TryBootstrap(t, dbaURL); !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatalf("bootstrap without CREATE on the database: err = %v, want permission denied (42501): the premise of this check is gone", err)
	}
	var leftBehind int
	queryRow(t, tdb.AdminURL, "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'sluiceway%'", &leftBehind)
	if leftBehind != 0 {
		t.Fatalf("a failed bootstrap left %d roles behind, want it to apply completely or not at all", leftBehind)
	}

	testdb.Exec(t, tdb.AdminURL,
		"DO $$ BEGIN EXECUTE format('GRANT CREATE ON DATABASE %I TO dba', current_database()); END $$")

	testdb.Bootstrap(t, dbaURL)
	testdb.Bootstrap(t, dbaURL) // and it is safe to run again as the same administrator

	db := mustOpen(t, tdb.URL)
	if _, err := db.Migrate(ctx, quiet); err != nil {
		t.Fatalf("Migrate after a CREATEROLE bootstrap: %v", err)
	}
	for _, role := range []store.Role{store.RoleResolver, store.RoleWorker} {
		if err := db.RoleTx(ctx, role, func(pgx.Tx) error { return nil }); err != nil {
			t.Errorf("RoleTx(%s) after a CREATEROLE bootstrap: %v", role, err)
		}
	}
	db.Close()

	// The schema belongs to the application role, and the script leaves the administrator with
	// nothing it did not have before: it cannot act as the application role.
	conn, err := pgx.Connect(ctx, dbaURL)
	if err != nil {
		t.Fatal("connect as dba failed")
	}
	defer func() { _ = conn.Close(ctx) }()
	var owner string
	var dbaCanSet, dbaInherits bool
	err = conn.QueryRow(ctx, `
		SELECT (SELECT r.rolname FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
		         WHERE n.nspname = $1),
		       pg_has_role('sluiceway', 'SET'), pg_has_role('sluiceway', 'USAGE')`, store.Schema).
		Scan(&owner, &dbaCanSet, &dbaInherits)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "sluiceway" {
		t.Errorf("schema owner = %q, want sluiceway", owner)
	}
	if dbaCanSet || dbaInherits {
		t.Errorf("after the bootstrap the administrator can SET ROLE sluiceway (%v) or inherits it (%v), want neither",
			dbaCanSet, dbaInherits)
	}

	// A second administrator, here the superuser, runs it again. Every membership now exists once
	// per grantor, all of them correct, and the service must still start.
	testdb.Bootstrap(t, tdb.AdminURL)
	var rows int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_auth_members m
		 WHERE m.member = 'sluiceway'::regrole
		   AND m.roleid IN ('sluiceway_resolver'::regrole, 'sluiceway_worker'::regrole)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("found %d helper membership rows, want 4 (two grantors): the premise of this check is gone", rows)
	}
	mustOpen(t, tdb.URL).Close()
}

// TestBootstrapLeavesTheAdministratorsMembershipAsItWas pins the promise in the script's header:
// the administrator ends with exactly the membership in "sluiceway" it started with. The script
// has to take SET on that role to create the schema, and Postgres keeps one membership row per
// grantor, so taking SET rewrites a row the administrator already granted itself instead of
// adding one. Giving it back must restore that row, not delete it.
func TestBootstrapLeavesTheAdministratorsMembershipAsItWas(t *testing.T) {
	tdb := testdb.NewRawCluster(t)

	// createrole_self_grant is how an administrator ends up with such a row without ever asking
	// for one: every role it creates is granted back to it, by itself, with these options.
	testdb.Exec(t, tdb.AdminURL, `
		CREATE ROLE dba LOGIN CREATEROLE NOSUPERUSER PASSWORD 'dba';
		ALTER ROLE dba SET createrole_self_grant = 'inherit';
		DO $$ BEGIN EXECUTE format('GRANT CREATE ON DATABASE %I TO dba', current_database()); END $$`)

	// The first run creates the roles, so there is no "before" to compare with. The helper roles
	// are never borrowed, so their rows show what the row for "sluiceway" must still look like.
	testdb.Bootstrap(t, testdb.As(t, tdb.AdminURL, "dba", "dba"))
	const selfGrantedInheritOnly = "dba: inherit=t set=f; postgres: inherit=f set=f"
	if got := membershipOf(t, tdb.AdminURL, "dba", "sluiceway_resolver"); got != selfGrantedInheritOnly {
		t.Fatalf("dba in sluiceway_resolver = %q, want %q: the premise of this check is gone", got, selfGrantedInheritOnly)
	}
	if got := membershipOf(t, tdb.AdminURL, "dba", "sluiceway"); got != selfGrantedInheritOnly {
		t.Errorf("after the first bootstrap, dba in sluiceway = %q, want %q", got, selfGrantedInheritOnly)
	}
	mustOpen(t, tdb.URL).Close()

	// A second administrator who administers the roles only through the first, so the grantor of
	// what the script borrows is not the role that runs it.
	testdb.Exec(t, tdb.AdminURL, `
		CREATE ROLE dba2 LOGIN CREATEROLE NOSUPERUSER PASSWORD 'dba2';
		GRANT dba TO dba2 WITH INHERIT TRUE;
		DO $$ BEGIN EXECUTE format('GRANT CREATE ON DATABASE %I TO dba2', current_database()); END $$`)

	// And one who was delegated exactly what the script needs and no more: ADMIN OPTION on the
	// helper roles to grant them, SET on "sluiceway" to create its schema, and no way to grant
	// "sluiceway" to anyone. If the script tried to borrow what it already has, it would fail here.
	testdb.Exec(t, tdb.AdminURL, `
		CREATE ROLE delegate LOGIN NOSUPERUSER PASSWORD 'delegate';
		GRANT sluiceway_resolver, sluiceway_worker TO delegate WITH ADMIN OPTION, INHERIT FALSE, SET FALSE;
		GRANT sluiceway TO delegate WITH INHERIT FALSE, SET TRUE;
		DO $$ BEGIN EXECUTE format('GRANT CREATE ON DATABASE %I TO delegate', current_database()); END $$`)

	cases := []struct {
		name  string
		admin string
		// setup runs as that administrator and puts its own membership row in the state named.
		setup string
		// want is the membership the case starts from, to prove setup built it, and must end with.
		want string
	}{
		{
			name:  "own grant WITH INHERIT TRUE, SET FALSE",
			admin: "dba",
			setup: "GRANT sluiceway TO dba WITH INHERIT TRUE, SET FALSE",
			want:  selfGrantedInheritOnly,
		},
		{
			name:  "own grant that already has SET",
			admin: "dba",
			setup: "GRANT sluiceway TO dba WITH INHERIT FALSE, SET TRUE",
			want:  "dba: inherit=f set=t; postgres: inherit=f set=f",
		},
		{
			name:  "own grant with both",
			admin: "dba",
			setup: "GRANT sluiceway TO dba WITH INHERIT TRUE, SET TRUE",
			want:  "dba: inherit=t set=t; postgres: inherit=f set=f",
		},
		{
			name:  "no grant of its own",
			admin: "dba",
			setup: "REVOKE sluiceway FROM dba",
			want:  "postgres: inherit=f set=f",
		},
		{
			name:  "no membership at all, borrowing through another administrator's ADMIN OPTION",
			admin: "dba2",
			setup: "SELECT 1",
			want:  "",
		},
		{
			name:  "SET but no ADMIN OPTION: nothing to borrow, and no right to",
			admin: "delegate",
			setup: "SELECT 1",
			want:  "postgres: inherit=f set=t",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adminURL := testdb.As(t, tdb.AdminURL, tc.admin, tc.admin)
			// Only creating the schema needs SET, so it has to be missing for the script to borrow.
			testdb.Exec(t, tdb.AdminURL, "DROP SCHEMA IF EXISTS sluiceway CASCADE")
			testdb.Exec(t, adminURL, tc.setup)
			if got := membershipOf(t, tdb.AdminURL, tc.admin, "sluiceway"); got != tc.want {
				t.Fatalf("before the bootstrap, %s in sluiceway = %q, want %q: the case does not build the state it names", tc.admin, got, tc.want)
			}

			if err := testdb.TryBootstrap(t, adminURL); err != nil {
				t.Fatalf("bootstrap as %s: %v", tc.admin, err)
			}

			if got := membershipOf(t, tdb.AdminURL, tc.admin, "sluiceway"); got != tc.want {
				t.Errorf("after the bootstrap, %s in sluiceway = %q, want it unchanged: %q", tc.admin, got, tc.want)
			}
			var owner string
			queryRow(t, tdb.AdminURL, `SELECT r.rolname FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
				WHERE n.nspname = 'sluiceway'`, &owner)
			if owner != "sluiceway" {
				t.Errorf("schema owner = %q, want sluiceway: the script did not create the schema", owner)
			}
			mustOpen(t, tdb.URL).Close()
		})
	}
}

// membershipOf renders every membership row of member in role, one per grantor. ADMIN OPTION is
// left out: the script never touches it.
func membershipOf(t *testing.T, adminURL, member, role string) string {
	t.Helper()
	var got string
	queryRow(t, adminURL, fmt.Sprintf(`
		SELECT coalesce(string_agg(format('%%s: inherit=%%s set=%%s', g.rolname, m.inherit_option, m.set_option),
		                           '; ' ORDER BY g.rolname), '')
		  FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.grantor
		 WHERE m.roleid = '%s'::regrole AND m.member = '%s'::regrole`, role, member), &got)
	return got
}

func queryRow(t *testing.T, connURL, sql string, dest ...any) {
	t.Helper()
	ctx := testCtx(t)
	conn, err := pgx.Connect(ctx, connURL)
	if err != nil {
		t.Fatal("connect failed")
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := conn.QueryRow(ctx, sql).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

func mustOpen(t *testing.T, url string) *store.DB {
	t.Helper()
	db, err := store.Open(testCtx(t), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// inheritsHelper asks, as the application role and outside store.Open, whether the privileges of
// a helper role are available without SET ROLE.
func inheritsHelper(t *testing.T, appURL string, helper store.Role) bool {
	t.Helper()
	ctx := testCtx(t)
	conn, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal("connect as the application role failed")
	}
	defer func() { _ = conn.Close(ctx) }()
	var inherits bool
	if err := conn.QueryRow(ctx, "SELECT pg_has_role($1, 'USAGE')", string(helper)).Scan(&inherits); err != nil {
		t.Fatal(err)
	}
	return inherits
}
