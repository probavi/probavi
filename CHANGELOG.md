# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Pre-1.0, minor versions may contain breaking changes; every one of them is
recorded here. The adapter protocol and the evidence schema carry their own
versions, independent of the binary (see `docs/`); changes to either are
always called out explicitly.

## [Unreleased]

### Added

- **An Oracle Database adapter** (`adapters/oracle` 0.1.0), the
  eighteenth engine and the top of the DB-Engines ranking. It imports
  one Data Pump dump file (`oracle_datapump`, the output of an `expdp`
  schema, table or tablespace export) into the pluggable database of the
  official `container-registry.oracle.com/database/free` image, which
  pulls anonymously and idles under `command: sleep infinity`; the
  adapter starts the image's prebuilt instance itself from a parameter
  file that references the spfile and never rewrites it. Three measured
  facts shaped it. (1) The instance refuses a loopback-only host at its
  IPC layer (`ORA-00600 [ksipc: no private ips avail for use]`, unchanged
  by any parameter), so this is the first adapter that cannot run under
  the docker provider's `--network none`: the sandbox joins an internal
  Docker network, and zero ingress is restored on the other side — the
  listener is never started and the dispatchers are off, so no TCP
  socket listens at all (measured, and read back by the integration
  suite after every restore). (2) A `DBMS_SCHEDULER` job travels in a
  dump enabled and runs the moment it lands: a purge job deleted every
  imported row before the first read while `impdp` reported five rows
  imported and success (measured) — the data-lifecycle rule of issue
  #166 in its purest form. The pins are `job_queue_processes=0` and
  `aq_tm_processes=0` at launch, read back through the engine; the job
  stays `ENABLED` with zero runs, suspended and never rewritten, proven
  from both sides by an integration test with an unpinned control
  instance. (3) A dump damaged mid-file passes the header check and hangs
  the import client forever on a job whose state reads `UNDEFINED`
  (measured, over ten minutes), so the import runs under a watchdog that
  polls `DBA_DATAPUMP_JOBS` and turns the hang into `source_corrupt`.
  The dump's header is read through the engine's own
  `DBMS_DATAPUMP.GET_DUMPFILE_INFO` — file type, writing version,
  encryption flags, the export's wall clock (no zone, so
  `source.params.backup_timezone` applies) — never by parsing the bytes
  on the host; `impdp`'s exit codes are the verdict, and an import that
  completed with errors is `restore_failed`, never green. The check
  runner is SQL*Plus over the bequeath adapter in the restored pluggable
  database, CSV markup with a tab delimiter, the session's NLS formats
  set so the core's `freshness` parses, `NLS_LANG` set so non-ASCII data
  survives; the core's generating built-in checks apply (identifiers
  named as the dictionary stores them). Verified against
  `database/free:23.26.3.0`; the `-lite` variant is refused because it
  cannot run Data Pump (measured). The memory floor is 3 GiB (at 2 GiB
  the instance mounts and is killed while opening), and the image is
  10 GB on disk. Zero core changes; conformance 15/15. RMAN backups,
  full-database dumps with remapping, multi-file dump sets and encrypted
  dumps are listed under the README's "deliberately not here".

## [0.17.0] - 2026-08-21

### Added

- **An Elasticsearch adapter** (`adapters/elasticsearch` 0.1.0), the
  seventeenth engine. It restores fs snapshot repositories as a
  directory (`elasticsearch_repo`) or a zip archive
  (`elasticsearch_repo_zip`) — zip rather than tar because the official
  9.x image is UBI-based and ships no tar, gzip, python3 or jq at all
  (measured; `unzip` it has, on both lines). The newest snapshot by its
  own claimed instant is restored with `*` — regular indices and data
  streams with their `.ds-` backing indices, the cluster state left in
  the artifact — and the verdict is read from the shard counts and the
  cluster health, never the HTTP status: damaged blobs return 200 with
  every shard failed (measured), and a directory that is no repository
  registers silently and lists nothing, which is why the repository's
  own `index-N` is read host-side first. The data-lifecycle rule from
  #166 was designed in before the first line of code, and the engine
  needed it twice over: a restored index keeps its `index.lifecycle.name`
  while a fresh node ships 47 built-in ILM policies for it to name, and a
  data stream's retention travels inside its own metadata — measured
  with polling accelerated, a backup older than its retention lost a
  generation eight seconds after a restore that reported 4/4 shards
  successful. ILM is stopped through its own switch and verified
  STOPPED; the data stream lifecycle has no switch, so its poll interval
  is pinned to a hundred years as a launch setting and read back.
  Suspended, never rewritten: the retention still reads `1d` in the
  drill, and an integration test proves the pin from both sides against
  an unpinned control node. Two more engine facts shaped the adapter.
  Under `--network none` the 8.x line dies at startup resolving its own
  hostname where 9.x tolerates it, and the image cannot edit `/etc/hosts`
  as uid 1000 — so the JDK is pointed at a hosts file of the adapter's
  own. And the writing version is an integer index version (8537000,
  9111000), not a release string, so the pairing pre-check compares the
  integers the repository and `_nodes` state. A bonus: the dialect takes
  SQL-standard quoted identifiers and answers `max()` of a date as RFC
  3339, so the core's generating built-in checks apply — the first
  non-relational engine in the catalog where they do. Zero core changes,
  conformance 15/15, verified against Elasticsearch 8.19.20 and 9.5.2.

### Fixed

- **An OpenSearch drill no longer judges a sound restore by one instant**
  (`adapters/opensearch` 0.2.0). The health gate read `_cluster/health`
  once, right after the restore call returned — and that call returns
  when its own bookkeeping completes, while the shards' started events
  land asynchronously after it. On a slower host the gap is wide enough
  to read, and a primary still initializing from a snapshot is reported
  *yellow*, not red, so the gate refused a restore that was seconds from
  green. The sibling Elasticsearch adapter, which shares this code
  path's lineage, hit exactly that on a hosted runner before it shipped;
  this adapter had only the timing to thank. The gate now waits through
  the engine's own primitive, `wait_for_status=green` with a bounded
  timeout, and reads the verdict from the body: a wait that expires
  answers HTTP 408 with the current status (measured on 2.19), so that
  curl runs without `-f` — with it the body would be discarded and the
  gate would fall silent exactly when it matters.

- **Custom checks work again on the Prometheus and VictoriaMetrics
  adapters** (`adapters/prometheus` 0.4.0, `adapters/victoriametrics`
  0.2.0). Both declared `promtool query instant` as their check runner,
  and promtool prints an annotated sample — `{} => 45886 @[1787113801]` —
  while the core compares a check's `expect` against the runner's whole
  trimmed output. No custom check could pass, and none could be written
  that ever would: the line ends with an evaluation instant that changes
  with every backup. §6.1 of the adapter protocol requires a runner to
  print result rows "with no decoration", so this was an adapter defect
  rather than a missing protocol affordance. The runners now print the
  sample value alone, one line per series, reading a scalar and a vector
  sample the same way and unmoved by a label value containing the
  separator; output of any other shape fails the check loudly instead of
  reaching the core as if it were a value. Both READMEs' check examples
  now run as written.

- **An etcd drill no longer lets the backup's leases expire under it**
  (`adapters/etcd` 0.2.0). A key attached to a lease exists only while
  somebody renews it, and on restore the lessor re-arms every lease with
  its full time to live — so the countdown starts again when the sandbox
  starts and runs out *during* the drill. Measured on both verified
  versions: 100 keys attached to a twenty-second lease were gone
  twenty-seven seconds after a restore that reported success, while the
  plain keys beside them were untouched. The drill now refreshes the
  snapshot's leases for as long as the sandbox lives, leaving their
  declared time to live exactly as the backup recorded it. Auto-compaction,
  the mechanism this engine was surveyed for, turned out not to be the
  problem: it is off by default in both versions and removes superseded
  revisions rather than live keys. Ninth engine of the class opened by the
  Prometheus report in 0.16.0.

- **A MySQL or MariaDB drill no longer runs the backup's own scheduled
  jobs** (`adapters/mysql` 0.12.0, `adapters/mariadb` 0.3.0). A dump taken
  with `--events` carries the operator's `CREATE EVENT` statements, and a
  purge event deletes rows in the drill exactly as it does in production:
  measured, an artifact of ten rows and one such event held two rows five
  seconds after the restore, the event arriving `ENABLED`. Whether it runs
  is a default the two families disagree about — `mysql:8.4` and Percona
  ship `event_scheduler=ON`, MariaDB ships it off, and MySQL turned it on
  in 8.0 after years of the opposite — so the drill now pins it rather
  than trusting the answer: `SET GLOBAL event_scheduler = OFF` before a
  logical restore, a startup flag for the physical kind where a statement
  would race the scheduler, and a refusal if the engine will not comply. A
  server started with `--event-scheduler=DISABLED` is left alone. The
  events keep the definitions and status the backup recorded, so checks
  reading `information_schema.events` still see the operator's own
  schedule. Eighth engine of the class opened by the Prometheus report in
  0.16.0.

- **A Redis or Valkey drill no longer reports a successful restore of an
  empty server** (`adapters/redis` 0.4.0, `adapters/valkey` 0.3.0). Both
  engines store an absolute instant with every expiring key, so a backup
  drilled after those instants serves none of them — measured on all nine
  verified images, an artifact of 100 permanent and 100 expiring keys
  served 100 either way: dropped as the RDB is read (the engine reports
  `rdb_last_load_keys_expired`), or removed by the ordinary expiry cycle
  seconds after an append-only load, where the counters report nothing at
  all. Neither adapter ran a key census, so nothing said so. There is no
  setting to suspend it, and the one shape that skips the discard — a
  replica — is worse: it reports `DBSIZE 200` while every read misses.
  The drill now reads the engine's own account of the load and fails when
  the artifact carried keys and the restored server serves none. Seventh
  engine of the class opened by the Prometheus report in 0.16.0.

- **A Cassandra drill no longer reports a table proven when it reads
  nothing** (`adapters/cassandra` 0.2.0). Cassandra filters TTL'd cells at
  read time, so a snapshot drilled after its own time-to-live serves less
  than it holds: measured on the baseline image, a table with
  `default_time_to_live = 60` went from 100 rows in the snapshot to 0 in
  the drill, and one whose rows were half written `USING TTL 60` to 50 —
  while `sstabledump` shows the artifact itself intact, every partition
  present and marked expired. Unlike the four engines before it, this one
  offers no policy to suspend: of the 358 `cassandra.*` system properties
  the jar names, none makes a read return expired data. So the
  post-restore probe, which used to accept an empty answer, now fails the
  drill when the artifact's own sstables declare a time-to-live — a table
  nobody ever wrote (no sstable) and a table whose rows were all deleted
  (`TTL max: 0`) are both still accepted, measured. Fifth engine of the
  class opened by the Prometheus report in 0.16.0.

- **InfluxDB drills no longer let the sandbox enforce the backup's own
  retention** (`adapters/influxdb` 0.2.0). `influx restore` restores a
  bucket's retention period along with its data, and the restored server
  then applies it to what it has just been handed. Measured on the
  baseline image: a one-hour bucket holding seven points spread over three
  hours kept three of them one retention check after the restore, while a
  control bucket with infinite retention was untouched — and the adapter's
  bucket census saw all five buckets throughout, because a bucket that
  lost every point it had is still a bucket. The enforcer runs on a
  thirty-minute ticker by default, so whether a drill saw the loss
  depended on how long it took. The sandbox instance now starts with
  `--storage-retention-check-interval` set past any drill's life; the
  buckets' declared retention is unchanged, so checks reading it still see
  the operator's own policy. Fourth engine of the class opened by the
  Prometheus report in 0.16.0.

- **ClickHouse drills no longer let the sandbox expire the artifact they
  are proving** (`adapters/clickhouse` 0.2.0). A `TTL` clause states what
  a running server should keep; a backup that spent a night in storage
  holds rows that were inside their TTL when it was taken and are past it
  by the drill, and the restored server deletes them exactly as production
  would. Measured on both verified images: a table whose rows had all
  expired went from 60 rows to 0, one where some had from 200 to 146, a
  `TTL … GROUP BY` rollup from 60 to 10, and a column TTL blanked its
  payload while leaving the row count untouched — the quiet case a census
  would not catch. `RESTORE ALL` reported success for every one of them.
  The restore now runs in two passes with `SYSTEM STOP TTL MERGES`
  between them, because the lock only covers tables that already exist and
  the restore is what creates them; a sandbox that refuses the statement
  fails the drill instead of recording a database that deleted part of
  itself. Third engine of the class opened by the Prometheus report in
  0.16.0.

## [0.16.0] - 2026-08-18

One field report set the whole cycle's direction. A Prometheus drill
refused a healthy backup, and the cause turned out not to be the census
that refused it but the sandbox behind it: the restored server was
applying its own fifteen-day retention to data it had just been handed.
The fix is small. The question it raised is not — *which other engines
quietly enforce a policy about what a running server should keep, on an
artifact a drill is supposed to prove?* — and this release is the first
two answers, both yes. MongoDB deletes expired documents about a minute
after the sandbox starts, so what a drill proved depended on how long the
restore took. TimescaleDB is worse: a restored retention policy runs in
the same second the restore frame closes, deterministically, taking half
the rows with it. Both are now held back, and both refuse the drill
rather than record a database that deleted part of itself.

The cycle's new engine arrived with that lesson already applied.
VictoriaMetrics keeps one month by default, and the adapter pins retention
off before its first release rather than after a bug report — one of four
fences that came out of measurement, the sharpest being that a copy of a
live storage directory starts and serves every sample in a quiet moment,
which is exactly why it must be refused by name.

### Added

- **A VictoriaMetrics adapter** (`adapters/victoriametrics` 0.1.0), the
  sixteenth engine. It restores `vmbackup` outputs in three forms: one
  backup directory (`victoriametrics_backup`), a tar archive of one
  (`victoriametrics_backup_tar`, plain or gzip), or the newest of a
  directory of them (`victoriametrics_backup_dir`, ranked by the instant
  each backup's own metadata states rather than by file times a copy
  would reset). Four measured fences carry it. A copy of a live
  `-storageDataPath` is refused by name — it carries the server's lock
  and scratch directories, and it is the dangerous artifact precisely
  because it starts and serves every sample in a quiet moment. A backup
  without the `backup_complete.ignore` marker vmbackup writes last is
  refused, without reaching for the `-skipBackupCompleteCheck` flag the
  tool itself offers. A part named in a partition's `parts.json` but
  absent from the artifact is refused host-side, before a byte is
  transferred, with the engine's own startup check as the backstop for an
  archive the host could only walk for markers. And the restored server's
  own series count refuses a well-formed zero, because `vmrestore`
  restores a truncated backup silently (measured). The sandbox pins
  `-retentionPeriod=100y`: the engine keeps one month by default, so a
  restored 90-day history would otherwise serve 48 of its 89 samples with
  nothing reporting the loss — the survey in #166 applied to a new engine
  before its first line of code. Checks are MetricsQL through `promtool`
  as a query client, travelling as one argv element, because a
  form-encoded query decodes `.+` as `. ` and answers zero on a populated
  server (measured). A drill needs the server, `vmrestore` and a client in
  one image and VictoriaMetrics ships them apart, so the README documents
  a four-line wrapper that also ties their versions together. Zero core
  changes, conformance 16/16, verified against VictoriaMetrics 1.150 and
  1.120.

### Fixed

