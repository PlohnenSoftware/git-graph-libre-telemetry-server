package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// This file must never read, log, or forward the client's address. There is no
// IP column, no ip_hash, and no access log written here — an explicit
// maintainer decision. Do not add one "just for debugging".
//
// Rate limiting also lives upstream at Cloudflare, not here: it is free there
// and upstream of this box's bandwidth, so anything Go could reject has already
// cost us the traffic.

// insertTimeout bounds a single batch write. The client never retries, so a
// slow database costs one batch rather than a pile of stuck goroutines.
const insertTimeout = 5 * time.Second

// projectURL is where every GET other than the health check is sent.
const projectURL = "https://github.com/PlohnenSoftware/git-graph-libre"

// store is the slice of DB the handlers need. An interface rather than *DB so
// the routes are testable without Postgres.
type store interface {
	InsertEvents(ctx context.Context, rows []eventRow) error
	Ping(ctx context.Context) bool
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	// The response body is consumed by our own client, which ignores it; a
	// write failure here is not worth surfacing.
	_ = json.NewEncoder(w).Encode(payload)
}

func handleHealth(db store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), insertTimeout)
		defer cancel()

		reachable := db.Ping(ctx)
		status := http.StatusOK
		if !reachable {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"ok": reachable, "database": reachable})
	}
}

// handleProjectRedirect sends anything that is not the health check to the
// project page. This service has no UI and nothing to serve at its root, and a
// human who pastes the ingest URL into a browser is looking for the project,
// not for `404 page not found`.
func handleProjectRedirect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 302 rather than 301: browsers cache a permanent redirect more or less
		// forever, which would be painful to undo if this service ever grows a
		// real root route.
		http.Redirect(w, r, projectURL, http.StatusFound)
	}
}

func handleEvents(db store, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		events, err := validateBatch(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		rows := make([]eventRow, 0, len(events))
		for _, event := range events {
			rows = append(rows, mapEvent(event))
		}

		ctx, cancel := context.WithTimeout(r.Context(), insertTimeout)
		defer cancel()

		if err := db.InsertEvents(ctx, rows); err != nil {
			// The client swallows failures and never retries, so a 5xx costs
			// exactly one batch. Losing telemetry always beats the ingest
			// becoming a source of load. The error is logged without the
			// request body, which carries user telemetry.
			logger.Error("insert failed", "error", err, "events", len(rows))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func newMux(db store, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth(db))
	mux.HandleFunc("POST /v1/events", handleEvents(db, logger))
	// Catch-all for GET only. `GET /healthz` matches a strict subset of this
	// pattern, so ServeMux still routes it to the health handler; everything
	// else a browser or crawler asks for — `/`, `/favicon.ico`, even
	// `GET /v1/events` — lands here instead of on a bare 404 or 405. Non-GET
	// methods are unaffected and keep returning 405.
	mux.HandleFunc("GET /", handleProjectRedirect())
	return mux
}
