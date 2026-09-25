package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// binlog.go replays binary logs onto a restored physical full, which is
// what turns the xtrabackup kind's "the instant the backup was taken" into
// "any instant the archive still covers".
//
// A full backup proves one moment. Everything written after it — the hours
// an incident actually spans — is outside the drill unless the logs that
// recorded it are replayed too, and a recovery run-book that ends at the
// full is not the run-book anyone follows at 3am.
//
// The coordinate comes from the backup itself. `xtrabackup --backup`
// writes xtrabackup_binlog_info naming the log file and the position the
// server was at when the backup completed, so the replay starts exactly
// where the full stops — no overlap to re-apply and no gap to guess at.
//
// # What this deliberately does not do
//
// The replay is positional, not GTID-based. xtrabackup_binlog_info may
// carry a GTID set as a third field and this adapter ignores it: a
// GTID-based replay asks the server to skip what it already has, which is
// a different guarantee needing a different proof, and mixing the two
// would leave a record that does not say which one it rested on.

const binlogInfoFile = "xtrabackup_binlog_info"

// binlogNamePattern is how a server names a log in the series: a base name
// and a zero-padded ordinal. Anything else in the directory — the index
// file, a checksum sidecar, a README — is not a log to replay.
var binlogNamePattern = regexp.MustCompile(`^(.+)\.(\d{6,})$`)

// binlogStart is where the full backup leaves off.
type binlogStart struct {
	file     string
	position int64
}

// readBinlogStart reads the coordinate out of the backup directory. Its
// absence is refused rather than worked around: without it the replay
// would have to start at the beginning of a log and re-apply writes the
// full already contains, and "restored twice" is not a thing a drill may
// quietly do.
func readBinlogStart(backupDir string) (binlogStart, *protoError) {
	path := filepath.Join(backupDir, binlogInfoFile)
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case os.IsNotExist(err):
		return binlogStart{}, protoErr("source_corrupt", false,
			"the backup carries no %s, so there is no position to replay binary logs from: "+
				"it was taken from a server with the binary log disabled, and a point-in-time "+
				"drill cannot rest on it", binlogInfoFile)
	case err != nil:
		return binlogStart{}, protoErr("source_unreadable", false, "read %s: %v", binlogInfoFile, err)
	}

	// One line, tab-separated: file, position, and on a GTID-enabled
	// server a third field this adapter does not use (above).
	fields := strings.Split(strings.TrimSpace(firstTextLine(raw)), "\t")
	if len(fields) < 2 || fields[0] == "" {
		return binlogStart{}, protoErr("source_corrupt", false,
			"%s does not name a log file and position: %q", binlogInfoFile, firstTextLine(raw))
	}
	name := fields[0]
	if name != filepath.Base(name) {
		return binlogStart{}, protoErr("source_corrupt", false,
			"%s names %q, which is a path rather than a log file name", binlogInfoFile, name)
	}
	pos, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	if err != nil || pos < 0 {
		return binlogStart{}, protoErr("source_corrupt", false,
			"%s does not carry a readable position: %q", binlogInfoFile, fields[1])
	}
	return binlogStart{file: name, position: pos}, nil
}

// firstTextLine returns the first line of a file's bytes, without its
// terminator.
func firstTextLine(raw []byte) string {
	text := string(raw)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		return text[:i]
	}
	return text
}

// binlogFilesFrom lists the logs to replay, in the server's own order,
// beginning with the one the backup named.
//
// The ordering is the ordinal in the name, read as a number rather than
// compared as text: from the millionth log on, binlog.999999 sorts after
// binlog.1000000 as a string, and a chain replayed in that order would
// apply an hour of writes twice and then refuse.
//
// Two refusals rather than a best effort. A directory missing the log the
// backup named cannot be replayed at all — the chain has no beginning. A
// gap in the middle is worse: the replay would succeed, stop early, and
// leave a signed record claiming a recovery that skipped whatever the
// missing log held. Both say which file is missing.
func binlogFilesFrom(dir string, start binlogStart) ([]string, *protoError) {
	startBase, startOrdinal, ok := splitBinlogName(start.file)
	if !ok {
		return nil, protoErr("source_corrupt", false,
			"%s names %q, which is not a binary log file name (expected <base>.<ordinal>)",
			binlogInfoFile, start.file)
	}
	logs, perr := scanBinlogs(dir, startBase, startOrdinal)
	if perr != nil {
		return nil, perr
	}
	if perr := assertUnbrokenChain(logs, start, startOrdinal); perr != nil {
		return nil, perr
	}
	files := make([]string, 0, len(logs))
	for _, l := range logs {
		files = append(files, l.name)
	}
	return files, nil
}

// binlogFile is one log in the series, ordered by its ordinal.
type binlogFile struct {
	name    string
	ordinal int64
}

