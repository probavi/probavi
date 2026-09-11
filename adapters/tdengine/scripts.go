package main

// scripts.go holds the shell the adapter runs inside the sandbox.
//
// Arguments travel as positional parameters, never interpolated into the
// script text: a check comes from drill config, and the only safe
// assumption about drill config is that it is data.
//
// The verified images are Ubuntu-based and carry bash, curl, tar and the
// engine's own tools — taosd, taosadapter, taos and taosdump (measured) —
// so the adapter drives the vendor's restore tool and speaks to the
// server over the HTTP endpoint the checks use.

const (
	// serverURL is where taosAdapter answers inside the sandbox. Nothing
	// is published: checks run in-sandbox through the runner below.
	serverURL = "http://127.0.0.1:6041"
	// credentials are the image's own defaults. The sandbox has no
	// network and no published ports, so they reach nothing else; a drill
	// against an image with different ones is out of scope for this
	// version rather than silently wrong.
	credentials = "root:taosdata" //nolint:gosec // G101: the image's own documented default, inside a sandbox with no network and no published ports
	// stagingDir is where a directory artifact lands inside the sandbox,
	// and where an archive is unpacked to.
	stagingDir = "/tmp/probavi-tdengine"
	// archivePath is where an archive artifact lands — a file path, since
	// put_file copies one file and a directory cannot share its name.
	archivePath = "/tmp/probavi-tdengine.tar"
)

// readyScript answers how many of the cluster's own nodes are ready, over
// the endpoint the checks use.
//
// It asks that rather than whether a query answers, because the two are
// not the same instant and the difference is a failed restore. Measured
// on a freshly started engine: `SHOW DATABASES` answers and
// `SERVER_STATUS()` reads 1 at t+338 ms, while `CREATE DATABASE` — the
// first thing any restore does — still fails with "Out of dnodes" (error
// 820); the dnode reports itself ready at t+1006 ms, which is exactly
// when the create succeeds. A gate on the earlier signal hands the drill
// a server that cannot yet do the work.
const readyScript = `set -u
curl -sf -u ` + credentials + ` -d "SELECT count(*) FROM information_schema.ins_dnodes WHERE status = 'ready'" "` +
	serverURL + `/rest/sql" | sed -n 's/.*"data":\[\[\([0-9]*\)\].*/\1/p'`

// startScript starts the engine the image ships, and answers when its own
// endpoint does.
//
// The adapter starts it rather than leaving it to the image's entrypoint,
// because that entrypoint does not always finish. Measured on both
// verified images, on GitHub's runners, twice: the container's trace stops
// at the line where it reads its data directory —
//
//	++ taosd -C
//	++ grep -E 'dataDir\s+(\S+)' -o
//	++ head -n1
//
// — with only that config-dump process alive, and nothing else ever
// starts. The same image starts in 0.6 s on the development machine, so
// whatever the pipeline waits for is the host's, not the backup's, and a
// drill has no business depending on it.
//
// taosd first, then the HTTP endpoint that every check speaks to: the
// second connects to the first, and starting them the other way round
// only makes it retry.
const startScript = `set -u
nohup taosd >/tmp/probavi-taosd.log 2>&1 &
for i in $(seq 1 40); do
  taos -s "show databases;" >/dev/null 2>&1 && break
  sleep 0.5
done
nohup taosadapter >/tmp/probavi-taosadapter.log 2>&1 &
echo started`

// runningScript answers whether the engine is already up — 1 when the
// server process is there, 0 when the sandbox is idle. A sandbox whose
// entrypoint did finish is left alone rather than started twice.
const runningScript = `ps -eo comm 2>/dev/null | grep -qx taosd && echo 1 || echo 0`

// startupErrorScript surfaces what the engine said when it never came up.
// The server and the HTTP endpoint are two processes with two logs, and a
// sandbox that answers neither is a question the operator cannot chase
// afterwards: the container is gone by the time they read the record.
const startupErrorScript = `set -u
for f in /var/log/taos/taosdlog.0 /var/log/taos/taosadapter_*.log /tmp/probavi-taosd.log /tmp/probavi-taosadapter.log; do
  [ -f "$f" ] || continue
  grep -iE "error|fail|cannot|refus|unable" "$f" 2>/dev/null | tail -2
done
ps -eo comm 2>/dev/null | sort -u | grep -i taos | tr '\n' ' '`

// extractScript unpacks a tar artifact and answers with the directory
// the restore tool has to be pointed at: the one holding the schema,
// wherever the archive happens to nest it. $1 is the destination, $2 the
// archive.
const extractScript = `set -u
rm -rf "$1"
mkdir -p "$1"
tar -xf "$2" -C "$1" || exit 90
find "$1" -maxdepth 3 -name dbs.sql -printf '%h\n' |
  while read -r d; do grep -qi "CREATE DATABASE" "$d/dbs.sql" && echo "$d"; done | sort | head -1`

// restoreScript runs the vendor's restore tool and hands back everything
// it said, because what it says is the only thing worth reading.
//
// taosdump exits 0 whatever happens (measured, three ways): pointed at
// the directory `-o` was given it restores nothing, with a damaged avro
// file it restores what it can and prints a failure line, and asked
// before the server is ready it prints "Retry to connect" and stops.
// So the script reports the tool's own words and the caller judges them
// against what the artifact says it holds.
const restoreScript = `set -u
taosdump -i "$1" 2>&1`

// databaseScript answers whether the restored database exists and how
// many tables it serves, as one number.
const databaseScript = `set -u
curl -sf -u ` + credentials + ` -d "SELECT count(*) FROM information_schema.ins_tables WHERE db_name = '$1'" "` +
	serverURL + `/rest/sql" | sed -n 's/.*"data":\[\[\([0-9]*\)\].*/\1/p'`

// runnerScript absorbs the check dialect declaratively.
//
// TDengine speaks SQL, so the core's generating built-ins apply here
// unchanged. $1 is the statement. The endpoint answers JSON: a scalar
// query comes back as data:[[value]], and a refusal as a non-zero code
// with the engine's own words in desc, which go to stderr while the exit
// code carries the verdict — the core records that a check failed and
// with what exit code, never the engine's diagnostic text.
const runnerScript = `set -u
out=$(curl -s -w '\n%{http_code}' -u ` + credentials + ` --data-binary "$1" "` + serverURL + `/rest/sql") || {
  echo "the engine could not be reached" >&2; exit 1; }
code=${out##*$'\n'}
body=${out%$'\n'*}
[ "$code" = 200 ] || { printf '%s\n' "$body" >&2; exit 1; }
case "$body" in
  *'"code":0'*) ;;
  *) printf '%s\n' "$body" >&2; exit 1;;
esac
printf '%s\n' "$body" |
  sed -n 's/.*"data":\[\[\(.*\)\]\].*/\1/p' |
  sed 's/^"//; s/"$//; s/","/\t/g'`
