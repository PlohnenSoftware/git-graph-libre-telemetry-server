package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct {
	batches   [][]eventRow
	insertErr error
	pingOK    bool
}

func (f *fakeStore) InsertEvents(_ context.Context, rows []eventRow) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.batches = append(f.batches, rows)
	return nil
}

func (f *fakeStore) Ping(_ context.Context) bool { return f.pingOK }

// Discards output so a failing-path test does not spray the test log.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func post(t *testing.T, store store, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	newMux(store, quietLogger()).ServeHTTP(recorder, request)
	return recorder
}

func TestPostEventsAcceptsAValidBatch(t *testing.T) {
	store := &fakeStore{pingOK: true}
	body := `{"events":[
		{"name":"activate","data":{"common.extversion":"1.3.0"}},
		{"name":"feature","data":{"feature":"pushTag","ok":true,"common.vscodemachineid":"m1"}}
	]}`

	recorder := post(t, store, body)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	if len(store.batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(store.batches))
	}
	rows := store.batches[0]
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Event != EventActivate || rows[1].Event != EventFeature {
		t.Errorf("events = %q/%q, want activate/feature", rows[0].Event, rows[1].Event)
	}
	if got := deref(t, rows[1].Feature, "feature"); got != "pushTag" {
		t.Errorf("feature = %q, want pushTag", got)
	}
	if got := deref(t, rows[1].MachineID, "machine_id"); got != "m1" {
		t.Errorf("machine_id = %q, want m1", got)
	}
}

func TestPostEventsRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"no events", `{"events":[]}`},
		{"unknown event name", `{"events":[{"name":"pageview","data":{}}]}`},
		{"malformed feature", `{"events":[{"name":"feature","data":{"feature":"NOPE!"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{pingOK: true}
			recorder := post(t, store, tc.body)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", recorder.Code)
			}
			if len(store.batches) != 0 {
				t.Error("a rejected batch must not reach the database")
			}
		})
	}
}

// The cap is enforced at the socket rather than by trusting a client-supplied
// content-length.
func TestPostEventsRejectsAnOversizedBody(t *testing.T) {
	store := &fakeStore{pingOK: true}
	padding := strings.Repeat("x", MaxBodyBytes+1024)
	body := `{"events":[{"name":"activate","data":{"pad":"` + padding + `"}}]}`

	recorder := post(t, store, body)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", recorder.Code)
	}
	if len(store.batches) != 0 {
		t.Error("an oversized body must not reach the database")
	}
}

// The client never retries, so a database failure costs exactly one batch.
func TestPostEventsReportsDatabaseFailureAs503(t *testing.T) {
	store := &fakeStore{pingOK: true, insertErr: errors.New("connection refused")}

	recorder := post(t, store, `{"events":[{"name":"activate","data":{}}]}`)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
}

func TestHealthReflectsDatabaseReachability(t *testing.T) {
	cases := []struct {
		name   string
		pingOK bool
		want   int
	}{
		{"reachable", true, http.StatusOK},
		{"unreachable", false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			recorder := httptest.NewRecorder()
			newMux(&fakeStore{pingOK: tc.pingOK}, quietLogger()).ServeHTTP(recorder, request)

			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

func TestMuxRejectsUnknownRoutesAndMethods(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"unknown path", http.MethodGet, "/", http.StatusNotFound},
		{"wrong method on events", http.MethodGet, "/v1/events", http.StatusMethodNotAllowed},
		{"wrong method on health", http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			recorder := httptest.NewRecorder()
			newMux(&fakeStore{pingOK: true}, quietLogger()).ServeHTTP(recorder, request)

			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

func TestResolvePort(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"empty falls back to the default", "", defaultPort, false},
		{"valid", "9000", 9000, false},
		{"zero", "0", 0, true},
		{"negative", "-1", 0, true},
		{"too large", "70000", 0, true},
		{"not a number", "http", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePort(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("port = %d, want %d", got, tc.want)
			}
		})
	}
}
