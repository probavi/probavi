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
	// httpPort is where taosAdapter listens inside the sandbox.
	httpPort = "6041"
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
// Two names have to resolve before any of that, and in a zero-ingress
// sandbox neither is guaranteed to.
//
// **The container's own hostname.** Docker writes no line for it into
// /etc/hosts when the sandbox has no network, so the engine's startup
// lookup has nothing to answer it and no nameserver to ask: measured, the
// image's entrypoint stops at `taosd -C | grep dataDir` with that process
// alive and never returning, and nothing else in the container ever
// starts. Podman does write the line, which is why this took five rounds
// of CI to see — the development machine could not reproduce it.
//
// **The name the configuration names.** An image can carry one that means
// nothing here: 3.3.5.8 ships `fqdn buildkitsandbox`, the hostname of the
// machine that built the image, where 3.3.6.13 ships `localhost` (both
// measured). With that name unresolvable the engine refuses to start, in
// its own words:
//
//	failed to get ip from fqdn:buildkitsandbox since Resource temporarily
//	unavailable, dnode can not be initialized
//	failed to start since read config error
//
// So both names are mapped to loopback — only when /etc/hosts does not
// already carry them, so a host that resolves its own name is left alone
// — and the engine is pinned to loopback as well, which is the only
// address a sandbox with no network and no published ports can serve on.
// Measured under Docker with nothing else changed: the hostname line and
// these two variables take the engine from never starting to serving on
// the first poll, with a ready node on the second.
//
// taosd first, then the HTTP endpoint that every check speaks to: the
// second connects to the first, and starting them the other way round
// only makes it retry.
const engineEnv = `export TAOS_FQDN=localhost TAOS_FIRST_EP=localhost:6030
`

// The script waits for the engine inside itself, the way the sibling
// adapters that start an engine do, and exits non-zero with the engine's
// own last lines when it does not come up. Backgrounding the processes
// and returning immediately looked equivalent and was not: on CI's
// runtime the call that started them stayed open for twenty minutes,
// where the same script returned at once here (measured). A start that
// answers only when the engine is ready has nothing left to outlive it.
// Each half starts only if it is not already there, and what counts as
// "there" is deliberately different for the two. The image's entrypoint
// may have got one of them up and not the other — on 3.3.5.8 its taosd
// dies on the name baked into the image while its taosadapter keeps the
// HTTP port, and starting a second one made that one exit, which the
// drill then reported as an engine that would not start (measured on CI,
// where that combination is what the runner produces).
//
// For the server, a client that answers is the test. For the endpoint it
// is the port being bound, not a query succeeding: a taosadapter with no
// server behind it answers errors, and treating that as absent starts a
// second one that cannot bind.
const startScript = `set -u
for name in "$(hostname)" "$(sed -n 's/^fqdn *\([^ ]*\).*/\1/p' /etc/taos/taos.cfg | head -1)"; do
  [ -n "$name" ] || continue
  grep -qE "[[:space:]]$name([[:space:]]|$)" /etc/hosts && continue
  echo "127.0.0.1 $name" >> /etc/hosts 2>/dev/null || true
done
` + engineEnv + `
bound() { (exec 3<>/dev/tcp/127.0.0.1/` + httpPort + `) 2>/dev/null; }
if ! taos -s "show databases;" >/dev/null 2>&1; then
  nohup taosd > /tmp/probavi-taosd.log 2>&1 &
  tpid=$!
  i=0
  while [ $i -lt 60 ]; do
    kill -0 "$tpid" 2>/dev/null || { echo "taosd exited while starting:" >&2; tail -5 /tmp/probavi-taosd.log >&2; exit 1; }
    taos -s "show databases;" >/dev/null 2>&1 && break
    i=$((i+1)); sleep 1
  done
fi
if ! bound; then
  nohup taosadapter > /tmp/probavi-taosadapter.log 2>&1 &
  apid=$!
  i=0
  while [ $i -lt 60 ]; do
    bound && break
    kill -0 "$apid" 2>/dev/null || { echo "taosadapter exited while starting:" >&2; tail -5 /tmp/probavi-taosadapter.log >&2; exit 1; }
    i=$((i+1)); sleep 1
  done
fi
i=0
while [ $i -lt 60 ]; do
  n=$(curl -sf -u ` + credentials + ` -d "SELECT count(*) FROM information_schema.ins_dnodes WHERE status = 'ready'" "` + serverURL + `/rest/sql" | sed -n 's/.*"data":\[\[\([0-9]*\)\].*/\1/p')
  case "${n:-0}" in ''|0) ;; *) echo started; exit 0;; esac
  i=$((i+1)); sleep 1
done
echo "the engine reported no ready node within 60s:" >&2
tail -5 /tmp/probavi-taosd.log >&2
exit 1`

