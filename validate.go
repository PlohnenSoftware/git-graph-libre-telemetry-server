package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// The endpoint is unauthenticated by design: any ingest token shipped inside a
// .vsix is public, because a .vsix is a zip. So validation is the real defense.
// It is deliberately cheap and strict — reject early, allocate late, and never
// let a caller decide how much memory a row costs.

const (
	// MaxEventsPerBatch bounds one request. The client batches at 25.
	MaxEventsPerBatch = 100
	// MaxBodyBytes bounds the raw request body, enforced before decoding.
	MaxBodyBytes = 64 * 1024
	// MaxStringLength bounds any single string property value; longer values
	// are truncated rather than rejected.
	MaxStringLength = 512
	// MaxPropertyCount bounds how many distinct keys are kept per event.
	MaxPropertyCount = 64
)

// Event names the ingest accepts. Anything else is rejected outright.
const (
	EventActivate = "activate"
	EventFeature  = "feature"
)

// Feature ids are our own command names (pushTag, deleteBranch, ...), so a
// narrow charset is safe and stops junk cheaply. A per-name whitelist was
// considered and deferred: it would mean keeping a second list in sync across
// two deployables.
var featureNamePattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9._-]{0,63}$`)

// errEmptyBatch is returned for a well-formed body carrying no events. The
// handler treats it like any other rejection; it is a distinct value only so
// tests can assert on it without matching message text.
var errEmptyBatch = errors.New("body.events is empty")

type rawBatch struct {
	Events []rawEvent `json:"events"`
}

type rawEvent struct {
	Name string         `json:"name"`
	TS   *float64       `json:"ts"`
	Data map[string]any `json:"data"`
}

// validEvent is an accepted event, normalized. Data holds only primitives:
// string, float64, or bool.
type validEvent struct {
	Name     string
	ClientTS *time.Time
	Data     map[string]any
}

// normalizeClientTS converts advisory client epoch-milliseconds into a time.
//
// Client clocks lie, and received_at is the authoritative column, so a missing,
// non-finite, or absurd value is dropped rather than rejected — a skewed clock
// must not cost us the event.
func normalizeClientTS(ms *float64) *time.Time {
	if ms == nil {
		return nil
	}
	v := *ms
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	earliest := float64(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	latest := float64(time.Now().Add(24 * time.Hour).UnixMilli())
	if v < earliest || v > latest {
		return nil
	}
	t := time.UnixMilli(int64(v)).UTC()
	return &t
}

// normalizeData keeps only primitive values. Nested objects and arrays are
// dropped rather than serialized: they are not something this schema promises
// to carry, and accepting them would let a caller decide how large a row is.
func normalizeData(data map[string]any) map[string]any {
	normalized := make(map[string]any, len(data))
	kept := 0
	for key, raw := range data {
		if kept >= MaxPropertyCount {
			break
		}
		switch v := raw.(type) {
		case string:
			if len(v) > MaxStringLength {
				v = v[:MaxStringLength]
			}
			normalized[key] = v
		case bool:
			normalized[key] = v
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			normalized[key] = v
		default:
			continue
		}
		kept++
	}
	return normalized
}

func validateEvent(raw rawEvent, index int) (validEvent, error) {
	if raw.Name != EventActivate && raw.Name != EventFeature {
		return validEvent{}, fmt.Errorf("events[%d].name is not a known event", index)
	}

	data := normalizeData(raw.Data)

	if raw.Name == EventFeature {
		feature, _ := data["feature"].(string)
		if !featureNamePattern.MatchString(feature) {
			return validEvent{}, fmt.Errorf("events[%d].data.feature is missing or malformed", index)
		}
	}

	return validEvent{Name: raw.Name, ClientTS: normalizeClientTS(raw.TS), Data: data}, nil
}

// validateBatch decodes and validates a request body.
//
// Rejection is all-or-nothing: a batch is one client's flush, so a malformed
// entry means that client is broken or hostile and the rest of its batch is not
// trustworthy either.
func validateBatch(body []byte) ([]validEvent, error) {
	var batch rawBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}

	if batch.Events == nil {
		return nil, errors.New("body.events is not an array")
	}
	if len(batch.Events) == 0 {
		return nil, errEmptyBatch
	}
	if len(batch.Events) > MaxEventsPerBatch {
		return nil, fmt.Errorf("body.events exceeds %d entries", MaxEventsPerBatch)
	}

	validated := make([]validEvent, 0, len(batch.Events))
	for index, raw := range batch.Events {
		event, err := validateEvent(raw, index)
		if err != nil {
			return nil, err
		}
		validated = append(validated, event)
	}
	return validated, nil
}
