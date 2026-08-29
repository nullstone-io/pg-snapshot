# pg-snapshot — Design

Scrubbed production snapshots, restored into lower environments.

**Status:** implemented
**Repos:** `nullstone-io/pg-snapshot` (binary + image) and one per module under `nullstone-modules` (MIT, public)

---

## 1. Goals

Give a customer a repeatable way to load realistic production data into staging without
production secrets ever leaving production.

- Customer selects the tables and columns to scrub, at snapshot time.
- Sensitive values never enter the snapshot artifact at all.
- Snapshots live in a production bucket; lower environments get read-only access.
- The production schema may lag the target environment's schema. The restore reconciles it.
- Cutover to the restored data is fast and recoverable.

### Non-goals

- Point-in-time recovery or disaster recovery. Use the cloud provider's native snapshots.
- Cross-engine migration. Postgres only.
- Restoring *into* production. The restore capability is structurally absent from production.

---

## 2. Background: why not `session_replication_role`

An earlier attempt loaded data into an existing database and needed
`SET session_replication_role = replica` to suppress foreign-key and trigger enforcement
during the load. Cloud SQL does not permit this: `cloudsqlsuperuser` is not a true superuser,
and `GRANT SET ON PARAMETER` requires one.

**This design does not need it.** `pg_dump` emits three sections:

| Section | Contents |
|---|---|
| `pre-data` | schemas, extensions, types, functions, `CREATE TABLE` (inline `CHECK`, `NOT NULL`, defaults) |
| `data` | table rows, sequence values |
| `post-data` | primary keys, unique constraints, **foreign keys**, indexes, triggers |

Foreign keys and triggers are created *after* all data is loaded. Restoring into a fresh,
empty database means there is nothing to suppress. The constraint that blocked the previous
attempt does not exist here.

The only constraints active during the load are inline `CHECK`, `NOT NULL`, and domain
constraints — a scrubbing concern (§4.5), not an ordering one.

---

## 3. Architecture

```
  PRODUCTION                          BUCKET                  TARGET ENV (e.g. staging)
  ┌──────────────┐                 ┌──────────┐              ┌──────────────────┐
  │ postgres     │ ──snapshot──▶   │  GCS/S3  │  ──restore──▶ │ postgres         │
  │              │   scrub during  │          │   read-only   │  restored_1234   │
  │              │   export        │          │               │      ↓ swap      │
  └──────────────┘                 └──────────┘               │  core            │
        ▲                                                     └──────────────────┘
        │ pg-db-admin (existing)                                    ▲
        └─ mints ns_snapshot_* role                                 │ pg-db-admin (existing)
                                                                    └─ mints ns_restore_* role
```

A single platform-agnostic Go binary, `pgsnap`, with two subcommands plus a repair path — **this
repo**.

The Terraform that wires up identity, network, storage and schedule lives in five separate
`nullstone-modules` repos (§7). Where the sections below say "the snapshot module" or "the restore
capability", they mean those repos, not anything in this one.

---

## 4. Snapshot

### 4.1 Identity and grants

The snapshot module mints its own role through the db module's `db_admin_invoker`, the same
way `aws-postgres-access` / `gcp-postgres-access` do (`aws_lambda_invocation` with
`lifecycle_scope = "CRUD"` on AWS; the `restapi` provider against `db_admin_func_url` on GCP).

```jsonc
// 1. the role
{ "type": "roles", "data": { "name": "ns_snapshot_a1b2", "password": "…", "useExisting": true } }

// 2. read-only across every schema, including tables that do not exist yet
{ "type": "table_privileges", "data": {
    "database":              "core",
    "schema":                "*",
    "role":                  "ns_snapshot_a1b2",
    "privileges":            ["SELECT"],
    "includeFuture":         true,
    "futureFromTableOwners": true,
    "grantConnect":          true
} }
```

Deliberately **not** granted:

- **No `role_member` into the database owner, no `schema_privileges`.** Those confer write
  access. A snapshot role has no business holding it.
- **No `pg_read_all_data`.** It would confer blanket `SELECT` and override column-level
  `GRANT SELECT (col)` / `REVOKE`.

Consequence, stated plainly: **row-level security remains a functioning control.** Grants are
not ownership, so an RLS-protected table filters this role like any other. Column-level
restriction does not survive a table-level `GRANT SELECT ON ALL TABLES`; a customer wanting
per-column control must skip the blanket grant and grant per column. Either way the preflight
reports exactly what it cannot read.

