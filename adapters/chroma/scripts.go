package main

// The shell this adapter runs inside the sandbox.
//
// Chroma's official image carries bash, sleep, tar, gzip, cp, find, cat and
// grep — and no HTTP client at all: no curl, no python, no jq (measured
// 2026-09-13 on chromadb/chroma:1.5.9). Every read below therefore goes
// through bash's own /dev/tcp, which the image's bash 5.2.37 supports. That
// is why the sandbox needs no tooling added to it: the drill brings its
// client with it, as text.

// apiBase is the v2 path prefix for the default tenant and database. Chroma
// creates both on first start and a persistence directory restored here
// carries them; a collection lives under this prefix by its own id.
const apiBase = "/api/v2/tenants/default_tenant/databases/default_database/collections"

// httpPort is the loopback port the engine is started on. Nothing outside
// the sandbox reaches it — the provider publishes no ports.
const httpPort = "8000"

// httpFn is a complete HTTP/1.0 client in bash, shared by every script
// below. HTTP/1.0 with no keep-alive means the server closes the
// connection at the end of the body, so `cat` reads exactly the payload
// and stops; the header loop consumes up to the blank line first.
//
// The body is read with `cat` rather than a read loop on purpose: a JSON
// response carries no trailing newline, and a `while read` loop drops the
// last line when it is unterminated — which is the whole body here.
const httpFn = `http() {
  local m=$1 p=$2 b=${3-} line
  exec 3<>/dev/tcp/127.0.0.1/` + httpPort + ` || { echo 'chroma is not listening on ` + httpPort + `' >&2; return 9; }
  { printf '%s %s HTTP/1.0\r\nHost: localhost\r\n' "$m" "$p"
    [ -n "$b" ] && printf 'Content-Type: application/json\r\nContent-Length: %d\r\n' "${#b}"
    printf '\r\n%s' "$b"; } >&3
  while IFS= read -r line <&3; do case "$line" in ''|$'\r') break;; esac; done
  cat <&3
  exec 3<&-
}
`

// prepareScript makes the one directory put_file copies into, and only
// that one. `docker cp` refuses a destination whose parent does not exist
// (measured: "Could not find the file /probavi-chroma in container"), and
// it copies a source directory *inside* a destination that already exists
// (measured too, by this adapter failing that way first). So the parent is
// created and the destination itself deliberately is not: the transfer
// creates it, as a copy of the artifact rather than a directory holding
// one.
const prepareScript = `set -eu
rm -rf ` + rootDir + `
mkdir -p ` + rootDir + `
`

// startScript starts the engine on the restored persistence directory and
// returns immediately.
//
// The server is detached with nohup so the exec that starts it can return —
// the core's exec verb waits for the command to finish, and this command
// must outlive it. Its output goes to a file the diagnosis script reads
// when readiness never arrives.
//
// --host 127.0.0.1 rather than 0.0.0.0: the sandbox publishes no ports and
// every read happens inside it, so there is nothing to gain from listening
// wider, and a restored database holds production data.
const startScript = `set -u
nohup chroma run --path ` + dataDir + ` --host 127.0.0.1 --port ` + httpPort + ` >` + engineLog + ` 2>&1 &
echo started`

// readyScript asks whether the engine answers yet. The heartbeat is the
// cheapest endpoint that proves the frontend is serving.
const readyScript = `set -u
` + httpFn + `
http GET /api/v2/heartbeat | grep -q heartbeat`

// verdictScript is the read that decides whether the restore produced a
// database worth anything, and it is deliberately not a count.
//
// Measured on chromadb/chroma:1.5.9: with the write queue already purged,
// deleting a collection's whole HNSW segment directory leaves `count`
// answering exactly right — 2200 of 2200 — and every document readable,
// while the vector search the store exists for returns nothing. No error,
// no warning, no line in the log. A drill that asked for a row count would
// have reported a green restore of a database that can no longer answer a
// single nearest-neighbour query.
//
// So each collection is queried with a vector taken out of that same
// collection, and the answer must have as many neighbours as were asked
// for. Using a stored vector rather than a synthetic one removes the
// dimension from the script entirely and makes the query one the index must
// be able to answer: the vector is in there.
const verdictScript = `set -u
` + httpFn + `
ids=$(http GET ` + apiBase + ` | grep -oE '"id":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"' | cut -d'"' -f4 | sort -u)
if [ -z "$ids" ]; then echo 'no collections' >&2; exit 3; fi
seen=0
for id in $ids; do
  seen=$((seen+1))
  n=$(http GET ` + apiBase + `/$id/count)
  case "$n" in ''|*[!0-9]*) n=0 ;; esac
  want=$n; [ "$want" -gt 5 ] && want=5
  if [ "$want" -eq 0 ]; then continue; fi
  emb=$(http POST ` + apiBase + `/$id/get '{"limit":1,"include":["embeddings"]}' |
        grep -o '"embeddings":\[\[[^]]*\]\]' | sed 's/.*\[\[//; s/\]\].*//')
  got=0
  if [ -n "$emb" ]; then
    got=$(http POST ` + apiBase + `/$id/query "{\"query_embeddings\":[[$emb]],\"n_results\":$want}" |
          grep -o '"ids":\[\[[^]]*\]\]' | sed 's/.*\[\[//; s/\]\].*//' | tr ',' '\n' | grep -c .)
  fi
  if [ "$got" -ne "$want" ]; then
    printf 'collection %s holds %s records and answered a nearest-neighbour query with %s of the %s asked for\n' \
      "$id" "$n" "$got" "$want" >&2
    exit 4
  fi
done
printf '%s\n' "$seen"`

