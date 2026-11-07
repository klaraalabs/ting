package dbmigrations

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

func TestSupersessionMigrationFencesHistoricalTransfers(t *testing.T) {
	databaseURL := os.Getenv("TING_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TING_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	base, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := base.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer base.ExecContext(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := fs.Sub(migrations, "sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES('app_one','one'),('app_two','two')`); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct {
		name, app, token string
		active           bool
	}{
		{"current", "app_one", "shared", true},
		{"transferred", "app_one", "shared", false},
		{"unique logout", "app_one", "unique", false},
		{"other App", "app_two", "unique", true},
		{"inactive predecessor", "app_one", "inactive shared", false},
		{"inactive successor", "app_one", "inactive shared", false},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO installations(app_id,id,external_user_id,platform,apns_environment,encrypted_push_token,push_token_hash,credential_hash,active,mutation_generation)
 VALUES($1,$2,$3,'android','','encrypted',$4,'credential', $5,7)`, record.app, uuid.NewString(), record.name, []byte(record.token), record.active)
		if err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		// Repeated startup migration is harmless.
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT external_user_id,superseded_at IS NOT NULL,mutation_generation,credential_hash FROM installations`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var transferred bool
			var generation int64
			var credential []byte
			if err := rows.Scan(&name, &transferred, &generation, &credential); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			want := name == "transferred" || strings.HasPrefix(name, "inactive ")
			if transferred != want || generation != 7 || string(credential) != "credential" {
				rows.Close()
				t.Fatalf("incorrect historical fence for %s", name)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE installations SET active=true WHERE external_user_id='transferred'`); err == nil {
			t.Fatal("database allowed a superseded installation to reactivate")
		}
		if pass == 0 {
			if _, err := provider.DownTo(ctx, 6); err != nil {
				t.Fatal(err)
			}
		}
	}
}