`futureFromTableOwners: true` enumerates current table owners and sets `ALTER DEFAULT
PRIVILEGES` for each, so tables added by later migrations are readable without re-applying
the module.

Requires `db_admin_version >= 0.7`, gated the same way `aws-postgres-access` gates it.

### 4.2 Preflight

Runs to completion before any data is exported. A snapshot that fails after an hour of
export is a waste; one that fails before it starts is a bug report.

```sql
-- readability and RLS exposure, per table
SELECT n.nspname, c.relname,
       pg_get_userbyid(c.relowner)                   AS owner,
       c.relrowsecurity,
       c.relforcerowsecurity,
       pg_has_role(current_user, c.relowner, 'USAGE') AS is_owner_equivalent
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p')
  AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema';
```

Per-column readability via `has_column_privilege(current_user, attrelid, attname, 'SELECT')`.

Checks, in order:

1. Server major ≥ 16.
2. Role can connect and see the target database.
3. **Scrub config references only columns that exist.** A rule naming a dropped column is a
   broken config — the rule silently would not apply. Fails.
4. **Readability.** Any table that is RLS-filtered or missing `SELECT`, and is not excluded
   in the scrub config, fails with the remediation for its specific case (§4.3).
5. Generated columns identified and excluded from export column lists.
6. New columns since the previous manifest are **logged, not blocked** (§4.4).

### 4.3 Security events

Fails closed, names the cause, names the fix:

```
snapshot aborted: 2 tables cannot be exported in full by role "ns_snapshot_a1b2".

  public.patient_records   owner=app_core   RLS=on   FORCE=no
      → grant membership in the owner role:
          GRANT "app_core" TO "ns_snapshot_a1b2";
        or add it to the module's table_owner_roles

  public.billing_events    owner=app_core   RLS=on   FORCE=yes
      → owner membership will NOT help; FORCE ROW LEVEL SECURITY is set.
        Grant BYPASSRLS (requires a true superuser — unavailable on both
        Cloud SQL and RDS), or export the table empty:
          tables:
            public.billing_events: { mode: skip-data }

No data was exported.
```

`table_owner_roles` is empty by default. Owner membership bypasses RLS, so shipping it populated
would silently defeat the protection the customer chose. The grant is made at apply time through
pg-db-admin, which keeps the escalation visible in the Terraform rather than implied by a boolean —
see §10.

Every export session runs:

```sql
START TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY;
SET row_security = off;
```

`READ ONLY` means even a transiently elevated session cannot write to production.
`row_security = off` means a non-exempt role gets a **loud error rather than silently
filtered rows** — a snapshot that quietly ships 60% of a table is worse than one that fails.

### 4.4 Scrub configuration

Module vars on the snapshot module, so it is editable from the Nullstone UI or from IaC.

```yaml
version: 1

tables:
  public.users:
    columns:
      email:      "'user' || id || '@example.invalid'"   # deterministic → preserves UNIQUE
      ssn:        "null"
      last_name:  "md5(last_name || :salt)"              # deterministic → preserves joins
  public.audit_logs:
    mode: skip-data                                       # schema only, no rows
  public.other_teams_table:
    mode: skip                                            # not in the snapshot at all
  public.events:
    where: "created_at > now() - interval '30 days'"
```

Semantics:

- Unlisted columns export as-is. The config declares what to scrub; that is the whole
  contract. The tool does not guess at what is sensitive.
- Unlisted tables export in full.
- `mode: skip-data` keeps the table's schema and exports zero rows; it restores empty.
- `mode: skip` leaves the table out of the artifact entirely — `pg_dump` is passed
  `--exclude-table` for it. This is the only mode available for a table the snapshot role cannot
  read: `pg_dump` locks every table it dumps in `ACCESS SHARE`, `--schema-only` included, and that
  lock requires `SELECT`, so one unreadable table otherwise fails the schema dump for the whole
  database. Preflight refuses an exclusion that would strand a dependent — an inbound foreign key,
  a view, a SQL-bodied function — because that failure otherwise surfaces at restore time, one
  artifact too late.
- `where` filters rows. **This can break foreign keys** — filtering a parent orphans its
  children and `post-data` FK creation fails. `fk_mode: not_valid` is the escape hatch.
