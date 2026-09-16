package main

// scripts.go holds the shell this adapter runs inside the sandbox.
//
// Arguments travel as positional parameters, never interpolated into the
// script text: a check comes from drill config, and the only safe
// assumption about drill config is that it is data. The one secret — the
// password of the restored engine's user — travels in the exec's
// environment and reaches curl on its standard input, so it is on no
// argument list inside the sandbox.
//
// Both verified images are Ubuntu 20.04 with bash 5.0, curl 7.68 and mawk
// 1.3.4, and no jq, no python and no JDK compiler (measured on 2.0.11 and
// 1.3.7). So the engine is read over its REST service with curl, and the
// JSON it answers is taken apart by the awk program below — the single
// JSON reader this adapter has, shared by the check runner and every
// verdict, so it is tested once.

// restPort is where the engine's REST service answers. The service is off
// by default and this adapter turns it on in the sandbox's copy of the
// configuration; nothing is published, and every read happens inside.
const restPort = "18080"

// passwordEnv is the variable the restored engine's password reaches a
// script in: the runner's from the core (sql_runner.env), every other
// script's from this adapter. Unset or empty means IoTDB's own default.
const passwordEnv = "PROBAVI_IOTDB_PASSWORD" //nolint:gosec // G101 false positive: the name of the variable a password arrives in, not a password

