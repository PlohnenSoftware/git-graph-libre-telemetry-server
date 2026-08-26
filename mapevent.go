package main

import (
	"strconv"
	"strings"
	"time"
)

// createTelemetryLogger injects a fixed set of common.* properties into every
// event. Those are the ones worth querying, so they get real columns; anything
// left over falls through to props. Kept separate from db.go so the mapping is
// testable without a database.

// eventRow is one row of the events table. Nullable columns are pointers so a
// missing property is stored as NULL rather than an empty string.
type eventRow struct {
	ClientTS        *time.Time
	Event           string
	Feature         *string
	OK              *bool
	MachineID       *string
	SessionID       *string
	ExtName         *string
	ExtVersion      *string
	VSCodeVersion   *string
	Product         *string
	OS              *string
	NodeArch        *string
	PlatformVersion *string
	UIKind          *string
	RemoteName      *string
	IsNewInstall    *bool
	Props           map[string]any
}

// Keys consumed into columns and therefore removed from props. VS Code
// lowercases the injected keys, so match the lowercase form and do not "tidy"
// them into camelCase.
var consumedKeys = map[string]bool{
	"common.vscodemachineid": true,
	"common.vscodesessionid": true,
	"common.extname":         true,
	"common.extversion":      true,
	"common.vscodeversion":   true,
	"common.product":         true,
	"common.os":              true,
	"common.nodearch":        true,
	"common.platformversion": true,
	"common.uikind":          true,
	"common.remotename":      true,
	"common.isnewappinstall": true,
	"feature":                true,
	"ok":                     true,
}

// text reads a nullable text column.
//
// A number or boolean here is a client-side bug, not an attack, so it is
// stringified rather than dropped: storing "1" beats losing the row. An empty
// string is stored as NULL so queries do not have to check for both.
func text(data map[string]any, key string) *string {
	raw, ok := data[key]
	if !ok {
		return nil
	}
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case bool:
		s = strconv.FormatBool(v)
	case float64:
		s = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return nil
	}
	if s == "" {
		return nil
	}
	return &s
}

// boolean reads a nullable boolean column.
//
// The shape varies across VS Code versions and forks — a real boolean,
// "true"/"false", or 1/0 — so all three are accepted. Anything else maps to
// NULL rather than false, because guessing would silently skew the numbers.
func boolean(data map[string]any, key string) *bool {
	raw, ok := data[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case bool:
		return &v
	case float64:
		switch v {
		case 1:
			t := true
			return &t
		case 0:
			f := false
			return &f
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			t := true
			return &t
		case "false":
			f := false
			return &f
		}
	}
	return nil
}

func mapEvent(event validEvent) eventRow {
	data := event.Data

	props := make(map[string]any, len(data))
	for key, value := range data {
		if consumedKeys[key] {
			continue
		}
		props[key] = value
	}

	row := eventRow{
		ClientTS:        event.ClientTS,
		Event:           event.Name,
		MachineID:       text(data, "common.vscodemachineid"),
		SessionID:       text(data, "common.vscodesessionid"),
		ExtName:         text(data, "common.extname"),
		ExtVersion:      text(data, "common.extversion"),
		VSCodeVersion:   text(data, "common.vscodeversion"),
		Product:         text(data, "common.product"),
		OS:              text(data, "common.os"),
		NodeArch:        text(data, "common.nodearch"),
		PlatformVersion: text(data, "common.platformversion"),
		UIKind:          text(data, "common.uikind"),
		RemoteName:      text(data, "common.remotename"),
		IsNewInstall:    boolean(data, "common.isnewappinstall"),
		Props:           props,
	}

	// feature and ok are meaningful only on a feature event; leaving them NULL
	// on activate keeps the ranking query's WHERE clause honest.
	if event.Name == EventFeature {
		row.Feature = text(data, "feature")
		row.OK = boolean(data, "ok")
	}

	return row
}
