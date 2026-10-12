package vault_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/internal/vault"
)

// The markers this file looks for. Each is a throwaway string of this file's own, and each stands
// for a different thing:
//
//   - vaultToken is the credential the vault is given. It must be in no table, in any column.
//   - plainSecret is the webhook verification secret of a subscription, which ADR 11 puts in a
//     plain bytea column on purpose. It is the control for the bytea half of the scan. ADR 11
//     also assigned encrypting that column to this item, which this item did not do: ADR 16
//     decision 6 is the record of the deferral, and B14 owns the column. The control and the
//     omission are the same fact, so this is where a reader of one meets the other.
//   - plainName is a tenant's name, a plain text column. It is the control for the text half.
//
// The two controls are what makes a clean result mean something: a scan that found nothing
// because its SQL was broken, or because row-level security hid every row from it, would report
// exactly the same clean answer as a vault that works.
const (
	vaultToken  = "lawang-test-vault-token-5c1d4e9f7a3b"
	plainSecret = "lawang-test-plain-secret-8b6a2f0c5d1e"
	plainName   = "lawang-test-plain-name-2e7d9b4a1c86"
)

// column is one column of one table, as the scan walks them.
type column struct {
	table, name, dataType string
}

// searchMode is how a column's value is compared with the needle.
type searchMode int

const (
	// asBytes compares the needle with the column's bytes.
	asBytes searchMode = iota + 1
	// asText compares the needle with the column's text rendering.
	asText
)

// searchModes is every data_type the schema has and the way the scan can search it. A data_type
// that is not in this map fails the scan instead of taking a branch.
//
// The dispatch used to be "bytea as bytes, everything else cast to text", which is right for
// every type the schema has today and silently wrong for the first one it does not have. A
// bytea[] column reports data_type = 'ARRAY' (udt_name '_bytea'), so it would take the text
// branch, render as {"\\x6c61..."} and never match an ASCII needle: the scan would be clean for
// every value such a column could ever hold. That is exactly the broken pattern the plainSecret
// control exists to catch, reintroduced for a type neither control exercises.
// TestTheScanRefusesAColumnTypeItCannotSearch builds that column and shows both halves of it.
// jsonb is the same hazard by another route, because a value's text rendering escapes a quote and
// a backslash.
//
// So the set is closed and a migration that adds a type makes the scan name the column it does
// not know how to read, the way internal/store's TestEveryPolicySaysWhatTheDesignSaysItSays makes
// a new policy announce itself. Adding a type here is one line, taken on purpose, and it needs a
// control that proves the chosen way really matches a value of that type.
var searchModes = map[string]searchMode{
	"bytea": asBytes,
	// Everything below renders its whole value in its text cast, unescaped and untruncated, so a
	// needle that is in the value is in the rendering. The tenant_id domain is not listed because
	// information_schema reports a domain's base type here, which is text.
	"text":                        asText,
	"integer":                     asText,
	"bigint":                      asText,
	"boolean":                     asText,
	"timestamp with time zone":    asText,
	"timestamp without time zone": asText, // goose_db_version.tstamp
}

// searchQuery is the statement that looks for the needle in one column, or a refusal naming the
// column whose type this scan does not know how to search.
//
// It is a function of its own, and not a branch inside the loop, so that a test can hold a real
// catalogue row against it without the scan's own t.Fatalf ending the test first.
func searchQuery(c column) (string, error) {
	table, name := pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.name}.Sanitize()
	switch searchModes[c.dataType] {
	case asBytes:
		return fmt.Sprintf("SELECT count(*)::int FROM lawang.%s WHERE position($1::bytea in %s) > 0", table, name), nil
	case asText:
		return fmt.Sprintf("SELECT count(*)::int FROM lawang.%s WHERE position($1 in %s::text) > 0", table, name), nil
	default:
		// A column the scan cannot search is one it would report clean whatever it holds, which is
		// worse than not scanning it at all: the clean answer is what this file's whole evidence
		// chain rests on. So the type is named, and so is what to do about it.
		return "", fmt.Errorf(
			"lawang.%s.%s is of type %q, which this scan does not know how to search: add it to searchModes, with a control that proves the way chosen really matches a value of that type",
			c.table, c.name, c.dataType)
	}
}