// jsonToTSV turns one REST answer into §6.1 rows: one line per row, columns
// separated by tabs, no header, no decoration.
//
// The CLI was measured and refused as the source of rows: its bordered
// table splits a value containing "|", trims the spaces around a value,
// and prints NULL and the string "null" alike. The REST service answers
// JSON that keeps all three, and this program keeps them in turn — a NULL
// becomes an empty field, a string is decoded exactly, \u escapes and
// surrogate pairs included, into UTF-8 bytes (awk runs under LC_ALL=C, so
// a byte is a byte in mawk and gawk alike).
//
// The two services answer in two shapes (measured): the tree dialect's
// /rest/v2/query is column-oriented — values[column][row], with the times
// in a separate "timestamps" array — and the table dialect's
// /rest/table/v1/query is row-oriented. A number is never converted: an
// INT64 past 2^53 would lose digits in awk's doubles, so every number stays
// the text the engine wrote. A timestamp — the tree dialect's time column,
// and any column the answer types TIMESTAMP — is rendered as RFC 3339 in
// UTC at the cluster's precision, by string arithmetic for the same reason:
// a nanosecond epoch does not fit a double exactly.
//
// Variables: mode is "tree" or "table"; prec is ms, us or ns. An error
// answer — an object with a code and no values — goes to stderr and exits
// 3; anything unparseable exits 4.
const jsonToTSV = `
function fail(msg) { printf "%s\n", msg > "/dev/stderr"; bad = 1; exit 4 }
function ws(   c) {
  while (pos <= n) { c = substr(s, pos, 1); if (c == " " || c == "\t" || c == "\n" || c == "\r") pos++; else break }
}
function hexval(h,   i, v, c) {
  v = 0
  for (i = 1; i <= 4; i++) { c = index("0123456789abcdef", tolower(substr(h, i, 1))); if (c == 0) fail("bad \\u escape"); v = v * 16 + c - 1 }
  return v
}
function utf8(cp) {
  if (cp < 128) return sprintf("%c", cp)
  if (cp < 2048) return sprintf("%c%c", 192 + int(cp / 64), 128 + cp % 64)
  if (cp < 65536) return sprintf("%c%c%c", 224 + int(cp / 4096), 128 + int(cp / 64) % 64, 128 + cp % 64)
  return sprintf("%c%c%c%c", 240 + int(cp / 262144), 128 + int(cp / 4096) % 64, 128 + int(cp / 64) % 64, 128 + cp % 64)
}
function str(   out, c, e, cp, lo) {
  pos++; out = ""
  while (pos <= n) {
    c = substr(s, pos, 1)
    if (c == "\"") { pos++; return out }
    if (c != "\\") { out = out c; pos++; continue }
    e = substr(s, pos + 1, 1)
    if (e == "n") out = out "\n"; else if (e == "t") out = out "\t"; else if (e == "r") out = out "\r"
    else if (e == "b") out = out "\b"; else if (e == "f") out = out "\f"
    else if (e == "u") {
      cp = hexval(substr(s, pos + 2, 4)); pos += 4
      if (cp >= 55296 && cp <= 56319 && substr(s, pos + 2, 2) == "\\u") {
        lo = hexval(substr(s, pos + 4, 4))
        if (lo >= 56320 && lo <= 57343) { cp = 65536 + (cp - 55296) * 1024 + (lo - 56320); pos += 6 }
      }
      out = out utf8(cp)
    } else out = out e
    pos += 2
  }
  fail("unterminated string")
}
function val(path,   c, i, key, start) {
  ws(); c = substr(s, pos, 1)
  if (c == "{") {
    pos++; ws()
    if (substr(s, pos, 1) == "}") { pos++; return }
    while (1) {
      ws(); if (substr(s, pos, 1) != "\"") fail("expected a key")
      key = str(); ws()
      if (substr(s, pos, 1) != ":") fail("expected a colon")
      pos++; val(path == "" ? key : path SUBSEP key); ws()
      c = substr(s, pos, 1); pos++
      if (c == "}") return
      if (c != ",") fail("expected a comma in an object")
    }
  }
  if (c == "[") {
    pos++; ws(); i = 0
    if (substr(s, pos, 1) == "]") { pos++; len[path] = 0; return }
    while (1) {
      val(path SUBSEP i); i++; ws()
      c = substr(s, pos, 1); pos++
      if (c == "]") { len[path] = i; return }
      if (c != ",") fail("expected a comma in an array")
    }
  }
  if (c == "\"") { v[path] = str(); t[path] = "s"; return }
  start = pos
  while (pos <= n && index(",]} \t\r\n", substr(s, pos, 1)) == 0) pos++
  if (pos == start) fail("expected a value")
  v[path] = substr(s, start, pos - start); t[path] = (v[path] == "null") ? "z" : "n"
}
function floordiv(a, b,   q) { q = int(a / b); if (q * b > a) q--; return q }
function stamp(raw,   digits, neg, whole, frac, sec, days, rem, z, era, doe, yoe, y, doy, mp, d, m) {
  digits = (prec == "ns") ? 9 : (prec == "us") ? 6 : 3
  neg = substr(raw, 1, 1) == "-"; if (neg) raw = substr(raw, 2)
  if (raw !~ /^[0-9]+$/) return raw
  while (length(raw) <= digits) raw = "0" raw
  whole = substr(raw, 1, length(raw) - digits) + 0
  frac = substr(raw, length(raw) - digits + 1)
  if (neg) { whole = -whole; if (frac + 0 > 0) { whole--; frac = sprintf("%0" digits "d", 10 ^ digits - frac) } }
  days = floordiv(whole, 86400); rem = whole - days * 86400
  z = days + 719468; era = floordiv(z, 146097); doe = z - era * 146097
  yoe = int((doe - int(doe / 1460) + int(doe / 36524) - int(doe / 146096)) / 365)
  y = yoe + era * 400; doy = doe - (365 * yoe + int(yoe / 4) - int(yoe / 100))
  mp = int((5 * doy + 2) / 153); d = doy - int((153 * mp + 2) / 5) + 1
  m = (mp < 10) ? mp + 3 : mp - 9; if (m <= 2) y++
  return sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%sZ", y, m, d, int(rem / 3600), int(rem / 60) % 60, rem % 60, frac)
}
function cell(path, type) {
  if (!(path in t) || t[path] == "z") return ""
  if (type == "TIMESTAMP" && t[path] == "n") return stamp(v[path])
  return v[path]
}
{ s = s $0 }
END {
  if (bad) exit 4
  n = length(s); pos = 1
  if (n == 0) fail("empty answer")
  val(""); ws()
  if (pos <= n) fail("trailing bytes after the answer")
  if (("code" in t) && !("values" in len)) { printf "%s: %s\n", v["code"], v["message"] > "/dev/stderr"; exit 3 }
  if (!("values" in len)) fail("the answer carries no values")
  if (mode == "table") {
    for (r = 0; r < len["values"]; r++) {
      line = ""
      for (c = 0; c < len["values" SUBSEP r]; c++) line = line (c ? "\t" : "") cell("values" SUBSEP r SUBSEP c, v["data_types" SUBSEP c])
      print line
    }
    exit 0
  }
  cols = len["values"]; timed = ("timestamps" in len) && len["timestamps"] > 0
  rows = timed ? len["timestamps"] : (cols ? len["values" SUBSEP 0] : 0)
  for (r = 0; r < rows; r++) {
    line = timed ? cell("timestamps" SUBSEP r, "TIMESTAMP") : ""
    for (c = 0; c < cols; c++) line = line ((timed || c) ? "\t" : "") cell("values" SUBSEP c SUBSEP r, v["data_types" SUBSEP c])
    print line
  }
}
`

