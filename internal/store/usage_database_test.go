package store

import (
	"database/sql"
	"testing"
)

func TestUsageDatabaseBorrowsInitializedCoreResource(t *testing.T) {
	db := new(sql.DB)
	core := &PostgresStore{db: db, cfg: PostgresStoreConfig{Schema: " selected_schema "}}
	borrowed, schema := core.UsageDatabase()
	if borrowed != db || schema != " selected_schema " {
		t.Fatalf("resource mismatch: db=%p want=%p schema=%q", borrowed, db, schema)
	}
	core.cfg.Schema = ""
	if _, schema = core.UsageDatabase(); schema != "" {
		t.Fatalf("empty schema must preserve core search_path selection: %q", schema)
	}
	var absent *PostgresStore
	if borrowed, schema = absent.UsageDatabase(); borrowed != nil || schema != "" {
		t.Fatal("nil core store exposed a resource")
	}
}
