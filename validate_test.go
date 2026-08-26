package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func featureBody(feature string) []byte {
	return []byte(fmt.Sprintf(`{"events":[{"name":"feature","data":{"feature":%q,"ok":true}}]}`, feature))
}

func TestValidateBatchAcceptsAFeatureEvent(t *testing.T) {
	events, err := validateBatch(featureBody("pushTag"))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != EventFeature {
		t.Errorf("name = %q, want %q", events[0].Name, EventFeature)
	}
	if got := events[0].Data["feature"]; got != "pushTag" {
		t.Errorf("data.feature = %v, want pushTag", got)
	}
}

func TestValidateBatchAcceptsAnActivateEventWithNoFeature(t *testing.T) {
	events, err := validateBatch([]byte(`{"events":[{"name":"activate","data":{}}]}`))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if events[0].Name != EventActivate {
		t.Errorf("name = %q, want %q", events[0].Name, EventActivate)
	}
}

func TestValidateBatchRejectsMalformedBatches(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"events missing", `{}`},
		{"events not an array", `{"events":{}}`},
		{"events empty", `{"events":[]}`},
		{"unknown event name", `{"events":[{"name":"pageview","data":{}}]}`},
		{"feature missing", `{"events":[{"name":"feature","data":{}}]}`},
		{"feature not a string", `{"events":[{"name":"feature","data":{"feature":7}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateBatch([]byte(tc.body)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestValidateBatchRejectsOversizedBatches(t *testing.T) {
	events := make([]string, MaxEventsPerBatch+1)
	for i := range events {
		events[i] = `{"name":"activate","data":{}}`
	}
	body := `{"events":[` + strings.Join(events, ",") + `]}`

	if _, err := validateBatch([]byte(body)); err == nil {
		t.Fatal("expected an error for a batch over the limit")
	}
}

// Feature ids are our own command names, so the charset is deliberately narrow.
func TestValidateBatchEnforcesTheFeatureNameCharset(t *testing.T) {
	valid := []string{"pushTag", "a", "git-graph-libre.view", "load_more", "a.b-c_d"}
	for _, feature := range valid {
		t.Run("valid/"+feature, func(t *testing.T) {
			if _, err := validateBatch(featureBody(feature)); err != nil {
				t.Fatalf("expected %q to be accepted, got %v", feature, err)
			}
		})
	}

	invalid := []string{
		"",                      // empty
		"PushTag",               // must start lowercase
		"9lives",                // must start with a letter
		"push tag",              // no spaces
		"push/tag",              // no slashes
		"drop table events;--",  // no SQL
		strings.Repeat("a", 65), // over the 64-character bound
		"../../etc/passwd",      // no traversal
	}
	for _, feature := range invalid {
		t.Run("invalid/"+feature, func(t *testing.T) {
			if _, err := validateBatch(featureBody(feature)); err == nil {
				t.Fatalf("expected %q to be rejected", feature)
			}
		})
	}
}

// Nested values are dropped rather than serialized: the schema does not promise
// to carry them, and accepting them would let a caller decide row size.
func TestNormalizeDataKeepsOnlyPrimitives(t *testing.T) {
	body := []byte(`{"events":[{"name":"activate","data":{
		"s":"text","b":true,"n":12.5,
		"nested":{"a":1},"list":[1,2],"nothing":null
	}}]}`)

	events, err := validateBatch(body)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	data := events[0].Data

	for _, key := range []string{"s", "b", "n"} {
		if _, ok := data[key]; !ok {
			t.Errorf("expected %q to be kept", key)
		}
	}
	for _, key := range []string{"nested", "list", "nothing"} {
		if _, ok := data[key]; ok {
			t.Errorf("expected %q to be dropped", key)
		}
	}
}

func TestNormalizeDataTruncatesLongStrings(t *testing.T) {
	long := strings.Repeat("x", MaxStringLength+50)
	body, err := json.Marshal(map[string]any{
		"events": []any{map[string]any{"name": "activate", "data": map[string]any{"v": long}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	events, err := validateBatch(body)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	got, _ := events[0].Data["v"].(string)
	if len(got) != MaxStringLength {
		t.Errorf("len = %d, want %d", len(got), MaxStringLength)
	}
}

func TestNormalizeDataCapsPropertyCount(t *testing.T) {
	data := make(map[string]any, MaxPropertyCount+20)
	for i := 0; i < MaxPropertyCount+20; i++ {
		data[fmt.Sprintf("k%d", i)] = i
	}
	body, err := json.Marshal(map[string]any{
		"events": []any{map[string]any{"name": "activate", "data": data}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	events, err := validateBatch(body)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(events[0].Data) != MaxPropertyCount {
		t.Errorf("kept %d properties, want %d", len(events[0].Data), MaxPropertyCount)
	}
}

// received_at is authoritative, so a bad client clock must never cost the
// event — the timestamp is dropped and the event still lands.
func TestClientTimestampIsAdvisory(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		wantNil bool
	}{
		{"absent", "", true},
		{"before 2020", `,"ts":1000000`, true},
		{"far future", `,"ts":99999999999999`, true},
		{"plausible", `,"ts":1756000000000`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"events":[{"name":"activate","data":{}%s}]}`, tc.ts)
			events, err := validateBatch([]byte(body))
			if err != nil {
				t.Fatalf("expected the event to be accepted, got %v", err)
			}
			if tc.wantNil && events[0].ClientTS != nil {
				t.Errorf("expected a dropped timestamp, got %v", events[0].ClientTS)
			}
			if !tc.wantNil {
				if events[0].ClientTS == nil {
					t.Fatal("expected a timestamp, got nil")
				}
				if events[0].ClientTS.Year() != 2025 {
					t.Errorf("year = %d, want 2025", events[0].ClientTS.Year())
				}
			}
		})
	}
}

// A batch is one client's flush, so a single bad entry means the rest is not
// trustworthy either.
func TestValidateBatchRejectsTheWholeBatchOnOneBadEvent(t *testing.T) {
	body := []byte(`{"events":[
		{"name":"feature","data":{"feature":"pushTag"}},
		{"name":"feature","data":{"feature":"NOPE!"}}
	]}`)

	if _, err := validateBatch(body); err == nil {
		t.Fatal("expected the batch to be rejected")
	}
}

func TestNormalizeClientTSRejectsNonFiniteValues(t *testing.T) {
	inf := math.Inf(1)
	if got := normalizeClientTS(&inf); got != nil {
		t.Errorf("expected nil for +Inf, got %v", got)
	}
	recent := float64(time.Now().UnixMilli())
	if got := normalizeClientTS(&recent); got == nil {
		t.Error("expected a value for a current timestamp")
	}
}
