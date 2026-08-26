// Command telemetry-ingest accepts batched usage events from the Git Graph
// Libre VS Code extension and writes them to Postgres.
//
// Deliberately small: two routes plus a redirect to the project page, one
// table, no framework, no ORM. The stdlib covers everything except the
// Postgres driver.
//
// This service must never read, log, or store the client's IP address. There
// is no such column and no access log written here — an explicit maintainer
// decision. See README.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	defaultPort = 8080
	// Slowloris protection: a client must send its headers promptly. Without
	// this a single idle connection can hold a goroutine indefinitely.
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	// Bounds a Coolify redeploy: finish in-flight batches, then stop.
	shutdownTimeout = 10 * time.Second
	// Bounds startup so a misconfigured DATABASE_URL fails fast and visibly
	// instead of hanging the container in a restart loop with no output.
	startupTimeout = 30 * time.Second
)

func resolvePort(raw string) (int, error) {
	if raw == "" {
		return defaultPort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		return 0, errors.New("PORT is not a valid port number")
	}
	return port, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	port, err := resolvePort(os.Getenv("PORT"))
	if err != nil {
		logger.Error("invalid configuration", "error", err, "PORT", os.Getenv("PORT"))
		os.Exit(1)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	db, err := newDB(startupCtx, dsn)
	if err != nil {
		logger.Error("database connect failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Applied on every boot. The schema is idempotent, so a redeploy against an
	// existing database is a no-op rather than a step someone has to remember.
	if err := db.Migrate(startupCtx); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           newMux(db, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serverFailed := make(chan error, 1)
	go func() {
		logger.Info("listening", "port", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverFailed <- err
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serverFailed:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case received := <-signals:
		logger.Info("shutting down", "signal", received.String())
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