// restFn is the shared client: iotdb_query <database> <sql> prints the
// answer as TSV and returns 0, or prints the engine's refusal on stderr and
// returns non-zero — 2 for a refused login, 3 for a refused statement, 4
// for a dialect the engine does not serve (1.3 has no table model, and its
// REST service answers that dialect's path with 404, measured), 1 for a
// service that did not answer.
//
// An empty database means the tree dialect and anything else the table
// dialect, run in that database: the tree dialect has no current database
// and the table dialect needs one for an unqualified name, so the one
// value says both. The user comes from IOTDB_USER, the password from
// ` + passwordEnv + `; curl reads both from its standard input as a config
// line, quoted by curl's own rules.
const restFn = `
iotdb_json_escape() {
  local x=$1
  x=${x//\\/\\\\}; x=${x//\"/\\\"}; x=${x//$'\n'/\\n}; x=${x//$'\r'/\\r}; x=${x//$'\t'/\\t}
  printf '%s' "$x"
}
iotdb_post() {
  local u=${IOTDB_USER:-root} p=${` + passwordEnv + `:-root}
  u=${u//\\/\\\\}; u=${u//\"/\\\"}; p=${p//\\/\\\\}; p=${p//\"/\\\"}
  printf 'user = "%s:%s"\n' "$u" "$p" |
    curl -s -K - -H 'Content-Type: application/json' --data-binary "$2" -w '\n%{http_code}' \
      "http://127.0.0.1:` + restPort + `$1"
}
iotdb_precision() {
  local out code body
  out=$(iotdb_post /rest/v2/query '{"sql":"show variables"}') || return 1
  code=${out##*$'\n'}; body=${out%$'\n'*}
  [ "$code" = 200 ] || return 1
  printf '%s' "$body" | LC_ALL=C awk -v mode=tree -v prec=ms "$IOTDB_JSON_TO_TSV" |
    awk -F '\t' '$1 == "TimestampPrecision" { print $2 }'
}
iotdb_query() {
  local db=$1 sql path body out code prec rc
  sql=$(iotdb_json_escape "$2")
  if [ -z "$db" ]; then
    path=/rest/v2/query; body="{\"sql\":\"$sql\"}"
  else
    path=/rest/table/v1/query; body="{\"database\":\"$(iotdb_json_escape "$db")\",\"sql\":\"$sql\"}"
  fi
  out=$(iotdb_post "$path" "$body") || { echo "the engine's REST service did not answer" >&2; return 1; }
  code=${out##*$'\n'}; out=${out%$'\n'*}
  case "$code" in
    200) ;;
    401) printf '%s\n' "$out" >&2; return 2 ;;
    404) echo "the engine serves no $path" >&2; return 4 ;;
    *) printf 'HTTP %s: %s\n' "$code" "$out" >&2; return 1 ;;
  esac
  prec=$(iotdb_precision); prec=${prec:-ms}
  printf '%s' "$out" | LC_ALL=C awk -v mode="$([ -z "$db" ] && echo tree || echo table)" -v prec="$prec" "$IOTDB_JSON_TO_TSV"
  rc=$?
  [ "$rc" = 0 ] && return 0
  [ "$rc" = 3 ] && return 3
  return 1
}
`