// startupErrorScript surfaces what the engine said when it never came up.
// The server and the HTTP endpoint are two processes with two logs, and a
// sandbox that answers neither is a question the operator cannot chase
// afterwards: the container is gone by the time they read the record.
const startupErrorScript = `set -u
for f in /tmp/probavi-taosd.log /tmp/probavi-taosadapter.log /var/log/taos/taosdlog.0 /var/log/taos/taosadapter_*.log; do
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
` + engineEnv + `
taosdump -i "$1" 2>&1`

// databaseScript answers whether the restored database exists and how
// many tables it serves, as one number.
const databaseScript = `set -u
curl -sf -u ` + credentials + ` -d "SELECT count(*) FROM information_schema.ins_tables WHERE db_name = '$1'" "` +
	serverURL + `/rest/sql" | sed -n 's/.*"data":\[\[\([0-9]*\)\].*/\1/p'`

// backtick is the identifier quote TDengine takes. It cannot be written
// inside the raw string literal below, which is delimited by one.
const backtick = "`"

// runnerScript unwraps the engine's JSON answer, and does the one thing
// no declaration can.
//
// It used to do more. The core composed its generating built-ins with
// SQL-standard quoted identifiers — `SELECT count(*) FROM "rig"."events"`
// — which TDengine refuses outright: error 9728, `syntax error near
// ""rig"."events""` (measured on 3.3.6.13). So table_exists and row_count
// failed on every drill while the README promised they worked (issue
// #276), and this script rewrote the quotes. **That rewriting is gone.**
// The adapter declares `identifier` instead (§6.1.1) and the core composes
// backticks itself.
//
// What went with it is worth naming, because it was the expensive part.
// The rewrite could not be positional: TDengine also accepts "a" as a
// string literal (measured), so a check of the operator's own carrying a
// double-quoted string would have been turned into an identifier — a
// different query, answering a different number, into a signed record. The
// guard was therefore a regular expression matching the *whole* statement
// against the grammar the core generates, which is an adapter recognising
// its own core's SQL in order to correct it. Declaring the dialect deletes
// the correction and the guard together, and with them that risk: nothing
// here rewrites a statement any more, so a user's `sql` check now reaches
// the engine byte for byte, which is what the README always promised.
//
// freshness still needs this script, and the reason is the one thing a
// static declaration cannot express. TDengine's max() refuses a TIMESTAMP
// argument — error 10242, `Invalid parameter data type : max` — with
// backticks exactly as with quotes (issue #293). The engine's own
// newest-value function is not the answer: last() reads the column in the
// row with the newest primary timestamp, which for any other TIMESTAMP
// column is not its maximum — measured, a second column holding
// 2026-09-16 12:00 and 2026-09-15 00:00 answered max 12:00 and last 00:00,
// and a freshness check reading the second would sign a different age than
// the one it names. max() over the column's integer form does keep the
// meaning, and casting the answer back gives the engine's own timestamp
// rendering, which the core parses: measured exact to the digit in
// databases of all three precisions (ms, us, ns).
//
// That cast is only right for a TIMESTAMP column, so the runner asks the
// engine what the column is before using it — and that lookup is why a
// declared statement could not replace this one. Cast unconditionally, a
// freshness check over an integer column would turn its number into an
// instant and could pass, where every other engine reports output the core
// cannot read as a time. The lookup needs the database the table lives in,
// which only a qualified name carries; an unqualified one is refused by the
// endpoint anyway (error 9750, `Database not specified`). Anything the
// lookup does not confirm — a tag rather than a column, a name the
// catalogue stores differently, a lookup that fails — leaves the statement
// exactly as the core composed it, and the engine's refusal stands. The
// three names are written into the lookup's string literals only because
// the pattern that captured them admits letters, digits and underscores and
// nothing else.
//
// $1 is the statement. The endpoint answers JSON: a scalar query comes back
// as data:[[value]], and a refusal as a non-zero code with the engine's own
// words in desc, which go to stderr while the exit code carries the verdict
// — the core records that a check failed and with what exit code, never the
// engine's diagnostic text.
const runnerScript = `set -u
name='[A-Za-z_][A-Za-z0-9_]*'
bt='` + backtick + `'
newest="^SELECT max\($bt($name)$bt\) FROM $bt($name)$bt\.$bt($name)$bt$"
stmt=$1
if [[ $1 =~ $newest ]]; then
  col=${BASH_REMATCH[1]} db=${BASH_REMATCH[2]} tbl=${BASH_REMATCH[3]}
  type=$(curl -sf -u ` + credentials + ` --data-binary "SELECT col_type FROM information_schema.ins_columns WHERE db_name = '$db' AND table_name = '$tbl' AND col_name = '$col'" "` +
	serverURL + `/rest/sql" | sed -n 's/.*"data":\[\["\([A-Z]*\)"\]\].*/\1/p')
  if [ "$type" = TIMESTAMP ]; then
    stmt="SELECT CAST(max(CAST($bt$col$bt AS BIGINT)) AS TIMESTAMP) FROM $bt$db$bt.$bt$tbl$bt"
  fi
fi
out=$(curl -s -w '\n%{http_code}' -u ` + credentials + ` --data-binary "$stmt" "` + serverURL + `/rest/sql") || {
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
