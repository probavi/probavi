package main

// scripts.go holds the bash fragments the adapter runs inside the
// sandbox. Arguments travel as positional parameters, never interpolated
// into the scripts, and every fragment runs as the sandbox's exec
// identity — the same identity `neo4j start` then runs the server under,
// so the store the load writes is readable by the process that serves it.

// hostsScript makes the sandbox's own hostname resolve, and does nothing
// when it already does.
//
// Neo4j refuses to boot in a host that cannot resolve its own name. A
// zero-ingress sandbox is exactly that: `docker run --network none` gives
// the container a hostname but no address, so nothing is written to
// /etc/hosts for it and the JVM's own-host lookup fails. What the
// operator sees is "Configuration is invalid. See log for more info." —
// with nothing in neo4j.log, nothing in debug.log, and a configuration
// that `neo4j-admin server validate-config` calls valid. Measured: the
// same container starts once this line exists, and setting
// server.default_advertised_address=localhost instead does not help.
//
// The guard is what makes this safe to run everywhere. On a bare host
// (the remotehost provider) the name already resolves, so the file is
// never touched; only a runtime that gave itself a name it cannot look
// up gets the line, and that runtime is thrown away at teardown.
const hostsScript = `set -u
name=$(hostname) || exit 1
getent hosts "$name" >/dev/null 2>&1 && exit 0
printf '127.0.0.1 %s\n' "$name" >> /etc/hosts`

// toolScript verifies the toolchain every later step runs on. The
// engine's tools live in the sandbox image, so a wrong image is worth
// one clear sentence rather than three confusing failures.
const toolScript = `command -v neo4j >/dev/null && command -v neo4j-admin >/dev/null && command -v cypher-shell >/dev/null`

// passwordScript sets the initial password of the built-in neo4j user.
// It only takes effect before the server's first start, which is why the
// drill's sandbox must start idle. $1 password.
//
// The value is the documented sandbox constant, not a secret (see the
// sandboxPassword comment): the core's ephemeral per-drill secret cannot
// be used, because it would have to cross the protocol to reach the
// engine, and §2.5 forbids that. neo4j-admin accepts the password only
// as a positional argument — measured: stdin is refused with "Missing
// required parameter: '<password>'" — so a real secret would land in the
// sandbox's process list either way.
//
//nolint:gosec // G101 false positive: a shell fragment naming the command that sets the sandbox password, not a credential.
const passwordScript = `set -u
neo4j-admin dbms set-initial-password "$1" >/dev/null`

// loadScript loads one dump into a database that is not mounted. $1
// database, $2 directory holding <database>.dump.
//
// The tool derives the file name from the database name, so the adapter
// places the artifact as <database>.dump and the operator's file name
// never has to match anything. --overwrite-destination is deliberately
// absent: the drill's sandbox is fresh, so there is nothing to replace,
// and a store that somehow exists is a surprise worth failing on rather
// than deleting. Progress and errors both go to stderr (measured).
const loadScript = `set -u
neo4j-admin database load "$1" --from-path="$2"`

// infoScript asks the engine what the artifact is, without loading it:
// the archive's own metadata (database, format, file and byte counts).
// A file that is not a Neo4j archive fails here, before the drill spends
// a restore on it. $1 database, $2 directory.
const infoScript = `set -u
neo4j-admin database load "$1" --from-path="$2" --info`

// startScript starts the server and returns when the bootloader says it
// is up. That is not the same as ready — "There may be a short delay
// until the server is ready" is the tool's own wording, measured at
// about two seconds past this call — so a readiness poll still follows.
const startScript = `set -u
neo4j start >/dev/null`

// onlineScript asks the system database one yes/no question: is the
// database the dump was loaded into mounted and online? It answers 1 or
// 0, and fails outright while the server is not answering yet — which is
// what makes it the readiness poll as well as the gate. $1 database.
//
// It runs against the system database explicitly. A session against the
// default database would fail for a second reason — that database not
// being available — and the drill would then wait out its readiness
// budget instead of saying what is wrong. `RETURN 1` is not an option
// there: the system database refuses it ("This Cypher command can only
// be executed in a user database", measured), so the readiness question
// and the gate have to be the same system command.
//
// This is the drill's gate against a green that proves nothing. Neo4j
// Community mounts exactly the databases its system database knows, and
// a dump loaded under any other name lands on disk without ever being
// mounted (measured: /data/databases/orders exists, SHOW DATABASES never
// lists it, and a query against it answers "this database does not
// exist"). The server starts perfectly either way, so nothing but this
// question distinguishes a restored database from an empty one that
// happens to serve.
//
// The name reaches the query as a Cypher string literal. That is safe
// because databasePattern has already refused everything but lowercase
// letters, digits, dots and dashes — there is no quote to close and no
// backslash to escape with.
const onlineScript = `set -o pipefail
cypher-shell --format plain --non-interactive --access-mode read --database system \
  "SHOW DATABASES YIELD name, currentStatus WHERE name = '$1' AND currentStatus = 'online' RETURN count(*)" | tail -n +2`

// servedScript lists what the server does have, as `name, status` lines
// with the header and the quoting removed. It runs only when the gate
// above has already failed: a refusal that names the databases the
// engine actually serves turns a puzzling drill failure into an obvious
// one, and nothing else in the drill needs the listing.
const servedScript = `set -o pipefail
cypher-shell --format plain --non-interactive --access-mode read --database system \
  "SHOW DATABASES YIELD name, currentStatus RETURN name, currentStatus" | tail -n +2 | tr -d '"'`

// healthScript is the healthcheck operation: one trivial query through
// the client the checks use, against the restored database itself, so a
// database that stopped serving answers unhealthy. $1 database.
const healthScript = `set -o pipefail
cypher-shell --format plain --non-interactive --access-mode read \
  --database "$1" "RETURN 1 AS ok" | tail -n +2`

// runnerScript is the declared sql_runner (§6.1): it turns cypher-shell's
// output into the undecorated rows the core's contract requires. $1
// database, $2 the check's Cypher.
//
// cypher-shell has no undecorated mode. --format plain — its most
// minimal — still prints a header row, quotes string values with \" and
// \\ escapes, and joins columns with ", " (measured). The awk pass drops
// the header and scans each row honouring the quotes, so a value that
// itself contains ", " stays one column, and prints the fields
// tab-separated and unquoted. pipefail carries cypher-shell's own exit
// code through the pipe, which is what makes a failed query a failed
// check.
//
// --access-mode read is a property, not a precaution: a check may read
// what the drill restored and may not change it. A check that writes
// fails loudly (measured: "Writing in read access mode not allowed")
// instead of quietly altering the artifact the evidence record is about.
//
// The check text travels as one positional parameter after --, so a
// query beginning with a dash reaches Cypher as text rather than as an
// option (measured).
const runnerScript = `set -o pipefail
cypher-shell --format plain --non-interactive --access-mode read --database "$1" -- "$2" | awk 'NR==1{next}
{
  n=length($0); out=""; f=""; q=0; i=1
  while(i<=n){
    c=substr($0,i,1)
    if(q){
      if(c=="\\"&&i<n){ d=substr($0,i+1,1); if(d=="\""||d=="\\"){ f=f d; i+=2; continue } }
      if(c=="\""){ q=0; i++; continue }
      f=f c; i++; continue
    }
    if(c=="\""){ q=1; i++; continue }
    if(c==","&&substr($0,i+1,1)==" "){ out=out f "\t"; f=""; i+=2; continue }
    f=f c; i++
  }
  print out f
}'`
