-- Git Graph Libre — telemetry schema.
--
-- Applied idempotently on ingest boot (see db.go), so it is safe to run
-- against an existing database. Read it directly with DataGrip; there is no
-- dashboard service by design.
--
-- Deliberately absent: any column holding, hashing, or deriving from a client
-- IP address. The ingest must never store one. See README.md.

-- Global database migration ledger. events.schema_version separately records
-- the format of each event row; this table tracks changes to any DB object.
create table if not exists public.schema_migrations (
  version     integer     primary key check (version > 0),
  description text        not null check (description <> ''),
  applied_at  timestamptz not null default now()
);

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
  schema_version   integer not null default 1,
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

-- Existing databases predate event-row versioning. Mark their historical
-- rows as version 0 while the table definition above stamps fresh rows as 1.
alter table public.events
  add column if not exists schema_version integer;

update public.events
  set schema_version = 0
  where schema_version is null;

alter table public.events
  alter column schema_version set default 1;

alter table public.events
  alter column schema_version set not null;

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

-- Covers feature and activation reports grouped by machine.
create index if not exists events_machine_id_idx
  on public.events (machine_id)
  where machine_id is not null;

insert into public.schema_migrations (version, description)
values (1, 'Event storage and event row schema versioning')
on conflict (version) do nothing;

create or replace view public.recent_feature_usage as
select feature,
       count(distinct machine_id)           as installs,
       count(*)                             as uses,
       round(100.0 * avg((not ok)::int), 1) as fail_pct
from public.events
where event = 'feature'
  and received_at > now() - interval '30 days'
group by feature;

insert into public.schema_migrations (version, description)
values (2, 'Recent feature and per-machine action reports')
on conflict (version) do nothing;

-- Remove the earlier fixed-30-day row report if this database briefly ran the
-- first revision of this branch.
drop view if exists public.recent_machine_action_counts;

-- Map compact, safe pivot column identifiers back to complete machine ids.
create or replace view public.machine_action_pivot_column_map as
select distinct machine_id,
       'machine_' || md5(machine_id) as pivot_column
from public.events
where machine_id is not null;

-- Build an action-count pivot for every machine observed in the selected
-- window. NULL means all history; positive intervals remain rolling at query
-- time. Refresh after changing the period or observing a new machine.
create or replace procedure public.refresh_machine_action_pivot_view(
  p_period interval default null
)
language plpgsql
as $procedure$
declare
  v_machine_columns   text;
  v_machine_count     integer;
  v_period_expression text;
  v_time_condition    text;
begin
  if p_period is null then
    v_period_expression := 'null::interval';
    v_time_condition := 'true';
  elsif p_period <= interval '0 seconds' then
    raise exception 'Time period must be greater than zero';
  else
    v_period_expression := format('%L::interval', p_period::text);
    v_time_condition := format(
      'event.received_at > now() - %L::interval',
      p_period::text
    );
  end if;

  select count(*),
         string_agg(
           format(
             'coalesce(sum(execution_count) filter '
             || '(where machine_id = %L), 0)::bigint as %I',
             machine_id,
             'machine_' || md5(machine_id)
           ),
           E',\n    ' order by machine_id
         )
    into v_machine_count, v_machine_columns
    from (
      select distinct machine_id
      from public.events
      where machine_id is not null
        and (p_period is null or received_at > now() - p_period)
    ) as machines;

  if v_machine_count = 0 then
    raise exception 'No machine IDs exist in the requested time period';
  end if;

  if v_machine_count > 1596 then
    raise exception
      'Requested period contains % machines; pivot supports at most 1596',
      v_machine_count;
  end if;

  drop view if exists public.machine_action_pivot;

  execute format(
    $view$
    create view public.machine_action_pivot as
    select %s as time_period,
           counts.event_type,
           counts.feature,
           counts.action,
           %s
      from (
        select event.event as event_type,
               event.feature,
               case
                 when event.event = 'activate' then '(activation)'
                 else event.feature
               end as action,
               event.machine_id,
               count(*) as execution_count
          from public.events as event
         where event.machine_id is not null
           and %s
         group by event.event, event.feature, event.machine_id
      ) as counts
     group by counts.event_type, counts.feature, counts.action
    $view$,
    v_period_expression,
    v_machine_columns,
    v_time_condition
  );
end;
$procedure$;

insert into public.schema_migrations (version, description)
values (3, 'Time-selectable per-machine action pivot')
on conflict (version) do nothing;