- `tail_rows: n` exports ≈ the newest `n` rows by physical heap position, via a `ctid` window
  over the heap's trailing pages (a Tid Range Scan). For large append-only tables where `where`
  would seq-scan everything and an `ORDER BY … LIMIT` would sort it. Sized from
  `pg_relation_size` plus a live-density probe of the tail pages, with a margin, so it overshoots
  `n` rather than undershooting. Same FK caveat as `where`; mutually exclusive with `where` and
  either skip mode. The manifest records the window (pages read, actual rows, and the min/max of an
  optional `tail_report_column`) because the tail-is-newest assumption degrades silently under
  UPDATE traffic or after a `VACUUM FULL`.
- `:salt` is a per-run random value, held in memory and never written to the manifest.
  Deterministic within a run, rotating between runs.

Built-in transforms (`null`, `redact`, `md5`, `email`) plus raw SQL as the escape hatch.
`md5()` is chosen over `digest()` because it needs no extension.

**How it reaches the container.** `SCRUB_CONFIG` carries the YAML directly;
`SCRUB_CONFIG_FILE` names a path instead. On EKS/GKE the module renders a ConfigMap and
mounts it; on ECS it goes in the container definition as an env var. The config is not secret —
it names columns, it does not contain data — though it does disclose which columns are
considered sensitive to anyone who can read the task definition.

This is why **the snapshot job runs the stock image** while only the restore image is
customer-extended. Nothing about a snapshot is customer-specific: the binary, the config, the
connection, and the bucket credentials all arrive from outside the image. That asymmetry is
deliberate rather than incidental — the snapshot runs in production against production data,
and keeping a customer-built image supply chain out of that environment is worth more than the
symmetry of building both. Migration tooling has to be baked in, and it only has to be baked in
where it runs.

Note the ECS ceiling: a task definition is capped at 64 KiB in total and env var values count
against it. A realistic scrub config is 1–10 KB, but `SCRUB_CONFIG_FILE` exists for a
schema wide enough to approach it.

Three failure modes worth documenting loudly:

| Trap | Effect |
|---|---|
| Scrubbing a `UNIQUE` column non-deterministically | collisions → `post-data` index creation fails |
| Scrubbing FK-participating values inconsistently | dangling references → FK creation fails |
| A transform that violates an inline `CHECK` | COPY fails during load |

**Drift visibility, not drift blocking.** Each run diffs its manifest against the previous
one and logs new columns:

```
3 columns new since last snapshot:
  public.users.referral_code, public.orders.gift_note, public.orders.gift_from
```

Information in the run output. No gate, no acknowledgement step.

### 4.5 Artifact

```
<bucket>/<prefix>/<database>/<timestamp>/
  manifest.json
  schema.dump                       # pg_dump -Fc --schema-only --no-owner --no-acl --no-comments
  data/
    public.users.copy.gz
    public.orders.copy.gz
    …
```

Flags explained:

- `--schema-only` — data comes from scrubbed `COPY`, never from `pg_dump`.
- `--no-owner --no-acl` — production role names never leave production; the restore applies
  its own ownership via `pg_restore --role`.
- `--no-comments` — `COMMENT ON EXTENSION` fails for non-superusers on both RDS and
  Cloud SQL. This is the single most common restore error.

`manifest.json` carries: artifact format version, `pgsnap` version, source server major,
source database, timestamp, `scrubbed: true`, normalized scrub config + its hash, per-table
column lists and applied transforms, row counts, **sequence values**, and per-file checksums.

Sequence values matter: `--schema-only` does not carry them, so they are captured explicitly
and replayed via `setval` after the data load.

### 4.6 Column handling

Build the `COPY` column list from the catalog per table, never `SELECT *`:

- **Generated columns** (`pg_attribute.attgenerated <> ''`) — excluded. `COPY` rejects them
  outright. They recompute on load, so a generated column derived from a scrubbed column
  gets a correctly scrubbed value for free.
- **Identity columns** — included normally. `COPY FROM` is exempt from the `GENERATED ALWAYS`
  restriction that blocks `INSERT`; this is how `pg_dump` round-trips them. The `setval`
  above finishes the job.
- **Partitioned tables** — enumerate leaf partitions, not the parent.

### 4.7 Consistency and parallelism

One `REPEATABLE READ READ ONLY` transaction exports the schema and captures sequence values.
Parallel workers join it via `pg_export_snapshot()` + `SET TRANSACTION SNAPSHOT`, giving a
consistent cross-table view without serializing the export.

