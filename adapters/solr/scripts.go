package main

// scripts.go holds the shell the adapter runs inside the sandbox.
//
// Arguments travel as positional parameters, never interpolated into the
// script text: a collection name and a check both come from drill config,
// and the only safe assumption about drill config is that it is data.
//
// The verified images carry curl, bash, awk, sed and tar, and carry
// neither jq nor python3 (measured), so every response is taken apart
// with the tools that are there.

// serverURL is where Solr answers inside its own sandbox. The sandbox has
// no published ports, so this address is reachable from nowhere else.
const serverURL = "http://127.0.0.1:8983/solr"

// homeScript asks the sandbox where Solr keeps its data, rather than
// assuming it.
//
// The backup location has to be somewhere Solr is willing to read:
// anything outside SOLR_HOME, SOLR_DATA_HOME or coreRootDirectory is
// refused outright — "Path /var/solr/bk must be relative to SOLR_HOME,
// SOLR_DATA_HOME coreRootDirectory. Set system property
// 'solr.security.allow.paths' to add other allowed paths" (measured). The
// official image sets SOLR_HOME to /var/solr/data; an image that sets it
// elsewhere is followed rather than overruled.
const homeScript = `printf %s "${SOLR_HOME:-` + defaultSolrHome + `}"`

// defaultSolrHome is where the official image keeps SOLR_HOME. It is the
// answer when the sandbox cannot give a usable one — writing there and
// letting Solr refuse the path loudly beats guessing quietly at a
// different one.
const defaultSolrHome = "/var/solr/data"

// readyScript asks whether Solr is answering yet.
const readyScript = `curl -sf -o /dev/null "` + serverURL + `/admin/info/system?wt=json"`

// cloudScript answers whether this server runs in SolrCloud mode, as a
// count rather than a name — the one question the adapter is asking.
//
// The two current lines do not agree: solr:10 starts in SolrCloud mode
// with an embedded ZooKeeper, and solr:9.10 starts in `std` mode with no
// ZooKeeper at all, where the Collections API answers 400 (both
// measured). This adapter restores through that API, so a standalone
// server has to be told apart from a broken one.
const cloudScript = `curl -s "` + serverURL + `/admin/info/system?wt=json" | grep -c '"mode":"solrcloud"'`

// restoreScript restores one backup into one collection.
//
// $1 is the directory the artifact was transferred to, $2 the backup name
// inside it, $3 the collection to create.
//
// The Collections API answers synchronously for a single-shard restore
// and can report a failure inside a 200 body, so the script decides and
// answers the one question the adapter is asking — did the restore
// happen, 1 or 0 — with the engine's own words on stderr for the
// diagnosis. Keeping the verdict and the explanation on separate streams
// is what lets the caller classify without parsing a body it did not
// write.
const restoreScript = `set -u
out=$(curl -s -w '\n%{http_code}' --get "` + serverURL + `/admin/collections" \
  --data-urlencode "action=RESTORE" \
  --data-urlencode "location=$1" \
  --data-urlencode "name=$2" \
  --data-urlencode "collection=$3") || { echo "the Collections API could not be reached" >&2; echo 0; exit 0; }
code=${out##*$'\n'}
body=${out%$'\n'*}
if [ "$code" = 200 ] && ! printf '%s' "$body" | grep -q '"msg"'; then
  echo 1
else
  printf 'HTTP %s %s\n' "$code" "$body" >&2
  echo 0
fi`

// extractScript unpacks a tar artifact into the layout the Collections
// API restores from: $1/$2/<collection>/…
//
// The archive's own top-level naming is not trusted — operators tar a
// backup from wherever it sat — so the collection directory is found by
// the file the engine itself writes, backup_N.properties, and moved into
// place. GNU find and tar are both in the verified images (measured). The
// collection it found is printed, which is what names the restore when
// the host-side pass could not read the archive.
const extractScript = `set -u
stage="$1/.probavi-stage"
rm -rf "$stage" "$1/$2"
mkdir -p "$stage"
tar -xf "$3" -C "$stage" || exit 90
src=$(find "$stage" -type f -name 'backup_*.properties' -printf '%h\n' | sort | head -1)
[ -n "$src" ] || exit 91
mkdir -p "$1/$2"
mv "$src" "$1/$2/" || exit 92
rm -rf "$stage"
basename "$src"`

// liveScript answers whether the named collection exists and is serving,
// as a single count. It is the gate a restore has to pass before this
// adapter calls it a success: the Collections API can answer 0 and leave
// a collection that never came up.
const liveScript = `set -u
curl -s "` + serverURL + `/admin/collections?action=LIST&wt=json" |
  tr ',' '\n' | sed -n 's/.*"\(.*\)".*/\1/p' | grep -cx -- "$1"`

// servedScript lists what the server does serve, for the message when the
// gate above answers zero.
const servedScript = `curl -s "` + serverURL + `/admin/collections?action=LIST&wt=json" |
  tr ',' '\n' | sed -n 's/.*"\(.*\)".*/\1/p' | grep -v '^$'`

// healthScript proves the restored collection answers a query.
const healthScript = `set -u
curl -sf "` + serverURL + `/$1/select?q=*:*&rows=0&omitHeader=true&wt=json" |
  sed -n 's/.*"numFound":\([0-9]*\).*/\1/p' | head -1`

// runnerScript absorbs the check dialect declaratively.
//
// A check is one Solr query string — everything an operator would put
// after `select?` — and $1 is the collection provision restored into.
// Solr's CSV writer emits documents and nothing else, so a query that
// asks for no documents answers with a header line and no rows
// (measured). That is precisely the shape of a counting check, and its
// answer is the one thing such a query has to say: when no document rows
// come back, the runner asks the same query again for `numFound` and
// prints it. A query matching nothing therefore answers 0 rather than
// nothing, which is a value a check can compare.
//
// The check text is one query string, so it is split on & and each
// parameter handed to curl separately: --data-urlencode encodes the value
// and leaves the name alone, which is what keeps `q=n:[0 TO 9]` working
// while `rows=0` stays a parameter of its own. Encoding the whole string
// as a single value instead silently folds every later parameter into q —
// measured, and the reason this is written down.
//
// An HTTP status other than 200 fails the check with the engine's own
// body on stderr.
const runnerScript = `set -u
ask() {
  local args
  args=(--get "` + serverURL + `/$1/select" --data-urlencode "wt=$2" --data-urlencode "omitHeader=true")
  local IFS='&' part
  for part in $3; do
    [ -n "$part" ] && args+=(--data-urlencode "$part")
  done
  curl -s -w '\n%{http_code}' "${args[@]}"
}
out=$(ask "$1" csv "$2") || exit $?
code=${out##*$'\n'}
body=${out%$'\n'*}
body=${body%$'\n'}
[ "$code" = 200 ] || { printf '%s\n' "$body" >&2; exit 1; }
rows=$(printf '%s\n' "$body" | tail -n +2)
if [ -n "$rows" ]; then
  printf '%s\n' "$rows"
  exit 0
fi
out=$(ask "$1" json "$2") || exit $?
code=${out##*$'\n'}
body=${out%$'\n'*}
[ "$code" = 200 ] || { printf '%s\n' "$body" >&2; exit 1; }
printf '%s\n' "$body" | sed -n 's/.*"numFound":\([0-9]*\).*/\1/p' | head -1`
