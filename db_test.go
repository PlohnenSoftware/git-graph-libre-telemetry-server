package main

import (
	"strings"
	"testing"
)

func TestSchemaVersionsExistingAndNewEventRows(t *testing.T) {
	sql := strings.Join(strings.Fields(strings.ToLower(schemaSQL)), " ")
	steps := []string{
		"add column if not exists schema_version integer",
		"set schema_version = 0 where schema_version is null",
		"alter column schema_version set default 1",
		"alter column schema_version set not null",
	}

	previous := -1
	for _, step := range steps {
		position := strings.Index(sql, step)
		if position < 0 {
			t.Fatalf("schema is missing event migration step %q", step)
		}
		if position <= previous {
			t.Fatalf("event migration step %q is out of order", step)
		}
		previous = position
	}

	for _, column := range insertColumns {
		if column == "schema_version" {
			t.Fatal("ingest must omit schema_version so the DB default stamps new rows")
		}
	}
}

func TestSchemaInstallsOnlyNonIPReports(t *testing.T) {
	sql := strings.Join(strings.Fields(strings.ToLower(schemaSQL)), " ")
	checks := []string{
		"create table if not exists public.schema_migrations",
		"create or replace view public.recent_feature_usage",
		"drop view if exists public.recent_machine_action_counts",
		"create or replace view public.machine_action_pivot_column_map",
		"create or replace procedure public.refresh_machine_action_pivot_view",
		"create view public.machine_action_pivot",
		"values (1, 'event storage and event row schema versioning')",
		"values (2, 'recent feature and per-machine action reports')",
		"values (3, 'time-selectable per-machine action pivot')",
	}

	for _, check := range checks {
		if !strings.Contains(sql, check) {
			t.Errorf("schema is missing %q", check)
		}
	}

	for _, forbidden := range []string{
		"cf_connecting_ip",
		"suspected_actor",
		"client_ip",
		"ip_hash",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("schema must not contain IP/actor storage: found %q", forbidden)
		}
	}
}
