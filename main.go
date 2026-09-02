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
	"fmt"
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
	// Bounds the container HEALTHCHECK probe. Must exceed insertTimeout, which
	// is what /healthz spends waiting on Postgres: a shorter budget than the
	// handler's own means a database outage always surfaces as a client
	// timeout, and the probe never gets to report the 503 that says why. Kept
	// under the `--timeout` in the Dockerfile so the process reports a reason
	// instead of being killed.
	healthCheckTimeout = insertTimeout + time.Second
)

// healthCheckArg invokes this binary as its own health probe rather than as the
// server. See runHealthCheck.
const healthCheckArg = "healthcheck"

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

// runHealthCheck probes a running instance over HTTP and is what the image's
// HEALTHCHECK executes. The runtime image is distroless — no shell, no curl —
// so the only thing in there capable of testing the service is the service's
// own binary, invoked as `telemetry-ingest healthcheck`.
//
// It deliberately reads no configuration but PORT: a probe that needed
// DATABASE_URL would fail for reasons that have nothing to do with whether this
// process is serving.
func runHealthCheck(url string) error {
	client := &http.Client{Timeout: healthCheckTimeout}

	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	// /healthz already answers 503 when Postgres is unreachable, so status
	// alone is the whole verdict.
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %d", url, response.StatusCode)
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Probe mode, before any other configuration is read. Output goes to
	// stderr in plain text rather than through the JSON logger: Docker keeps
	// the last few probe outputs in the container's health log, where a bare
	// sentence is easier to read than a log record.
	if len(os.Args) > 1 && os.Args[1] == healthCheckArg {
		port, err := resolvePort(os.Getenv("PORT"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// 127.0.0.1, not localhost: on hosts where localhost resolves to ::1
		// first, a probe against a v4-only listener would fail spuriously.
		if err := runHealthCheck(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

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