// jsonProgramVar binds the awk program into the scripts that use it. The
// program is data to bash, never parsed as shell: it sits in a quoted
// heredoc.
const jsonProgramVar = "IOTDB_JSON_TO_TSV=$(cat <<'IOTDB_AWK'\n" + jsonToTSV + "\nIOTDB_AWK\n)\n"

// runnerScript is the §6.1 check runner. $1 is the database a table-dialect
// check runs in (empty: the tree dialect), $2 the user, $3 the statement.
const runnerScript = `set -u
` + jsonProgramVar + restFn + `
IOTDB_USER=$2
iotdb_query "$1" "$3"`

// Exit codes the scripts below answer with, so a verdict never rests on
// parsing a message.
const (
	// exitNoData: no IoTDB data directory where one was expected.
	exitNoData = 3
	// exitAmbiguous: an archive holds more than one data directory.
	exitAmbiguous = 5
	// exitUnpack: tar could not read the archive.
	exitUnpack = 90
	// exitNotReady: the engine does not serve yet — polled again.
	exitNotReady = 1
	// exitLoginRefused: the engine refused the configured user.
	exitLoginRefused = 2
	// exitRestoredNothing: every series and table reads empty.
	exitRestoredNothing = 3
	// exitUndecodable: a page failed to decode on the full read.
	exitUndecodable = 4
	// exitTTLHidesAll: a TTL scope holds series or a table and reads no row.
	exitTTLHidesAll = 5
	// exitReadDisagrees: the full read counted other than the statistics.
	exitReadDisagrees = 6
	// exitNoSuchDatabase: options.database names no table-model database.
	exitNoSuchDatabase = 7
	// exitQueryFailed: a verdict query was refused for another reason.
	exitQueryFailed = 8
)

// prepareScript clears the adapter's root and makes it, and makes nothing
// inside it: `docker cp` copies a source directory *into* a destination
// that already exists rather than as it (the chroma measurement), so the
// staging directory is created by the transfer alone.
// $1 is the adapter's root.
const prepareScript = `set -eu
rm -rf "$1"
mkdir -p "$1"
`

// findDataRootFn locates an IoTDB data directory inside $1: the directory
// that holds both nodes' system property files. The backup tool's target
// holds a whole installation with the data under data/ (measured), and a
// plain copy of /iotdb/data holds it at the top; both are accepted, and so
// is one level of wrapping in an archive.
const findDataRootFn = `
iotdb_data_roots() {
  find "$1" -mindepth 0 -maxdepth 6 -type f -path '*/confignode/system/confignode-system.properties' 2>/dev/null |
    while IFS= read -r f; do
      d=${f%/confignode/system/confignode-system.properties}
      [ -f "$d/datanode/system/system.properties" ] && printf '%s\n' "$d"
    done
}
`

// placeDirScript moves a transferred data directory to where the engine
// will be pointed. $1 is the destination, $2 the staging copy.
const placeDirScript = `set -u
` + findDataRootFn + `
roots=$(iotdb_data_roots "$2")
[ -n "$roots" ] || exit 3
[ "$(printf '%s\n' "$roots" | wc -l)" = 1 ] || exit 5
mkdir -p "$(dirname "$1")" && mv "$roots" "$1"`

