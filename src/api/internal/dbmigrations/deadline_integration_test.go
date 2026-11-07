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

func TestDeadlineMigrationPreservesAcceptedAge(t *testing.T) {
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
	if _, err = base.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
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
	if _, err = provider.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// Seed a delivery under the previous schema, with an acceptance age of two days.
	_, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES('app_migration','migration');
 INSERT INTO installations(app_id,id,external_user_id,platform,apns_environment,encrypted_push_token,push_token_hash,credential_hash)
 VALUES('app_migration','00000000-0000-4000-8000-000000000001','user','android','','encrypted','hash','hash');
 INSERT INTO notifications(id,app_id,title,body,request_hash)
 VALUES('00000000-0000-4000-8000-000000000002','app_migration','title','body','hash');
 INSERT INTO deliveries(app_id,notification_id,installation_id,installation_revision,created_at)
 VALUES('app_migration','00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001',1,now()-interval '2 days');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var ageCorrect, expired bool
	err = db.QueryRowContext(ctx, `SELECT expires_at=created_at+interval '24 hours',expires_at<now() FROM deliveries`).Scan(&ageCorrect, &expired)
	if err != nil || !ageCorrect || !expired {
		t.Fatalf("migration extended old work: %v %v %v", ageCorrect, expired, err)
	}
	var generation int64
	var credential []byte
	if err = db.QueryRowContext(ctx, `SELECT mutation_generation,credential_hash FROM installations`).Scan(&generation, &credential); err != nil || generation != 0 || string(credential) != "hash" {
		t.Fatalf("migration changed legacy installation ownership: generation=%d err=%v", generation, err)
	}
	if _, err = provider.DownTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries`).Scan(&count); err != nil || count != 1 {
		t.Fatal("migration rollback lost delivery")
	}
}