The snapshot module takes a single `postgres` connection. Pointing it at a read replica
rather than the primary is the customer's choice — the connection contract is identical, and
the export is read-only either way. On a large database this is strongly preferable: it keeps
a long-lived transaction off the primary.

---

## 5. Restore

### 5.1 Identity

The restore module mints its own role through the same `db_admin_invoker`:

```jsonc
{ "type": "roles",        "data": { "name": "ns_restore_x9y8", "password": "…",
                                    "attributes": { "createDb": true }, "useExisting": true } }
{ "type": "role_members", "data": { "target": "cloudsqlsuperuser",   // rds_superuser on AWS
                                    "member": "ns_restore_x9y8", "useExisting": true } }
```

Instance-level admin. It needs `CREATEDB`, ownership of the target database, the ability to
terminate other sessions, and the ability to create non-trusted extensions — the single
membership covers all of it, and `pg-db-admin`'s existing `role_members` endpoint issues a
generic `GRANT <target> TO <member>` with no allowlist, so no changes are required there.

**Production gating is structural.** The restore module does not exist in production, so
neither does the role. No feature flag to misconfigure.

A distinct role rather than reusing the admin credentials directly, for three reasons:
identity in `pg_stat_activity` and logs, independent rotation, and revoking it does not break
`pg-db-admin`.

### 5.2 Sequence

1. Resolve the newest (or pinned) snapshot from the bucket. Validate the manifest:
   `scrubbed == true`, artifact format supported, **source major ≤ target major**.
2. Drop any orphaned `restored_*` from a prior failed run. Drop backups beyond
   `backup_retention` (default 1).
3. `CREATE DATABASE restored_<id>` owned by the target owner role.
4. `pg_restore --section=pre-data --role=<db-owner>` — serial; it is one DDL dependency chain
   and fast.
5. **Parallel data load.** N workers, one `COPY <table> FROM STDIN` each, scheduled
   largest-object-first so workers finish together. No FKs or triggers exist yet, so order is
   irrelevant and there is no contention.
6. `setval` for every sequence from the manifest.
7. `pg_restore --section=post-data -j N` — concurrent index builds and FK validation scans.
   This is where parallelism actually pays.
8. **Migrations** (§5.4) against `restored_<id>`.
9. Reapply database ACLs and per-database settings via `pg-db-admin` (§5.5).
10. `vacuumdb --analyze-only -j N`. Without this, staging query plans are terrible and
    everyone blames the tool.
11. **Swap** (§5.3).

Session settings during the load:

| Setting | Why |
|---|---|
| `synchronous_commit = off` | Usually the single biggest win. Safe *here specifically* because a crashed restore discards the whole database. |
| `maintenance_work_mem` high | What actually makes `post-data` index builds fast. |
| `max_parallel_maintenance_workers` | Parallelism *within* each index build, on top of `-j` across them. |

`restore_parallelism` defaults to 4. The bottleneck is the database instance, not the job
container — this is a tuning knob, not "set it to your core count". `pg_restore -j` needs the
archive on local disk rather than a stream, trivially satisfied since ours is schema-only.

### 5.3 Swap

```sql
ALTER DATABASE core WITH ALLOW_CONNECTIONS false;   -- FIRST: defeats pooler reconnect
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname = 'core' AND pid <> pg_backend_pid();
ALTER DATABASE core          RENAME TO core_backup_202608041530;
ALTER DATABASE restored_1234 RENAME TO core;
ALTER DATABASE core WITH ALLOW_CONNECTIONS true;
```

The order is load-bearing. If the environment fronts the database with PgBouncer or RDS
Proxy, terminating alone loses a race — the pooler reconnects before the rename lands.
`ALLOW_CONNECTIONS false` first is what makes it safe.

Run from a connection to the `postgres` database; a database cannot rename itself. Also
terminate the migration job's own idle connections to `restored_<id>` before its rename.

**Recovery is catalog-derived.** No state table, no journal — the catalog *is* the state:

| `core` | `core_backup_*` | `restored_*` | Meaning | Action on startup |
|:---:|:---:|:---:|---|---|
| ✅ | — | — | idle | proceed |
| ✅ | — | ✅ | crashed during prepare | `DROP DATABASE restored_* WITH (FORCE)`, proceed |
| ❌ | ✅ | ✅ | **crashed mid-swap** | rename newest backup → `core`, re-enable connections, abort |
| ❌ | ✅ | — | crashed mid-swap, late | rename newest backup → `core`, re-enable connections, abort |
| ❌ | ❌ | ✅ | should be impossible | abort loudly, touch nothing |