// placeTarScript unpacks an archive and moves the data directory inside it
// to where the engine will be pointed. The compression is read from the
// bytes host-side, never from the name. $1 is the destination, $2 the
// directory to unpack into, $3 the archive.
func placeTarScript(gzip bool) string {
	flags := "-xf"
	if gzip {
		flags = "-xzf"
	}
	return `set -u
` + findDataRootFn + `
mkdir -p "$2"
tar ` + flags + ` "$3" -C "$2" 2>&1 >/dev/null | tail -n 1 >&2
[ "${PIPESTATUS[0]}" = 0 ] || exit 90
rm -f "$3"
roots=$(iotdb_data_roots "$2")
[ -n "$roots" ] || exit 3
[ "$(printf '%s\n' "$roots" | wc -l)" = 1 ] || exit 5
mkdir -p "$(dirname "$1")" && mv "$roots" "$1"`
}

// propertiesScript prints what the restored copy records about the node it
// was taken from, and which engine the sandbox carries, as key=value lines:
// cn.* from the ConfigNode's system properties, dn.* from the DataNode's,
// engine.version and engine.home. $1 is the data directory.
const propertiesScript = `set -u
for pair in "cn:$1/confignode/system/confignode-system.properties" "dn:$1/datanode/system/system.properties"; do
  prefix=${pair%%:*}; file=${pair#*:}
  grep -E '^[a-z_]+=' "$file" | sed "s/^/$prefix./"
done
home=${IOTDB_HOME:-}
if [ -z "$home" ]; then
  start=$(command -v start-datanode.sh 2>/dev/null) && home=$(cd "$(dirname "$start")/.." && pwd)
fi
printf 'engine.home=%s\n' "$home"
jar=$(ls "$home"/lib/iotdb-server-*.jar 2>/dev/null | head -n 1)
jar=${jar##*/iotdb-server-}; printf 'engine.version=%s\n' "${jar%.jar}"`

