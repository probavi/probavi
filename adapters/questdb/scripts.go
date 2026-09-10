package main

// scripts.go holds the shell the adapter runs inside the sandbox.
//
// Arguments travel as positional parameters, never interpolated into the
// script text: a check comes from drill config, and the only safe
// assumption about drill config is that it is data.
//
// The verified images are Fedora-based and carry bash, curl, awk, sed and
// find, and carry neither tar nor psql nor python3 (measured). Every
// answer is therefore taken apart with the tools that are there, and the
// engine is spoken to over its HTTP endpoint rather than through a client
// the image does not ship.

const (
	// serverURL is where QuestDB answers inside its own sandbox. The
	// sandbox has no published ports, so this address is reachable from
	// nowhere else.
	serverURL = "http://127.0.0.1:9000"
	// dataRoot is where the official image keeps the server's data — the
	// directory a backup is a copy of.
	dataRoot = "/var/lib/questdb"
	// stagingDir is where the artifact lands before it becomes the data
	// root. It sits outside dataRoot so that emptying the data root cannot
	// touch what was just transferred.
	stagingDir = "/var/lib/probavi-questdb"
)

// idleScript answers whether the sandbox is idle — 1 when nothing is
// serving on the engine's port yet, 0 when something is.
//
// This adapter replaces the data root, so it needs the engine stopped when
// it starts: the sandbox must be created idle and the adapter starts the
// server itself. An operator who leaves the image's own entrypoint running
// gets a refusal that names the parameter to add, rather than a restore
// that fights a running server for its own files.
const idleScript = `curl -sf -o /dev/null --max-time 3 "` + serverURL + `/exec?query=select%201" && echo 0 || echo 1`

// placeScript makes the transferred artifact the server's data root.
//
// The transfer lands beside the data root rather than inside it, so the
// emptying below can be unconditional: everything the image shipped goes,
// and what the operator's backup holds — conf included — takes its place.
// Restoring a backup means running the configuration it was taken with.
const placeScript = `set -u
rm -rf "$1"/* "$1"/.[!.]* 2>/dev/null || true
mv "$2"/* "$1"/ 2>/dev/null || { echo "the artifact holds no files" >&2; exit 91; }
mv "$2"/.[!.]* "$1"/ 2>/dev/null || true
rmdir "$2" 2>/dev/null || true
ls -A "$1" | head -1`

// startScript starts the engine on the restored data root and returns
// immediately; readiness is polled separately.
//
// Telemetry is switched off for the drill (ADR 0018 in the core: no
// telemetry, no phone-home). The image ships `telemetry.enabled=true` as
// its default, and a restored copy of production data is the last place
// that should report anywhere.
//
// The image's own entrypoint is what starts the server, so the engine runs
// with the arguments its packagers chose and as the user they chose, and
// the adapter adds nothing but the environment above it.
const startScript = `set -u
export QDB_TELEMETRY_ENABLED=false
nohup /docker-entrypoint.sh >/tmp/probavi-questdb.log 2>&1 &
echo started`

// readyScript asks whether the engine answers a query yet.
const readyScript = `curl -sf -o /dev/null --max-time 5 "` + serverURL + `/exec?query=select%201"`

// tablesScript counts the tables the restored server serves. QuestDB's
// tables() lists what the operator created and not the engine's own
// (measured), so this is the number that says whether the restore
// produced anything a check can read.
const tablesScript = `set -u
curl -sf -G "` + serverURL + `/exp" --data-urlencode "query=select count(*) from tables()" |
  tail -n +2 | tr -d '"'`

// startupErrorScript surfaces what the engine said when it did not come
// up. The log is the adapter's own capture of the entrypoint's output.
const startupErrorScript = `grep -iE "error|exception|caused by" /tmp/probavi-questdb.log 2>/dev/null | head -3`

// runnerScript absorbs the check dialect declaratively.
//
// A check is one SQL statement — QuestDB speaks SQL, so the core's
// generating built-ins (table_exists, row_count, freshness) apply here
// unchanged, quoted identifiers included (measured). $1 is the statement.
//
// The CSV endpoint answers with a header line and then the rows, and
// quotes text values, so the header is dropped and the surrounding quotes
// with it: a check compares the value, not the engine's markup. A failed
// statement answers HTTP 400 with the engine's own words in a JSON body,
// which go to stderr while the exit code carries the verdict — the core
// records that a check failed and with what exit code, never the engine's
// diagnostic text (evidence redaction).
const runnerScript = `set -u
out=$(curl -s -w '\n%{http_code}' -G "` + serverURL + `/exp" --data-urlencode "query=$1") || {
  echo "the engine could not be reached" >&2; exit 1; }
code=${out##*$'\n'}
body=${out%$'\n'*}
[ "$code" = 200 ] || { printf '%s\n' "$body" >&2; exit 1; }
printf '%s\n' "$body" | tail -n +2 | sed 's/^"//; s/"$//'`
