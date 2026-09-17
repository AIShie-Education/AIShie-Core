// Command aishiterud is the AIshiteru Core server and its operator tooling.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/config"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/version"
)

const usage = `aishiterud — AIshiteru Core

Usage:
  aishiterud serve                   run the HTTP server
  aishiterud migrate up              apply every pending migration
  aishiterud migrate down --yes      revert the last migration (DESTROYS DATA)
  aishiterud migrate down --all --yes
                                     revert every migration (DESTROYS ALL DATA)
  aishiterud migrate version         print the applied and the embedded version
  aishiterud migrate force N         record version N without running anything
  aishiterud seed                    insert the built-in permission presets
  aishiterud version                 print build information

Environment:
  DATABASE_URL    default postgres:///aishiteru (local unix socket)
  HTTP_ADDR       default :8080
  SHUTDOWN_GRACE  default 15s
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aishiterud:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		return serve(cfg)
	case "migrate":
		return migrate(cfg, args[1:])
	case "seed":
		return seed(cfg)
	case "version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	latest, err := db.LatestEmbedded()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewMux(httpapi.Deps{Pool: pool, LatestSchema: latest}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.HTTPAddr, "version", version.Version, "schema_latest", latest)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", cfg.ShutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func migrate(cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate: want up, down, version or force N")
	}
	m, err := db.NewMigrator(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	switch args[0] {
	case "up":
		if err := m.Up(); err != nil {
			return err
		}
	case "down":
		fs := flag.NewFlagSet("migrate down", flag.ContinueOnError)
		all := fs.Bool("all", false, "revert every migration")
		yes := fs.Bool("yes", false, "confirm that data will be destroyed")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*yes {
			return errors.New("migrate down destroys data; pass --yes to confirm")
		}
		if *all {
			err = m.Down()
		} else {
			err = m.Steps(-1)
		}
		if err != nil {
			return err
		}
	case "force":
		if len(args) != 2 {
			return errors.New("migrate force: want a version number")
		}
		v, err := strconv.Atoi(args[1])
		if err != nil || v < 0 {
			return fmt.Errorf("migrate force: %q is not a version number", args[1])
		}
		if err := m.Force(v); err != nil {
			return err
		}
	case "version":
		// falls through to the report below
	default:
		return fmt.Errorf("migrate: unknown subcommand %q", args[0])
	}

	v, dirty, err := m.Version()
	if err != nil {
		return err
	}
	latest, err := db.LatestEmbedded()
	if err != nil {
		return err
	}
	fmt.Printf("schema version %d (embedded latest %d)", v, latest)
	if dirty {
		fmt.Print(" DIRTY — fix the database by hand, then `migrate force N`")
	}
	fmt.Println()
	return nil
}

func seed(cfg config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Seed(ctx, pool); err != nil {
		return err
	}
	fmt.Println("built-in presets seeded")
	return nil
}