// configureScript writes the sandbox's own copy of the engine's
// configuration and leaves the image's untouched.
//
// Every path the engine writes is pointed under the adapter's root: the
// data directories by absolute value, because the ConfigNode resolves a
// relative one against its install directory whatever CONFIGNODE_DATA_HOME
// says (measured: a relocated start with only the environment set wrote a
// fresh cluster into /iotdb/data and the DataNode was refused as unknown).
//
// Three settings exist for the drill rather than the operator:
//   - the REST service, which every check and verdict reads through;
//   - wal_buffer_size_in_byte at 4 MiB. Every data region reserves this
//     much direct memory, and at the default 32 MiB a copy of four regions
//     does not start at 1 GiB — the DataNode exits during startup — while
//     with 4 MiB it starts in 4.8 s (measured). It sizes a write buffer, and
//     a drill writes nothing;
//   - the REST row limit, raised so a verdict read one row per device does
//     not stop at 10,000 devices. Past the limit the service refuses the
//     query (708) rather than truncating it (measured), so the limit can
//     only ever fail a drill, never pass one.
//
// Memory is set from the sandbox's cgroup limit, half to the DataNode and
// three tenths to the ConfigNode — the fractions 2.0.11 computes for
// itself. 1.3.7 sizes both heaps from the host instead (-Xmx12724M for the
// DataNode inside a 2 GiB container, measured), and its scripts assign
// MEMORY_SIZE unconditionally, so the variable has to be written into the
// copied files rather than exported.
//
// Each setting replaces the line the file already has, rather than being
// appended after it: the start scripts refuse a file that names a key
// twice ("Duplicate cn_internal_port entries found", measured).
//
// $1 is the adapter's root, $2 the engine's install directory, $3 the data
// directory; every further argument is a key=value setting — the node
// addresses and ports the copy records, which the engine refuses to start
// without (measured). Values are validated before they get here.
const configureScript = `set -eu
root=$1 home=$2 data=$3
shift 3
conf=$root/conf
rm -rf "$conf" && mkdir -p "$conf" "$root/logs"
cp -a "$home/conf/." "$conf/"
{
  printf 'enable_rest_service=true\nrest_service_port=` + restPort + `\n'
  printf 'rest_query_default_row_size_limit=1000000\n'
  printf 'wal_buffer_size_in_byte=4194304\n'
  printf 'cn_system_dir=%s\n' "$data/confignode/system"
  printf 'cn_consensus_dir=%s\n' "$data/confignode/consensus"
  printf 'cn_pipe_receiver_file_dir=%s\n' "$data/confignode/system/pipe/receiver"
  printf 'dn_system_dir=%s\n' "$data/datanode/system"
  printf 'dn_data_dirs=%s\n' "$data/datanode/data"
  printf 'dn_consensus_dir=%s\n' "$data/datanode/consensus"
  printf 'dn_wal_dirs=%s\n' "$data/datanode/wal"
  printf 'dn_tracing_dir=%s\n' "$root/tracing"
  printf 'dn_sync_dir=%s\n' "$data/datanode/sync"
  printf 'sort_tmp_dir=%s\n' "$data/datanode/tmp"
  printf 'dn_pipe_receiver_file_dirs=%s\n' "$data/datanode/system/pipe/receiver"
  for line in "$@"; do printf '%s\n' "$line"; done
} > "$root/overrides"
awk -F '=' 'FNR == NR { want[$1] = $0; order[++n] = $1; next }
  /^[a-z_]+=/ && ($1 in want) { if (!done[$1]++) print want[$1]; next }
  { print }
  END { for (i = 1; i <= n; i++) if (!done[order[i]]++) print want[order[i]] }' \
  "$root/overrides" "$conf/iotdb-system.properties" > "$conf/iotdb-system.properties.new"
mv "$conf/iotdb-system.properties.new" "$conf/iotdb-system.properties"
limit=
if [ -r /sys/fs/cgroup/memory.max ]; then limit=$(cat /sys/fs/cgroup/memory.max)
elif [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then limit=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)
fi
case "$limit" in ''|max|*[!0-9]*) ;; *)
  mb=$((limit / 1048576))
  if [ "$mb" -lt 1048576 ]; then
    sed -i "s/^MEMORY_SIZE=.*/MEMORY_SIZE=$((mb / 2))M/" "$conf/datanode-env.sh"
    sed -i "s/^MEMORY_SIZE=.*/MEMORY_SIZE=$((mb * 3 / 10))M/" "$conf/confignode-env.sh"
  fi ;;
esac
`

// hostsScript maps the host names the copy records to loopback, so the
// engine can bind the addresses it was configured with. A copy taken from a
// node addressed by name starts only when the sandbox names the same host
// and the name resolves to loopback — either half alone gave no answer in
// 150 s (measured) — and a sandbox with no network resolves no name it is
// not told. Names already present are left alone. $@ are the names.
const hostsScript = `set -u
for name in "$@"; do
  grep -qE "[[:space:]]$name([[:space:]]|$)" /etc/hosts && continue
  printf '127.0.0.1 %s\n' "$name" >> /etc/hosts || exit 1
done`

// startScript starts both nodes on the sandbox's configuration and returns
// at once. Their output goes to files and their standard streams are
// detached, because the exec that starts them has to return while they
// keep running. $1 is the adapter's root.
const startScript = `set -u
root=$1
export IOTDB_CONF=$root/conf CONFIGNODE_CONF=$root/conf IOTDB_LOG_DIR=$root/logs CONFIGNODE_LOGS=$root/logs
nohup start-confignode.sh >"$root/logs/confignode.out" 2>&1 </dev/null &
nohup start-datanode.sh >"$root/logs/datanode.out" 2>&1 </dev/null &
echo started`

