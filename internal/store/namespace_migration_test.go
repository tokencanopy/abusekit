package store_test

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/abusekit/internal/store/migrations"
	"testing"
)

func TestNamespaceMigrationPreservesValuesAndOldReasons(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Postgres")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, newThrowawayDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `CREATE TABLE corpus_examples(id bigint, features jsonb); CREATE TABLE verdicts(reason text);
 INSERT INTO corpus_examples VALUES (1,'{"subject_age_h":1.125,"key_total":3,"name_brand_match":1}'),(2,'{}');
 INSERT INTO verdicts VALUES ('subject_age_h=1');`)
	if err != nil {
		t.Fatal(err)
	}
	sql, err := migrations.FS.ReadFile("008_namespaced_features.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	var correct bool
	err = tx.QueryRow(ctx, `SELECT features='{"core.subject_age_h":1.125,"core.credential_total":3,"brand.name_match":1}'::jsonb AND feature_key_space='ns-v1' FROM corpus_examples WHERE id=1`).Scan(&correct)
	if err != nil || !correct {
		t.Fatalf("migration lost feature values: %v %v", correct, err)
	}
	err = tx.QueryRow(ctx, `SELECT reason='subject_age_h=1' AND reason_version=1 FROM verdicts`).Scan(&correct)
	if err != nil || !correct {
		t.Fatalf("migration changed historical reason: %v %v", correct, err)
	}
}

func TestNamespaceMigrationRejectsLossyInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Postgres")
	}
	for name, features := range map[string]string{"collision": `{"key_total":1,"core.credential_total":2}`, "unknown": `{"unregistered_flat":3}`} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, newThrowawayDatabaseURL(t))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if _, err = pool.Exec(ctx, `CREATE TABLE corpus_examples(id bigint, features jsonb);CREATE TABLE verdicts(reason text);`); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO corpus_examples VALUES(1,$1)`, features); err != nil {
				t.Fatal(err)
			}
			sql, err := migrations.FS.ReadFile("008_namespaced_features.sql")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, string(sql))
			if err == nil {
				t.Fatal("migration accepted a lossy key conversion")
			}
			_ = tx.Rollback(ctx)
			var equal bool
			if err = pool.QueryRow(ctx, `SELECT features=$1::jsonb FROM corpus_examples WHERE id=1`, features).Scan(&equal); err != nil || !equal {
				t.Fatalf("failed migration changed original data: %v %v", equal, err)
			}
		})
	}
}
