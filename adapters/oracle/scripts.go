package main

// scripts.go holds the bash fragments the adapter runs inside the
// sandbox. Arguments travel as positional parameters, never interpolated
// into the scripts; every SQL*Plus session connects as SYSDBA through the
// bequeath adapter (`/ as sysdba`), which is local IPC — no listener, no
// password, no TCP endpoint anywhere in the drill.

// toolScript verifies the toolchain every later step runs on.
const toolScript = `command -v sqlplus >/dev/null && command -v impdp >/dev/null && [ -n "${ORACLE_HOME:-}" ] && [ -n "${ORACLE_SID:-}" ]`

// startScript starts the image's prebuilt instance from a parameter file
// of its own: the spfile the image ships stays untouched, and the drill's
// pins layer on top of it at launch — before any background process
// exists to act on a default.
//
//   - dispatchers "" and shared_servers 0: without the listener (never
//     started) and without shared-server dispatchers the instance holds
//     no listening TCP socket at all (measured: `ss -ltn` empty). The
//     sandbox must join a network for the instance to start — ksipc
//     refuses a loopback-only host with ORA-00600 [ksipc: no private ips
//     avail for use], measured, and no parameter changes that — so this
//     is how zero ingress is restored: nothing listens.
//   - job_queue_processes 0: the scheduler's job coordinator never runs,
//     so DBMS_SCHEDULER jobs travelling in the dump stay ENABLED in the
//     dictionary (suspended, never rewritten) and never fire (measured: a
//     purge job arriving with the dump deleted every imported row before
//     the first read; with the pin, every row stays and the job reads
//     ENABLED with zero runs).
//   - aq_tm_processes 0: the queue time manager, which expires messages
//     in queue tables, does not run either.
//
// SQL*Plus startup is synchronous — the call returns when the database is
// open or prints why not — so there is no readiness poll.
const startScript = `set -u
work=$1
mkdir -p "$work" || exit 1
printf 'spfile=%s/dbs/spfile%s.ora\ndispatchers=""\nshared_servers=0\njob_queue_processes=0\naq_tm_processes=0\n' "$ORACLE_HOME" "$ORACLE_SID" > "$work/init.ora" || exit 1
sqlplus -S -L / as sysdba <<EOF
whenever sqlerror exit 1
startup pfile='$work/init.ora'
alter pluggable database all open;
EOF`

// identityScript reads what the started instance is, in one session:
// its release, the pluggable databases open for writing, and the two
// pins read back through the engine — a pin that did not take is refused
// rather than assumed (adapter-development skill: never rest on a
// default).
const identityScript = `set -u
sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
set heading off feedback off pagesize 0 linesize 200
select 'version=' || version_full from v$instance;
select 'pdb=' || name from v$pdbs where name <> 'PDB$SEED' and open_mode = 'READ WRITE' order by name;
select name || '=' || value from v$parameter where name in ('job_queue_processes', 'aq_tm_processes');
EOF`

// headerScript creates the directory object the import reads from and
// asks the engine what the file is, through DBMS_DATAPUMP's documented
// header reader. $1 pluggable database, $2 directory path, $3 file name.
// A file the engine cannot read as a dump raises ORA-39211 (measured on
// a truncated file and on random bytes), which exits the session
// non-zero with the engine's words on stdout.
const headerScript = `set -u
pdb=$1; dir=$2; dump=$3
export ORACLE_PDB_SID="$pdb" NLS_LANG=.AL32UTF8
sqlplus -S -L / as sysdba <<EOF
whenever sqlerror exit 1
set serveroutput on size unlimited feedback off
create or replace directory ` + directoryName + ` as '$dir';
declare
  info ku\$_dumpfile_info;
  ft number;
begin
  dbms_datapump.get_dumpfile_info(filename => '$dump', directory => '` + directoryName + `', info_table => info, filetype => ft);
  dbms_output.put_line('filetype=' || ft);
  for i in 1..info.count loop
    dbms_output.put_line('item' || info(i).item_code || '=' || info(i).value);
  end loop;
end;
/
EOF`

