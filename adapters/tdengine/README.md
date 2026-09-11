# probavi-adapter-tdengine

Restores a [TDengine](https://tdengine.com) backup into a disposable
sandbox and proves the engine serves it, for [Probavi](../../README.md).
It speaks `probavi-adapter/0` ([spec](../../docs/adapter-protocol.md)) and
is stdlib-only Go with no imports from the core.

## What a TDengine backup is

What `taosdump` writes:

```sh
$ cd /backups/nightly && taosdump -D power -o /backups/nightly
```

The tool leaves `dump_result.txt` and a header-only `dbs.sql` in the
output directory, and the payload in a `taosdump.<serial>` directory
beside them: the schema with its own `CREATE DATABASE` line, the tag
files, and the rows as avro.

**`dump_result.txt` lands in the directory taosdump runs in, not in the
one `-o` names** (measured) — hence the `cd` above. That file is where the
artifact says when it was taken and how many rows it holds, and both are
used below, so a dump written without it simply says less about itself.

## Source kinds

| Kind | Artifact |
| --- | --- |
| `taosdump` | One `taosdump` output directory, named at either level |
| `taosdump_dir` | A directory of them; the newest by the dumps' own recorded instant |
| `taosdump_tar` | One tar archive of such a directory |

"Either level" is not a convenience. **`taosdump -i` pointed at the
directory `-o` was given exits 0 and creates nothing at all** (measured),
so a drill naming the level an operator naturally thinks of would report
success having restored nothing. The adapter finds the payload directory
itself — by the `CREATE DATABASE` line, not by the directory's name — and
refuses an outer directory that holds several dumps rather than guessing
which one the drill meant.

An archive is read host-side in one streaming pass, so it says the same
things about itself a directory does: the database it holds, the retention
it declares, when it was taken, how many rows it claims.

## The exit code is not the verdict

`taosdump` answers 0 in every case worth telling apart. Measured on
3.3.6.13:

| What happened | What it printed | Exit |
| --- | --- | --- |
| Everything restored | `OK: 250 row(s) dumped in!` | 0 |
| One avro file truncated | `OK: 125 row(s) dumped in!` **and** `ERROR: 1 failures occurred to dump in!` | 0 |
| Pointed at the outer directory | no summary at all | 0 |
| Asked before the server was ready | `Retry to connect for 3 …` | 0 |

So the adapter reads what the tool said and holds it against what the
artifact claims: `# total row count:` from the dump's own accounting has
to match the `OK: N row(s) dumped in!` the restore reports, a failure line
is a corrupt backup, and a run that said nothing about restoring rows is a
restore that did not happen. Output carrying none of the tool's own
markers is left alone — that is not a restore this adapter can judge, and
the engine-facing gate below still has to find tables.

After the restore the database has to exist and serve at least one table.
A well-formed zero is refused: reporting one green is the failure this
project exists to prevent.

## Retention is a fence, not a switch

This is the engine's instance of the data-lifecycle rule. TDengine's
retention is `KEEP`, a per-database number of days, enforced on write: a
row outside the window is refused with `Timestamp data out of range`
(measured, error 1547, on a database created `KEEP 3d` with a row six days
old).

There is nothing to suspend. `KEEP` travels inside the backup — taosdump
writes the operator's own `CREATE DATABASE` line, retention included — so
the restore recreates the database with the policy they declared, and
widening it would rewrite what a check is entitled to read. taosdump has
no switch for it either; its `--loose-mode` is about escaping names
(measured).

What is left is exact rather than a guess: **a dump older than its own
`KEEP` holds nothing the engine will accept**, because every row in it is
outside the window before the restore starts. The adapter refuses such a
drill up front, naming the backup's age and the retention, instead of
restoring an empty database and calling it green. A dump that carries no
`dump_result.txt` cannot be dated, and the fence stays quiet rather than
guessing.

## Checks

TDengine speaks SQL, so the core's generating built-ins apply unchanged —
`table_exists`, `row_count` and `freshness` all work. A `sql` check is one
statement, run through the engine's HTTP endpoint inside the sandbox:

```yaml
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: power.meters
    min: 1
  - name: no-impossible-voltage
    sql: "SELECT count(*) FROM power.meters WHERE voltage < 0"
    expect: 0
```

## Sandbox

The sandbox starts idle and the adapter starts the engine:

```yaml
sandbox:
  provider: docker
  params:
    image: tdengine/tdengine:3.3.6.13
    command: sleep infinity     # required: the adapter starts the engine
    memory: 1g
  timeout: 30m
```

**The image's own entrypoint does not always finish.** Measured on both
verified images, on CI's runners, twice: the container's trace stops at
the line where the entrypoint reads its data directory —

```
++ taosd -C
++ grep -E 'dataDir\s+(\S+)' -o
++ head -n1
```

— with only that config-dump process alive, and nothing else ever starts.
The same image serves in 0.6 s on a development machine, so whatever that
pipeline waits for belongs to the host rather than to the backup, and a
drill has no business depending on which host it landed on. The adapter
therefore starts `taosd` and then `taosadapter` itself. A sandbox whose
entrypoint did finish is left alone: the engine is what matters, not who
started it.

The restore runs at **256 MiB** (measured).

### Readiness is a node the cluster calls ready

Not a query that answers. Measured on a freshly started engine:

| | t+338 ms | t+1006 ms |
| --- | --- | --- |
| `SHOW DATABASES` | answers | answers |
| `SELECT SERVER_STATUS()` | `1` | `1` |
| nodes reporting `ready` | **0** | **1** |
| `CREATE DATABASE` — what a restore does first | **error 820, "Out of dnodes"** | succeeds |

So the gate reads `information_schema.ins_dnodes`, and the healthcheck
reads the same thing: an engine that answers while no node is ready is
one a restore would fail against, and calling that healthy would be a
drill reporting on a server that cannot work.

## Environment

None. The sandbox has no published ports and the adapter speaks to the
server over loopback with the image's own default credentials, so nothing
is read from the environment and nothing is redacted from the record. An
image whose credentials differ is out of scope for this version rather
than silently wrong.

## Error codes

| Code | When |
| --- | --- |
| `source_not_found` | the path does not exist, or a directory holds no dump |
| `source_unreadable` | the artifact cannot be read, or is still being written |
| `source_corrupt` | the directory is no taosdump output, the tool reported failures, the restore and the artifact disagree about the row count, or the restored database serves no table |
| `unsupported_source` | an unknown kind, or an outer directory holding several dumps |
| `invalid_request` | a PITR request, or a file where a directory belongs |
| `restore_failed` | the backup is older than its own `KEEP`, the tool never reached the server, or it restored nothing |
| `engine_not_ready` | the server did not answer within three minutes |

## Verified against

`tdengine/tdengine:3.3.6.13` (baseline) and `tdengine/tdengine:3.3.5.8`. A
3.3.6.13 artifact restores into 3.3.5.8 (measured) — which is what those
two versions did on the day, not a supported-version range. TDengine
publishes no end-of-life dates, so the manifest records none.
