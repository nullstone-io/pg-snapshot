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

### Sampling large append-only tables with `tail_rows`

```yaml
tables:
  public.activity_log:
    tail_rows: 2000                  # ≈ the newest 2000 rows, by heap position
    tail_report_column: created_at   # optional: record the window's time range in the manifest
```

`tail_rows: n` exports approximately the **newest** `n` rows of a table instead of all of them,
located by physical heap position rather than by any column. It exists for the table `where`
cannot afford: a large append-only table — an events log, say — with no index on its timestamp.
There, `where: "created_at > …"` seq-scans the entire table inside the export's long-lived
transaction, and `ORDER BY created_at DESC LIMIT n` adds a full sort that can exhaust the source's
temp disk. The tail export instead measures the heap's size and the live-row density of its last
pages, then copies only a computed window of trailing pages with a `ctid` range predicate —
reading megabytes where the alternatives read the whole table.

Three things to know:

- **It assumes append-only.** "The end of the heap is the newest data" holds for insert-only
  tables and degrades silently if the table starts taking heavy `UPDATE` traffic or is
  `VACUUM FULL`'d — reclaimed space early in the heap absorbs new rows. Name a
  `tail_report_column` and watch the reported window in the manifest: a range that stops looking
  recent is the symptom.
- **It overshoots on purpose.** The window is sized with a margin, so the export lands somewhat
  *over* `n` (a few percent at small `n`, more at very large `n`) rather than ever under it. A
  probe that finds no live rows in the tail falls back to exporting the whole table — loudly,
  never to a silent empty export — and a run that still comes back short of `n` (possible when
  the window is wider than the probed pages) is logged as a warning.
- **It is a row filter, exactly like `where`.** Rows in other tables that reference excluded rows
  become orphans, and FK creation on restore fails unless `fk_mode: not_valid` is set.

`tail_rows` is mutually exclusive with `mode: skip` and with `where`, and composes normally with
`columns` scrubbing. On partitioned tables, configure the leaf partitions (as with every rule);
`n` then applies per partition. Each tail-sampled table records its window in the manifest:
requested rows, pages read of total, actual exported row count, and the report column's min/max.

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
and publishes `POSTGRES_URL`, `RESTORE_TARGET_DATABASE`, `RESTORE_OWNER_ROLE` and
`RESTORE_BACKUP_RETENTION`. Bucket access comes from `aws-s3-access` / `gcp-gcs-access`, and
everything else is app configuration.

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

### Cross-account bucket access

Snapshots live in a production bucket and the restore runs somewhere else, so the grant spans two
accounts. `aws-s3-access` and `gcp-gcs-access` only write the half that lives in the restore's own
account; the other half is on the bucket, in production, where Terraform running in a lower
environment has no reach. `gcp-gcs-access` is explicit about it — it skips the IAM binding entirely
when the app and the bucket are in different projects.

Run the matching command below **once, against the bucket's account or project**.

Grant reads and nothing else. The restore fetches objects, and lists prefixes to find the newest
snapshot when `SNAPSHOT` is unset. It never writes to the bucket and never deletes from it —
pruning old snapshots is the snapshot side's job, running in production where the bucket already is.

#### AWS

`put-bucket-policy` replaces the bucket's entire policy rather than adding to it, so start from
whatever is already there:

```bash
aws s3api get-bucket-policy --bucket "$BUCKET" --query Policy --output text > bucket-policy.json
```

A `NoSuchBucketPolicy` error means there is no policy yet — start from
`{"Version": "2012-10-17", "Statement": []}`.

Add this statement, using the restore app's IAM role ARN:

```json
{
  "Sid": "PgSnapshotRestoreRead",
  "Effect": "Allow",
  "Principal": { "AWS": "arn:aws:iam::<restore-account-id>:role/<restore-role-name>" },
  "Action": ["s3:GetObject", "s3:ListBucket"],
  "Resource": [
    "arn:aws:s3:::<bucket>",
    "arn:aws:s3:::<bucket>/*"
  ]
}
```