// readyScript answers whether the restored engine serves: both nodes
// Running and every region Running, in both dialects where the engine has
// two. The nodes alone are not enough: both reported Running while a region
// could not be created for want of direct memory, and every write to it was
// refused with 906 "no available DataRegionGroups" (measured), so a copy
// whose regions do not come up is not called ready.
const readyScript = `set -u
` + jsonProgramVar + restFn + `
cluster=$(iotdb_query "" "show cluster" 2>/dev/null); rc=$?
[ "$rc" = 2 ] && exit 2
[ "$rc" = 0 ] || exit 1
[ "$(printf '%s\n' "$cluster" | grep -c .)" -ge 2 ] || exit 1
printf '%s\n' "$cluster" | awk -F '\t' '$3 != "Running" { bad = 1 } END { exit bad }' || exit 1
regions=$(iotdb_query "" "show regions" 2>/dev/null) || exit 1
tables=$(iotdb_query information_schema "show regions" 2>/dev/null); rc=$?
[ "$rc" = 0 ] || [ "$rc" = 4 ] || exit 1
printf '%s\n%s\n' "$regions" "$tables" | awk -F '\t' 'NF > 2 && $3 != "Running" { bad = 1 } END { exit bad }' || exit 1
exit 0`

// healthScript is the healthcheck: the engine answers an authenticated
// query. Region state was proved at provision; this asks only whether it
// still serves.
const healthScript = `set -u
` + jsonProgramVar + restFn + `
iotdb_query "" "show version" >/dev/null`

// startupErrorScript surfaces what the engine said when it never came up:
// the drill's sandbox is gone by the time anyone reads the record, so the
// engine's own last error has to travel in it. $1 is the adapter's root.
const startupErrorScript = `set -u
for f in "$1"/logs/log_confignode_error.log "$1"/logs/log_datanode_error.log "$1"/logs/log_confignode_all.log "$1"/logs/log_datanode_all.log; do
  [ -f "$f" ] || continue
  grep -E "ERROR|Exception:" "$f" 2>/dev/null | grep -v '^[[:space:]]*at ' | tail -n 1 | cut -c1-400
done | tail -n 2`

