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
//     plain bytea column on purpose. It is the control for the bytea half of the scan.
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
		var query string
		if c.dataType == "bytea" {
			query = fmt.Sprintf("SELECT count(*)::int FROM lawang.%s WHERE position($1::bytea in %s) > 0",
				pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.name}.Sanitize())
		} else {
			query = fmt.Sprintf("SELECT count(*)::int FROM lawang.%s WHERE position($1 in %s::text) > 0",
				pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.name}.Sanitize())
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
	// and is a plain column by design (ADR 11).
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
