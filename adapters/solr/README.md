# probavi-adapter-solr

Restores Apache Solr backups into a disposable sandbox so a drill can
prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

## Supported source kinds

| Kind | What it takes |
| --- | --- |
| `solr_backup_tar` | A tar archive of one backup directory |
| `solr_backup` | One Collections API backup directory (`action=BACKUP`) |
| `solr_backup_dir` | A directory of backup directories; the newest is restored |

A backup directory is what `action=BACKUP&name=<name>` leaves behind: one
subdirectory per collection, each holding `backup_N.properties`,
`shard_backup_metadata`, `zk_backup_N` and `index`.

The drill restores **one collection**. A backup holding several is
refused rather than restored in part — proving one of them would prove
less than the backup contains, quietly.

## SolrCloud, not standalone — and the two current lines disagree

This adapter restores through the **Collections API**. That decides which
servers it can drill, and the answer is not the same across Solr's two
current lines:

| Image | Mode it starts in | Collections API |
| --- | --- | --- |
| `solr:10` | `solrcloud`, with an embedded ZooKeeper | works |
| `solr:9.10` | `std` (standalone) | answers HTTP 400 |

Both measured. So only the 10.x line is listed under `verified`, and a
drill against a standalone server is refused with that reason rather than
left to puzzle over a 400:

> this sandbox runs Solr in standalone mode, and this adapter restores
> through the Collections API, which such a server refuses

A 9.10 server started with `-c` runs in SolrCloud mode and is a different
matter — but nothing here has proven that, and `verified` says only what
CI restores from.

## The sandbox needs no idle command

Unlike most adapters in this catalog, this one does not start the engine.
The official image starts Solr itself, and it serves under `--network
none` in about two seconds — no published ports, no name resolution
needed. The drill config needs an image and a memory limit and nothing
else.

## Where the backup goes (`SOLR_HOME`)

Solr refuses to read a backup from anywhere outside its own data
directories:

```
Path /var/solr/bk must be relative to SOLR_HOME, SOLR_DATA_HOME
coreRootDirectory. Set system property 'solr.security.allow.paths' to add
other allowed paths.
```

So the adapter asks the sandbox for `SOLR_HOME` and transfers the backup
there. Where the image cannot answer, it uses `/var/solr/data`, the
official image's value, and lets Solr refuse the path in its own words if
that is wrong — a refusal that names the setting beats a quiet guess at a
different one.

## A backup that deletes its own documents is refused

This is the one thing to know before pointing a drill at a Solr backup.

`DocExpirationUpdateProcessorFactory` deletes every document whose expiry
field has passed, on a timer. No configset the official image ships
enables it — but **a backup carries its collection's configset**, and a
restore installs it. Measured end to end on Solr 10:

| | |
| --- | --- |
| backup taken while 3 documents were live | `numFound` 3, status 0 |
| restored after their expiry had passed | status 0 — **success** |
| the restored collection, seconds later | **`numFound` 0** |

The restore reports success and the collection then empties itself. A
drill would call that green and prove nothing, or fail a count check and
blame a backup that is perfectly intact.

Nothing can be suspended: the setting is in your own collection
configuration, and an adapter that rewrote it to make a drill pass would
be proving something other than your backup. So the drill is **refused**,
and — because the configset is a file in the artifact — refused before a
byte is transferred, for archives as well as directories.

What the fence reads is the artifact and nothing outside it: regular files
within it, and at most the first 4 MiB of each. A `solrconfig.xml` that is
a symlink is not followed — a backup is input this adapter does not trust,
and the archive pass has never followed one either — so a directory backup
that keeps its configuration behind a link is not inspected for expiry.

If you drill a collection that uses document expiry, the honest options
are to drill a collection that does not, or to take the backup from one
where expiry is not configured.

## Checks: the Solr query dialect

A check is one Solr query string — everything you would write after
`select?`:

```yaml
checks:
  - sql: "q=*:*&rows=0"                    # how many documents came back
  - sql: "q=status:paid&rows=0"            # how many match a filter
  - sql: "q=id:order-7&rows=1&fl=id,total" # the row itself
```

Two things the dialect settles, both measured:

**Counts.** Solr's CSV writer emits documents and nothing else, so a query
asking for no documents answers with a header line and no rows. That is
exactly the shape of a counting check, so when no document rows come back
the runner asks the same query again for `numFound` and prints that. A
query matching nothing therefore answers `0` — a value a check can
compare — rather than nothing at all.

**Parameters.** The query string is split on `&` and each parameter is
url-encoded separately, so `q=n:[0 TO 9]` keeps its spaces while `rows=0`
stays a parameter of its own. Encoding the whole string as one value
instead folds every later parameter into `q`, silently.

The check text is never pasted into a shell or a URL by hand.

## When the backup was taken

The engine records it, so the adapter reports it: `startTime` from
`backup_N.properties`, in UTC, reaches the evidence record as
`backup.created_at`. Nothing is invented from a directory's mtime — that
would date a copy.

Because the engine's own timestamp is already absolute,
`source.params.backup_timezone` has nothing to correct and is refused
rather than silently ignored.

## Backup identity

`solr_backup` and `solr_backup_dir` hash the tree: every regular file's
path and content, in sorted order. `solr_backup_tar` hashes the archive's
bytes. Either way the checksum in the evidence record is a measurement of
what was restored.

## Errors it reports

| Code | When |
| --- | --- |
| `source_not_found` | the path does not exist, or a directory holds no backups |
| `source_unreadable` | the artifact cannot be read, or is still being written |
| `source_corrupt` | Solr could not restore a core from it, an archive holds no backup, or an archive carries more collection and configuration names than a backup holds |
| `unsupported_source` | an unknown kind, several collections, or document expiry (above) |
| `invalid_request` | a standalone server, a PITR request, or a collection name Solr will not take |
| `engine_not_ready` | Solr did not answer within three minutes |
| `restore_failed` | the Collections API refused, or reported success for a collection it does not serve |

That last one is a gate, not a formality: the API can answer 0 and leave a
collection that never came up, and every check would then run against
nothing.

The gate asks the collection to answer a query — the same thing a check
does — rather than asking the Collections API whether the name is listed.
Those answer at two different instants: a collection appears in `LIST`
while a node is still answering 404 for `/select`, measured in CI where
the first check after a good restore failed and the next three passed. So
provision waits out that window, up to a minute, instead of handing it to
whatever runs first. The listing still decides how long to wait: a
collection missing from it after a synchronous `RESTORE` reported success
is not late but absent, and is refused straight away.

## Drill config options

| Option | Effect |
| --- | --- |
| `collection` | restore into this collection instead of the name the backup carries |

## Environment

None. The sandbox has no published ports and Solr in the official image
needs no credentials, so nothing is read from the environment and nothing
is redacted from the record.

## Point-in-time recovery

Not supported. A Solr backup is a snapshot of an index at one instant,
and the engine offers nothing to recover between two of them.
