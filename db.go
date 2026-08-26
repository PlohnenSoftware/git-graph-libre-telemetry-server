package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema is embedded rather than read from disk so the container image can
// be a bare binary with no data files alongside it.
//
//go:embed schema.sql
var schemaSQL string

// Insert column order. buildPlaceholders and rowValues both depend on this
// slice's length and order — change all three together or not at all.
var insertColumns = []string{
	"client_ts",
	"event",
	"feature",
	"ok",
	"machine_id",
	"session_id",
	"ext_name",
	"ext_version",
	"vscode_version",
	"product",
	"os",
	"node_arch",
	"platform_version",
	"ui_kind",
	"remote_name",
	"is_new_install",
	"props",
}

// DB is the whole persistence layer: a pool, an idempotent migration, and one
// multi-row insert. No ORM and no migration framework — there is one table and
// it is read directly with DataGrip.
type DB struct {
	pool *pgxpool.Pool
}

func newDB(ctx context.Context, dsn string) (*DB, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	config.MaxConns = 4
	config.MaxConnIdleTime = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Migrate applies the embedded schema. It runs on every boot: the schema is
// idempotent, so a redeploy against an existing database is a no-op rather than
// a step someone has to remember to run.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// buildPlaceholders returns ($1,$2,...),($18,$19,...) for a multi-row insert.
//
// Postgres caps a statement at 65535 parameters; at 17 columns the ingest's
// 100-event batch limit leaves three orders of magnitude of headroom.
func buildPlaceholders(rowCount int) string {
	var b strings.Builder
	for row := 0; row < rowCount; row++ {
		if row > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for column := range insertColumns {
			if column > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(row*len(insertColumns) + column + 1))
		}
		b.WriteByte(')')
	}
	return b.String()
}

func rowValues(row eventRow) ([]any, error) {
	props, err := json.Marshal(row.Props)
	if err != nil {
		return nil, fmt.Errorf("marshal props: %w", err)
	}
	return []any{
		row.ClientTS,
		row.Event,
		row.Feature,
		row.OK,
		row.MachineID,
		row.SessionID,
		row.ExtName,
		row.ExtVersion,
		row.VSCodeVersion,
		row.Product,
		row.OS,
		row.NodeArch,
		row.PlatformVersion,
		row.UIKind,
		row.RemoteName,
		row.IsNewInstall,
		string(props),
	}, nil
}

func (d *DB) InsertEvents(ctx context.Context, rows []eventRow) error {
	if len(rows) == 0 {
		return nil
	}

	query := fmt.Sprintf(
		"insert into events (%s) values %s",
		strings.Join(insertColumns, ","),
		buildPlaceholders(len(rows)),
	)

	values := make([]any, 0, len(rows)*len(insertColumns))
	for _, row := range rows {
		rowArgs, err := rowValues(row)
		if err != nil {
			return err
		}
		values = append(values, rowArgs...)
	}

	if _, err := d.pool.Exec(ctx, query, values...); err != nil {
		return fmt.Errorf("insert events: %w", err)
	}
	return nil
}

// Ping reports reachability without returning the error, so the health route
// can answer "degraded" instead of failing.
func (d *DB) Ping(ctx context.Context) bool {
	return d.pool.Ping(ctx) == nil
}

func (d *DB) Close() {
	d.pool.Close()
}
