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

// readyScript asks whether the REST endpoint answers a query yet.
//
// It is deliberately the later of the two readiness signals. The native
// client connects about 0.6 s after the container starts and the REST
// endpoint about 1.9 s after that (measured), and REST is the path every
// check takes — so a gate on the CLI would hand the drill a server its
// own checks cannot reach.
const readyScript = `curl -sf -o /dev/null -u ` + credentials +
	` -d "SHOW DATABASES" "` + serverURL + `/rest/sql"`

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