Both resources are required and they are not interchangeable: `s3:ListBucket` is authorized against
the bucket ARN, `s3:GetObject` against the objects inside it. Granting only the `/*` form produces
an `AccessDenied` on listing that reads as though the snapshot does not exist.

```bash
aws s3api put-bucket-policy --bucket "$BUCKET" --policy file://bucket-policy.json
```

If the bucket is encrypted with SSE-KMS under a customer-managed key, the key policy needs the same
principal — a bucket policy alone leaves you with `AccessDenied` on `GetObject` and nothing to
suggest why:

```bash
aws kms get-key-policy --key-id "$KEY_ID" --policy-name default \
  --query Policy --output text > key-policy.json
```

```json
{
  "Sid": "PgSnapshotRestoreDecrypt",
  "Effect": "Allow",
  "Principal": { "AWS": "arn:aws:iam::<restore-account-id>:role/<restore-role-name>" },
  "Action": ["kms:Decrypt"],
  "Resource": "*"
}
```

```bash
aws kms put-key-policy --key-id "$KEY_ID" --policy-name default --policy file://key-policy.json
```

#### GCP

```bash
gcloud storage buckets add-iam-policy-binding "gs://${BUCKET_NAME}" \
  --member="serviceAccount:${RESTORE_SA_EMAIL}" \
  --role="roles/storage.objectViewer" \
  --project="${BUCKET_PROJECT}"
```

`RESTORE_SA_EMAIL` is `service_account_email` from the restore app's outputs. This one is additive
rather than a read-modify-write, so it is safe to re-run.

`objectViewer` rather than the `objectAdmin` that `gcp-gcs-access` grants same-project: it carries
`storage.objects.get` and `storage.objects.list`, which is the whole of what a restore does.

Customer-managed encryption needs no extra grant here, unlike on AWS. Cloud Storage decrypts with
its own service agent, so the reader never touches the key.

### Logical replication

The swap replaces the target by renaming, and everything bound to the old database's OID goes with
it. That includes the environment's replication setup, so a restore would otherwise break whatever
was replicating out of it — Datastream, a warehouse feed, a downstream subscriber.

Both halves are carried automatically. Set `RESTORE_REPLICATION=off` to skip it.

**Publications** are copied from the target onto the staging database before the swap, using
`pg_dump`, so `publish` parameters, `publish_via_partition_root`, column lists and row filters all
survive exactly. This is the environment's *own* publication — production's are excluded from the
snapshot entirely, because production's replication topology has no business running in a lower
environment.

It happens after your migration step. Applying a publication before migrations is destructive:
dropping a published table silently removes it from the publication, and dropping a column named in
a publication's column list fails outright, breaking the migration.

A publication naming a table the restored schema does not have fails the restore *before* the swap,
leaving the target untouched.

**Replication slots** are recreated after the swap. A slot is bound to a database OID, so the rename
leaves it pointing at the backup, and there is no operation that rebinds one — a consumer
reconnecting to the target by name is told `replication slot "…" was not created in this database`.
The restore drops the orphan and creates a fresh slot with the same name and plugin.

This half is best-effort. By the time it runs the database is live and correct, so a slot that
cannot be recreated is logged as an error and the restore still succeeds.

Two things to know:

- The restore role needs the `REPLICATION` attribute. It is not inherited through role membership,
  so holding `cloudsqlsuperuser` or `alloydbsuperuser` is not enough — the restore-access
  capabilities grant it directly. A restore warns before the swap if it is missing.
- **A recreated slot starts at the current LSN.** Position is not transferable, and would be
  meaningless anyway: the restore replaced every row. Downstream needs a backfill after each restore.

`FOR ALL TABLES` and `FOR TABLES IN SCHEMA` store no table references and resolve membership at
decode time, so they keep covering tables your migrations add. An enumerated `FOR TABLE a, b` list is
frozen at the moment it was written; the restore carries it verbatim and warns about the tables it
does not cover rather than extending it, because the list is a deliberate statement about what to
replicate.

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
