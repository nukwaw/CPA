package store

import (
	"database/sql"
	"strings"
)

// UsageDatabase exposes the initialized core PostgreSQL resource to the optional
// usage adapter. The connection is borrowed: only PostgresStore owns its lifetime.
// An empty schema retains the core store's search_path semantics; the adapter
// resolves the effective schema before creating or querying its own tables.
func (s *PostgresStore) UsageDatabase() (*sql.DB, string) {
	if s == nil {
		return nil, ""
	}
	if strings.TrimSpace(s.cfg.Schema) == "" {
		return s.db, ""
	}
	// Match fullTableName: non-empty configured identifiers are used verbatim.
	return s.db, s.cfg.Schema
}