- **A Prometheus drill no longer inherits the server's default
  retention** (`adapters/prometheus` 0.3.0, #165). Started without
  retention flags, the sandbox server applies its 15-day default when the
  TSDB opens and *deletes* every block lying wholly outside that window
  from the restored copy — so a snapshot covering more than 15 days, the
  ordinary shape of a monitoring history kept for compliance, failed the
  block census as a partial restore; and had the census not caught it,
  every check would have read less than the backup holds. The drill now
  starts the server with retention pinned off in both dimensions
  (`--storage.tsdb.retention.time=100y --storage.tsdb.retention.size=0`),
  the right trade for a disposable sandbox: retention states what a
  running server should keep, while a drill proves what the backup
  holds — and the operator's real policy is already expressed in which
  blocks the snapshot contains. Measured on both verified versions, where
  the trigger turned out to be the snapshot's own span rather than its
  age: a snapshot taken a minute ago loses blocks if it covers more than
  the window, while a year-old one covering two days does not.

- **A MongoDB drill no longer lets the sandbox expire the artifact**
  (`adapters/mongodb` 0.5.0, #166). MongoDB deletes documents past a TTL
  index's expiry from a background thread whose first pass lands about a
  minute after mongod starts, so a backup restored later than its own TTL
  window — an hour-long session expiry in yesterday's archive, a
  ninety-day audit collection in one older than that — arrived intact and
  emptied itself mid-drill while `mongorestore` reported every document
  restored successfully. Measured on the verified image: 500 of 500
  expired documents gone in a single pass, a collection without a TTL
  index untouched, and this adapter runs no document census, so nothing
  would have reported the loss. Worse than the loss was what it depended
  on: the pass fires on the server's clock, not the drill's, so a small
  backup that finished inside the first minute saw its data and a
  production-sized one did not. The adapter now disables the monitor
  before the restore, and refuses the drill with `invalid_request` if the
  engine will not let it — a record whose content depends on how long a
  restore took is not evidence. First verdict of the survey in #166.


- **A TimescaleDB drill no longer lets the restored database trim itself**
  (`adapters/postgres` 0.12.0, #166). `timescaledb_post_restore()` does
  not merely release the background workers: a restored retention policy
  runs in the same second it returns, because `bgw_job_stat` is absent
  from the dump and a job with no `next_start` is due immediately.
  Measured on a hypertable holding 200 days under a 90-day policy: 15 of
  29 chunks and 52% of the rows gone before the frame closed, with the
  restore reported successful — deterministic, not a race. The framed
  kinds now park every job in the restored catalog
  (`next_start => 'infinity'`) inside the window the frame already owns,
  and fail the restore if they cannot. The lever is `next_start` rather
  than the job's `scheduled` flag on purpose: the dump carries
  `scheduled` but not the statistics row, so the pin fills a field the
  restore left empty and overwrites nothing the backup held — checks
  reading `timescaledb_information.jobs` still see the policies the
  backup carried, they simply never run. Second verdict of the survey in
  #166.

## [0.15.0] - 2026-08-17

The standing cadence holds — InfluxDB, the fifteenth engine — but this
cycle's character is consolidation: three adapters gained the halves
their first releases deliberately deferred. Redis and Valkey now restore
the append-only directory beside the RDB snapshot, each with the
manifest as its completeness contract, and the postgres adapter frames
TimescaleDB restores with the procedure the extension mandates, earning
a variant image of its own. The cycle's one field report is fixed with
them: a Prometheus snapshot taken during a compaction window is a
healthy backup, and the block census now applies the server's own
exclusion rule instead of counting directories.

### Added

- **An InfluxDB adapter** (`adapters/influxdb` 0.1.0), the fifteenth
  engine. It restores InfluxDB 2.x `influx backup` outputs in three
  forms: one tar archive (`influx_backup_tar`, plain or gzip), one
  backup directory (`influx_backup` — a reused target directory
  restores its newest set by the stems the backups named themselves),
  or a directory of them (`influx_backup_dir`). The manifest is the
  contract: a partial copy is refused by the member it lost before a
  byte reaches the sandbox, and after `influx restore` the bucket
  census compares the restored organization's own listing against the
  manifest's — a partial restore is never green. No credential from
  the backup is ever needed (measured): the sandbox instance is
  initialized with documented public constants and a plain restore
  creates the backup's organizations itself; `--full` is deliberately
  not used, so the drill never locks itself out behind the backup's
  tokens. The 1.x portable format is refused by name as the migration
  it is — host-side, from a recovered manifest, and on the engine side
  alike — the ROADMAP mandate for this engine. Zero core changes,
  conformance 15/15, verified against InfluxDB 2.7.12, 2.8.0, and
  2.9.1.

- **TimescaleDB source kinds on the postgres adapter**
  (`adapters/postgres` 0.11.0, new kinds `timescaledb_dump` and
  `timescaledb_dump_dir`, verified against
  `timescale/timescaledb:2.29.1-pg17`): the restore is framed with the
  extension's mandated `timescaledb_pre_restore()`/
  `timescaledb_post_restore()` procedure, and every framing second is
  part of the measured restore. The frame is not ceremony — measured: a
  production-shaped dump (compressed chunks, continuous aggregate,
  retention policy) restored unframed aborts partway with "could not
  find hypertable", while a trivial one happens to work, so the
  outcome depends on the backup's shape. Both directions are fenced:
  the plain logical kinds now refuse a TimescaleDB dump by name on
  positive evidence (the archive's own table of contents, or the
  extension statement in a script's bounded head), pointing at the
  framed kind — the gzip-compressed archive form stays unfenced,
  deliberately, since it offers no exact probe and fails loudly either
  way — and the framed kind on an image without the extension names
  the image as the fix. The variant matrix job runs the whole logical
  suite on the timescale image plus a hypertable drill that proves
  compressed chunks, the continuous aggregate, and the retention
  policy all survive.

- **Valkey append-only restores** (`adapters/valkey` 0.2.0, new source
  kind `valkey_aof`), the mirror of the redis half below: a copy of the
  append-only directory Valkey kept from Redis 7 — manifest, base,
  incremental segments — replayed in full, with the same
  manifest-completeness gate and the derived `--appendfilename` that
  stops an unmatched name from silently starting an empty set. The
  in-sandbox integrity gate is member-by-member — the base through
  `valkey-check-rdb`, each incremental segment through
  `valkey-check-aof` — because the tool's manifest mode misreads the
  `VALKEY`-magic base a 9.x rewrite writes as a RESP base and rejects
  the healthy set the server itself produced (measured); a gate
  stricter than the engine would fail restorable backups. The base
  RDB's header feeds the valkey version pre-check and the Redis-dialect
  fence — a Redis-saved base, or one carrying the post-fork format
  floor, is refused toward the redis adapter, both header layouts read
  (`REDIS` through 8.x, `VALKEY` from 9.0). `backup.created_at` stays
  deliberately null. Integration-proven on real servers: a rewritten
  base plus a post-rewrite tail both replay.

- **Redis append-only restores** (`adapters/redis` 0.3.0, new source
  kind `redis_aof`): a copy of the Redis 7+ append-only directory —
  manifest, base, incremental segments — is replayed in full. The
  manifest is the completeness gate this artifact needs most: a copy
  taken mid-rewrite loses members, and a manifest naming a file the
  backup does not hold is refused as an incomplete copy before a byte
  reaches the sandbox. In the sandbox `redis-check-aof` vets the whole
  set, and the server reads the staged manifest by its own derived
  `--appendfilename` — an unmatched name would silently start an empty
  set, exactly the false green a drill must not produce. The base RDB's
  header feeds the existing version pre-check and Valkey dialect fence;
  `backup.created_at` stays deliberately null (the base's ctime dates
  the last rewrite, not the backup). The integration suite proves a
  rewritten base plus a post-rewrite tail both replay, measured against
  real servers.

### Fixed

- **The Prometheus block census no longer refuses compaction-window
  snapshots** (`adapters/prometheus` 0.2.0, #155). A snapshot taken
  while compaction sources still sat on disk legitimately holds both a
  compacted block and the parents it replaced; the server skips a block
  named in another present block's `compaction.parents` — deduplication,
  not a failed load (measured). The census now expects present blocks
  minus present-and-superseded ones, mirroring the server's own rule, so
  a healthy compaction-window snapshot passes while a block that truly
  failed to load still refuses the drill. Parent lists are read
  tolerantly (objects or bare ULID strings), metadata claiming every
  block supersedes another is refused as damage, and the count of
  skipped sources is logged with each drill.

## [0.14.0] - 2026-08-16

The standing cadence holds: exactly one new engine in this cycle —
OpenSearch, the fourteenth — the first built on the sysctl decision's
measured no-privilege design, with the census and shard-gate fences an
engine forgiving in both directions demanded. The verified matrix also
gains its first two variant images, pgvector and Percona Server, each
earning its listing by exercising exactly what makes it a variant.

### Added

- **An OpenSearch adapter** (`adapters/opensearch` 0.1.0), the
  fourteenth engine — the first shipped on the sysctl decision's
  measured design, and the proof the dilemma was dissolved rather than
  postponed. It restores `fs` snapshot repositories in two forms: one
  repository directory (`opensearch_repo`, the `location` of a
  registered `fs` repository) or one tar archive of it
  (`opensearch_repo_tar`, plain or gzip, the repository at the root or
  under one wrapping directory). A repository holds every snapshot ever
  taken into it, so the drill restores the one whose own metadata
  claims the newest instant (`end_time_in_millis` — also `created_at`,
  epoch-exact), refuses it by name if its state is not `SUCCESS`, and
  refuses the version pairing before the transfer when the metadata
  names a writing engine newer than the sandbox (snapshots do not
  restore on older engines; the engine's own refusal is mapped to the
  same answer when the metadata is silent). The node starts *before*
  the transfer — `path.repo` is a static setting — in exactly the
  loopback dev mode the decision measured: single-node discovery, the
  security plugin off, `node.store.allow_mmap: false`; no sysctl, no
  privilege, and no wrapper image (the official
  `opensearchproject/opensearch` images idle under `command: sleep
  infinity`, entrypoint pass-through measured). Checks are OpenSearch
  SQL through the bundled SQL plugin, its raw format absorbed
  declaratively in the runner template; the generating built-in checks
  do not apply (the plugin rejects SQL-standard quoted identifiers,
  measured — the same trade the mongodb, etcd and prometheus adapters
  document). System indices are excluded from the restore: they belong
  to the running node and collide with it by name (measured). Zero
  core changes; conformance 15/15; verified against OpenSearch 2.19.6
  (the baseline) and 3.8.0. Deliberately absent, with reasons in the
  README: remote repositories (S3/Azure/GCS — a zero-ingress sandbox
  cannot reach them by design), system indices and cluster state, and
  Elasticsearch snapshots (a different engine's artifact; the pairing
  refusal names the mismatch).
- **The census and the shard gate, fences against an engine that is
  forgiving in both directions** — measured, OpenSearch registers a
  directory that is no repository at all without a word and simply
  lists zero snapshots, and a restore from damaged repository data
  returns HTTP 200 with failed shards and the cluster red. Neither is
  callable green here: host-side, the repository's own files
  (`index.latest`, the `index-<gen>` it names) must parse and claim at
  least one snapshot before a byte is transferred — for the archive
  kind judged from the tar stream in one pass, without unpacking — and
  the engine's post-registration listing is compared against that
  claim; sandbox-side, the restore verdict is read from the shard
  counts and a green-cluster gate (replicas forced to the single
  node's honest zero), never from the HTTP status. The API calls run
  `curl` without `-f` deliberately, because an HTTP error's body is
  where the engine states its reason in its own words. Raw
  data-directory copies (and tars of them) are refused by their own
  markers (`nodes`, `_state` — never in an `fs` repository), teaching
  the repository workflow in the message.
- **pgvector joins the postgres adapter's verified matrix** — the first
  variant-image entry, under the rule added to
  `docs/engine-versions.md` §1: a variant may be listed only when the
  suite exercises what makes it a variant. The `pgvector/pgvector`
  image's matrix job seeds a vector column under an HNSW index, dumps
  it, restores it through the drill, proves the index was rebuilt, and
  answers a nearest-neighbour query through the declared runner — a
  plain dump restoring on the image would have said nothing about
  vectors. Zero adapter-code changes. The README records the two
  operational truths the ROADMAP line predicted: the extension's
  version comes from the sandbox image, not the backup (`pg_dump`
  records `CREATE EXTENSION` without one), and HNSW/IVFFlat rebuilds
  are part of the measured restore, so a vector-heavy drill's RTO trend
  tracks index build time.
- **Percona Server joins the mysql adapter's verified matrix** — the
  second variant-image entry (`percona/percona-server:8.4.10`, the 8.4
  LTS line), zero adapter-code changes. The variant job runs the whole
  logical suite on the Percona image — the drop-in claim, exercised on
  every run — and adds the physical pairing the ROADMAP line named: a
  backup taken by XtraBackup 8.4 *from* Percona Server, restored *into*
  Percona Server. The official image ships no XtraBackup, so the suite
  assembles the mysqld+xtrabackup+gosu contract from official Percona
  parts (a measured recipe — XtraBackup 8.4's binaries with their
  private-library layout — which the README ships for operators
  wanting the same in-sandbox prepare on Percona).

## [0.13.0] - 2026-08-16

The standing cadence holds: exactly one new engine in this cycle —
Apache Cassandra, the thirteenth — with the census and digest fences its
never-say-no restore tooling demanded, on measured facts.

### Added

- **An Apache Cassandra adapter** (`adapters/cassandra` 0.1.0), the
  thirteenth engine — the biggest name left in the catalog's
  fits-the-current-model group, and the engine whose restore tooling
  most needed distrust. It restores collected `nodetool snapshot`
  backups in three forms: one tar archive (`cassandra_snapshot_tar`,
  plain or gzip, keyspaces at the root or under one wrapping directory),
  one collected tree (`cassandra_snapshot`, `<keyspace>/<table>/`
  holding each table's `snapshots/<tag>/` contents — the README shows
  the exact collection loop, and the integration suite runs that very
  loop), or the newest of a directory of trees
  (`cassandra_snapshot_dir`), ranked by the instant each snapshot's own
  manifests claim — `manifest.json` states it in UTC (measured on 4.1
  and 5.0), which is also `created_at`, exact. The adapter prepares a
  single node for the zero-ingress sandbox itself (hostname-to-loopback
  and address pinning, both measured as required under `--network
  none`; the official images idle under `command: sleep infinity` with
  no wrapper), recreates each table from the backup's own `schema.cql`,
  streams the sstables in, and reads one row of every restored table
  before reporting. The cqlsh dialect — decorated output and all — is
  absorbed declaratively in the runner template (an awk filter turns
  the measured table shapes into the protocol's undecorated
  tab-separated rows, pipefail carrying cqlsh's own exit code), so the
  generating built-in checks apply unchanged. Honesty stated where it
  belongs: a per-node snapshot is not cluster-consistent, the drill
  proves exactly what was restored, and the keyspace is created
  drill-locally at replication factor 1 because `schema.cql` carries
  table DDL only (measured). Version pairing measured in both
  directions: 4.1 sstables stream into a 5.0 node cleanly, while a 5.0
  snapshot's own schema fails on 4.1 ("Unknown property
  'allow_auto_snapshot'") and is mapped to a refusal naming both
  sides. Zero core changes; conformance 15/15; verified against
  Cassandra 4.1 (the baseline) and 5.0. Deliberately absent, with
  reasons in the README: Medusa, incremental `backups/`, commitlog
  PITR, and multi-node topology restores.
- **The census and the digest, fences against a loader that never says
  no.** Measured twice, refused twice — host-side, from the artifact's
  own claims, before a byte is transferred: `sstableloader` finding no
  complete sstable streams zero bytes and **exits 0**, so every
  component a sstable's own `TOC.txt` and the snapshot's `manifest.json`
  list must exist; and the loader streams a **corrupted Data file
  without a word** — the damage surfacing only when the restored table
  is read — so every Data file is verified against the CRC-32 its own
  `Digest.crc32` sidecar claims. For the archive kind both are judged
  from the tar stream in one pass, without unpacking. The raw-copy
  fence refuses table directories carrying `snapshots/` or `backups/`
  (a copy taken from under a running node, never a snapshot's shape),
  system keyspaces are refused as never being what a backup drill
  means, and directory names must be unquoted CQL identifiers before
  they may reach composed CQL. A read probe after the restore closes
  whatever none of the claims predicted, carrying the engine's own
  refusal.

## [0.12.0] - 2026-08-16

The standing cadence holds: exactly one new engine in this cycle —
Prometheus, the twelfth — with the census and probe fences its
forgiveness demanded, on measured facts.

### Added

- **A Prometheus adapter** (`adapters/prometheus` 0.1.0), the twelfth
  engine — monitoring history is backup-worthy data with a compliance
  clock of its own, and this is the engine whose forgiveness demanded
  the most fences. It restores API snapshots in three forms: one tar
  archive (`prometheus_snapshot_tar`, plain or gzip, blocks at the root
  or under one wrapping directory — both measured), one snapshot
  directory (`prometheus_snapshot`), or the newest from a directory of
  them (`prometheus_snapshot_dir`) — ranked by the instant each
  snapshot's own blocks claim to cover, never by file times a copy
  would reset. `created_at` is that same claim, epoch milliseconds,
  exact. Checks are PromQL through `promtool`, evaluated at the
  backup's own newest instant: the protocol's `{{database}}`
  placeholder delivers it into `--time`, so a drill reads the restored
  data deterministically instead of an empty now. Zero core changes;
  conformance 15/15; verified against Prometheus 3.5 (the LTS line,
  baseline) and 3.13, with cross-version restores measured in both
  directions. The official images pin the server binary as their
  entrypoint and cannot idle as a drill sandbox (measured), so drills
  run a two-line wrapper (`ENTRYPOINT []`), recipe in the adapter
  README — which also names what is deliberately absent: object-store
  blocks (Thanos/Mimir/Cortex), raw data-directory copies, and a
  full-chunk read at provision time, because PromQL has no
  collision-free read-everything query (measured) and the README says
  exactly what is verified instead.
- **The census and the probe, fences against a server too forgiving to
  be trusted alone.** Measured: a block with a corrupted index makes
  the server refuse to start — surfaced within seconds as
  `source_corrupt` carrying the server's own log line (both lines'
  wordings measured), not after the readiness budget — but a block
  whose meta.json is unreadable is *silently skipped*, the server
  reports ready, and generic queries answer; and corrupted chunk data
  is caught only when actually read ("cannot populate chunk …:
  checksum mismatch", measured). So after readiness the drill compares
  `prometheus_tsdb_blocks_loaded` from the restored server's own
  /metrics against the block count the artifact itself states —
  refusing a partial restore by name — and counts every series at the
  newest instant the backup claims, refusing a well-formed zero: a
  server that is up but serves none of the promised data is exactly
  the false green a monitoring backup invites. The raw-copy fence
  completes the set: a data directory copied (or tarred) from under a
  running server carries `wal`, `chunks_head` and `lock` — entries an
  API snapshot never contains (measured) — and is refused with the
  snapshot API in the message.

## [0.11.0] - 2026-08-16

The standing cadence holds: exactly one new engine in this cycle —
DuckDB, the eleventh, the second embedded one — carrying the version
fence its format makes real, on measured facts.

### Added

- **A DuckDB adapter** (`adapters/duckdb` 0.1.0), the eleventh engine and
  the second embedded one, riding the pattern the sqlite adapter proved:
  no server, no shell in any step — direct argv throughout — and the
  restored file's path reaches the declared runner through
  `{{database}}`. Zero core changes. It restores copies of cleanly
  closed database files (`duckdb_db`, newest-in-directory via
  `duckdb_db_dir`) and `EXPORT DATABASE` directories (`duckdb_export`),
  with CSV and Parquet exports both restoring offline (measured — the
  extensions are bundled) and `IMPORT DATABASE` resolving the paths
  `load.sql` baked in against wherever the export now sits (measured
  against a moved export). Conformance 15/15; verified against DuckDB
  1.4 — the designated LTS, the baseline — and 1.5. The official
  `duckdb/duckdb` images carry only the engine binary: no shell, not
  even a way to idle (measured), and the binary does not start on musl
  (measured), so drills run a two-line wrapper on a glibc base, recipe
  in the adapter README, and the integration suite builds exactly that
  wrapper from the manifest's listed image. Deliberately absent, with
  reasons in the README: encrypted databases (1.4's `ENCRYPTION_KEY`
  needs a key-handling design, not an assumption), compressed
  artifacts, and crash-image `db`+`.wal` pair restores.
- **The storage-format fence, this engine's version pre-check** — the
  first embedded engine where the impossible pairing is real: a DuckDB
  header names its storage format version and the library that wrote it
  (offsets 12 and 52, measured), DuckDB writes the oldest compatible
  format by default so files travel both ways between the verified
  lines (measured), but a file written with a newer `STORAGE_VERSION`
  is refused by an older engine at open (measured: 1.4.5 answers "we
  can only read versions between 64 and 67" to a v1.5.0-format file).
  The adapter maps that engine refusal to `invalid_request` naming both
  sides — the format and writer from the file's own header, the engine
  from its version probe — because it is a drill config pairing a
  backup with a sandbox image that cannot read it, the
  `docs/engine-versions.md` §5 shape. The live-copy fence carries over
  from the sqlite cycle on this engine's own measurements: a non-empty
  `.wal` sibling means the database file alone opens cleanly and
  silently misses every committed transaction still in the write-ahead
  log (505 rows live, 500 in the copy — measured), so the copy is
  refused by name with the fix in the message. No zero-byte or
  corruption gates duplicate the engine here: DuckDB checksums its
  blocks and refuses invalid files at open (measured), which is exactly
  the honesty the sqlite adapter had to build host-side.

## [0.10.0] - 2026-08-16

The standing cadence holds: exactly one new engine in this cycle —
SQLite, the tenth, and the first embedded one — together with the
live-copy fence its ROADMAP line demanded, built on measured facts.

### Added

- **An SQLite adapter** (`adapters/sqlite` 0.1.0), the tenth engine and
  the first embedded one: there is no server to start, so provision
  places the artifact — or replays the dump — under the sandbox's
  scratch directory, and every check opens the restored file through the
  probe-declared runner, whose `{{database}}` placeholder delivers a
  path the probe could not know. Zero core changes: the protocol's
  connection shape expresses "nothing listens here" as written. It
  restores self-contained database files from `sqlite3 .backup` or
  `VACUUM INTO` (`sqlite_db`, newest-in-directory via `sqlite_db_dir`)
  and `.dump` SQL text (`sqlite_dump`, `sqlite_dump_dir`), vetted
  in-sandbox by `PRAGMA integrity_check` — whose contract of printing
  problems while exiting 0 (measured) a sandbox-side shell folds into an
  exit code, the same declarative absorption the runner template
  performs for dialects. Conformance 15/15; verified against SQLite
  3.46, 3.49, 3.50, 3.51 and 3.53 — there is no official SQLite image,
  so the community image `keinos/sqlite3` is the named verification
  target and any image carrying a POSIX shell and the sqlite3 CLI works.
  Deliberately absent, with reasons in the adapter README: Litestream
  PITR, compressed artifacts, crash-image `db`+`-wal` pair restores, and
  any version pre-check — the file format is compatible in both
  directions since 3.0.0, so unlike SQL Server and the RDB engines there
  is no impossible pairing to refuse up front.
- **The live-copy fence, this engine's own false green** — the ROADMAP
  line ordered the adapter "must not pretend a copy of a live database
  is safe", and the measured facts made it three fences, each firing on
  positive evidence only. A database copied beside a non-empty `-wal` or
  `-journal` sibling is refused by name: the main file alone passes
  `integrity_check` with `ok` while silently missing every transaction
  still in the write-ahead log (measured), which is precisely the green
  drill an auditor must never see. A zero-byte artifact is refused
  host-side because sqlite3 accepts it as a valid empty database —
  `integrity_check` says `ok` and queries answer (measured) — while even
  a schema-less database backs up as a full header page. And a `.dump`
  that opens with its exact signature but lost its `COMMIT;` trailer is
  refused before transfer, because replaying such a file exits 0 and
  leaves an empty database — the wrapping transaction never commits and
  the rollback erases every row without a word (measured — the
  truncation no exit code will ever report). The healthcheck counts
  `sqlite_schema` instead of `SELECT 1` for the same family of reasons:
  a bare constant query answers even against a file that is not a
  database on older CLIs (measured on 3.46).

## [0.9.0] - 2026-08-15

The standing cadence again: exactly one new engine in this cycle —
Valkey — together with the cross-dialect fence its ROADMAP line
demanded, enforced on both sides of the fork.

### Added

- **A Valkey adapter** (`adapters/valkey` 0.1.0), the ninth engine — a
  distinct engine with its own version matrix, exactly as ROADMAP.md
  demanded, because since the fork the two RDB dialects are not
  interchangeable above format version 11 and restoring one in the
  other's sandbox would be a false green. It restores RDB snapshots
  (`valkey_rdb`, newest-in-directory via `valkey_rdb_dir`) the same
  measured way the redis adapter does — placed in the adapter's own data
  directory, vetted by `valkey-check-rdb`, loaded by a daemonized
  `valkey-server` with persistence pinned off — and reads both header
  layouts Valkey has shipped: the pre-fork `REDIS0011` magic its 7.2/8.x
  lines write, and the `VALKEY`-magic numbering 9.0 switched to. The
  `valkey-ver` aux field (written by every Valkey since the fork, never
  by Redis — measured) feeds the asymmetric version pre-check, and the
  dialect is fenced on positive evidence before a byte is transferred: a
  `redis-ver` aux or a `REDIS`-magic format version ≥ 12 refuses the
  artifact as `unsupported_source`, pointing at the redis adapter —
  necessary because `valkey-check-rdb` passes a post-fork Redis file
  that the server then refuses to load ("Can't handle RDB format
  version 12", measured). A sandbox whose `valkey-server --version`
  reports Redis is refused the same way. Zero core changes,
  conformance 15/15, verified against Valkey 7.2, 8.0, 8.1, 9.0, 9.1.

### Changed

- **The redis adapter fences the Valkey dialect by name** (redis 0.2.0),
  the mirror image of the new adapter's fence: an RDB carrying a
  `valkey-ver` aux field or the `VALKEY` magic is refused as
  `unsupported_source` pointing at the valkey adapter — previously a
  9.x-magic file failed opaquely as `source_corrupt`, and a pre-fork
  Valkey file restored silently into an engine the backup does not
  belong to. A sandbox whose `redis-server --version` reports Valkey
  (the Valkey images ship `redis-*` compatibility symlinks, so presence
  alone cannot tell) is refused as `invalid_request`. Refusals fire on
  positive evidence only; artifacts and sandboxes that state nothing
  keep restoring exactly as before.

## [0.8.0] - 2026-08-15

Back to the roadmap's standing cadence after the recorded 0.7.0
exception: exactly one new engine in this cycle.

### Added

- **Physical restores refuse the wrong engine version before they start**
  (postgres 0.10.0, mysql 0.10.0, mariadb 0.2.0, mssql 0.8.0). A physical
  backup restores only into its own major (PostgreSQL) or release series
  (MySQL, MariaDB) — `docs/engine-versions.md` §5 recorded the
  requirement when the version matrix shipped — and every physical format
  here names its origin server: pgBackRest's `backup.info`,
  `xtrabackup_info` / `mariadb_backup_info`, and the `.bak` header row
  the mssql adapter already reads. Each adapter now compares that against
  the engine in the sandbox and refuses an impossible pairing as
  `invalid_request`, with a message naming both sides and what to change
  — where the backup is read host-side, before a byte is transferred —
  instead of letting the engine fail minutes later in its own words.
  SQL Server's rule is asymmetric and encoded as such: only the downgrade
  is refused, because restoring an older backup onto a newer engine is
  the supported upgrade path. The check refuses only on positive
  evidence: an encrypted manifest, a missing `server_version`, a header
  that stops short, or an unanswerable engine skips it, and the restore
  speaks for itself.

- **A Redis adapter** (`adapters/redis` 0.1.0), the eighth engine. It
  restores RDB snapshots — one named artifact (`redis_rdb`) or the newest
  in a directory (`redis_rdb_dir`) — by placing the file in its own data
  directory, having `redis-check-rdb` vet it, and starting a daemonized
  `redis-server` on it with persistence pinned off, so the artifact is
  never rewritten under the drill. Zero core changes, conformance 15/15,
  verified against Redis 7.2, 7.4, 8.2, and 8.10.

  The RDB header pays for itself twice. `ctime` records the save instant
  in epoch seconds, so `backup.created_at` is exact with no
  `backup_timezone` declaration — the second format after pgBackRest with
  that property — and the directory kind ranks candidates by what each
  artifact records about itself, never by file times alone. And
  `redis-ver` names the server that saved the backup, which feeds the
  version pre-check above: Redis's rule is asymmetric like SQL Server's,
  so only a newer server's RDB handed to an older engine is refused.

  Checks follow the MongoDB/etcd precedent for engines without SQL: a
  check is a line of redis-cli arguments run through the declared
  word-splitting runner, with `-e` turning command errors into failing
  exits. The restored data carries no credentials to reset — requirepass
  and ACLs live in server config, not in the RDB — and the engine flow
  needs no shell at all. Deliberately not shipped, with reasons in the
  adapter README: AOF restores (Redis's own docs recommend RDB snapshots
  for backups; the 7.x append-only directory is a separate, measured
  piece of work), compressed artifacts (refused by name), and any Valkey
  claim — a distinct engine with its own matrix.

## [0.7.1] - 2026-08-15

This release corrects the record of v0.7.0, whose git tag was placed one
merge too early: on the roadmap update that preceded the release
preparation, not on the preparation itself. Everything v0.7.0 *shipped*
is correct — the binaries take their version from the tag name at build
time, nothing but documentation and version strings differs between the
two commits, and the release notes describe what the binaries do. What
is wrong is the tagged tree's account of itself: its README says
"Released as v0.6.0" and names four engines instead of seven, its
changelog holds the release's content under Unreleased with no [0.7.0]
section — the very section the release notes link to — and its install
examples point at 0.6.0 artifacts.

The tag stays where it is. The Go module proxy records a version
permanently on first fetch, so moving a published tag can leave anyone
who already resolved it with an unrepairable checksum mismatch — the
same immutability that froze v0.1.0 under its old module path. A project
whose product is an append-only record corrects its record the same way
it expects its users to: not by rewriting the entry, but by appending
the correction. This entry is that correction, and v0.7.1 is cut on a
tree that describes itself truthfully. `spec/evidence/v0.4.0` needs no
successor: the verifier code at that tag is exactly what [0.7.0]
announced, and the only stale line in it — the module README's own
install pin — is documentation, not code.

### Fixed

- Nothing in the software. The only changes since v0.7.0 are the version
  references this repository's documentation gates hold to the current
  release: the README status line and install examples, the Docker and
  packaging documentation, the packaging scripts' usage comments, and
  the binary's dev-version stamp. The v0.7.0 and v0.7.1 binaries behave
  identically; upgrading matters only if you build from the tagged
  source or cite the tagged tree.

## [0.7.0] - 2026-08-15

This release deviates, once and on record, from the roadmap's standing rule
of at most one new engine per release cycle: ClickHouse, MariaDB, and etcd
all ship in it. The rule stays. What it protects — that no adapter ships
which nobody keeps green — is protected here by machinery rather than pace:
each of the three landed as its own reviewed pull request with conformance
15/15, and the engine-version matrix below re-proves every claimed engine
version weekly, on every manifest change, and again before this tag was
allowed to build.

### Added

- **The postgres adapter restores plain-SQL dumps, and gzip-compressed
  dumps of either format** (postgres 0.9.0). `pg_dump`'s *default* output
  is plain SQL, and `pg_dump … | gzip > db.sql.gz` is what a great many
  backup jobs write, yet every logical kind accepted only an uncompressed
  custom-format archive. A gzipped archive was worse than unsupported: it
  reached `pg_restore`, which reported "input file does not appear to be a
  valid archive", and the drill recorded `source_corrupt` — telling an
  auditor the backup was damaged when it was merely compressed.

  Format and compression are both recognised from the artifact's bytes,
  never from its name, and the artifact is restored **as stored**: the
  decompression happens inside the sandbox, streamed into the client, so
  `backup.checksum` and `size_bytes` cover the bytes the backup archive
  actually retains. A plain-SQL dump is replayed by `psql -v
  ON_ERROR_STOP=1`, an archive by `pg_restore`, and either may be fed by
  `gzip -dc`. The `pgdump_with_globals` members are sniffed independently,
  so a job may compress one and not the other. The `-Ft` tar format stays
  unsupported and is now refused by name instead of being handed to a
  client that would misreport it.

  Directory ranking is unaffected by either dimension: both formats record
  when the dump began in their *head* — the archive header, or the
  `-- Started on` line `pg_dump` writes under `--verbose` — so a candidate
  stored compressed is inflated a few kilobytes rather than whole, and the
  ordering still comes only from what each artifact records about itself.
  A plain dump taken without `--verbose` carries no date at all and ranks
  below every dump that does, as an undatable artifact always has.

- **A ClickHouse adapter** (`adapters/clickhouse` 0.1.0), the fifth engine
  and the first one added since the engine catalog went into `ROADMAP.md`.
  It restores native backup archives — `BACKUP … TO File('name.zip')` —
  either one named artifact (`clickhouse_backup`) or the newest in a
  directory (`clickhouse_backup_dir`). Zero core changes, conformance
  15/15, verified against ClickHouse 26.3 and 26.7.

  Three things are worth knowing before you point a drill at it, and all
  three are measured rather than assumed:

  - **The built-in checks work unchanged.** ClickHouse speaks SQL, so
    `row_count`, `table_exists` and `freshness` reach it exactly as the
    core composes them — the first engine since MongoDB where that is
    true, and the reason this one was picked first. The end-to-end test
    validates the restore with the core's own generated statements rather
    than a hand-written query, so the claim cannot quietly stop being
    true.
  - **`backup.created_at` is real.** A ClickHouse archive carries a
    `.backup` manifest whose header records when the `BACKUP` ran. What it
    does not carry is an offset, so `source.params.backup_timezone` says
    where that server stood; without it the record's `created_at` stays
    null rather than claiming a wrong instant. The same timestamp is what
    ranks a directory — never the file's modification time, which dates a
    copy.
  - **A half-written archive stops the drill instead of being skipped.**
    A backup job still writing its zip leaves an unreadable archive newer
    than everything else; quietly restoring last night's instead would
    leave a record the operator reads as covering tonight's. An unreadable
    archive *older* than the chosen one is ignored, so one broken artifact
    does not block every future drill.

  `RESTORE ALL` covers every artifact shape, so the adapter never has to
  know what you backed up. Unpacked directory backups, `clickhouse-backup`
  layouts, and PITR are not supported; the README says why for each.

- **A MariaDB adapter** (`adapters/mariadb` 0.1.0), the sixth engine.
  Logical restores of `mariadb-dump`/`mysqldump` SQL files
  (`mariadb_dump`), plain or gzip-compressed, single file or
  newest-in-directory (`mariadb_dump_dir`), and **physical restores of
  unprepared `mariadb-backup` full backups** (`mariadb_backup`) — for
  which, unlike the sibling adapter's XtraBackup flow, no separate tool
  image is needed: the official `mariadb` images carry everything. Zero
  core changes, conformance 15/15, verified against MariaDB 10.11, 11.4,
  11.8 and 12.3.

  A separate adapter rather than a second engine under `adapters/mysql`,
  and the deciding fact is measured: **the official `mariadb:12` image no
  longer ships `mysql`-named binaries at all**, so the sibling adapter —
  which drives the `mysql` client — cannot even start against the newest
  MariaDB line. Every drill here runs the `mariadb`-named tools, and every
  evidence record names `engine: mariadb`, which is what an auditor should
  read for a MariaDB restore.

  Two lessons from the sibling's open defects are built in rather than
  inherited: dumps are fed to the client on **stdin**, never through the
  client-side `source` command whose handling shifted under the MySQL 9
  client; and `mariadbd` having no `--daemonize` (measured) means the
  physical flow backgrounds the server and, when readiness times out,
  reads the server's own error log so a start failure names the engine's
  reason instead of "never became ready". The dump-completeness gate
  carries over: a dump that announces itself must end with its
  `-- Dump completed` sign-off, and both banner lineages are recognised.
  Both metadata generations of the physical format are read too — running
  every claimed version before claiming it caught that MariaDB 11.0
  renamed `xtrabackup_checkpoints`/`xtrabackup_info` to
  `mariadb_backup_*`, which a 10.11-only test would never have shown.

  Not yet ported: the `*_with_users` accounts kind — MariaDB's account and
  role machinery differs enough from MySQL 8's that the principal-chain
  verification will be ported measured, not assumed.

- **An etcd adapter** (`adapters/etcd` 0.1.0), the seventh engine — the
  smallest adapter in the repository and arguably the highest stakes per
  byte, because an etcd snapshot that does not restore is a Kubernetes
  cluster that does not come back. It restores `etcdctl snapshot save`
  artifacts (`etcd_snapshot`, or newest-in-directory via
  `etcd_snapshot_dir`), starts the server on the restored data, and
  serves checks written as etcdctl argument lines. Zero core changes,
  conformance 15/15, verified against etcd 3.5 and 3.6.

  The engine forced two design points worth knowing. **The official etcd
  images are distroless** — measured, they contain `etcd`, `etcdctl` and
  `etcdutl` and nothing else, no shell — while a snapshot restore
  *requires* starting the server after `etcdutl` writes the data
  directory, which needs a shell to detach the process. The drill sandbox
  therefore runs a two-line wrapper image (alpine plus the three binaries
  copied from the official image; the recipe is in the adapter README),
  and a drill pointed at the raw official image fails up front with a
  message that says so. **A snapshot cannot date itself**: it records
  revisions and raft terms, not wall clocks, so `backup.created_at` is
  always null, `backup_timezone` is refused rather than ignored, and the
  directory kind ranks by file time with the shared in-flight guard.

  Snapshots lacking the integrity hash `etcdctl snapshot save` appends —
  `db` files copied out of a live data directory — are refused with a
  message that says how to take the backup instead, because the hash is
  the only self-verification this format has.

- **Adapters are now verified against several engine versions, and the
  claim has to earn itself.** `docs/capabilities.json` named one version
  per adapter — PostgreSQL 16, MySQL 8.4, MongoDB 7, SQL Server 2022 —
  because the accessor the integration suites called returned the *first*
  entry of a list that could already hold many. Listing more would have
  published claims nothing exercised, which is the failure that file exists
  to prevent.

  Exactly one entry per adapter is now the `baseline` every push restores
  from; the rest run in a matrix that fires weekly, on any pull request
  that edits a manifest, and again before every release tag. The jobs are
  generated from the manifests rather than written into a workflow, a test
  fails when the jobs and the published claims disagree in either
  direction, and the suites refuse an image the manifest does not list — so
  no workflow can report a green run for a version this project never
  claimed.

  Now verified: **PostgreSQL 14, 15, 16, 17**, **MySQL 8.4**, **MongoDB
  7.0**, **SQL Server 2019, 2022 and 2025**.

  Policy in `docs/engine-versions.md`: what qualifies for the list, when
  each version runs, and the two things `verified` never means — that an
  unlisted version fails, or that a listed one says anything about your
  data. Physical restores get a paragraph of their own, because for
  pgBackRest and XtraBackup sources the matrix is correctness rather than
  test hygiene: those formats restore into their own major version and no
  other.

- **Three engine versions do not work, and the matrix found all three on
  its first run.** They are named here rather than left as gaps, because a
  drill failing for the adapter's reasons is worse than one that never ran:

  - **MySQL 9.x** — the adapter loads a dump with `mysql -e "source …"`.
    The 8.4 client still interprets that client-side command; the 9.7
    client hands it to the server, which rejects it. Every logical restore
    fails, and the verdict is `source_corrupt` — the drill blames a
    perfectly good backup for an adapter defect.
  - **PostgreSQL 18** — the official image moved `PGDATA` from
    `/var/lib/postgresql/data` to `/var/lib/postgresql/18/docker`, and the
    adapter holds the old path as a constant. Logical dumps restore fine;
    both pgBackRest paths, plain and PITR, fail before they start.
  - **SQL Server 2017** — the adapter runs
    `/opt/mssql-tools18/bin/sqlcmd`, which that image does not ship; its
    client is at `/opt/mssql-tools/bin/sqlcmd`. The engine starts and
    reports itself ready, then every command fails and the drill times out
    blaming the engine.

  All three are tracked and none is listed as verified until it is fixed.
  A fourth version, **MongoDB 8.0**, restores correctly but is not listed
  either: one test builds its oplog scenario with a server failpoint 8.0
  no longer has, and a version stays off the list while anything about it
  is red — the rule is what makes the rest of the list worth reading.

- **`evidence verify` now says when an intact log proves nothing.** An empty
  evidence log verifies: no lines, no damage, no failed assertion, so the
  schema's algorithm returns VALID (§9) and the command exits `0` — exactly
  as a log full of verified drills does. That is the right answer to the
  question a verifier asks, which is whether the file was tampered with
  rather than whether any drill was ever run, but it means a monitoring
  check that branches on the exit status cannot tell *"every drill
  verified"* from *"nothing has ever run"*.

  Both verifiers — `probavi evidence verify` and the independent
  `probavi-evidence-verify` — now write one line to **stderr** in that case.
  Nothing else moves: the exit code is normative and stays `0`, and the
  machine-readable result on stdout is untouched (it already carries the
  record count, which is what a script should branch on). The message is
  translated into all 23 shipped locales.

  Surfaced by the new fuzz targets, which initially asserted that VALID
  implies something was verified. The specification disagreed, and it was
  right; the assertion was wrong. What survived was the observation that the
  two cases look identical from the outside.

  Because the independent verifier changed, its module moves for the first
  time since `spec/evidence/v0.3.0`: this release cuts
  **`spec/evidence/v0.4.0`** (the warning line plus the fuzz targets;
  format and exit codes untouched), and the documented install pin moves
  with it. The evidence schema itself is unchanged — a v0.3.0 verifier
  still accepts every log a v0.4.0 one does.

- **Community documents**: [CONTRIBUTING.md](CONTRIBUTING.md),
  [GOVERNANCE.md](GOVERNANCE.md) and
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). The rules themselves are not
  new — AGENTS.md has carried the engineering gates and the DCO decision
  from the start — but a contributor had to assemble them from a document
  written for coding agents. Now one file says how to contribute, one how
  decisions are made, one what conduct is expected; and GOVERNANCE.md
  writes the standing commitments down in one place — verification never
  paywalled, the open-core list closed, evidence append-only, no
  telemetry — so a future maintainer inherits them explicitly instead of
  by folklore.

- **Code scanning, a published supply-chain score, and a security policy.**
  CodeQL reads the Go of both modules — the core and `spec/evidence`, the
  independent verifier — and the workflow files themselves; findings land in
  the Security tab and gate nothing, so a query added upstream can never
  wedge a release. OpenSSF Scorecard grades this repository from the outside
  and publishes the result: how a tool that asks auditors to trust its
  output is itself built is fair game for scrutiny. Three things the grader
  was right about are fixed rather than argued with — [SECURITY.md](SECURITY.md)
  now names what counts as a vulnerability in a product whose output is
  evidence, Dependabot moves the pins that nothing was moving (Go modules,
  actions, base-image digests), and no workflow checkout leaves the job's
  token behind in `.git/config`, since nothing here pushes.

- **The README says where to go and check.** A badge row, identical in all
  five READMEs, links the CI run list, CodeQL, the published Scorecard, the
  coverage report, the newest release, the license, the Go version the
  module builds against, the platforms released binaries exist for, and how
  often they have been downloaded. A gate holds the row to this repository:
  a badge naming another project's slug, or reporting on a workflow file
  that no longer exists, fails the docs tests rather than quietly rendering
  somebody else's green — neither failure shows up as a broken image.
  Coverage is now also reported to Codecov, and gates nothing there: the
  gate remains the ratchet against the committed `.coverage-floor`.

### Changed

- **A plain-SQL restore now has to prove the dump was whole** (postgres
  0.9.0). `psql` reports that no statement failed; it does not report that
  it reached the end of a complete dump. Measured: fed a dump cut on a line
  boundary it restores 477 of 1000 rows, treats the stream's end as the end
  of the data, and exits 0 — a silent partial restore, which the adapter
  protocol (§5) forbids reporting as success. The file that produces one is
  ordinary: a backup job running `pg_dump | gzip` whose `pg_dump` dies of a
  full disk leaves a *perfectly valid* gzip member holding an unfinished
  dump, and every byte in it restores.

  A plain-SQL member is now a pass only when the dump's own closing line
  arrives with it. For a compressed dump the stream is tapped while `psql`
  consumes it and only its tail kept, rather than inflating the artifact
  twice: measured on a 218 MiB dump, the tap costs 2% of the restore where
  a second inflate pass would cost 70%, and the restore duration is an RTO
  figure somebody reads. A dump without that line fails as `source_corrupt`
  saying the backup stops early, which is a claim about the job that wrote
  it rather than about the restore.

  This applies to the `pgdump_with_globals` globals script too, where the
  hole was wider: that load runs with `ON_ERROR_STOP` off by design, so a
  truncated globals script created the roles it got to, said nothing, and
  passed. A globals script that is not a complete `pg_dumpall` output now
  fails the drill. Compressed plain-SQL restores need `mkfifo` and `tee` in
  the engine image alongside `gzip`; the official `postgres` images have
  all three, and an image without them fails naming the image.

- **A plain-SQL dump the server cannot parse is `source_corrupt`, not
  `restore_failed`** (postgres 0.9.0). `pg_restore` says as much about an
  archive it does not recognise; for a script, `psql`'s own syntax error is
  the same verdict, and it belongs to the artifact rather than to the
  restore. Other server errors — a missing role, out of memory — remain
  `restore_failed`, and a restore that dies on ownership now names the
  `pgdump_with_globals` kind, since a plain dump carries its `OWNER TO`
  statements inline where `pg_restore --no-owner` could have dropped them.

- **A mysql restore now has to prove the dump was whole** (mysql adapter
  0.9.0). The postgres work above turned up a defect the same survey found
  in exactly one other adapter. The mysql client reports that no statement
  it ran failed; it does not report that it reached the end of a complete
  dump, and a dump that stops on a statement boundary is valid SQL as far
  as it goes. Measured against a real server: a three-table dump cut where
  mysqldump would have died after the first restores that one table, the
  client exits 0, the decompressor exits 0, and the drill passed — having
  proved a third of the backup. For the `mysqldump_with_users` accounts
  script the hole was wider, because that replay runs with `--force` and so
  cannot abort at all.

  A member is now a pass only when mysqldump's `-- Dump completed` sign-off
  arrives with it; for a compressed member the stream is tapped while the
  client consumes it, rather than inflating the artifact twice. A member
  without that line fails as `source_corrupt` saying the backup stops
  early, which is a claim about the job that wrote it rather than about the
  restore.

  **Comment-free dumps stay exempt.** Measured across the flags a backup
  job plausibly uses, the mysqldump banner and the sign-off travel
  together: the default and `--skip-dump-date` write both, `--compact` and
  `--skip-comments` write neither. So only a dump that announces itself is
  held to its ending, and the residual is documented rather than hidden — a
  truncated `--compact` dump cannot be detected, because nothing in that
  format says where it should have stopped. Restoring a compressed member
  now also needs `mkfifo` and `tee` in the engine image alongside `gzip`;
  the official `mysql:8.x` images have all three.

  The other two adapters were measured and need nothing: `mongorestore`
  exits non-zero on a truncated archive, and SQL Server refuses a truncated
  `.bak` outright (`Msg 3287`), because both formats describe their own
  extent where a SQL script does not.

## [0.6.0] - 2026-08-10

### Added

- **`bak_chain`, a SQL Server source kind that restores the whole backup
  set rather than just its newest full** (mssql adapter 0.6.0; closes
  #98). A backup set is a chain — full, then differentials, then
  transaction log backups — and `bak_dir` restores the full and stops.
  Measured on a directory holding one full, two differentials and four
  logs: the full alone recovers 1 row, the chain recovers all 7.

  The chain is built from the log sequence numbers in each backup header,
  not from file names: every differential and log carries the checkpoint
  of the full it builds on, which separates two fulls' chains inside one
  directory, and the logs cover contiguous ranges the restore follows by
  carrying a redo point forward. Members restore `WITH NORECOVERY` and
  the last one `WITH RECOVERY`, so the database becomes usable exactly
  once. `backup.checksum` covers every member in restore order and
  `created_at` is the last member's completion time.

  A gap in the log sequence fails the drill, naming the point reached and
  the log that starts too late; a directory holding several databases
  needs `source.params.database_name`. Point-in-time recovery stays out
  and the probe still declares `pitr: false` — measured, `RESTORE LOG …
  WITH STOPAT` beyond the available logs exits successfully while leaving
  the database in the restoring state, which would look like a passing
  drill on a database nobody can open.

- **The mysql adapter restores gzip-compressed dumps** (mysql 0.8.0;
  closes #106). `mysqldump … | gzip -c > db.sql.gz` is what dump
  pipelines produce once dumps get large, and the adapter had no notion
  of compression: the client read the gzip header as SQL and the drill
  died as `source_corrupt`, pointing an operator at a backup that was
  perfectly intact.

  The compression is recognised from the artifact's magic bytes, never
  from its name, and the artifact is restored **as stored** — the
  decompression happens inside the sandbox, streamed into the client,
  so `backup.checksum` and `size_bytes` cover the bytes the backup
  archive actually retains. Decompressing outside Probavi instead would
  have left the record identifying a temporary file nobody keeps. The
  sandbox needs `gzip`, which every official `mysql:8.x` image has; an
  image without it fails the drill with a message naming the image
  rather than the backup.

  Both ends of that pipeline are judged. A pipeline reports its last
  command's status, so a decompressor that dies partway is invisible to
  a client that accepted the truncated prefix — the exact shape of the
  partial restore the protocol forbids (§5). The decompressor's status
  is captured separately and a truncated archive fails as
  `source_corrupt`.

  Directory sources rank compressed candidates the same way as plain
  ones, by the dump's own `-- Dump completed on` trailer, which for a
  gzip member means decompressing it: a gzip header records no usable
  date (measured — `gzip` zeroes that field when it compresses a pipe,
  which is the `mysqldump | gzip -c` shape), and there is no index to
  seek by. That costs about a second per 60 MiB of compressed data per
  candidate. Ranking those backups by file modification time instead was
  the alternative, and it is the claim the ranking change below removes.

- **The two physical backup kinds now date themselves** (postgres 0.7.0,
  mysql 0.6.0; closes #99). `pgbackrest` and `xtrabackup` were the only
  kinds left reporting no `backup.created_at` after the timestamp work in
  0.5.0, and both formats keep metadata that answers the question.

  **pgBackRest is the exception in this project, and a welcome one:**
  `backup.info` records `backup-timestamp-start`/`-stop` as **epoch
  seconds** — measured, a repository written on a host in Asia/Tokyo
  stores `1786289869`, which is 15:37:49 UTC, and pgbackrest's own info
  output renders the same instant as 00:37:49+09. An epoch value carries
  no zone question, so that kind reports an exact creation time **without
  any `source.params.backup_timezone` declaration**. The newest backup in
  the repository dates it, because that is the one a restore without a
  target uses; an encrypted manifest cannot be read and leaves the field
  null rather than guessed.

  XtraBackup writes a bare wall clock (`end_time = 2026-08-10 00:50:25`
  for a backup taken at 15:50:25 UTC — measured), so that kind joins the
  existing mechanism: with a declared zone it reports the completion
  instant, without one it reports nothing.

### Changed

- **A directory source now restores the newest backup, not the newest
  file** (postgres 0.8.0, mysql 0.7.0, mssql 0.7.0; closes #100).
  Candidates were ranked by modification time, so a backup copied into
  the directory afterwards — `cp` without `-p`, an object-store
  download, an `rsync` without `-t` — looked like the newest thing there
  and was the one the drill proved. Measured end to end on real engines:
  with a stale dump carrying the newest file time, the drill restored 3
  rows where the actual newest backup holds 11.

  Ranking now uses the time each backup records about itself: the
  pg_dump archive header, mysqldump's `-- Dump completed on` trailer,
  and `RESTORE HEADERONLY`'s `BackupFinishDate`. That value does not
  move when a file is copied. It needs no declared zone — two backups
  being compared came off the same host, so its offset cancels out;
  `source.params.backup_timezone` is still what turns one into the
  instant `backup.created_at` reports. A backup the adapter cannot date
  ranks below every backup it can, and between two undatable files the
  previous rule still decides: newest file, ties broken by name.

  For SQL Server this means every candidate is probed before one is
  chosen, where the scan could previously stop at the first full backup
  it found; only the transfer that feeds the restore is still counted as
  recovery time. `mongodump_dir` keeps ranking by file time and says so
  in its README and in `docs/capabilities.json`: a mongodump archive
  records no timestamp of its own, so there is nothing else to rank by.

## [0.5.0] - 2026-08-09

### Changed

- **`backup.created_at` now comes from the backup, not from the file's
  modification time** (postgres 0.6.0, mysql 0.5.0, mongodb 0.4.0, mssql
  0.5.0; tracked in #91). An mtime dates a copy: `cp` without `-p` resets
  it (measured), and a month-old artifact then looks like last night's
  while restoring perfectly — a signed record would carry a fresh-looking
  timestamp for a stale backup. No adapter reports an mtime as a creation
  time any more.

  Each format is read where it has something to say: a `pg_dump`
  custom-format archive stores its creation time in the header (archive
  1.14 and 1.15/1.16 place it at different offsets — measured across
  servers 13, 14, 16 and 17, and a header this parser does not recognise
  yields no timestamp rather than a wrong one), `mysqldump` signs off with
  `-- Dump completed on ...`, and SQL Server's `BackupFinishDate` comes
  free with the header probe added in #88. A mongodump archive carries no
  timestamp at all (measured), so that adapter reports none.

  **What no format records is a UTC offset.** All three store the backup
  host's wall clock: a backup taken at 12:08 UTC on a host in Asia/Tokyo
  is written as 21:08, and reading it as UTC would put an instant in the
  record that is wrong by nine hours. The offset is a fact only the
  operator has, so a drill declares it with the new
  `source.params.backup_timezone` — an **IANA zone name**, because the
  offset depends on the date of the backup: a January backup in
  Europe/Budapest is +01:00 and a July one +02:00, and a fixed number in a
  config file would be wrong for half of every year. Zone data is compiled
  into the adapters, so no `/usr/share/zoneinfo` is needed on the host; an
  unknown name fails the drill instead of quietly dropping the timestamp;
  and the mongodb adapter refuses the key outright rather than accept a
  declaration it cannot honour.

  **Without the declaration `backup.created_at` is null**, which the
  evidence schema provides for ("the backup's own creation time if
  derivable"). Two-member sources are dated by the member that carries a
  timestamp — the companion script never does — and the pgbackrest and
  xtrabackup kinds report none for now; their own metadata files record
  real timestamps, which is separate work.

### Fixed

- **A drill no longer races the backup job that is writing its source**
  (postgres 0.5.0, mysql 0.4.0, mongodb 0.3.0, mssql 0.4.0; tracked in
  #91). When a drill names a directory, the adapter picks the artifact
  itself — and the newest file there is quite often the one a backup job
  is writing right now. Measured on all four engines: the partial
  artifact is picked, and restoring it fails in a different way per
  engine (postgres "end of file"; mysql loads 24 036 rows then reports
  `ERROR 1064`, which its classifier calls *corrupt*; mongodb "0
  document(s) restored"; mssql fails at `RESTORE` — while
  `RESTORE HEADERONLY` accepts the truncated backup without complaint,
  so the #88 header probe cannot stand in for this).

  The chosen artifact is now observed twice, a moment apart, and one that
  changed in between fails the drill as `source_unreadable` with a
  message naming the fix. It is deliberately **not** skipped in favour of
  the previous backup: that would prove an older backup while the record
  implied the newest, and the evidence could not say which one it was. An
  artifact untouched for longer than the window is taken as finished
  without any wait, so ordinary drills pay nothing. Artifacts the config
  names outright are never second-guessed — the operator chose them.

  All four adapter READMEs now also state what `created_at` means for a
  directory source (the file's modification time, which `cp` without `-p`
  resets — measured) and recommend the arrangement that removes the race
  outright: write to a temporary name, rename on completion, so the
  directory only ever shows finished files.

### Fixed

- **SQL Server drills no longer fail on a healthy backup set** (mssql
  adapter 0.3.0; tracked in #88). A real backup directory holds full,
  differential, and transaction log backups side by side, and the newest
  file is typically a log backup — which cannot create a database. The
  old "newest file wins" rule therefore reported `restore_failed` on a
  perfectly restorable set: a false alarm, the direction that costs an
  operator's trust rather than merely withholding it.

  Nothing outside the engine can tell the types apart — measured: the
  `.bak`/`.trn` extensions are convention SQL Server ignores, and all
  three types share one media format, so `RESTORE FILELISTONLY` (which
  the adapter already ran) returns the same answer for each. Candidates
  are now transferred newest-first and identified with
  `RESTORE HEADERONLY`; the first holding a full backup is restored.
  Files that are not backup media (checksum sidecars, log files) are
  skipped before transfer and named in the failure message; a backup file
  the engine cannot read fails the drill rather than falling back to an
  older one. A directory with no full backup now says exactly that,
  instead of quoting Msg 3118 at the operator.

  Two related fixes came with it: backup media holding several appended
  sets is restored from its **newest full set** (`WITH FILE = n`) instead
  of the engine's default first set — the oldest backup on the file — and
  the same type-aware choice covers `bak_with_logins` when its
  `params.bak` is omitted. Selection is not counted in the measured
  recovery time: `transfer_seconds` covers the chosen artifact only.

### Added

- **`mongodump_with_users` and `mongodump_with_oplog`, two MongoDB source
  kinds that close the account-layer and consistency gaps** (mongodb
  adapter 0.2.0; tracked in #90). Users and roles live in the `admin`
  database, so a per-database archive carries them only with
  `--dumpDbUsersAndRoles` — and `mongorestore` puts them back only when
  asked. Measured: an archive that *did* carry the account layer restored
  without it silently, exit 0, zero users. Separately, an archive taken
  with `--oplog` restored with its captured window ignored, so a write
  issued during the dump into an already-copied collection was absent from
  the restored data and nothing said so.

  `mongodump_with_users` restores the accounts (`options.database` is
  required — `mongorestore` refuses the flag without a database, and
  defaulting it would restore them into `admin`) and then gates on two
  facts: the account layer arrived, and every role a restored user holds
  or a restored role inherits still resolves via `rolesInfo` — which is
  how a user pointing at a role in another database, one a single-database
  archive cannot carry, is caught. `mongodump_with_oplog` replays the
  captured window and gates on the replay having happened, so its record
  means *restored to a consistent point* rather than merely *restored*.

  Unlike the sibling adapters' two-member kinds, both are single-artifact
  sources — mongodump keeps users, roles, and the oplog inside the same
  archive — so the backup identity is unchanged. Engine diagnostics bound
  for protocol messages are scrubbed of SCRAM material (`salt`,
  `storedKey`, `serverKey`), which `admin.system.users` documents embed.

- **`mysqldump_with_users`, a MySQL source kind that replays the account
  layer before the dump and gates on the restored principal chain** (mysql
  adapter 0.3.0; tracked in #89). MySQL accounts and grants live in the
  `mysql` system schema, never in a single-database dump, so a plain
  `mysqldump` drill passes while the application account cannot log in and
  every `SQL SECURITY DEFINER` object fails at invocation (`ERROR 1449`).
  The source is one directory with both members named in `source.params`
  (`users`, and optionally `dump`); identity is the size-framed two-member
  composite with the *older* member's mtime, as in the sibling kinds.

  Decisions measured against a real MySQL 8.4 instance: the script is fed
  through stdin with `--force` (without it the client aborts mid-script,
  and the `source` command aborts even with it); exactly `ERROR 1396` for
  `root` and the reserved `mysql.*` accounts is tolerated. Three gates run
  after the load — orphaned definers, database reachability, and view
  resolution via `EXPLAIN`. The reachability gate exists because grants
  are database-scoped: restored under a name the script does not grant on,
  the account layer silently covers nothing (measured), so the drill must
  restore under the source database name and the gate's message says so.
  Diagnostics bound for protocol messages are scrubbed of `IDENTIFIED`
  literals, `$A$...` hash literals, and long hex literals.

- **`charset` and `collation` drill options for MySQL logical restores**
  (part of #89). The restore target used to be created with the sandbox
  server's defaults, silently discarding the source database's collation —
  which governs comparisons, ordering, and uniqueness. The options pin the
  source values when the adapter creates the target; the README documents
  the default's limits.

- **`bak_with_logins`, a SQL Server source kind that replays server
  logins before the restore and refuses to pass while any restored user
  is orphaned** (mssql adapter 0.2.0; tracked in #87). SQL Server splits
  principals across two scopes — logins in `master`, database users
  inside the `.bak`, linked by SID — so a plain `bak` drill restores the
  users orphaned: `RESTORE` succeeds, checks pass, the record says
  `pass`, and the application still cannot log in. This is the quiet
  sibling of the PostgreSQL globals gap fixed in 0.4.0, without the loud
  failure that made that one visible.

  The source is one directory with both members named in `source.params`
  (`logins`, and optionally `bak`; plain filenames, never paths). The
  backup identity covers both members — a composite, size-framed SHA-256
  in load order — and `created_at` is the *older* member's mtime: the set
  is only as current as its stalest member. The logins script is replayed
  with `sqlcmd -i` deliberately without `-b` (batch-abort would silently
  skip every login after a mid-script collision); exactly one failure
  class is tolerated, `Msg 15025` for principals the sandbox engine
  itself created (`sa` and the `##...##` internal principals). After the
  restore an orphan gate queries the restored database and fails the
  provision if any SQL user is left without a matching server login — an
  incomplete logins script cannot pass. Engine diagnostics bound for
  protocol messages are scrubbed of `PASSWORD` literals and long hex
  literals: a measured syntax error echoed a full password hash back, and
  such text must never reach a signed evidence record.

## [0.4.0] - 2026-08-07

### Added

- **`pgdump_with_globals`, a PostgreSQL source kind that restores the
  cluster globals before the dump** (postgres adapter 0.4.0; reported in
  #84). A logical recovery has two steps — `pg_dumpall --globals-only`,
  then the database dumps — and no `pg_dump` carries the first. A drill
  restoring only the dump proved the second half of a two-step path, and
  its `backup.checksum` covered only the dump, so the record claimed less
  than the recovery it stood for. `pg_restore --no-owner` drops `OWNER TO`
  but never `GRANT`, so such a restore dies on the first grant naming a
  role nothing created — the drill was right, and the gap was what came
  after it.

  The source is one directory with both members named in `source.params`
  (`globals`, and optionally `dump`; plain filenames, never paths).
  Explicit names rather than a filename pattern: renaming a backup file
  must not silently change what a drill proves. One directory rather than
  two paths because the core only hands an adapter files belonging to the
  drill's configured backup source (protocol §4.2), a guard that exists so
  a third-party adapter binary cannot pull arbitrary host files into a
  sandbox it controls. Leaving `dump` out restores the newest file that is
  not the globals, so a rotating backup directory keeps working
  unattended; naming it lets one directory serve several databases, each
  drilled separately with its own checks and its own record.

  `backup.checksum` covers **both** members — a checksum blind to the
  globals would let the roles a restore depends on change without the
  evidence noticing — using the tree hash's framing over exactly the two
  chosen members, so a sibling database's backup never moves this drill's
  identity. `backup.created_at` is the **older** member's mtime: a set is
  only as current as its stalest part, and stale globals are the gap being
  closed. The globals load counts into the measured restore duration,
  because the recovery it stands for includes it.

  **No evidence schema change** (v2 stays frozen; `backup.kind` carries
  the new value) and **no core change** — the second extension axis proven
  again.

  Two behaviours were measured against a real cluster rather than assumed,
  and both shaped the implementation: `pg_dumpall` emits `CREATE ROLE` for
  the bootstrap superuser as well, so **every** globals script collides
  with the role `initdb` already created. That one error — and only that
  one, for the connected superuser — is tolerated; `ON_ERROR_STOP` stays
  off, because the collision sits mid-script and stopping there would
  silently skip every role sorting after it, making a drill's completeness
  depend on role naming. And a globals script carries role password
  verifiers, which PostgreSQL quotes back in syntax errors: `--echo-errors`
  is refused, and SQL password literals are scrubbed from every engine
  diagnostic before it can reach a protocol message and, through it, a
  signed record (evidence schema §8).

## [0.3.1] - 2026-08-05

### Fixed

- **The container image stamped its binary with the git tag**, so
  `probavi version` reported `probavi v0.3.0` inside the image while the
  archives and packages of the same release reported `probavi 0.3.0`.
  That stamp is what a drill signs into every record as
  `env.probavi_version`, so one release produced two spellings of itself
  in an audit trail — and `docs/docker.md` §3 documented the output
  without the `v`, which made the document wrong rather than the image.

  The same mistake as the image *tag* fixed a moment earlier, in the
  second place it appears: the tag was corrected, the build argument next
  to it was not. The gate now covers both.

  **v0.3.0 was not re-cut**, deliberately. The Go module proxy had
  already recorded `github.com/probavi/probavi@v0.3.0` and
  `…/spec/evidence@v0.3.0` against the tagged commit, and the checksum
  database is immutable by design: re-pointing either tag would make
  `go install …@v0.3.0` fail permanently for everyone, exactly the way
  `v0.1.0` fails under the new module path today. A published image tag
  and a digest already printed in published release notes carry the same
  rule. The stamp is inconsistent, not invalid — the schema asks only for
  a non-empty string — so 0.3.1 supersedes it rather than rewriting it.

  The version gate learned something from this too: it required the
  documented `probavi-evidence-verify@vX.Y.Z` to equal the release, but
  `spec/evidence` is a separate module tagged independently, and this
  release does not touch it. Its pin is now held to the newest
  `spec/evidence` tag the changelog names, which is what is actually
  true, and a release no longer drags a meaningless module tag along.

## [0.3.0] - 2026-08-05

### Added

- **Ready-made Homebrew formulae attached to every release**, one per
  binary, with both architectures selected automatically. **There is no
  hosted Probavi tap** — decided 2026-08-05, for the same reason there is
  no apt repository: one more thing to host and keep in step, for little
  gain over a checksummed download. The formulae name no tap, so
  `brew tap-new` plus a `curl` per formula gives a working `brew install`
  in a tap of your own (`docs/packaging.md` §5.2).

  **No signed `.pkg`, and none is needed.** Homebrew downloads without
  setting the quarantine attribute, so Gatekeeper does not block the
  binaries — while a `.pkg` distributed any other way would need a
  Developer ID certificate, notarisation on every release, and Apple
  credentials in CI: a paid account and a new long-lived secret, for a
  CLI tool whose users already have `brew`. `docs/packaging.md` §5 states
  the other side of that trade too — downloading a release tarball
  directly *does* get quarantined, and how to clear it.

  Note the split is the opposite of the container image's, deliberately:
  installing a second formula is one command, while composing a derived
  Docker image is a Dockerfile plus a build plus a registry. Adapter
  formulae depend on the core, unversioned, and on nothing else; the core
  formula depends on nothing at all, so verifying an evidence log stays a
  one-formula install.

  Formulae are rendered from the release's own `SHA256SUMS`, never from
  recomputed checksums, so one can only pin what the release actually
  published — a missing entry stops the render rather than yielding a
  formula that fails on a user's Mac. CI renders them on every pull
  request, syntax-checks the Ruby, and proves the missing-checksum guard
  fires. It earned that on the first run: the initial templates produced
  Ruby that did not parse.

  `docs/packaging.md` §5.1 states the cost of that decision rather than
  hiding it: a release tarball downloaded **directly** is quarantined by
  macOS, and Gatekeeper refuses it until `xattr -d com.apple.quarantine`
  clears the flag. Homebrew does not set the attribute, so §5.2 has no
  such step. A signed, notarised `.pkg` would remove it everywhere, in
  exchange for a paid Apple account and a signing certificate to guard —
  a second long-lived secret, which is what the whole packaging set
  avoids.

  A gate holds this honest: nothing in the README, the packaging document
  or the formulae may name `probavi/tap/…` outside a comment. It is a
  plausible line to write and an install command nobody can run, which is
  the worst kind of documentation error — it looks tested.

- **Distribution packages for every release**: `.deb`, `.rpm` and `.apk`
  for amd64 and arm64, plus a `PKGBUILD` and a Gentoo ebuild that build
  from source. `docs/packaging.md` is normative for what they contain.

  One package per binary, matching the release archives: `probavi` plus
  `probavi-adapter-<engine>`. **`probavi` declares no hard dependency at
  all** — `probavi evidence verify` reads a log and a public key and needs
  no runtime, so an auditor who installs it to check a log must not have a
  container engine pulled onto their machine. A sandbox runtime belongs to
  whichever *provider* the drill config names; `apt` installs `Recommends`
  by default, so a container engine sits in `Suggests` or it is a hard
  dependency wearing a different hat.

  Adapter packages depend on `probavi`, unversioned, and on nothing else —
  in particular **no engine client**, because the engine's own tools run
  inside the sandbox image, not on the drill host.

  **No Probavi apt or yum repository, and no packaging GPG key.** Hosting
  one means a second long-lived signing key to guard, in a project whose
  trust proposition is how it handles the first — the key that signs
  evidence. Instead every artifact carries a **sigstore build-provenance
  attestation**, which proves the file came from this repository's release
  workflow and needs no key from anyone (`gh attestation verify`). Signing
  the `.deb` files would be close to theatre anyway: `dpkg` does not check
  package signatures by default.

  **A new CI job builds the packages and installs them** on Debian,
  Fedora and Alpine, then asks each install to resolve an adapter for
  real. It exists because building a package proves nothing about whether
  it works — and it caught precisely that: `nfpm` expands environment
  variables in scalar fields but **not** in a content path, so the first
  version produced packages that installed cleanly with the adapter at a
  literal `/usr/bin/probavi-adapter-${ADAPTER}`, where the core’s `PATH`
  lookup would never find it. The recipes are now rendered with
  `envsubst` before nfpm sees them, which removes the distinction.

  Four gates in `internal/docs` hold the recipes to the decisions behind
  them: binaries on `PATH` and never in `/usr/libexec` (the FHS instinct
  that would break every drill), no engine client and no version pin in
  the adapter dependency, no hard dependency on the core package, and the
  hand-written Arch and Gentoo recipes covering every adapter the
  generated manifest declares. All were checked to fail when violated.

- **A container image**, `ghcr.io/probavi/probavi`, built for
  `linux/amd64` and `linux/arm64` on every release, plus
  `docs/docker.md` — normative for what the image is and what running it
  costs.

  One image carrying the core and every in-repo adapter, deliberately not
  the per-binary split the archives and packages use: adapters run as
  child processes of the core and must share its filesystem, so separate
  images would only compose through a `COPY --from` Dockerfile the user
  writes.

  **The document leads with the consequence rather than burying it.** The
  docker sandbox provider creates *sibling* containers, so a containerised
  Probavi needs a daemon it can reach — and bind-mounting the host socket
  to give it one grants root-equivalent access to that host. The
  alternative, `DOCKER_HOST=ssh://…`, is stated beside it, and the
  container is explicitly **not** presented as the recommended
  deployment: the plain binary needs neither a socket nor a credential.

  `kubectl` is absent on purpose. The Kubernetes provider needs a
  kubeconfig and RBAC to create Jobs — an in-cluster deployment with a
  service account, not a container holding a socket. Bundling it would
  suggest the two are one shape.

  Verified by running a real drill from inside the image against the host
  daemon: sandbox created, `pg_dump` backup restored, both checks passed,
  a `probavi-evidence/2` record appended with both build digests, sandbox
  destroyed, zero orphans — and the log then verified both by the image
  and by the independent verifier. That run is also what confirmed the
  file-visibility rule now in §4.1: the backup must be readable by the
  *Probavi container*, not by the daemon's host, because `put_file` is a
  `docker cp` from the client's own filesystem.

  Three gates in `internal/docs` keep the image honest: the runtime
  packages each shipped feature needs, the unprivileged user, and both
  base images pinned by digest. All three were checked to fail when
  violated.

  One claim was corrected before it shipped. The first draft said HTTPS
  notifications fail without the `ca-certificates` package; measuring it
  showed the Alpine base already carries `ca-certificates-bundle`, so
  public HTTPS works without it. The package stays for the two reasons
  that survive measurement — the dependency is stated rather than
  inherited from a base image that may change, and `update-ca-certificates`
  is how an operator trusts a private CA (§9) — and the Dockerfile, the
  document and the gate all say that instead.

- **The core writes evidence schema v2**: every record now carries
  `adapter.digest` — the adapter executable that performed the restore —
  and `env.probavi_digest` — the `probavi` binary that signed the result.
  Schema v2 is frozen (`docs/evidence-schema.md` §11.2).

  A record already named the adapter and its version; it could not say
  which *build*. Two materially different builds could produce records
  claiming the same provenance, which is exactly the question an auditor
  asks when a restore is disputed. `adapter.version` cannot answer it:
  it is a number the adapter reports about itself.

  **An unreadable executable records null and never fails the drill.** A
  record with a null digest still proves the restore; a drill that died
  because a hash could not be taken proves nothing. Both paths are tested.

  The hashing lives on the core side of the adapter boundary. The protocol
  client exposes the executable it resolved and knows nothing about the
  record format — `sha256:` is an evidence-schema convention, and
  `internal/adapter` deliberately does not import `internal/evidence`.

  `log_v2.jsonl` joins the published conformance vectors, carrying both
  forms deliberately: a record with the digests populated, and an
  infrastructure failure whose `adapter.digest` is null because the
  adapter never resolved. It verifies offline with only the committed
  public key — by this repository's writer *and* by the independent
  verifier that shares no code with it. `log_v1.jsonl` is byte-frozen
  alongside `log_v0.jsonl`: a record written under a published version is
  never regenerated (§10), and a test now holds both.

- **The independent verifier accepts `probavi-evidence/2`**, and a new
  test pins its supported set to the versions the published JSON Schema
  declares — in both directions.

  §10 has always obliged a verifier to support every published version,
  for the lifetime of the format. Nothing enforced it. The specification
  could publish a version `spec/evidence` refuses, which is how an auditor
  ends up holding a log the independent verifier calls INVALID for no
  reason but a stale allow-list; the reverse — a version silently dropped
  — would be worse, because records already written would stop verifying.
  Both are now build failures.

  Covered by construction rather than by assertion about a map: a v2
  record signed the way §6 prescribes verifies against the committed
  public key, with the digests populated *and* null, since §3 makes them
  nullable so an unreadable executable never costs a drill its record. A
  log whose writer moved from v1 to v2 mid-file chains straight through,
  which is what a real upgrade produces. `probavi-evidence/3` is still
  refused, so the allow-list grew by one entry rather than into a
  wildcard. `spec/evidence` holds its 100% statement coverage.

- **Evidence schema v2 (`probavi-evidence/2`): `adapter.digest` and
  `env.probavi_digest`** — spec and JSON Schema; no writer emits v2 yet,
  and `docs/evidence-schema.md` §11.1 lists what still has to land before
  one may.

  A record named the adapter and its semantic version but carried no
  **build identity**, so two materially different builds could produce
  records claiming the same provenance — indistinguishable to the auditor
  those records exist for. `adapter.version` cannot close this: it is a
  number the adapter reports about itself, and the CI gate below reduces
  the drift without removing it, and says nothing at all about a
  third-party adapter.

  Both fields were taken in one version rather than the adapter alone. The
  same argument applies verbatim to the orchestrator — the core chooses
  the sandbox, runs the checks and signs the record — and §10 makes every
  field addition a major bump, so deferring the symmetric half would have
  cost a second migration for every verifier. Both are nullable: an
  unreadable executable must never cost a drill its signed record.

  The attestation limit is written into §3 rather than implied. The digest
  covers the bytes of the file the core selected, hashed before launch. It
  does not prove those bytes are the instructions that ran — a file
  swapped between hashing and `exec` would go unnoticed, and closing that
  window means reading `/proc/<pid>/exe`, which does not exist on every
  platform Probavi supports.

  **A pre-existing defect surfaced and is fixed here:** the §3 worked
  example declared `probavi-evidence/0` while carrying
  `drill.pitr_target`, a combination the published JSON Schema rejects —
  the example was never moved to v1 when that field was added, so a reader
  copying it as a starting point would have produced records their own
  verifier refuses. The example is now a v2 record, and a new gate in
  `internal/spec` validates it against the schema on every run, so the
  normative document and the schema derived from it cannot drift again.
  That gate caught a malformed digest in this very change before it
  reached review.

- **A CI gate that will not let an adapter's source change without its
  `adapterVersion` moving** (`internal/tools/adapterversion`, wired into
  `ci.yml` for pull requests).

  The constant is not bookkeeping: each adapter reports it through the
  protocol and the core signs it into every evidence record as
  `adapter.version`. Nothing forced it up, so two materially different
  adapter builds could publish records claiming the same provenance —
  indistinguishable to the auditor those records exist for.

  What counts as a change is a **deny-list, not an allow-list**, so a file
  type nobody has thought of yet fails closed. Only four things under an
  adapter directory are known not to reach its binary and are excluded:
  `*_test.go`, anything under `testdata/`, `README.md`, and `adapter.json`
  (read from disk by the capabilities generator, never compiled in). An
  adapter that grows a `//go:embed` is already covered.

  Two limits are stated in the tool rather than implied. It cannot see a
  Go toolchain change, which is pinned by `go.mod` and would alter every
  binary at once — a repository-wide event that should *not* push every
  adapter's semantic version up, or the number stops meaning anything. And
  it says nothing about which bytes produced a given record: that is build
  identity, and the evidence schema carries no digest today.

  The escape hatch is a pull-request label, `adapter-version-exempt`, not
  a commit marker — a change that genuinely cannot alter behaviour should
  be waved through where a reviewer can see the waiver. The failure
  message names the label, so nobody has to go looking for it.

- Translated README introductions in Hungarian, German, French, and
  Spanish (`README.hu.md`, `README.de.md`, `README.fr.md`,
  `README.es.md`), matching four of the shipped CLI locales — an operator
  who reads Probavi's diagnostics in their language can now read what
  Probavi is in the same language. Deliberately narrow (`docs/i18n.md`
  §7): only the stable spans of `README.md` are translated — what Probavi
  is, why it exists, the non-goals. Status, install, examples, and every
  capability inventory stay English-only, because a translated copy of
  them is a claim nobody can keep in sync; `docs/capabilities.json`
  remains the single machine-readable statement of what ships. The
  specs in `docs/` are normative and stay English-only.

  Translations are pinned to the English bytes they were made from: each
  file records the SHA-256 of every span it covers, and the new
  test-only `internal/docs` package fails the build when an English edit
  leaves a translation behind, when a pin outlives its span, when a
  translation smuggles in a version claim, or when the language row and
  the committed files disagree. Same principle as the catalog gates of
  `docs/i18n.md` §4: a translation may exist here only while a machine
  can prove it is current. Terminology follows each language's shipped
  CLI catalog, so the README and the terminal agree. All four texts have
  since been through the linguistic review recorded under *Changed*
  below; native-speaker feedback remains welcome and is folded in as it
  arrives (docs/i18n.md §5).

- The independent evidence verifier has a tagged, pinnable release:
  `spec/evidence/v0.2.0`. It lives in its own Go module, so it carries its
  own `spec/evidence/vX.Y.Z` tags and versions independently of the
  `probavi` binary — until now it had no tags at all, and
  `@latest` resolved to a pseudo-version of `main`, which meant the one
  artifact whose whole purpose is independent verification could not be
  named in an audit. Pin it when the verification itself has to be
  reproducible:

  ```sh
  go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@v0.2.0
  ```

  `@latest` keeps working and stays the recommended default for casual use.

- **`spec/evidence/v0.3.0`** — the independent verifier is retagged with
  this release because it changed materially: it accepts
  `probavi-evidence/2`, which the core now writes. Pinning
  `spec/evidence/v0.2.0` would pin a verifier that rejects every record
  produced from this release onward. The two tags move together from now
  on whenever the verifier changes; the schema version, not either tag,
  remains the compatibility contract (evidence-schema.md §10).

### Changed

- **A full linguistic review of every translated surface** — all 23 locale
  catalogs and all four README translations — landing 170 catalog
  corrections and the terminology and typography rules behind them
  (`docs/i18n.md` §8, new and normative).

  The headline finding is a product-level one, not a wording one:
  **nineteen of the twenty-three catalogs used the same word for a *drill*
  and for a *game-day*.** Both render as "exercise" in most European
  languages, so a localized operator could not tell one restore proof from
  an exercise made of many. Where the collision exists the game-day is now
  named `game-day` — a term already untranslated in every catalog — while
  the drill keeps the language's own word. Two further distinctions the
  product depends on are now stated and applied: *restore* against
  *recovery/recoverability*, and the frozen protocol *conformance checks*
  against a drill's *validation checks*.

  The rest are ordinary translation defects, each verified against the
  English source: `hard wall-clock limit` had been flattened to "time
  limit" in twenty catalogs, dropping the fact that the limit is on real
  elapsed time; Swedish said "dependencies to a failed member" where the
  English says *dependents of*; five catalogs left a bare adjective where
  the noun it modified had been dropped; Danish had a wrong-gender pronoun
  and a wrong-gender article; Greek left an English word (`mode`) inside a
  Greek sentence, and Greek and Irish both described the frozen check list
  with the word for *frozen solid*. In the READMEs: Hungarian rendered
  "row counts" as *ordinal numbers*, Spanish rendered "data freshness"
  with the word used for food and inverted the range it appears in,
  German used *Korruption* — bribery, not data corruption — and both
  German and Hungarian closed their opening typographic quote with an
  ASCII `"`.

  Three of the four open questions from the translations' own review notes
  are answered in `docs/i18n.md` §8 rather than left to the next
  translator: the drill/game-day and restore/recovery splits above, and
  French spacing — the README takes the non-breaking space French
  typography requires, the catalogs keep a plain space, because terminal
  output is grepped and copied.

- **Specs:** `docs/adapter-protocol.md` §6.2 and `docs/evidence-schema.md`
  §3 now state the `created_at` contract that neither of them carried: the
  adapter may report any RFC 3339 instant, and the core converts it to the
  evidence record's UTC millisecond form by truncation — never rounding, so
  a backup is never recorded as newer than it is. An unparseable value is
  an `adapter_crash` verdict, never a lost record.

  The gap was real and reachable: the evidence schema requires exactly
  millisecond precision, the protocol required nothing, the protocol's own
  example showed a second-precision instant, and `probavi adapter
  conformance` accepts any RFC 3339 — so a third-party adapter could pass
  "15/15 conformant" and then end every drill with "evidence record could
  not be written" after a successful restore. The four in-repo adapters
  already emit the millisecond form, which is why CI never saw it.

  No wire change and no version bump for either contract: nothing required
  of adapters is tightened, and no adapter that conforms today stops
  conforming. The core-side normalization that makes this true is a
  separate change.

### Fixed

- **The documented checksum command verified nothing, and said it was
  fine.** `nfpm` spells a pre-release `0.3.0~rc.1` — correct version
  ordering, since `~` sorts before the final release — but GitHub will
  not keep a `~` in a release asset's filename. The file uploaded as
  `probavi_0.3.0.rc.1_amd64.deb` was therefore checksummed under a name
  nobody could download, so `sha256sum -c SHA256SUMS --ignore-missing`,
  the command the release notes print, skipped **every package** and
  exited 0. A green tick for something it never looked at, in a product
  whose whole proposition is verifiable artifacts. Package filenames are
  now normalised where they are produced, and any name a release asset
  could not keep is a hard error. The version inside the package is
  untouched, so the ordering still holds.

- **The container image was published under a tag no document names.**
  `github.ref_name` is the git tag, so the workflow pushed
  `probavi:v0.3.0` while `docs/docker.md` told readers to
  `docker pull ghcr.io/probavi/probavi:0.3.0` — a 404 for every one of
  them, and invisible to CI, because the push succeeds and the
  documentation is prose. The image now carries the version without the
  leading `v`, matching the archives and the documentation.

  Both were found by cutting `v0.3.0-rc.1` and inspecting what it
  produced, which is what a release candidate is for.

- **The rendered `PKGBUILD` and ebuild were unusable**, and would have
  shipped attached to the first release that produced them. `envsubst`
  with no argument substitutes *every* `${...}` it finds and replaces the
  unset ones with nothing, so a PKGBUILD came out with `${pkgdir}`,
  `${srcdir}` and `${pkgbase}` erased — `cd "/probavi-"`, a download URL
  with no version in it, `-X main.version=`. It built cleanly into a
  release asset; the first person to notice would have been an Arch user
  running `makepkg`. Every render now names the variables it fills.

- **A pre-release tag produced invalid recipes.** `makepkg` rejects a
  hyphen in `pkgver` and portage's grammar wants `_rc1`, so `0.3.0-rc.1`
  is renamed to `0.3.0rc1` and `0.3.0_rc1` for those two — while the
  download URL keeps the tag that was actually pushed, which the naive
  rename would have broken. `nfpm` needed nothing: it already emits
  `~rc.1`, the form that sorts *before* the final release. Found by
  rendering the recipes for a release-candidate tag before cutting one.

  Three gates: a recipe must survive rendering with its own shell
  variables intact, every `envsubst` call must carry a SHELL-FORMAT
  argument, and a source recipe must build its download URL from the tag
  rather than from the version.

- **A downloaded release could not run a single drill.** The release
  workflow built `./cmd/probavi` and nothing else, but the core resolves
  `probavi-adapter-<name>` on `PATH` (AGENTS.md §2.1) — no config
  override, no bundled fallback, no search directory. So every published
  archive contained an orchestrator with nothing to orchestrate, and the
  only working install path was the Quickstart's `git clone` + `go build`.
  README had no install section at all.

  A release now publishes **one archive per binary**: `probavi` plus a
  separate `probavi-adapter-<engine>` for each adapter, on the same four
  platforms, through the same reproducible chain (`CGO_ENABLED=0`,
  `-trimpath`, empty build id, deterministic tar/gzip), all covered by one
  `SHA256SUMS`. Twenty archives per release instead of four, deliberately:
  an adapter is a separately installable thing, distro packaging splits
  the same way, and if adapters ever move to their own repositories
  (AGENTS.md §6) these artifact names do not change, so no install breaks.

  The adapters deliberately get no `-X main.version`. Each already carries
  its own version — the `adapterVersion` const it reports through the
  protocol, which lands in every signed evidence record as
  `adapter.version` and moves independently of the release tag. A second,
  disagreeing number on the same binary would leave an auditor deciding
  which one is authoritative. The generated release notes instead list
  each adapter's version, read straight from `docs/capabilities.json`, and
  state that the compatibility contract is neither number but the adapter
  protocol version negotiated at handshake.

  `README.md` gains an **Install** section: verify against `SHA256SUMS`,
  take the core plus an adapter per engine, put both on the same `PATH`.
  It also records what an auditor needs, which is less — `probavi evidence
  verify` reads a log and a public key, so the core alone suffices.

  The workflow enumerates adapters with a glob, so a new adapter ships
  without anyone editing CI. Two new gates in `internal/docs` keep that
  honest: the set of `adapters/*` directories must equal the set of
  adapters `docs/capabilities.json` declares — a directory the manifest
  does not know about would otherwise be built and published as a Probavi
  adapter — and the workflow must still derive its list from that glob, so
  the first gate keeps proving something about the release. Both were
  verified to fail when violated, not merely to pass today.

- The German usage block had one line running past the width the rest of
  the block wraps at, which shows in a terminal as a ragged paragraph. It
  is rewrapped; the block now measures exactly what the English does.

  Found by a systematic review pass over the translated surfaces —
  terminology against each catalog's own vocabulary, register, line widths,
  format verbs. It is not the native-speaker review `docs/i18n.md` §5 asks
  for, and that one is still owed; what it found is recorded as follow-up
  work rather than acted on where the call belongs to a native speaker.
- A `target.pitr.target_time` in the future is refused at config load. A
  drill can only prove recovery to an instant that has happened; an engine
  handed a future target simply recovers as far as it can, so the drill
  quietly proved something other than what the config asked for. The usual
  cause is a typed year or month, and catching it before a sandbox exists
  costs nothing.

  Targets within a minute of now are accepted, so ordinary clock skew
  between the host that wrote the config and the one running the drill
  cannot fail a drill.

  The diagnostic is translated into all 23 locale catalogs, as the gates of
  `docs/i18n.md` §4 require. **The 22 non-Hungarian translations ship
  without a native-speaker review pass** (§5); that review is owed, and
  corrections will follow.

- A recycled pid no longer makes a dead sandbox owner look alive. The
  orphan sweep decided ownership from the pid alone, so when the process
  that created a sandbox died abnormally and an unrelated process later
  inherited its pid, the sweep read "alive" and left the sandbox — holding
  restored production data — until that pid happened to be free during some
  later sweep.

  The pid label and the remotehost marker now carry an owner id: the pid,
  plus a token that a process inheriting the pid cannot match. The token is
  the process start time, which two processes sharing a pid cannot share,
  because the second started after the first exited.

  Linux only, and safe elsewhere by construction: where the operating
  system does not offer a start time cheaply, the id is the bare pid and
  the sweep falls back to the previous rule — the same answer as before,
  never the destructive one. An id written before this change is a bare pid
  too, so existing sandboxes keep being judged exactly as they were.

  The separator is a dash, not a colon: these ids become Kubernetes label
  values, where a colon is invalid — a test pins the id against that shape
  so the constraint cannot be broken from a unit test's blind side.
- The evidence log's directory entry is now flushed when the store opens.
  Every append fsyncs the file, which promises its bytes reached the disk —
  but the name pointing at those bytes lives in the parent directory, and
  for a newly created log that entry could still be in cache. A crash there
  lost the whole file, fsynced record and all: for an append-only log, the
  proof that a drill ran at all.

  One directory sync per drill, not per record. A filesystem that does not
  support syncing a directory (EINVAL) is not a reason to refuse to run a
  drill, so that one error is passed over; any other is reported rather
  than a durability guarantee being claimed that the store cannot make.

- Four small correctness items found during the audit:

  - `Timings.validate` ranged over a map, so a record with several negative
    phases named a different field on each run. Diagnostics a trust product
    prints have to be reproducible for whoever is comparing two logs.
  - The adapter environment could carry a variable twice when
    `source.credential_env` named a baseline one (`PATH`, `HOME`, `LANG`,
    `TZ`), leaving which value the adapter saw to exec's last-wins rule
    rather than to the §2.5 allow-list.
  - `checks.Result` carried a `Duration` nothing ever read, measured with a
    clock that ignored the injected one. The evidence schema records
    per-phase timings, not per-check ones, so the field is gone rather than
    published.
  - `evidence keygen` did not fsync the key files it wrote. A signing key
    that never reached a platter leaves a log nobody can verify and a
    signer nobody can rotate away from, because the records referencing it
    already exist.
- Three README claims had gone stale, all in the understating direction:
  the sandbox list omitted the `remotehost` provider that shipped
  2026-08-01, the design principles still called bare hosts future work,
  and the localization paragraph described Hungarian as "the first
  supported national language (further EU languages arrive per the
  ROADMAP)" while all 24 official EU languages were already shipping.

  `docs/capabilities.json` is generated from the code and CI-checked for
  drift, and AGENTS.md §5.8 makes it the only permitted source of
  capability claims for downstream surfaces — but nothing tied the
  README itself to it. `internal/docs` now does: every shipped adapter and
  sandbox provider must appear in the README, and a line that both names
  one and describes it as future work ("later", "planned", "arrives", …)
  fails the build. The rule reads only lines naming a shipped capability,
  so ordinary roadmap prose is untouched.

- A cancelled drill is now recorded as `cancelled`. `classify` mapped only
  `context.DeadlineExceeded` to a verdict, so Ctrl-C or SIGTERM produced
  whatever the dying adapter last managed to say — usually
  `adapter_crash`. The signed record therefore blamed a third party's
  adapter for the operator's own interrupt, in a document written to be
  read by an auditor, and `ROADMAP.md`'s "SIGTERM → cancelled record" was
  not true of any adapter that did not catch the signal in time.

  The drill's context now outranks the adapter's parting words in both
  directions: a deadline is a `timeout`, a cancellation is `cancelled`, and
  the adapter's message is kept inside the record either way. Teardown is
  told the matching reason, as the protocol §6.4 vocabulary expects.
- Both evidence verifiers read a log line into memory before applying the
  §4 size ceiling, so a file containing no newline at all was bounded only
  by the machine's memory. That is a poor property for the one part of
  Probavi designed to be pointed at a file someone else produced: `probavi
  evidence verify` and the standalone `probavi-evidence-verify` exist so an
  auditor can check a log they were handed.

  The size rule now bounds the read itself, in both implementations,
  written separately as the two-implementation rule requires: a line past
  the ceiling is an INVALID verdict at that line number and its remaining
  bytes are never gathered. Verdicts and line numbering are unchanged for
  every log that was valid before.

  The record-level size checks that used to sit after parsing are gone with
  it — the boundary that enforces the rule is now the read, and keeping a
  second, unreachable copy of it would be a branch no test could enter.

- `probavi adapter conformance` could abort with a harness error instead of
  reporting verdicts. The driver treated **any** stdin write failure as a
  suite-side failure, so an adapter that exited before the request landed —
  a cancelled run, an operator's Ctrl-C, or an adapter that answers and
  exits quickly — produced `write request: broken pipe` and exit 2 ("the
  suite could not be run to completion"). An adapter author saw the tool
  break for behaviour their adapter got right.

  A closed stdin is now no verdict by itself, which is what the core's own
  protocol client has always done: §2.1 lets an adapter stop the moment it
  sees EOF, and §2.3 makes "exited without a final response" a crash, so
  the read loop and the exit status decide what happened. `internal/conformance`
  is deliberately an independent implementation of the protocol, so it gets
  its own rule rather than a shared helper — the divergence CI surfaced is
  exactly what that independence is for.
- **Security:** the k8s and remotehost providers no longer put per-command
  environment values on the command line. Both passed `env NAME=value` to
  the command they ran, so a database password a check needs was readable
  from the process list — on the drill host and inside the pod for k8s, on
  the drill host and on the target for remotehost. The docker provider's
  half of this was fixed in the previous release entry; this completes it.

  Neither provider has an out-of-band environment channel (`kubectl exec`
  has no environment flag, ssh's `SendEnv` depends on the target's
  `AcceptEnv`), so the values now travel through stdin: the command runs
  under a shell prelude that reads exactly N `NAME=value` lines, exports
  each, and execs the command, whose own stdin continues untouched after
  those lines. Both providers already require `sh`, as `put_file` does.

  A value containing a newline cannot be expressed in a line protocol and
  is rejected rather than silently truncated — exporting the tail of a
  credential as a second variable would be the worse failure. The rejection
  never echoes the value.

  The `Descriptor.Constraints` of both providers — and therefore
  `docs/capabilities.json` — now describe the stdin channel instead of the
  exposure.

- **Security:** resolved database passwords no longer reach the docker
  provider's command line. `internal/checks` refuses `{{password}}` in an
  `sql_runner` argv template because "a password in argv would leak into
  process listings" — and the provider then passed the value as
  `docker exec -e NAME=value`, putting it in the drill host's process list
  for every local user to read. The value now travels in the docker CLI's
  own environment and only the variable's name appears in argv.

  The k8s and remotehost providers still have this exposure: `kubectl exec`
  has no environment flag and ssh's `SendEnv` depends on the server's
  `AcceptEnv`. It is now declared in both providers' `Descriptor.Constraints`
  and therefore in `docs/capabilities.json`, so the difference between the
  providers is published rather than discovered. A fix is designed and
  tracked separately.

- **Security:** an adapter could name any environment variable of the core
  process in `connection.password_env` and have its value handed to a
  process it controls inside the sandbox — exfiltration through a field
  meant for a database password. The name is now resolved against the same
  allow-list the adapter's own environment is built from (protocol §2.5):
  the core-generated ephemeral secret, or a variable the drill declared in
  `source.credential_env`. Anything else resolves to empty and is logged.
- Orphan detection was Linux-only, and releases ship macOS binaries. All
  three sandbox providers decided whether a sandbox's creating process was
  still running by stat-ing `/proc/<pid>`; on macOS that path does not
  exist, so the check failed for every pid and **every labeled sandbox
  looked orphaned**. Since the sweep runs at the start of every drill, a
  starting drill destroyed the running sandbox of any concurrent one on the
  same host — including parallel game-day members — mid-restore. The
  docker provider's own comment promised the opposite.

  Liveness is now decided by signalling the process with signal 0, which
  delivers nothing and only asks the kernel whether the pid exists and
  whether we may signal it. `EPERM` counts as alive: a process owned by
  another user is running, and treating "may not signal" as "gone" is the
  destructive answer. The check lives in `internal/sandbox` behind an
  injectable seam, so each provider's sweep decision is now unit-tested for
  both answers instead of depending on the host's filesystem layout.

  Unchanged: a recycled pid still looks alive, so an orphan can survive
  until its pid is free. That errs toward leaving a sandbox behind rather
  than destroying a live one.

- A drill whose composed record the store refused now leaves a **degraded
  record** instead of nothing (`docs/evidence-schema.md` §7.1, new). §7 has
  always required that every started drill end in exactly one appended,
  signed record; until now an unrepresentable value anywhere in the record
  broke that rule outright, and the drill vanished from the log — which
  reads exactly like a drill that was never scheduled.

  The replacement carries only what the core itself produced — drill
  identity, environment, total duration, `outcome: error` with
  `error.code: internal` and a message naming both the rejection and the
  verdict that was reached — and drops everything an adapter or the
  configuration supplied, since one of those values is why the record was
  refused. It uses no new fields, so no verifier needs to learn about it.

  Deliberately narrow: only shape rejections are retried. A store that
  cannot write is not a record the core can fix by rewriting it, and a
  second attempt would bury the real error. If even the degraded record
  fails, the original "left no evidence" error surfaces as before.

  A degraded record is a bug report, not a verdict — it is logged at error
  level and never claims `pass`.
- A drill that restored a backup successfully could end with **no evidence
  record at all** — exit 5, the failure mode `internal/core`'s package
  comment calls the highest-severity bug — because values an adapter
  reports were copied into the record without being checked against what
  the record accepts. `evidence.Record.Validate` rejects; it does not
  repair. Three reachable inputs did it:

  - `created_at` at second precision (valid RFC 3339, and the form the
    protocol's own example showed) — rejected as "not RFC 3339 UTC with
    millisecond precision",
  - a NaN phase timing — the `< 0` guard is false for NaN, and
    `int64(math.Round(NaN*1000))` is the most negative int64 there is, so
    the record was rejected for a negative duration,
  - a negative `size_bytes`.

  `validateProvisionResult` now holds the provision response to everything
  the record will demand of it: `created_at` is parsed and normalized to
  the schema's UTC millisecond form (truncated, never rounded), timings
  must be finite, non-negative and representable, `size_bytes`
  non-negative. A violation is an `adapter_crash` verdict **with** a
  signed record, which is what the boundary is for.

  Also accepted, here and in `probavi adapter conformance`: RFC 3339's
  lowercase `t`/`z` designators, which
  `docs/schemas/adapter/provision-response.json` has always declared valid
  while Go's `time.RFC3339` layout rejects them — an adapter whose output
  validates against the published schema must not fail certification.

- Probavi could sign records that fail Probavi's own published schema.
  `docs/schemas/evidence/record.json` constrains `error.code` to fourteen
  values; the core copied whatever code the adapter returned, and
  `Record.Validate` only checked that it was non-empty. An adapter
  answering `{"code":"banana_peel"}` produced a chain-valid, signed record
  that `probavi evidence verify` reports VALID while a schema check reports
  INVALID — the one contradiction a trust product cannot ship.

  The vocabulary now has a single home in `internal/evidence` (thirteen
  codes from the adapter protocol's §5 registry plus schema-defined
  `check_failed`), the core normalizes an unregistered code to `internal`
  and keeps the original in the message, and a schema test pins the Go list
  to the published enum in both directions so they cannot drift.

  It is normalization rather than rejection on purpose: refusing the record
  would trade a wrong code for no evidence at all, which is the worse
  failure.
- Non-ASCII failure text could make a completed drill leave **no evidence
  record**. Two producers of record fields measured in characters what the
  record layer measures in bytes, or cut a string without regard for
  encoding:

  - `core.sanitizeMessage` capped `error.message` at 500 *runes* against a
    512-*byte* limit, so an adapter reporting an engine error in an
    accented language — 400 characters, 800 bytes — was rejected;
  - `checks.truncateDetail` sliced `checks[].detail` at a byte offset,
    splitting a rune whenever engine stderr crossed the cap with non-ASCII
    text, and the invalid UTF-8 was rejected.

  Either one meant exit 5 after a successful restore. Both now use
  `evidence.TruncateLine`, which truncates on byte length at a rune
  boundary, alongside the exported `MaxDetailBytes` / `MaxErrorMessageBytes`
  caps — the mismatch between a producer's private constant and the
  schema's real limit was the bug, so the limits now have one home. The
  helper is swept across every budget and every UTF-8 stride, and a
  regression test drives a full drill with non-ASCII text through the real
  store.

- The open-core boundary in `ROADMAP.md` said "proving a **single drill**
  stays free forever". It was written on 2026-07-31, before game-days
  existed, and read as if `probavi gameday` — multi-database,
  dependency-ordered orchestration — were part of the commercial
  organisational layer. It never was: game-days shipped on 2026-08-02 as
  Apache-2.0 core and are a full free feature with no member limit. The
  boundary is now stated by what stays free rather than by scale, the
  README's license enumeration names game-days (plus notifications and
  metrics) explicitly, `docs/gameday.md` says it where a game-day user
  reads it, and `AGENTS.md` §6 declares the commercial list exhaustive so
  the ambiguity cannot come back. No licensing change — a wording
  correction to match what has always shipped.
- The README's status paragraph said "Released as **v0.1.0**" after
  `0.2.0` had shipped, while the install note two sections below already
  explained why `v0.2.0` is the version to use. The landing page of a
  trust product understating its own released version is exactly the
  drift the capabilities manifest exists to prevent, one surface up.

## [0.2.0] - 2026-08-03

### Changed

- **Breaking — the module path is now `github.com/probavi/probavi`.** The
  repository moved from `aafeher/probavi` to the `probavi` organisation on
  2026-08-03. GitHub redirects the old path, so clones and links keep
  working, but a Go module is identified by what its `go.mod` declares, not
  by where it is served from: without this change the canonical URL would
  have been the one address the project could not be installed from. Both
  modules moved — the core and `spec/evidence`, the independent verifier
  that lives in its own module so that the toolchain, not discipline,
  forbids it importing `internal/`. Update imports and install commands:

  ```sh
  go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@latest
  ```

  **`v0.1.0` is not installable under the new path and never will be.** Its
  `go.mod` declares the old module path, and Go checks that a module says
  what it was asked for, so building `github.com/probavi/probavi@v0.1.0`
  fails with a path mismatch. Retagging cannot fix it: `proxy.golang.org`
  and `sum.golang.org` have already recorded that version, and both are
  immutable by design — re-pointing the tag would replace a clear error
  with a checksum-mismatch security error. Use
  `github.com/aafeher/probavi@v0.1.0`, which still resolves and builds, or
  move to `v0.2.0` under the new path. The `v0.1.0` GitHub release and its
  binaries are unaffected; only module resolution is.

### Fixed

- The README's status paragraph said "Released as **v0.1.0**" after
  `0.2.0` had shipped, while the install note two sections below already
  explained why `v0.2.0` is the version to use. The landing page of a
  trust product understating its own released version is exactly the
  drift the capabilities manifest exists to prevent, one surface up.

- `probavi adapter conformance` usage text now documents exit code `2`.
  The command has always returned it when the suite cannot be driven to
  completion — an adapter that fails to exec, a stdout that cannot be
  written — but the help text listed only `0`, `1`, and `3`, so a CI
  pipeline branching on the documented set would have treated a suite
  that never ran as an unrecognized status. Surfaced while building the
  CLI contract table for `docs/capabilities.json`, which reads the exit
  codes from the same table the binary dispatches from. All 23 locale
  catalogs carry the new clause. **It shipped without a native-speaker
  review pass** (docs/i18n.md §5); that review is still owed, and a
  correction will follow in a later release if one is needed.

### Added

- Generated capabilities manifest: `docs/capabilities.json`
  (`probavi-capabilities/1`), the machine-readable statement of what
  Probavi can do in this repository — adapters with the engine versions
  CI actually restores from, sandbox providers with their parameters and
  isolation properties, built-in checks, the CLI commands with their
  exit-code contract, notification transports, available locales, the
  three contract versions, and the non-goals a consumer must never
  contradict. Its consumer contract is `docs/capabilities.md`
  (normative), its schema `docs/schemas/capabilities/capabilities.json`,
  and it is versioned independently of the binary like the adapter
  protocol and the evidence schema.

  It exists because downstream surfaces — the website reads this
  repository as a submodule — had no machine-readable source for
  capability claims and were writing them from README prose, which is
  exactly how a trust product ends up overstating what it does. Every
  fact is read from the code that implements it: the adapters' own probe
  goldens, the sandbox provider descriptors, the check registry, the CLI
  command table, the notification constants, the embedded locale
  catalogs, and the frozen contract version constants. The generator
  refuses to publish a claim the repository does not back — an adapter
  whose probe declares a source kind its manifest does not name, an
  engine version that does not appear in the image the integration suite
  pulls, a document path that no longer exists, an unknown maturity
  value — and a fifth CI gate ("Capabilities manifest") regenerates the
  file and fails on any diff. Output is deterministic and carries no
  timestamp or build metadata, so a diff always means a capability
  changed. `probavi` gains no new subcommand: the manifest describes the
  repository, not the binary.

- Independent evidence verifier (`spec/evidence`, evidence schema §12).
  Verifying a Probavi evidence log no longer depends on Probavi: a
  second implementation of the format, written from
  `docs/evidence-schema.md` alone, ships as the dependency-free
  `probavi-evidence-verify` tool, installable on its own with
  `go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@latest`.
  It lives in a separate Go module, so the toolchain — not convention —
  forbids it importing Probavi's own evidence code. Until now the only
  thing checking the hash chain and the signatures was the code that
  wrote them; both implementations now agree on every published schema
  version and on tampered input, down to the failing line number. The
  worked example moved from `internal/evidence/testdata/` to
  `docs/schemas/evidence/examples/` and is published as a conformance
  vector anyone can verify against. **No evidence-format change:** no
  field, serialization rule, or record byte is affected, and the frozen
  example logs are byte-identical.

- Internationalized CLI output (spec docs/i18n.md): the usage text and
  CLI diagnostics are now localizable, with Hungarian (`hu`) as the
  first national language. The locale comes from
  `PROBAVI_LANG → LC_ALL → LC_MESSAGES → LANG` (POSIX order, no new
  flags or config keys); English remains the default and the fallback
  for anything unknown. Catalogs are zero-dependency embedded JSON
  keyed by the English text itself, and CI gates enforce completeness,
  staleness, and format-verb parity per language — a partially
  translated language cannot ship. Machine contracts are never
  translated: evidence records, JSON summaries, the adapter protocol,
  notification payloads, and structured logs stay English.
  Configuration-validation diagnostics (drill and game-day files) are
  part of the translated surface: `probavi run --config broken.yaml`
  explains the problem in the operator's language, with config keys and
  locators kept verbatim.
- DR game-day orchestration (`probavi gameday`, spec docs/gameday.md):
  multi-database restore exercises in dependency order. A game-day
  config references normal drill files as members with `depends_on`
  edges; each member runs the full drill pipeline — sandbox, restore,
  checks, its own signed evidence record, metrics, notifications — so
  the evidence schema is untouched and members stay independently
  runnable. Dependents of a failed member are skipped with a recorded
  reason (cascading), independent branches always run to completion,
  and cancellation leaves signed `cancelled` records for running
  members. Execution is sequential by default; `max_parallel` opts in
  to bounded concurrency, with a load-time guard rejecting members
  that share an evidence log while allowed to overlap. The one-line
  JSON summary lists every member with its record location
  (evidence path + seq) and reports the end-to-end wall clock — the
  service-level recovery time. Exit codes: 0 all passed, 1 a member
  drill failed, 2 errors/cancellation left members unproven, 5 a
  member's record could not be written.
- Webhook notifications (`notify` config section, docs/notifications.md):
  one JSON POST per configured webhook after the evidence record is
  signed, carrying the `probavi-notification/1` payload — a signpost to
  the record (outcome, check counts, restore timing, sequence number),
  never a substitute for it. URLs come from config or, for token-bearing
  endpoints, from the environment (`url_env`) and are redacted from all
  logs and errors; optional HMAC signing (`secret_env`,
  `X-Probavi-Signature-256`) lets receivers authenticate pushes. An `on`
  filter narrows delivery per outcome; the default — every outcome —
  keeps dead-man's-switch receivers working. Delivery is bounded (60 s
  budget, 3 attempts, no redirects), runs outside the drill timeout so
  cancelled drills still notify, and never changes the drill's verdict
  or exit code. The payload has a machine-readable schema
  (`docs/schemas/notification/payload.json`) validated in CI.
- SQL Server adapter (`adapters/mssql`, `probavi-adapter-mssql`):
  restores native `BACKUP DATABASE` artifacts (`bak`, `bak_dir` kinds)
  under the drill's target name, with the file list read from the backup
  and every logical file `MOVE`d to sandbox paths. The sandbox starts
  idle and the adapter owns the engine: SQL Server cannot run without a
  superuser password, and a password in sandbox params would enter the
  signed evidence record — so the drill engine uses a documented public
  constant confined to the zero-ingress sandbox, the mssql analog of the
  postgres trust overwrite and the mysql empty root password. sqlcmd's
  dialect quirks are absorbed declaratively (`-I` for double-quoted
  identifiers, a `SQLCMDINI` startup script for undecorated rows), so
  builtin checks work unchanged. Conformance 15/15.
- MongoDB adapter (`adapters/mongodb`, `probavi-adapter-mongodb`):
  restores `mongodump --archive` backups — plain or `--gzip`, the
  compression sniffed from the artifact bytes — with
  `mongorestore --stopOnError`, so partial restores fail loudly. Source
  kinds: `mongodump` (one archive file) and `mongodump_dir` (newest file
  in a directory). Checks are mongosh `--eval` expressions carried by the
  declared sql_runner template; the core stays engine-free, and the
  adapter passes all 15 conformance checks. Third engine, zero core
  changes.
- `remotehost` sandbox provider: restore drills on dedicated hosts that
  cannot run any container runtime, over plain SSH + systemd
  (`docs/sandbox-bare-host.md`). One sandbox is one transient systemd
  slice plus one per-drill workspace; every command — including the
  engine the adapter starts — runs as a transient unit inside the slice,
  so resource caps bound the whole sandbox and stopping the slice kills
  the entire process tree. Cleanup is three-layered (destroy on every
  outcome, host-scoped orphan sweep, target-side deadline timer that
  survives a vanished drill host). The target is selected with
  `PROBAVI_SSH_TARGET` in the environment only; connection details never
  enter drill config or evidence records.
- Remote Docker over SSH: the docker sandbox provider is documented and
  CI-proven against remote daemons selected with `DOCKER_HOST=ssh://…` —
  drills run on the remote machine while backups stream through the SSH
  connection, never a published port. The endpoint lives in the
  environment only; connection details never enter evidence records.

### Fixed

- The docker provider's `put_file` lands files owned by the identity
  exec commands run as (root-run chown after the copy), matching the k8s
  and remotehost providers, where the exec user creates the file by
  construction. Previously `docker cp` preserved the host file's numeric
  uid, so on images with a non-root default user (SQL Server runs as
  `mssql`) the copied backup was unreadable by the engine and the mode
  step failed outright. `put_file` on the docker provider now requires
  `sh` in the image, like the other providers always did.
- The docker orphan sweep is host-scoped now (matching the k8s provider):
  sandboxes carry a `com.probavi.host` label, and when several drill
  hosts share one daemon, a host can no longer mistake another host's
  live drill for a dead orphan and sweep it mid-run.

## [0.1.0] - 2026-08-01

First tagged release. Everything below is new.

### Added

- `probavi run`: config-driven restore drills — disposable sandbox up,
  backup restored by an engine adapter, validation checks, guaranteed
  teardown, and exactly one signed evidence record per started drill, on
  every path including crashes and cancellation. Cron/CI-friendly exit
  codes; no built-in scheduler by design.
- **Adapter protocol v0** (`docs/adapter-protocol.md`, frozen): engine
  adapters are external processes speaking line-delimited JSON on
  stdin/stdout, acting on the sandbox only through core-mediated verbs
  (`exec`, `put_file`). Machine-readable JSON Schemas for every message
  shape in `docs/schemas/adapter/`.
- **PostgreSQL adapter**: `pgdump`, `pgdump_dir` (logical), `pgbackrest`
  (physical) sources; point-in-time recovery on pgBackRest sources.
- **MySQL/MariaDB adapter**: `mysqldump`, `mysqldump_dir` (logical),
  `xtrabackup` (Percona XtraBackup, physical) sources.
- Point-in-time recovery drills: `target.pitr` drill config with exactly
  one of `target_time` (absolute) or `target_age` (relative, resolved at
  drill start); the resolved instant is recorded in evidence as
  `drill.pitr_target`.
- Sandbox providers: **Docker** (zero-ingress defaults: no published
  ports, `--network none`) and **Kubernetes Job** (no service-account
  token, cluster-side cleanup backstop); both drive the respective CLI,
  label every resource, and sweep orphans on startup.
- **Evidence store** (`docs/evidence-schema.md` v1, frozen): append-only
  JSONL, RFC 8785 canonical bytes, SHA-256 hash chain, ed25519 signatures.
  `probavi evidence verify` proves a log offline with only the public key;
  `probavi evidence keygen` generates key pairs. Machine-readable schema
  covering all published record versions in `docs/schemas/evidence/`.
- Validation checks: `service_healthy`, `table_exists`, `row_count`,
  `freshness`, and user-defined SQL assertions — engine-agnostic via the
  adapter-declared `sql_runner` template, redaction-safe by construction.
- `probavi adapter conformance`: the frozen 15-check protocol conformance
  suite against a simulated sandbox — no container runtime needed; the
  mechanical definition of done for third-party adapters, with the
  developer guide in `docs/adapter-development.md`.
- `probavi adapter probe`: resolve an adapter and print its capabilities.
- Prometheus textfile metrics per drill, including rolling restore-duration
  quantiles (p50/p95/max over the last 100 restores) for RTO trend
  alerting.
- `probavi version`: prints the binary version and the contract versions
  the build speaks.

[Unreleased]: https://github.com/probavi/probavi/compare/v0.17.0...HEAD
[0.17.0]: https://github.com/probavi/probavi/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/probavi/probavi/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/probavi/probavi/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/probavi/probavi/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/probavi/probavi/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/probavi/probavi/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/probavi/probavi/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/probavi/probavi/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/probavi/probavi/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/probavi/probavi/compare/v0.7.1...v0.8.0
[0.7.1]: https://github.com/probavi/probavi/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/probavi/probavi/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/probavi/probavi/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/probavi/probavi/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/probavi/probavi/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/probavi/probavi/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/probavi/probavi/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/probavi/probavi/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/probavi/probavi/releases/tag/v0.1.0
