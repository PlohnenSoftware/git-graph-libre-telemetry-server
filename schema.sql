-- Git Graph Libre — telemetry schema.
--
-- Applied idempotently on ingest boot (see db.go), so it is safe to run
-- against an existing database. Read it directly with DataGrip; there is no
-- dashboard service by design.
--
-- Deliberately absent: any column holding, hashing, or deriving from a client
-- IP address. The ingest must never store one. See README.md.

create table if not exists events (
  id               bigserial   primary key,

  -- When the ingest accepted the row. Authoritative: client clocks lie.
  received_at      timestamptz not null default now(),
  -- When the client queued the event. Advisory only; may be skewed or absent.
  client_ts        timestamptz,

  -- 'activate' (once per session) or 'feature' (per invocation).
  event            text        not null,
  -- The command id, e.g. 'pushTag'. Null on 'activate'.
  feature          text,
  -- Whether the action succeeded. Null on 'activate'.
  ok               boolean,

  -- Properties VS Code injects into every event via createTelemetryLogger.
  machine_id       text,   -- common.vscodemachineid — anonymized per install
  session_id       text,   -- common.vscodesessionid
  ext_name         text,   -- common.extname
  ext_version      text,   -- common.extversion
  vscode_version   text,   -- common.vscodeversion
  product          text,   -- common.product — Code / Insiders / VSCodium / Cursor
  os               text,   -- common.os
  node_arch        text,   -- common.nodearch
  platform_version text,   -- common.platformversion
  ui_kind          text,   -- common.uikind — desktop vs web
  remote_name      text,   -- common.remotename — wsl / ssh-remote / dev-container
  is_new_install   boolean,-- common.isnewappinstall

  -- Everything else, including the activate settings-divergence snapshot.
  props            jsonb       not null default '{}'::jsonb
);

-- Ranking query filters on event + window, then groups by feature.
create index if not exists events_event_received_at_idx
  on events (event, received_at desc);

-- COUNT(DISTINCT machine_id) per feature is the reach metric; this is the
-- index that makes it cheap.
create index if not exists events_feature_machine_id_idx
  on events (feature, machine_id)
  where feature is not null;

-- Plain time-window scans (retention cleanup, activity over time).
create index if not exists events_received_at_idx
  on events (received_at desc);