// scanBinlogs reads the directory and keeps the logs of the backup's own
// series from the starting ordinal forward, in server order. Two series in
// one directory is refused here rather than replayed: a chain across two
// servers' logs would prove neither.
func scanBinlogs(dir, startBase string, startOrdinal int64) ([]binlogFile, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "binary log directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "read binary log directory: %v", err)
	}
	logs := make([]binlogFile, 0, len(entries))
	bases := map[string]bool{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		base, ordinal, ok := splitBinlogName(e.Name())
		if !ok {
			continue // the index file, a sidecar, anything not a log
		}
		bases[base] = true
		if base == startBase && ordinal >= startOrdinal {
			logs = append(logs, binlogFile{name: e.Name(), ordinal: ordinal})
		}
	}
	if len(bases) > 1 {
		names := make([]string, 0, len(bases))
		for b := range bases {
			names = append(names, b)
		}
		sort.Strings(names)
		return nil, protoErr("source_corrupt", false,
			"the binary log directory holds two log series (%s): a replay across two servers' logs "+
				"would prove neither, so point one drill at one server's logs",
			strings.Join(names, ", "))
	}

	// The ordinal is a number, not text: compared as strings, binlog.999999
	// sorts after binlog.1000000, and a chain replayed in that order would
	// apply an hour of writes twice and then refuse.
	sort.Slice(logs, func(i, j int) bool { return logs[i].ordinal < logs[j].ordinal })
	return logs, nil
}

// assertUnbrokenChain is the pair of refusals that keep a partial replay
// from being recorded as a complete one.
//
// A directory missing the log the backup named cannot be replayed at all —
// the chain has no beginning. A gap in the middle is worse: the replay
// would succeed, stop early, and leave a signed record claiming a recovery
// that skipped whatever the missing log held.
func assertUnbrokenChain(logs []binlogFile, start binlogStart, startOrdinal int64) *protoError {
	if len(logs) == 0 || logs[0].ordinal != startOrdinal {
		return protoErr("source_not_found", false,
			"the binary log directory does not hold %s, where the backup says the replay starts: "+
				"the log the full backup ended on is missing, so nothing written after it can be proved",
			start.file)
	}
	for i := 1; i < len(logs); i++ {
		if logs[i].ordinal != logs[i-1].ordinal+1 {
			return protoErr("source_not_found", false,
				"the binary log chain has a gap: %s is followed by %s, and the log between them is "+
					"missing — replaying across it would skip whatever it held while the record "+
					"claimed a complete recovery", logs[i-1].name, logs[i].name)
		}
	}
	return nil
}

// splitBinlogName reads a log's base name and ordinal.
func splitBinlogName(name string) (string, int64, bool) {
	m := binlogNamePattern.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	ordinal, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], ordinal, true
}

// binlogStopDatetime converts the protocol's absolute pitr.target_time into
// the form mysqlbinlog takes, and "" when the drill asked for no target.
//
// The value is UTC and the replay runs under TZ=UTC, because
// --stop-datetime is read in the *client's* local zone: left to the
// sandbox image's zone, the same drill config would stop at a different
// point on a differently configured host, and a recovery point that
// depends on the machine it was proved on is not evidence.
func binlogStopDatetime(req *provisionRequest) (string, *protoError) {
	if req.PITR == nil {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339, req.PITR.TargetTime)
	if err != nil {
		return "", protoErr("invalid_request", false,
			"pitr.target_time %q is not an RFC 3339 timestamp", req.PITR.TargetTime)
	}
	return ts.UTC().Format("2006-01-02 15:04:05"), nil
}

// binlogReplayScript pipes the logs into the restored server.
//
// pipefail is the whole point of using bash here: without it a
// mysqlbinlog that dies half way through a log leaves the pipeline's exit
// status to mysql, which happily reports success for the fragment it did
// receive — a drill that proved a partial recovery and said nothing.
//
// Paths are positional parameters rather than text folded into the script
// (physical.go says why), and the file list follows them.
const binlogReplayScript = `set -e
set -o pipefail
dir="$1"; pos="$2"; stop="$3"; user="$4"; shift 4
cd "$dir"
if [ -n "$stop" ]; then
  TZ=UTC mysqlbinlog --start-position="$pos" --stop-datetime="$stop" -- "$@" |
    mysql -h 127.0.0.1 -u "$user"
else
  TZ=UTC mysqlbinlog --start-position="$pos" -- "$@" | mysql -h 127.0.0.1 -u "$user"
fi`

// binlogReplayArgv composes the call: the script's fixed parameters, then
// one argument per log in replay order.
func binlogReplayArgv(dir string, start binlogStart, stop string, files []string) []string {
	argv := make([]string, 0, 8+len(files))
	argv = append(argv, "bash", "-c", binlogReplayScript, "bash",
		dir, strconv.FormatInt(start.position, 10), stop, defaultUser)
	return append(argv, files...)
}

// binlogSummary is what the log line and the diagnostics say about a
// replay: enough to recognise the chain without reprinting it.
func binlogSummary(files []string, stop string) string {
	if len(files) == 0 {
		return "no binary logs"
	}
	span := files[0]
	if len(files) > 1 {
		span = fmt.Sprintf("%s..%s (%d logs)", files[0], files[len(files)-1], len(files))
	}
	if stop == "" {
		return span + ", to the end of the archive"
	}
	return fmt.Sprintf("%s, to %s UTC", span, stop)
}