Guard the whole run with a session-level `pg_advisory_lock` on a hash of the target database
name, held on the `postgres` database, so two restores cannot interleave.

`backup_retention` defaults to 1: one rollback target, dropped at the start of the *next*
restore rather than at the end of this one. Steady state is 2× the database size — which
matters, because Cloud SQL storage never shrinks.

### 5.4 Migrations

The restore image is a base image customers extend:

```dockerfile
FROM nullstone/pg-snapshot:v1.0.0
COPY --from=migrations /app/bin/migrate /usr/local/bin/
COPY migrate.sh /app/migrate.sh
```

`MIGRATE_COMMAND` names the command -- a pipeline, or the path to a script baked into the image.
`POSTGRES_URL` and `DATABASE_URL` are both set to `restored_<id>` while it runs, and a non-zero
exit aborts the restore before the swap.

There is deliberately no conventional hook path. A script is a perfectly good migration step, but
naming it in `MIGRATE_COMMAND` gets the same result with one mechanism instead of two, and keeps it
visible in the app's configuration rather than hidden in a layer.

This is what reconciles schema drift. The snapshot carries production's migration-tracking
table, so the customer's migration tool applies exactly the delta between the production
schema and the target environment's. The tracking table must not be excluded in the scrub
config — the preflight warns if it appears to be.

### 5.5 What does not follow a rename

These live on the old database's OID and must be reapplied to `restored_<id>` *before* the
swap, via `pg-db-admin`:

- Database ACLs (`GRANT CONNECT ON DATABASE`)
- Per-database settings (`ALTER DATABASE core SET …`, in `pg_db_role_setting`)

Ownership is handled at load time — `pg_restore --role=<db-owner>` creates objects correctly
owned from the start, so `REASSIGN OWNED` is a repair path rather than a step.

---

## 6. Security model

| Property | Mechanism |
|---|---|
| Sensitive values never leave production | Scrubbed in the `COPY` projection; never materialized in the artifact |
| Snapshot cannot write to production | `SELECT`-only grants; `READ ONLY` transaction |
| RLS remains a real control | Grants confer privilege, not ownership; no `pg_read_all_data` |
| Silent partial exports impossible | `SET row_security = off` turns filtering into a hard error |
| Restore cannot write to production | Read-only bucket IAM; target-env credentials only |
| No unscrubbed artifact can be restored | Manifest `scrubbed: true`, verified before load |
| No restore into production | Restore module absent from production, so its role does not exist |
| Production role names never leak | `--no-owner --no-acl` |

Bucket IAM is wired out of band. The modules output the principal (GCP service account email
/ AWS role ARN) and document required permissions; the customer's platform team grants cross
account or cross project access. On AWS, remember the **KMS key policy** as well as the bucket
policy — that is the usual footgun.

Snapshots should carry a lifecycle expiry even though they are scrubbed.

---

## 7. Packaging

**This repo contains the binary and the image, and nothing else.** The Terraform modules are not
here — each one is its own repo in the `nullstone-modules` org, following that org's existing
one-repo-per-module convention, and each tags and publishes on its own.

What is in this repo:

```
cmd/pgsnap/     snapshot | restore | repair
export/         schema dump, column-list builder, COPY projections, sequence capture, manifest
restore/        create, load, migrate command, swap state machine
scrub/          config, transforms, drift detection
blobstore/      gcs + s3 adapters
pg/             connection plumbing and pg_dump/pg_restore/vacuumdb wrappers
```

The modules that consume it, elsewhere:

| Repo | Kind |
|---|---|
| [nullstone-modules/gcp-gke-pg-snapshot](https://github.com/nullstone-modules/gcp-gke-pg-snapshot) | app, one per platform |
| [nullstone-modules/aws-eks-pg-snapshot](https://github.com/nullstone-modules/aws-eks-pg-snapshot) | |
| [nullstone-modules/aws-ecs-pg-snapshot](https://github.com/nullstone-modules/aws-ecs-pg-snapshot) | |
| [nullstone-modules/aws-postgres-restore-access](https://github.com/nullstone-modules/aws-postgres-restore-access) | capability, one per cloud |
| [nullstone-modules/gcp-postgres-restore-access](https://github.com/nullstone-modules/gcp-postgres-restore-access) | |

Sections 4 and 5 describe what the snapshot and restore modules do, because the behaviour is this
binary's. The Terraform that wires it up is documented in each module's own repo, and this document
does not track it.

**Snapshot is an app; restore is a capability.** The asymmetry follows from who owns the image.
Nothing about a snapshot is customer-specific — the binary, the config, the connection and the
credentials all arrive from outside the image — so the module owns a job that runs the published
image, and there is nothing for a customer to deploy. A restore is the opposite: the migration step
has to be baked in, so the customer is building and deploying an image regardless. That makes the
restore *their* app, running on whatever job module they already use.

That reduces the restore capability to Postgres and only Postgres: minting the role through
pg-db-admin, which Terraform cannot reach directly, and publishing the connection and database
names. Bucket access comes from the existing `aws-s3-access` / `gcp-gcs-access` capabilities, and
the rest is app configuration. Nothing left in it differs between ECS, EKS and GKE, which is why
there are two restore modules rather than three.

**Each piece versions independently**, which puts the compatibility burden on the artifact rather
than on release choreography — the right place for it, since a snapshot module and a restore
capability run in different environments, in different clouds, and are upgraded at different times.
Release discipline could never have covered that gap anyway; only the artifact can.

So the manifest declares `artifactVersion`, and the restore validates it before loading anything:

```
snapshot uses artifact version 2, this build understands 1;
upgrade the restore module to match the snapshot module
```

The same validation rejects a dump from a newer Postgres major than the target runs, and refuses an
artifact not marked `scrubbed`. All three are in §4.5.

Images publish to `nullstone/pg-snapshot`, public — customers must be able
to `FROM` the restore image without Nullstone auth.

No new bucket or access modules: `aws-s3-bucket` / `gcp-gcs-bucket` and their `*-access`
capabilities already cover it, connected across environments.

**No changes required to any db module. No changes required to `pg-db-admin`.**

---

## 8. Requirements

- **Server:** PostgreSQL ≥ 16, source *and* target. PG 13 is EOL; 14 goes EOL Nov 2026.
- **Client binaries:** pinned to the highest supported major (18), shipped in the image.
  `pg_dump` must be ≥ the source server.
- **Source major ≤ target major**, validated from the manifest. Never restore a newer dump
  into an older server.
- **Disk:** the target instance needs 2× the database size during the swap.
- **`db_admin_version` ≥ 0.7.**

PG 16 as the floor removes: the `pg_read_all_data` availability check (14+), the pre-16
`GRANT … WITH INHERIT` branch, `DROP DATABASE WITH (FORCE)` fallbacks (13+), and pre-15
`public` schema special-casing. The restore path needs no feature-detection map at all.

---

## 9. Open risks

1. ~~**`FORCE ROW LEVEL SECURITY`**~~ — **cleared for the initial customer (2026-08-04).**
   The check below returned zero rows: no table in their production database has RLS enabled,
   so nothing blocks the export and `table_owner_roles` can stay empty there.

   The handling stays in the tool — this is a public OSS project and other users will have
   RLS. It remains the one condition that can make a table unexportable: owner membership does
   not help when `FORCE` is set, only `BYPASSRLS` does, and that is unavailable on both
   Cloud SQL and RDS. Affected tables must be exported empty via `mode: skip-data`.

   ```sql
   SELECT n.nspname, c.relname, c.relforcerowsecurity
   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relrowsecurity AND n.nspname NOT LIKE 'pg\_%';
   ```

   Note the consequence for this customer: with no RLS in play, **the scrub config is the only
   thing standing between a sensitive column and the bucket.** That is consistent with the
   design's stated posture — the customer declares what is sensitive — but it means the
   manifest drift log (§4.4) is the sole mechanism surfacing newly added columns.

2. **Parallel export against a read replica** — synchronized snapshots on standbys should
   work on PG 16, but this is asserted rather than observed. Verify before promising the
   replica path.

3. **`table_privileges` with `schema = "*"` and `futureFromTableOwners`** — acceptance tests
   exist in `pg-db-admin/acc/`, so this is confirmation rather than discovery.

4. **Cutover disruption** — applications hold dead connections across the swap and need a
   restart. Consider having the restore trigger a rolling restart of consumers.

---

## 10. Deviations found during implementation

Five places where the design as written did not survive contact with the code.

**`auto_grant_table_owner` became `table_owner_roles`.** A runtime grant would need the job to call
pg-db-admin, which means SigV4 on AWS and OIDC impersonation on GCP inside the container. The
module already talks to pg-db-admin at apply time, so the grant happens there instead, driven by an
explicit list of roles. The preflight error names the exact roles to add. Same outcome, no HTTP
client in the job, and the escalation is visible in the Terraform rather than implied by a boolean.

**Sequence positions usually come from `max()` rather than the catalog.** `pg_sequences` only
returns rows for sequences the caller can read, and `table_privileges` grants `SELECT ON ALL
TABLES`, which does not cover sequences. Rather than widen the grant, the export falls back to the
high-water mark of the column each sequence owns. The manifest records which method was used per
sequence, because the two disagree when production burned sequence numbers on rolled-back inserts
or when rows were filtered by a `where`. See §11 for the open question this raises.

**pgx replaced lib/pq for the data path.** lib/pq has no `COPY … TO STDOUT`, which the scrubbed
export is entirely built on. lib/pq remains for identifier and literal quoting.

**Backup names are disambiguated.** They carry a timestamp at second granularity; two swaps of the
same target inside one second would collide and the rename would fail with the target already
renamed away. Rare — the advisory lock serialises restores and a run takes minutes — but cheap to
rule out.

**`SHOW server_version_num` returns text.** Scanning it into an `int` failed. It now reads through
`current_setting(...)::int`. Caught by the acceptance suite; it would have failed the first step of
every snapshot and restore.

## 11. Open questions

1. **Sequence accuracy.** The `max()` fallback resumes a sequence above every row that was actually
   exported, which is correct for a restored environment but not identical to production. Widening
   the snapshot grant to cover sequences (`GRANT SELECT ON ALL SEQUENCES`) would need a new
   pg-db-admin endpoint, and would break the "no pg-db-admin changes" property. Worth it or not?

2. **Module scope.** These are purpose-built job modules, not full Nullstone app modules — they do
   not carry the capability, scaffold, volume, or secret surface that `aws-eks-job` has. If they
   should be first-class app modules that accept capabilities, that is a larger build.

3. **Where the scrub config is edited.** The Nullstone UI's rendering of a large multi-line YAML
   module var is unverified. If it is a poor editing experience, the alternative is a file in the
   customer's repo, which is coupled to the schema it describes anyway.

## Appendix: decision log

| Decision | Choice | Rationale |
|---|---|---|
| Export strategy | Split `pre-data` / scrubbed `COPY` / `post-data` | Sensitive values never enter the artifact; FK deferral for free |
| Swap | Database rename | Native `pg_restore`, no SQL rewriting; ms-long window |
| Swap recovery | Catalog-derived | Crash-proof by construction; no journal to corrupt |
| PG floor | 16 | Removes every feature-detection branch |
| Snapshot role | Minted by snapshot module, `SELECT`-only | Symmetric with `*-postgres-access`; keeps RLS functional |
| Restore role | Minted by the restore capability, instance admin | Structural production gating; no db module changes |
| `pg_read_all_data` | Not used | Would override column-level protections |
| `table_owner_roles` | Empty by default | Owner membership bypasses RLS |
| `unknown_columns` | Default allow | The user decides what is sensitive; drift is reported, not blocked |
| Migrations | Customer-extended image, `MIGRATE_COMMAND` | One mechanism, visible in config; a script is just a command |
| Scrub config | Module vars | Editable from the Nullstone UI or IaC |
| Snapshot source | Single connection, customer chooses | Primary and replica have an identical contract |
| Backup retention | Configurable, default 1 | Predictable 2× steady state |
| Bucket | Existing modules, cross-env connection | No new modules needed |
| Bucket IAM | Out of band | Cross-account/project grants are a platform-team concern |
| Repos | Binary in `nullstone-io/pg-snapshot`, one repo per module in `nullstone-modules` | Matches the org convention; compatibility is carried by the artifact version rather than by release choreography |
| Restore packaging | Capability, not an app module | The customer already builds and deploys the image; the role and bucket access are all that is left, and neither differs by platform |
| Env var names | Nullstone built-ins, no `PGSNAP_` prefix | `POSTGRES_URL` and the bucket vars are already published by existing access capabilities |