// verdictScript decides whether the restore produced a database worth
// anything, and prints how many values the full read decoded.
//
// It refuses three things, in this order:
//
// A TTL that hides a whole scope. TTL travels in the copy and is enforced
// when a query runs — rows backed up inside a five-minute TTL read back 14
// of 120 two and a half minutes later, and none 24 seconds after that
// (measured) — and there is no switch that suspends it: ttl_check_interval
// schedules only the physical deletion, and unsetting a TTL is rewriting a
// policy a check is entitled to read. So the drill is fenced where every
// row a TTL covers is hidden: a scope that holds series, or a table, and
// reads no row.
//
// A copy that restores nothing: every database reads empty.
//
// A read the engine cannot do. Counts prove nothing here: count(*) is
// answered from chunk statistics, and twelve single-byte changes to a data
// file all loaded as success with count and sum identical to the source
// (measured). count(cast(... as TEXT)) has to decode every value to answer,
// and it failed on every change that broke a page (724) — so both are
// asked, and the verdict is that the second answers at all and agrees with
// the first. A change that still decodes to a different value is invisible
// to any read the engine can do; the README says so.
//
// A tree database is read one row per device; a table one query per table,
// every column. A read the engine refuses because a page or a file's
// metadata does not decode is damage wherever in the script it surfaces: a
// truncated data file fails the statistics count itself, before the full
// read is asked. The two lines say it differently (both measured): 2.0.11
// answers 724 ("Failed to decode page data", "Failed to read timeseries
// metadata") or 305, and 1.3.7 answers the generic 301 carrying
// java.nio.BufferUnderflowException or "Error happened while scanning the
// file". Only those are damage; any other refusal is reported as the read
// failing, without blaming the backup. Output: the number of values
// decoded, on success.
const verdictScript = `set -u -o pipefail
` + jsonProgramVar + restFn + `
err=$(mktemp)
q() { iotdb_query "$@" 2>"$err"; }
refused() {
  if grep -qE '^(724|305):|^301: .*(BufferUnderflowException|Error happened while scanning the file)' "$err"; then
    printf '%s\n' "$1: $(head -c 300 "$err")" >&2; exit 4
  fi
  printf '%s\n' "$1: $(head -c 400 "$err")" >&2; exit 8
}
sumcols() { awk -F '\t' '{ for (i = 2; i <= NF; i++) if ($i ~ /^[0-9]+$/) s += $i } END { printf "%.0f\n", s + 0 }'; }

treedbs=$(q "" "show databases") || refused "list the tree databases"
tabledbs=$(q information_schema "show databases"); rc=$?
if [ "$rc" = 4 ]; then tabledbs=; elif [ "$rc" != 0 ]; then refused "list the table databases"; fi
tabledbs=$(printf '%s\n' "$tabledbs" | awk -F '\t' 'NF && $1 != "information_schema" { print $1 }')

if [ -n "${IOTDB_DATABASE:-}" ]; then
  printf '%s\n' "$tabledbs" | grep -qxF "$IOTDB_DATABASE" || { printf '%s\n' "$tabledbs" | tr '\n' ' ' >&2; exit 7; }
fi

ttls=$(q "" "show all ttl") || refused "list the TTLs"
while IFS=$'\t' read -r scope ttl; do
  case "$ttl" in ''|INF|*[!0-9]*) continue ;; esac
  series=$(q "" "count timeseries $scope") || refused "count the series under $scope"
  [ "${series:-0}" -gt 0 ] 2>/dev/null || continue
  rows=$(q "" "select count(*) from $scope" | sed 's/^/x\t/' | sumcols) || refused "count the rows under $scope"
  [ "$rows" = 0 ] && { printf 'tree\t%s\t%s\n' "$scope" "$ttl" >&2; exit 5; }
done <<< "$ttls"

total=0
for db in $(printf '%s\n' "$treedbs" | cut -f1); do
  stats=$(q "" "select count(*) from $db.** align by device") || refused "count $db"
  read_=$(q "" "select count(cast(* as TEXT)) from $db.** align by device") || refused "read every value of $db"
  [ "$stats" = "$read_" ] || { printf '%s\n' "$db" >&2; exit 6; }
  total=$((total + $(printf '%s\n' "$read_" | sumcols)))
done

for db in $tabledbs; do
  tables=$(q "$db" "show tables details") || refused "list the tables of $db"
  while IFS=$'\t' read -r table ttl _; do
    [ -n "$table" ] || continue
    ident=\"${table//\"/\"\"}\"
    cols=$(q "$db" "desc $ident") || refused "describe $db.$table"
    counts= casts=
    while IFS=$'\t' read -r col _ category; do
      [ -n "$col" ] && [ "$category" != TIME ] || continue
      c=\"${col//\"/\"\"}\"
      counts="$counts${counts:+, }count($c)"; casts="$casts${casts:+, }count(cast($c as STRING))"
    done <<< "$cols"
    [ -n "$counts" ] || continue
    rows=$(q "$db" "select count(*) from $ident") || refused "count $db.$table"
    case "$ttl" in ''|INF|*[!0-9]*) ;; *) [ "$rows" = 0 ] && { printf 'table\t%s.%s\t%s\n' "$db" "$table" "$ttl" >&2; exit 5; } ;; esac
    stats=$(q "$db" "select $counts from $ident") || refused "count the columns of $db.$table"
    read_=$(q "$db" "select $casts from $ident") || refused "read every value of $db.$table"
    [ "$stats" = "$read_" ] || { printf '%s.%s\n' "$db" "$table" >&2; exit 6; }
    total=$((total + $(printf 'x\t%s\n' "$read_" | sumcols)))
  done <<< "$tables"
done

[ "$total" -gt 0 ] || exit 3
printf '%s\n' "$total"`