// importScript runs the import under a watchdog. $1 pluggable database,
// $2 file name, $3 log path, $4 grace seconds.
//
// impdp's exit code is the verdict: 0 success, 5 completed with errors,
// 1 a file it could not open (measured: truncated → ORA-27046, random
// bytes → ORA-39411, both within seconds). One failure mode never
// returns: a dump damaged in the middle makes the worker die loading
// the master table — impdp prints ORA-39776/ORA-39376 and then waits
// forever on a job whose state reads UNDEFINED (measured, over ten
// minutes). So the job's own state is polled while the client runs: a
// job that is neither defining nor executing for the whole grace period
// while its client still waits is dead, and the client is killed with
// exit 125. A healthy job is EXECUTING until it completes, after which
// the client leaves within a second.
//
// The output is reduced to the lines that carry verdicts (ORA- lines,
// the job's own completion line) so a large import's per-object log
// cannot outgrow a protocol frame.
const importScript = `set -u
pdb=$1; dump=$2; log=$3; grace=$4
export ORACLE_PDB_SID="$pdb" NLS_LANG=.AL32UTF8
impdp \"/ as sysdba\" directory=` + directoryName + ` dumpfile="$dump" logfile=` + importLogName + ` job_name=` + jobName + ` > "$log" 2>&1 &
pid=$!
idle=0
while kill -0 "$pid" 2>/dev/null; do
  sleep 2
  state=$(sqlplus -S -L / as sysdba <<'EOF'
set heading off feedback off pagesize 0
select state from dba_datapump_jobs where owner_name = 'SYS' and job_name = '` + jobName + `';
EOF
)
  case $state in
    *EXECUTING*|*DEFINING*) idle=0 ;;
    *) idle=$((idle + 2)) ;;
  esac
  if [ "$idle" -ge "$grace" ]; then
    kill "$pid" 2>/dev/null; sleep 1; kill -9 "$pid" 2>/dev/null
    grep -E 'ORA-|^Job ' "$log" | head -n 200
    exit 125
  fi
done
wait "$pid"; rc=$?
grep -E 'ORA-|^Job ' "$log" | head -n 200
exit $rc`

// runnerScript absorbs the check dialect declaratively: the check text
// is one SQL statement, written to a script SQL*Plus runs in the
// pluggable database ORACLE_PDB_SID names (the probe's env carries the
// {{database}} placeholder there) with the session shaped for the
// protocol's output contract — no heading, no feedback, CSV markup with a
// tab delimiter and no quoting, so rows are undecorated tab-separated
// values (measured: no padding, NULL empty, numbers unformatted to forty
// digits), and the session's NLS formats render DATE, TIMESTAMP and
// TIMESTAMP WITH TIME ZONE in the forms the core's freshness check
// parses. A trailing semicolon is stripped so the closing slash runs the
// statement exactly once; `define off` keeps an ampersand in the text
// literal. A failed statement exits non-zero with the engine's ORA- line
// on stderr. Nothing is interpolated: the text reaches the engine as the
// string it was, measured against shell and SQL metacharacters.
const runnerScript = `set -u
s=$1
s=${s%"${s##*[![:space:]]}"}
s=${s%;}
f=$(mktemp /tmp/probavi-check.XXXXXX) || exit 2
{
  printf 'whenever sqlerror exit 1\nwhenever oserror exit 2\n'
  printf 'set heading off feedback off pagesize 0 linesize 32767 trimout on trimspool on tab off define off verify off echo off sqlblanklines on long 1000000 longchunksize 1000000 numwidth 40\n'
  printf 'set markup csv on quote off delimiter "\t"\n'
  printf "alter session set nls_date_format='YYYY-MM-DD HH24:MI:SS' nls_timestamp_format='YYYY-MM-DD HH24:MI:SS.FF6' nls_timestamp_tz_format='YYYY-MM-DD HH24:MI:SS.FF6TZH:TZM';\n"
  printf '%s\n/\n' "$s"
} > "$f"
out=$(sqlplus -S -L / as sysdba "@$f"); rc=$?
rm -f "$f"
if [ "$rc" -ne 0 ]; then
  printf '%s\n' "$out" | grep -m1 -E 'ORA-|SP2-|PLS-' >&2 || printf '%s\n' "$out" >&2
  exit 1
fi
printf '%s\n' "$out"`

// healthScript asks the pluggable database one question. $1 pluggable
// database.
const healthScript = `set -u
export ORACLE_PDB_SID="$1"
sqlplus -S -L / as sysdba <<'EOF'
whenever sqlerror exit 1
set heading off feedback off pagesize 0
select 1 from dual;
EOF`
