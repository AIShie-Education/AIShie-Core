package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion reads the version golang-migrate recorded, without taking its
// lock. A database no migration has touched reports 0.
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (version uint, dirty bool, err error) {
	err = pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return version, dirty, nil
}
