package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/testdb"
)

// TestMigrationsGoDownToZeroAndUpAgain runs every Down section, as the application role, on a
// database of its own. No command exposes Down, so nothing else ever executes those statements,
// and a Down that nobody runs rots as migrations are added: it drops things in the wrong order,
// or forgets one. Going back up proves Down left nothing behind that Up trips over.
func TestMigrationsGoDownToZeroAndUpAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	tdb := testdb.New(t)
	db, err := Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	applied, err := db.Migrate(ctx, quiet)
	if err != nil || applied == 0 {
		t.Fatalf("Migrate: applied %d, err = %v", applied, err)
	}

	provider, closeProvider, err := db.provider(quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	down, err := provider.DownTo(ctx, 0)
	if err != nil {
		t.Fatalf("down to zero: %v", err)
	}
	if len(down) != applied {
		t.Errorf("rolled back %d migrations, want the %d that were applied", len(down), applied)
	}

	// Everything the migrations made is gone: what is left in the schema is goose's own table.
	err = db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'relation ' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND c.relname NOT LIKE 'goose_db_version%'
			UNION ALL
			SELECT 'function ' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1
			UNION ALL
			SELECT 'type ' || t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
			 WHERE n.nspname = $1 AND t.typtype = 'd'`, Schema)
		if err != nil {
			return err
		}
		left, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(left) != 0 {
			t.Errorf("after down to zero the schema still holds %v", left)
		}
		var helperCanUseSchema bool
		if err := tx.QueryRow(ctx, "SELECT has_schema_privilege('lawang_worker', $1, 'USAGE')", Schema).Scan(&helperCanUseSchema); err != nil {
			return err
		}
		if helperCanUseSchema {
			t.Error("after down to zero lawang_worker still has USAGE on the schema")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := db.Migrate(ctx, quiet)
	if err != nil {
		t.Fatalf("Migrate after down to zero: %v", err)
	}
	if again != applied {
		t.Errorf("Migrate after down to zero applied %d, want %d", again, applied)
	}
}