// checkScript is the §6.1 runner: the core hands it a check's text and it
// speaks HTTP for it, because Chroma has no SQL and the generating
// built-ins therefore do not apply (the mongodb precedent).
//
// The text is a path, optionally followed by a space and a JSON body — a
// body makes it a POST. A path that does not start with "/" is taken as
// relative to the restored database's collections, so a check reads
// `<collection>/count` rather than spelling the whole v2 prefix.
// A collection is named by its own name, not its id. Chroma's read
// endpoints want the UUID (measured: a name there answers 400 "Collection
// ID is not a valid UUIDv4"), and a drill config should not have to carry
// a UUID that changes every time the collection is recreated — so the
// runner resolves the name through the collections path first, and passes
// a value already shaped like a UUID straight through.
//
// §6.1 requires rows on stdout and a non-zero exit on error. The status
// line is the verdict — an engine answering 404 has answered — and a body
// that is nothing but a number is printed as that number so a numeric
// check compares against what it expects rather than against JSON.
const checkScript = `set -u
` + httpFn + `
text=$2
spec=${text%% *}
body=''
case "$text" in *' '*) body=${text#* } ;; esac
case "$spec" in
  /*) path=$spec ;;
  *)
    coll=${spec%%/*}
    rest=''
    case "$spec" in */*) rest=/${spec#*/} ;; esac
    case "$coll" in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-*-*-*-*) ;;
      *)
        id=$(http GET ` + apiBase + `/"$coll" |
             grep -oE '"id":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"' |
             head -1 | cut -d'"' -f4)
        if [ -z "$id" ]; then
          printf 'no collection named %s in the restored database\n' "$coll" >&2
          exit 1
        fi
        coll=$id ;;
    esac
    path=` + apiBase + `/$coll$rest ;;
esac
exec 3<>/dev/tcp/127.0.0.1/` + httpPort + ` || { echo 'chroma is not listening on ` + httpPort + `' >&2; exit 1; }
{ if [ -n "$body" ]; then
    printf 'POST %s HTTP/1.0\r\nHost: localhost\r\n' "$path"
    printf 'Content-Type: application/json\r\nContent-Length: %s\r\n\r\n' "${#body}"
    printf '%s' "$body"
  else
    printf 'GET %s HTTP/1.0\r\nHost: localhost\r\n\r\n' "$path"
  fi
} >&3
resp=$(cat <&3)
exec 3<&-
status=$(printf '%s' "$resp" | head -1 | cut -d' ' -f2)
payload=$(printf '%s' "$resp" | awk 'BEGIN{h=1} h && /^\r?$/ {h=0; next} !h {print}')
case "$status" in
  2*) ;;
  *) printf 'chroma answered %s\n' "${status:-nothing}" >&2
     printf '%s' "$payload" | head -c 300 >&2; exit 1 ;;
esac
printf '%s\n' "$payload"`

// startupErrorScript returns what the engine said while failing to come up,
// so a readiness timeout names the engine's own reason instead of the
// budget that expired.
const startupErrorScript = `tail -c 2000 ` + engineLog + ` 2>/dev/null |
  sed 's/\x1b\[[0-9;]*m//g' | grep -iE 'error|panic|refus|denied|corrupt' | tail -3`

// exitNoDatabase is the exit code the unpack scripts use to say the
// artifact held no metadata database, so the adapter can name that instead
// of reporting a shell failure.
const exitNoDatabase = 20

// exitNoDatabaseText is the same code as script text.
const exitNoDatabaseText = "20"

// placeDirScript moves the staged persistence directory into place.
const placeDirScript = `set -eu
rm -rf ` + dataDir + `
mkdir -p "$(dirname ` + dataDir + `)"
mv ` + stagingDir + ` ` + dataDir + `
[ -f ` + dataDir + `/` + sqliteFile + ` ] || exit ` + exitNoDatabaseText

// placeTarScript extracts the staged archive and finds the persistence
// directory inside it.
//
// An archive may hold the directory's contents at its root — `tar -czf
// backup.tar.gz -C /data .` — or the directory itself, which is what
// `tar -czf backup.tar.gz /data` writes. Both are what operators have, so
// both are accepted: after extraction the metadata database is looked for
// at the root and then one level down, and a single wrapping directory
// holding it is promoted. Two candidates are refused rather than guessed
// between.
func placeTarScript(gzip bool) string {
	flags := "-xf"
	if gzip {
		flags = "-xzf"
	}
	return `set -eu
rm -rf ` + dataDir + ` ` + unpackDir + `
mkdir -p ` + dataDir + ` ` + unpackDir + `
tar ` + flags + ` ` + archivePath + ` -C ` + unpackDir + `
if [ -f ` + unpackDir + `/` + sqliteFile + ` ]; then
  root=` + unpackDir + `
else
  matches=$(find ` + unpackDir + ` -mindepth 2 -maxdepth 2 -name ` + sqliteFile + ` -type f)
  count=$(printf '%s' "$matches" | grep -c . || true)
  [ "$count" = 1 ] || exit ` + exitNoDatabaseText + `
  root=$(dirname "$matches")
fi
mv "$root"/* ` + dataDir + `/
[ -f ` + dataDir + `/` + sqliteFile + ` ] || exit ` + exitNoDatabaseText + `
`
}