// schemasWithBaseTables returns every schema that holds a base table, outside the two catalogue
// schemas.
//
// The column walk below is scoped to the lawang schema, so a table created outside it would not be
// scanned at all and "every table" would quietly mean "every table of one schema". Asserting the
// scope rather than widening it is deliberate: lawang.tenant_id, the grants and the policies all
// name that schema, so a base table somewhere else is a thing to find out about rather than a thing
// to scan.
func (e *env) schemasWithBaseTables(conn *pgx.Conn) []string {
	e.t.Helper()
	rows, err := conn.Query(e.ctx, `
		SELECT DISTINCT table_schema
		  FROM information_schema.tables
		 WHERE table_type = 'BASE TABLE'
		   AND table_schema NOT IN ('pg_catalog', 'information_schema')
		 ORDER BY table_schema`)
	if err != nil {
		e.t.Fatalf("read the catalogue: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatalf("read the catalogue: %v", err)
	}
	return got
}

// columns lists every column of every base table in the schema, read from the catalogue rather
// than from a list in this file: a table a later migration adds is scanned without anybody
// remembering to add it here, which is what "every table" in item B13 asks for.
func (e *env) columns(conn *pgx.Conn) []column {
	e.t.Helper()
	rows, err := conn.Query(e.ctx, `
		SELECT c.table_name, c.column_name, c.data_type
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = 'lawang' AND t.table_type = 'BASE TABLE'
		 ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		e.t.Fatalf("read the catalogue: %v", err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (column, error) {
		var c column
		err := r.Scan(&c.table, &c.name, &c.dataType)
		return c, err
	})
	if err != nil {
		e.t.Fatalf("read the catalogue: %v", err)
	}
	return got
}

// findings is where a scan found a string: "table.column" for each column that holds it.
type findings []string

// scan looks for needle in every column of every table, as the superuser, and returns where it
// found it.
//
// It runs as the superuser on purpose. The application role sees no row of a tenant-scoped table
// unless a tenant is bound, so a scan as that role would come back clean with the rows still
// there, which is the way this test can pass for the wrong reason. The row counts it returns are
// what the caller holds that to.
func (e *env) scan(needle string) (findings, map[string]int) {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()

	if got := e.schemasWithBaseTables(conn); !slices.Equal(got, []string{"lawang"}) {
		e.t.Fatalf("the database has base tables in %v, and this scan only walks lawang: a table outside it is not scanned at all", got)
	}
	cols := e.columns(conn)
	// A loop that ran fewer times than it should is the other way a scan reads as clean. Seven
	// tables of three to nine columns each is what the migrations define today, so twenty is a
	// floor a catalogue query that quietly came back with a handful of rows cannot clear.
	if len(cols) < 20 {
		e.t.Fatalf("the catalogue returned %d columns, so the scan would be clean whatever is stored", len(cols))
	}
	rowsPerTable := make(map[string]int)
	var found findings
	for _, c := range cols {
		if _, ok := rowsPerTable[c.table]; !ok {
			var n int
			if err := conn.QueryRow(e.ctx,
				fmt.Sprintf("SELECT count(*)::int FROM lawang.%s", pgx.Identifier{c.table}.Sanitize())).Scan(&n); err != nil {
				e.t.Fatalf("count %s: %v", c.table, err)
			}
			rowsPerTable[c.table] = n
		}
		// A bytea column is searched as bytes. Casting one to text would give its hex spelling
		// ("\x6c61..."), in which an ASCII needle never appears, so the bytea half of the scan
		// would be clean for every value it could ever hold: that is the broken pattern this
		// test's plainSecret control exists to catch.
		//
		// A type this scan has never met is a failure and not a guess, for the same reason: see
		// searchModes.
		query, err := searchQuery(c)
		if err != nil {
			e.t.Fatalf("%v", err)
		}
		var hits int
		if err := conn.QueryRow(e.ctx, query, needle).Scan(&hits); err != nil {
			e.t.Fatalf("scan %s.%s: %v", c.table, c.name, err)
		}
		if hits > 0 {
			found = append(found, c.table+"."+c.name)
		}
	}
	return found, rowsPerTable
}

// TestNoTokenMaterialReachesAnyTable is the first "Done when" clause of item B13: a test scans
// every table after a store and finds no token material.
//
// The order of the assertions is the point. "Nothing found" is worth nothing on its own: it is
// what a vault that stored nothing, a scan whose SQL matches nothing, and a scan that sees no
// rows all produce. So, in order:
//
//  1. the credential is stored, and the row is proven to be in the table and to hold a sealed
//     envelope that really decrypts to the credential;
//  2. the scan is proven to see rows, including the vault's own row and the two control values
//     that ARE in plain columns;
//  3. only then is the absence of the token asserted;
//  4. and finally the scan is pointed at a plaintext credential written into the very column
//     under test, which it must find. A scan that cannot see that column could not have cleared
//     it in step 3.
func TestNoTokenMaterialReachesAnyTable(t *testing.T) {
	t.Parallel()
	e := setup(t)

	// Other tables are given rows too, so the scan has somewhere to look besides the vault's own
	// table, and so the two controls exist. The subscription's secret is a different credential
	// and is a plain column by design (ADR 11 decision 1), and still plain after this item, by
	// the deferral ADR 16 decision 6 records.
	e.admin("INSERT INTO lawang.tenants (id, name) VALUES ($1, $2)", tenantA.String(), plainName)
	e.admin(`INSERT INTO lawang.subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
	         VALUES ($1, $2, $3, 'a-workspace', 'ws_1', 'sub_1', $4)`,
		ids.New(), tenantA.String(), providerKey, []byte(plainSecret))

	// Step 1: store the credential, and prove the row is real.
	e.store(tenantA, providerKey, map[string]string{"token": vaultToken, "client_secret": vaultToken + "-second"})
	env := e.ciphertext(tenantA)
	if len(env) < vault.MinEnvelopeLen || env[0] != vault.FormatV1 {
		t.Fatalf("the row does not hold an envelope (%d bytes, first byte %d)", len(env), env[0])
	}
	plaintext, err := sealerA(t).Open(tenantA, providerKey, env)
	if err != nil {
		t.Fatalf("the row's envelope does not open, so there is no proof the token was stored: %v", err)
	}
	cred, err := vault.DecodeCredential(plaintext)
	if err != nil {
		t.Fatalf("the row's envelope is not a credential: %v", err)
	}
	if got, err := cred.Field("token"); err != nil || got != vaultToken {
		t.Fatalf("the row holds %q, %v, want the token this test stored", got, err)
	}

	// Step 2: the scan sees rows, and it finds both control values where they were put.
	textFound, rows := e.scan(plainName)
	if got := rows["vault_credentials"]; got != 1 {
		t.Fatalf("the scan sees %d rows in vault_credentials, want the one that was stored: a scan that cannot see the row cannot clear it", got)
	}
	for _, table := range []string{"tenants", "subscriptions"} {
		if rows[table] == 0 {
			t.Fatalf("the scan sees no rows in %s, so its controls prove nothing", table)
		}
	}
	if len(rows) < 7 {
		t.Fatalf("the scan covered %d tables (%v), want every table of the schema", len(rows), slices.Sorted(tableNames(rows)))
	}
	for _, table := range []string{"tenants", "outbox", "outbox_record", "subscriptions", "record_ledger", "redaction_map", "vault_credentials"} {
		if _, ok := rows[table]; !ok {
			t.Errorf("the scan did not cover %s", table)
		}
	}
	if !slices.Contains(textFound, "tenants.name") {
		t.Fatalf("the text control was not found (found %v): the scan's text half matches nothing", textFound)
	}
	byteaFound, _ := e.scan(plainSecret)
	if !slices.Contains(byteaFound, "subscriptions.secret") {
		t.Fatalf("the bytea control was not found (found %v): the scan's bytea half matches nothing", byteaFound)
	}

	// Step 3: the credential is in no column of any table.
	for _, needle := range []string{vaultToken, vaultToken + "-second"} {
		if found, _ := e.scan(needle); len(found) != 0 {
			t.Errorf("token material reached %v", found)
		}
	}
	// And neither is the encoded credential, which is what an unsealed row would hold.
	if found, _ := e.scan(string(plaintext)); len(found) != 0 {
		t.Errorf("the encoded credential reached %v", found)
	}

	// Step 4: the control on the column under test. With the constraint dropped, a plaintext
	// credential is written into vault_credentials.ciphertext, and the scan must find it. This is
	// the only step that can tell "the scan cleared that column" from "the scan cannot read that
	// column".
	e.admin("ALTER TABLE lawang.vault_credentials DROP CONSTRAINT " + vault.SealedConstraint)
	e.admin("INSERT INTO lawang.vault_credentials (tenant_id, provider, ciphertext) VALUES ($1, $2, $3)",
		tenantB.String(), providerKey, plaintext)
	control, _ := e.scan(vaultToken)
	if !slices.Contains(control, "vault_credentials.ciphertext") {
		t.Fatalf("the control was not found in vault_credentials.ciphertext (found %v): step 3 was clean for the wrong reason", control)
	}
}

// tableNames is maps.Keys over the scan's row counts, written out because the assertion above wants
// a sorted list of table names and slices.Sorted takes an iterator.
func tableNames(m map[string]int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// TestTheScanRefusesAColumnTypeItCannotSearch is the control for the scan's type dispatch, which
// the controls above do not reach: they prove the two branches work, not that a column always
// takes one that works.
//
// It builds the column a later migration plausibly adds, a bytea[], and shows both halves of the
// problem on a real catalogue row and a real value:
//
//  1. the harm. The needle really is in the column, the bytes find it, and the text rendering
//     this scan would have fallen back to does not, because an array of bytea renders as hex.
//     So the old "everything that is not bytea, cast to text" branch would have reported that
//     column clean for every value it could ever hold.
//  2. the guard. searchQuery refuses the column by name instead of choosing that branch, and the
//     columns around it are still searchable, so the refusal is about the type and not about
//     everything.
func TestTheScanRefusesAColumnTypeItCannotSearch(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.admin(`CREATE TABLE lawang.zz_future (id text PRIMARY KEY, secrets bytea[] NOT NULL)`)
	e.admin(`INSERT INTO lawang.zz_future (id, secrets) VALUES ('one', ARRAY[$1::bytea])`, []byte(plainSecret))

	// 1. The harm, measured rather than argued.
	if got := queryAdmin[int](e,
		`SELECT count(*)::int FROM lawang.zz_future WHERE position($1::bytea in secrets[1]) > 0`,
		plainSecret); got != 1 {
		t.Fatalf("the needle is not in the column (%d rows), so this test is not what it says", got)
	}
	if got := queryAdmin[int](e,
		`SELECT count(*)::int FROM lawang.zz_future WHERE position($1 in secrets::text) > 0`,
		plainSecret); got != 0 {
		t.Fatalf("the text rendering of a bytea[] matched the needle (%d rows), so the fallback branch was not the hazard this guard is for", got)
	}

	// 2. The guard, over the catalogue as the scan itself reads it.
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	var refused, searchable int
	for _, c := range e.columns(conn) {
		if c.table != "zz_future" {
			if _, err := searchQuery(c); err != nil {
				t.Errorf("%s.%s: %v", c.table, c.name, err)
			}
			searchable++
			continue
		}
		if c.name != "secrets" {
			continue
		}
		if c.dataType != "ARRAY" {
			t.Fatalf("the catalogue calls a bytea[] column %q, so this test no longer drives the case it was written for", c.dataType)
		}
		q, err := searchQuery(c)
		if err == nil {
			t.Fatalf("the scan would have searched a bytea[] with %q, which can never match an ASCII needle", q)
		}
		for _, want := range []string{"zz_future", "secrets", "ARRAY"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q, so it does not say which column to go and look at: %v", want, err)
			}
		}
		refused++
	}
	if refused != 1 {
		t.Errorf("the walk refused %d columns, want the one it cannot search", refused)
	}
	if searchable < 20 {
		t.Errorf("only %d columns were found searchable, so the refusal above may be the walk refusing everything", searchable)
	}
}

// TestTheScanWouldSeeABaseTableOutsideTheSchemaItWalks is the control for the other half of
// "every table": the column walk is scoped to the lawang schema, so a base table created outside
// it is not scanned at all, and nothing in the scan would have said so.
func TestTheScanWouldSeeABaseTableOutsideTheSchemaItWalks(t *testing.T) {
	t.Parallel()
	e := setup(t)
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()

	// The control first: as migrated, lawang is the only schema with base tables, which is what
	// the scan asserts before it walks anything.
	if got := e.schemasWithBaseTables(conn); !slices.Equal(got, []string{"lawang"}) {
		t.Fatalf("a migrated database has base tables in %v, want only lawang", got)
	}
	e.admin(`CREATE SCHEMA zz_elsewhere`)
	e.admin(`CREATE TABLE zz_elsewhere.tokens (id text PRIMARY KEY, secret bytea NOT NULL)`)
	if got := e.schemasWithBaseTables(conn); !slices.Equal(got, []string{"lawang", "zz_elsewhere"}) {
		t.Fatalf("the schemas with base tables are %v, want the new one among them: the scan's scope assertion would not fire", got)
	}
	// And the walk really does miss it, which is why the assertion is worth making.
	for _, c := range e.columns(conn) {
		if c.table == "tokens" {
			t.Fatal("the column walk reached a table outside the lawang schema, so the assertion above guards nothing")
		}
	}
}

// TestTheScanWouldFindAPlaintextCredentialWrittenByStore is the mutation this test's own evidence
// rests on, as an assertion rather than as a note in a pull request: it writes what a Store that
// forgot to seal would write, through the same table, and shows the scan catching it.
//
// It is separate from the clause test above so that the clause test's own steps stay in one
// order, and because this one deliberately leaves a plaintext row in the table.
func TestTheScanWouldFindAPlaintextCredentialWrittenByStore(t *testing.T) {
	t.Parallel()
	e := setup(t)
	cred, err := vault.NewCredential(map[string]string{"token": vaultToken})
	if err != nil {
		t.Fatalf("NewCredential: %v", err)
	}
	plaintext, err := vault.Encode(cred)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(plaintext), vaultToken) {
		t.Fatal("the encoded credential does not carry the token, so this test is not what it says")
	}

	e.admin("ALTER TABLE lawang.vault_credentials DROP CONSTRAINT " + vault.SealedConstraint)
	e.admin("INSERT INTO lawang.vault_credentials (tenant_id, provider, ciphertext) VALUES ($1, $2, $3)",
		tenantA.String(), providerKey, plaintext)

	found, rows := e.scan(vaultToken)
	if rows["vault_credentials"] != 1 {
		t.Fatalf("the scan sees %d rows in vault_credentials", rows["vault_credentials"])
	}
	if !slices.Contains(found, "vault_credentials.ciphertext") {
		t.Fatalf("the scan did not find a plaintext credential in the table (found %v)", found)
	}
}
