# pg-snapshot

Scrubbed production Postgres snapshots, restored into lower environments.

`pgsnap` exports a production database with sensitive columns transformed **during** the export,
writes the result to object storage, and restores it into a target environment — reconciling
schema drift and swapping the result into place with a short, recoverable cutover.

Sensitive values never enter the snapshot artifact. They are scrubbed in the `SELECT` projection
that streams the data out, so there is no point at which an unscrubbed copy exists outside the
source database.

See [DESIGN.md](./DESIGN.md) for the full design.

## How it works

```
production                     bucket                target environment
  pg_dump --schema-only   ─┐
  COPY (SELECT …scrubbed) ─┼──▶  GCS / S3  ──▶  restored_1234 ──▶ migrate ──▶ swap ──▶ core
  sequence values         ─┘                                                    ↑
                                                                       core_backup_<ts>
```

Foreign keys and triggers live in `pg_dump`'s `post-data` section, so they are created *after*
the data lands. Restoring into a fresh database means there is nothing to disable — no
`session_replication_role`, and therefore no superuser requirement.

## Scrub configuration

The user decides what is sensitive. Columns the configuration does not mention are exported
as-is; the tool never guesses.

```yaml
version: 1

tables:
  public.users:
    columns:
      email:     email        # deterministic, preserves UNIQUE
      ssn:       "null"
      last_name: md5          # deterministic, preserves joins
  public.audit_logs:
    mode: skip                # structure only, no rows
  public.events:
    where: "created_at > now() - interval '30 days'"
```

Builtins are `null`, `md5`, `email`, and `redact`. Anything else is passed to Postgres as a raw
SQL expression.

`md5` and `email` are deterministic within a run and salted per run, so a value scrubbed in one
table matches the same value scrubbed in another — foreign keys over scrubbed columns still
resolve — while hashes are not comparable across snapshots.

A rule naming a column that no longer exists fails the snapshot. A rule the user wrote either
applies or is reported; silently not applying is the one outcome worse than no rule at all.

Row filtering with `where` can orphan child rows and break foreign key creation on restore. Set
`fk_mode: not_valid` when that is intentional.

## Requirements

- PostgreSQL **16 or newer**, source and target
- Source major version ≤ target major version
- Target instance needs 2× the database size during the swap
- `db_admin_version >= 0.7` on the Nullstone Postgres module

## Nullstone modules

Nullstone provides Terraform modules to aid configuration of pg-snapshot when performing a restore.
Each module is a capability that grants appropriate permissions to allow the restore job to create, rename, and swap out databases.
- [aws-postgres-restore-access](https://github.com/nullstone-modules/aws-postgres-restore-access)
- [gcp-postgres-restore-access](https://github.com/nullstone-modules/gcp-postgres-restore-access)

The capability covers Postgres and only Postgres: it mints the restore role through `pg-db-admin`
and publishes `POSTGRES_URL`, `TARGET_DATABASE`, `OWNER_ROLE` and `BACKUP_RETENTION`. Bucket access
comes from `aws-s3-access` / `gcp-gcs-access`, and everything else is app configuration.

```dockerfile
FROM nullstone/pg-snapshot:v1.0.0
COPY --from=migrations /app/bin/migrate /usr/local/bin/
COPY migrate.sh /app/migrate.sh
```

```
S3_BUCKET_URL / S3_BUCKET_REGION      where the snapshots live
MIGRATE_COMMAND                       e.g. /app/migrate.sh
```

Run it with `restore` — the image's entrypoint is already `pgsnap`.

### Version compatibility

The image and the modules version independently, so a snapshot and the restore that reads it are
not guaranteed to be the same release. The artifact declares its own format version and the restore
validates it before loading anything:

```
snapshot uses artifact version 2, this build understands 1;
upgrade the restore module to match the snapshot module
```

The same check covers Postgres versions in the other direction — a dump cannot be restored into an
older major, and the manifest records which one it came from.

## Development

```
make check      # fmt, vet, unit tests
make image      # build the container image
make acc        # acceptance tests against a real postgres
```
