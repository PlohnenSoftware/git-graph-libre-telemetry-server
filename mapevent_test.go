package main

import (
	"testing"
	"time"
)

func deref[T any](t *testing.T, p *T, label string) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want a value", label)
	}
	return *p
}

func TestMapEventSplitsCommonPropertiesIntoColumns(t *testing.T) {
	row := mapEvent(validEvent{
		Name: EventActivate,
		Data: map[string]any{
			"common.vscodemachineid": "machine-1",
			"common.vscodesessionid": "session-1",
			"common.extname":         "PlohnenSoftware.git-graph-libre",
			"common.extversion":      "1.3.0",
			"common.vscodeversion":   "1.98.2",
			"common.product":         "vscodium",
			"common.os":              "Linux",
			"common.nodearch":        "x64",
			"common.platformversion": "6.1.0",
			"common.uikind":          "desktop",
			"common.remotename":      "dev-container",
			"common.isnewappinstall": true,
		},
	})

	checks := map[string]struct {
		got  *string
		want string
	}{
		"machine_id":       {row.MachineID, "machine-1"},
		"session_id":       {row.SessionID, "session-1"},
		"ext_name":         {row.ExtName, "PlohnenSoftware.git-graph-libre"},
		"ext_version":      {row.ExtVersion, "1.3.0"},
		"vscode_version":   {row.VSCodeVersion, "1.98.2"},
		"product":          {row.Product, "vscodium"},
		"os":               {row.OS, "Linux"},
		"node_arch":        {row.NodeArch, "x64"},
		"platform_version": {row.PlatformVersion, "6.1.0"},
		"ui_kind":          {row.UIKind, "desktop"},
		"remote_name":      {row.RemoteName, "dev-container"},
	}
	for column, check := range checks {
		if got := deref(t, check.got, column); got != check.want {
			t.Errorf("%s = %q, want %q", column, got, check.want)
		}
	}

	if !deref(t, row.IsNewInstall, "is_new_install") {
		t.Error("is_new_install = false, want true")
	}
	if len(row.Props) != 0 {
		t.Errorf("props = %v, want empty (every key was consumed)", row.Props)
	}
}

func TestMapEventLeavesUnknownKeysInProps(t *testing.T) {
	row := mapEvent(validEvent{
		Name: EventActivate,
		Data: map[string]any{
			"common.extversion":              "1.3.0",
			"setting.repository.showRemotes": false,
			"settingsChanged":                3.0,
		},
	})

	if _, consumed := row.Props["common.extversion"]; consumed {
		t.Error("a mapped common property leaked into props")
	}
	if got, ok := row.Props["setting.repository.showRemotes"].(bool); !ok || got {
		t.Errorf("props[setting.repository.showRemotes] = %v, want false", row.Props["setting.repository.showRemotes"])
	}
	if got, ok := row.Props["settingsChanged"].(float64); !ok || got != 3 {
		t.Errorf("props[settingsChanged] = %v, want 3", row.Props["settingsChanged"])
	}
}

// feature and ok are meaningful only on a feature event. Leaving them NULL on
// activate is what keeps the ranking query's WHERE clause honest.
func TestMapEventOnlySetsFeatureColumnsForFeatureEvents(t *testing.T) {
	activate := mapEvent(validEvent{
		Name: EventActivate,
		Data: map[string]any{"feature": "pushTag", "ok": true},
	})
	if activate.Feature != nil {
		t.Errorf("feature = %v on an activate event, want nil", *activate.Feature)
	}
	if activate.OK != nil {
		t.Errorf("ok = %v on an activate event, want nil", *activate.OK)
	}

	feature := mapEvent(validEvent{
		Name: EventFeature,
		Data: map[string]any{"feature": "pushTag", "ok": false},
	})
	if got := deref(t, feature.Feature, "feature"); got != "pushTag" {
		t.Errorf("feature = %q, want pushTag", got)
	}
	if deref(t, feature.OK, "ok") {
		t.Error("ok = true, want false")
	}
}

// A number or boolean in a text column is a client bug, not an attack, so it is
// stringified rather than dropped: storing "1" beats losing the row.
func TestTextStringifiesNonStrings(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want *string
	}{
		{"string", "vscodium", ptr("vscodium")},
		{"number", 3.0, ptr("3")},
		{"fractional number", 1.5, ptr("1.5")},
		{"boolean", true, ptr("true")},
		{"empty string", "", nil},
		{"missing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			if tc.in != nil {
				data["k"] = tc.in
			}
			got := text(data, "k")
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// The boolean shape varies across VS Code versions and forks. Anything
// unrecognized maps to NULL rather than false, because guessing would silently
// skew the numbers.
func TestBooleanAcceptsEveryShapeAndNullsTheRest(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want *bool
	}{
		{"true", true, ptr(true)},
		{"false", false, ptr(false)},
		{"string true", "true", ptr(true)},
		{"string TRUE padded", "  TRUE  ", ptr(true)},
		{"string false", "false", ptr(false)},
		{"one", 1.0, ptr(true)},
		{"zero", 0.0, ptr(false)},
		{"other number", 7.0, nil},
		{"other string", "yes", nil},
		{"missing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			if tc.in != nil {
				data["k"] = tc.in
			}
			got := boolean(data, "k")
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %v, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got nil, want %v", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestMapEventCarriesTheClientTimestamp(t *testing.T) {
	when := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	row := mapEvent(validEvent{Name: EventActivate, ClientTS: &when, Data: map[string]any{}})
	if got := deref(t, row.ClientTS, "client_ts"); !got.Equal(when) {
		t.Errorf("client_ts = %v, want %v", got, when)
	}
}

func ptr[T any](v T) *T {
	return &v
}
