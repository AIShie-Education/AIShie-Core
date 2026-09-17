package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	dbfiles "github.com/AIShiteru-LMS/AIShiteru-Core/src"
)

// Seed inserts the built-in permission presets. It is safe to re-run: the
// file skips presets that already exist, so local edits to them survive.
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	sql, err := dbfiles.FS.ReadFile(dbfiles.PresetsSeed)
	if err != nil {
		return fmt.Errorf("embedded seed: %w", err)
	}
	// No arguments, so pgx uses the simple protocol and the file's own
	// BEGIN ... COMMIT runs as written.
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("seed presets: %w", err)
	}
	return nil
}
