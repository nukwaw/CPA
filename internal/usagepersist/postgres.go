package usagepersist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// All identifiers are qualified independently of a connection's search_path.
type postgresStore struct {
	ownsDB   bool
	db       *sql.DB
	schema   string
	events   string
	metadata string
}

func openUsagePostgres(ctx context.Context, dsn, schema string) (*postgresStore, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid usage PostgreSQL connection configuration")
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	s, err := openPostgresStore(ctx, db, schema)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.ownsDB = true
	return s, nil
}

func openPostgresStore(ctx context.Context, db *sql.DB, schema string) (*postgresStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("usage PostgreSQL database is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if strings.TrimSpace(schema) == "" {
		schema = ""
	}
	if strings.ContainsRune(schema, '\x00') {
		return nil, errors.New("usage PostgreSQL schema is unavailable or invalid")
	}
	var effective sql.NullString
	if schema == "" {
		err = tx.QueryRowContext(ctx, `SELECT pg_catalog.current_schema()`).Scan(&effective)
	} else {
		// Resolve the actual namespace, including PostgreSQL identifier length
		// normalization, so explicit and search_path selection share lock keys.
		err = tx.QueryRowContext(ctx, `SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace($1)`, pgx.Identifier{schema}.Sanitize()).Scan(&effective)
	}
	if err != nil {
		return nil, err
	}
	schema = effective.String
	if schema == "" {
		return nil, errors.New("usage PostgreSQL schema is unavailable or invalid")
	}
	s := &postgresStore{
		db: db, schema: schema,
		events:   pgx.Identifier{schema, "usage_events"}.Sanitize(),
		metadata: pgx.Identifier{schema, "usage_metadata"}.Sanitize(),
	}
	// Schema-qualified lock identities prevent unrelated PGSTORE instances from
	// contending while serializing concurrent startup in the same schema.
	if _, err = tx.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))`, s.lockKey("ddl")); err != nil {
		return nil, err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + s.events + ` (id TEXT PRIMARY KEY,requested_at TIMESTAMPTZ NOT NULL,provider TEXT NOT NULL,model TEXT NOT NULL,auth_index TEXT NOT NULL,key_id TEXT NOT NULL,failed BOOLEAN NOT NULL,payload JSONB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + s.metadata + ` (namespace TEXT NOT NULL,key TEXT NOT NULL,value JSONB NOT NULL,updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),PRIMARY KEY(namespace,key))`,
	}
	for _, index := range []struct{ name, columns string }{
		{"usage_events_time", "requested_at DESC,id DESC"},
		{"usage_events_model_time", "model,requested_at DESC"},
		{"usage_events_provider_time", "provider,requested_at DESC"},
		{"usage_events_auth_time", "auth_index,requested_at DESC"},
		{"usage_events_key_time", "key_id,requested_at DESC"},
	} {
		// PostgreSQL creates an index in its table's schema and does not permit a
		// schema-qualified index name in CREATE INDEX. Qualify the table instead.
		statements = append(statements, `CREATE INDEX IF NOT EXISTS `+pgx.Identifier{index.name}.Sanitize()+` ON `+s.events+` (`+index.columns+`)`)
	}
	for _, stmt := range statements {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("initialize usage schema: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *postgresStore) lockKey(parts ...string) string {
	// JSON framing avoids delimiter collisions for arbitrary schema/cache names.
	encoded, _ := json.Marshal(append([]string{"cpa-usage", s.schema}, parts...))
	return string(encoded)
}

func (s *postgresStore) Insert(ctx context.Context, e Event) (bool, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.events+`(id,requested_at,provider,model,auth_index,key_id,failed,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(id) DO NOTHING`, e.ID, e.RequestedAt, e.Provider, e.Model, e.AuthIndex, e.KeyID, e.Failed, payload)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
func postgresWhere(f Filter) (string, []any) {
	conditions := []string{"TRUE"}
	args := []any{}
	add := func(column, op string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf("%s %s $%d", column, op, len(args)))
	}
	if !f.From.IsZero() {
		add("requested_at", ">=", f.From)
	}
	if !f.To.IsZero() {
		add("requested_at", "<", f.To)
	}
	if f.Model != "" {
		add("model", "=", f.Model)
	}
	if f.Provider != "" {
		add("provider", "=", f.Provider)
	}
	if f.AuthIndex != "" {
		add("auth_index", "=", f.AuthIndex)
	}
	if f.KeyID != "" {
		add("key_id", "=", f.KeyID)
	}
	if f.Status != "" {
		add("failed", "=", f.Status == "failed")
	}
	return strings.Join(conditions, " AND "), args
}
func visitRows(rows *sql.Rows, fn func(Event) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if err := fn(event); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (s *postgresStore) Walk(ctx context.Context, f Filter, fn func(Event) error) error {
	where, args := postgresWhere(f)
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.events+` WHERE `+where+` ORDER BY requested_at DESC,id DESC`, args...)
	if err != nil {
		return err
	}
	return visitRows(rows, fn)
}
func (s *postgresStore) Events(ctx context.Context, f Filter, limit, offset int) ([]Event, int64, error) {
	where, args := postgresWhere(f)
	var total int64
	// A repeatable-read transaction keeps page count and rows consistent during ingestion.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+s.events+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT payload FROM %s WHERE %s ORDER BY requested_at DESC,id DESC LIMIT $%d OFFSET $%d`, s.events, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	events := []Event{}
	err = visitRows(rows, func(e Event) error { events = append(events, e); return nil })
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}
func (s *postgresStore) Cache(ctx context.Context, namespace string) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM `+s.metadata+` WHERE namespace=$1`, namespace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]json.RawMessage{}
	for rows.Next() {
		var key string
		var value []byte
		if err = rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, rows.Err()
}
func (s *postgresStore) UpdateCache(ctx context.Context, updates []cacheUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, update := range updates {
		if update.Delete {
			_, err = tx.ExecContext(ctx, `DELETE FROM `+s.metadata+` WHERE namespace=$1 AND key=$2`, update.Namespace, update.Key)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.metadata+`(namespace,key,value) VALUES($1,$2,$3) ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value,updated_at=now()`, update.Namespace, update.Key, []byte(update.Value))
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *postgresStore) MutateCache(ctx context.Context, namespace, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The schema-scoped per-key lock also protects an initially absent row.
	if _, err = tx.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))`, s.lockKey("metadata", namespace, key)); err != nil {
		return err
	}
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM `+s.metadata+` WHERE namespace=$1 AND key=$2`, namespace, key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	value, err := fn(old)
	if err != nil {
		return err
	}
	if value == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM `+s.metadata+` WHERE namespace=$1 AND key=$2`, namespace, key)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.metadata+`(namespace,key,value) VALUES($1,$2,$3) ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value,updated_at=now()`, namespace, key, []byte(value))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}
