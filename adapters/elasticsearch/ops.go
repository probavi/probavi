package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	adapterName    = "elasticsearch"
	adapterVersion = "0.2.0"

	// workDirName is created under the provider's scratch directory —
	// the official images run as the elasticsearch user (uid 1000,
	// measured), and scratch is the directory the provider guarantees
	// writable.
	workDirName = "probavi-elasticsearch"
	// zipName is where the archive kind places the artifact before
	// unpacking it.
	zipName = "repo.zip"
	// hostsName is the JDK hosts file the launch points the server at
	// (see startScript).
	hostsName = "hosts"
	// repoName is the fs repository name the drill registers.
	repoName = "probavi"

	serverURL = "http://127.0.0.1:9200"

	readinessBudget = 3 * time.Minute
	readinessPoll   = 2 * time.Second
)

// runnerScript absorbs the check dialect declaratively: the check text
// is one Elasticsearch SQL statement, JSON-encoded into the SQL API's
// request body by the shell itself — the verified images carry neither
// python3 nor jq (measured) — and the API's tsv format (a header line,
// then tab-separated rows, a tab inside a value escaped as the two
// characters `\t`, all measured) is filtered into the undecorated rows
// the protocol requires. The encoding escapes the backslash, the double
// quote, and the three whitespace controls SQL text carries; any other
// control character is refused loudly rather than sent malformed. An
// HTTP status other than 200 — the API answers 400 to a bad query, with
// the engine's reason in the body (measured) — fails the check with that
// body on stderr. Nothing is interpolated: the text reaches the engine
// as the string it was, measured against shell and SQL metacharacters.
const runnerScript = `set -u
s=$1
s=${s//\\/\\\\}
s=${s//\"/\\\"}
s=${s//$'\t'/\\t}
s=${s//$'\n'/\\n}
s=${s//$'\r'/\\r}
case $s in *[[:cntrl:]]*) echo "the check text contains a control character the runner cannot encode" >&2; exit 1;; esac
out=$(curl -s -w '\n%{http_code}' -XPOST '` + serverURL + `/_sql?format=tsv' -H 'Content-Type: application/json' --data-binary "{\"query\":\"$s\"}") || exit $?
code=${out##*$'\n'}
body=${out%$'\n'*}
body=${body%$'\n'}
[ "$code" = 200 ] || { printf '%s\n' "$body" >&2; exit 1; }
printf '%s\n' "$body" | tail -n +2`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "elasticsearch"},
		"sources": []map[string]any{
			{"kind": "elasticsearch_repo_zip", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "elasticsearch_repo", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Checks are Elasticsearch SQL through the node's SQL API
			// (see runnerScript). The core's generating built-ins apply:
			// the dialect accepts SQL-standard quoted identifiers and
			// answers max() of a date field as an RFC 3339 instant
			// (measured) — the README records the consequences.
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{sql}}"},
			"env":  map[string]string{},
		},
		"verbs_required": []string{"exec", "put_file"},
	}
}

// provisionRequest is the §6.2 request payload.
type provisionRequest struct {
	Source struct {
		Kind          string            `json:"kind"`
		Path          string            `json:"path"`
		Params        map[string]string `json:"params"`
		CredentialEnv []string          `json:"credential_env"`
	} `json:"source"`
	Sandbox struct {
		ScratchDir string `json:"scratch_dir"`
	} `json:"sandbox"`
	Options map[string]string `json:"options"`
	PITR    *struct {
		TargetTime string `json:"target_time"`
	} `json:"pitr"`
}

// apiSnapshot is the engine's own statement about one snapshot. The
// writing engine is named by version_id — its index version — because
// the version string beside it is rendered from that number and reads
// as a range ("8.19.9-8.19.20", measured) or worse for a number the node
// does not know.
type apiSnapshot struct {
	Snapshot  string   `json:"snapshot"`
	State     string   `json:"state"`
	VersionID int64    `json:"version_id"`
	EndTimeMs int64    `json:"end_time_in_millis"`
	Indices   []string `json:"indices"`
}

// engineIdentity is what the sandbox node says it is: the release for
// messages, the index version for the pairing.
type engineIdentity struct {
	version      string
	indexVersion int64
}

// engineErrorReason reads the engine's error envelope out of a response
// body. The API calls below run curl without -f deliberately: with -f
// an HTTP error discards the body, and the body is where the engine
// states its reason — the version-pairing refusal, a repository
// exception — in its own words. A body that is not the envelope's shape
// (the status field is a string in health answers, absent in successes)
// reports found false.
func engineErrorReason(stdout []byte) (string, bool) {
	e := struct {
		Error struct {
			Reason    string `json:"reason"`
			RootCause []struct {
				Reason string `json:"reason"`
			} `json:"root_cause"`
		} `json:"error"`
		Status int `json:"status"`
	}{}
	if err := json.Unmarshal(stdout, &e); err != nil || e.Status == 0 {
		return "", false
	}
	reason := e.Error.Reason
	if len(e.Error.RootCause) > 0 && e.Error.RootCause[0].Reason != "" {
		reason = e.Error.RootCause[0].Reason
	}
	if reason == "" {
		reason = fmt.Sprintf("the engine answered status %d", e.Status)
	}
	return firstLine([]byte(reason)), true
}

// opProvision restores the repository's newest snapshot into a fresh
// single node: preflight, start (loopback dev mode, mmap off — the
// sysctl decision's measured design), lifecycle suspended, transfer, a
// read-only repository registration, selection by the snapshots' own
// claimed instants, and a restore whose verdict is read from the shard
// counts and cluster health — never from the HTTP status, which stays
// 200 while shards fail (measured).
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req, scratch, perr := parseProvisionRequest(payload)
	if perr != nil {
		return nil, perr
	}

	src, perr := resolveSource(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"snapshots", len(src.census.snapshots))

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	repoDir := path.Join(workDir, "repo")
	logPath := path.Join(workDir, "elasticsearch.log")
	if perr := mkdirAll(ctx, c, repoDir); perr != nil {
		return nil, perr
	}

	readySeconds, engine, perr := startEngine(ctx, c, workDir, repoDir, logPath)
	if perr != nil {
		return nil, perr
	}
	if perr := checkVersions(src.census, engine); perr != nil {
		return nil, perr
	}

	transferSeconds, unpackSeconds, perr := transferArtifact(ctx, c, src, workDir, repoDir)
	if perr != nil {
		return nil, perr
	}

	chosen, restoreSeconds, perr := restoreNewest(ctx, c, src, repoDir, engine, logger)
	if perr != nil {
		return nil, perr
	}
	logger.Info("snapshot restored and verified", "snapshot", chosen.Snapshot,
		"ready_seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			// Checks reach the node over HTTP; there is no database
			// concept — indices are named in the SQL itself.
			"scheme": "http", "host": "127.0.0.1", "port": 9200,
			"database": "", "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The chosen snapshot's own claimed instant (nil when the
			// sandbox restored nothing to claim one).
			"created_at": formatCreatedAt(chosen.EndTimeMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + restoreSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "repo_dir": repoDir, "snapshot": chosen.Snapshot,
		},
	}, nil
}

// parseProvisionRequest validates the §6.2 payload and resolves the
// scratch directory.
func parseProvisionRequest(payload json.RawMessage) (*provisionRequest, string, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, "", protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, "", protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	if perr := rejectBackupTimezone(req.Source.Params); perr != nil {
		return nil, "", perr
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	return req, scratch, nil
}

// checkEngine verifies the toolchain every later step runs on: the
// elasticsearch launcher, curl for the API, unzip for the archive kind,
// bash for the scripts — all shipped by both verified images (measured;
// the 9.x image is the one that ships no tar, python3 or jq).
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c",
		"command -v elasticsearch >/dev/null && command -v curl >/dev/null && command -v unzip >/dev/null"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks the Elasticsearch toolchain (elasticsearch, curl, unzip over "+
				"bash): use an official elasticsearch image with command: sleep infinity (%s)",
			firstLine(stderr))
	}
	return nil
}

// startScript launches a single node in the loopback dev mode the
// sysctl decision measured: bootstrap checks not enforced, security off,
// mmap disabled by setting, the repository path allowed from the start
// (path.repo is a static setting), the GeoIP downloader off (it has no
// network to reach), and the data stream lifecycle poll interval pinned
// before the node exists to poll anything (retention.go).
//
// The hosts file is the 8.x line's own requirement: under a zero-ingress
// sandbox the container's hostname resolves to nothing, and an 8.19
// node's logging resolves it at startup and dies — "Could not determine
// local host name", exit within four seconds, measured — where 9.x
// tolerates the same. The image cannot edit /etc/hosts (root-owned,
// uid 1000, measured), so the JDK is pointed at a hosts file of its own
// that names the host, appended to whatever ES_JAVA_OPTS the operator
// set. Measured green on both lines.
//
// The node is detached by the shell and judged by readiness alone.
const startScript = `set -u
hosts=$1; repo=$2; log=$3
printf '127.0.0.1 localhost %s\n::1 localhost\n' "${HOSTNAME:-$(cat /etc/hostname)}" > "$hosts" || exit 1
(ES_JAVA_OPTS="${ES_JAVA_OPTS:+$ES_JAVA_OPTS }-Djdk.net.hosts.file=$hosts" elasticsearch ` +
	`-E discovery.type=single-node -E xpack.security.enabled=false -E node.store.allow_mmap=false ` +
	`-E ingest.geoip.downloader.enabled=false -E path.repo="$repo" ` +
	`-E ` + lifecyclePollSetting + `=` + lifecyclePollInterval + ` > "$log" 2>&1 &)`

// startEngine launches the node, waits for it, suspends its lifecycle
// machinery, and reads what it is.
func startEngine(ctx context.Context, c *core, workDir, repoDir, logPath string) (readySeconds float64, engine engineIdentity, perr *protoError) {
	start, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startScript, "bash",
		path.Join(workDir, hostsName), repoDir, logPath}})
	if perr != nil {
		return 0, engineIdentity{}, perr
	}
	if start.ExitCode != 0 {
		return 0, engineIdentity{}, protoErr("restore_failed", false,
			"the node failed to launch: %s", firstLine(stderr))
	}
	waited, perr := awaitReady(ctx, c, logPath)
	if perr != nil {
		return 0, engineIdentity{}, describeStartFailure(ctx, c, logPath, perr)
	}
	pinned, perr := pinLifecycle(ctx, c)
	if perr != nil {
		return 0, engineIdentity{}, perr
	}
	engine, perr = readEngine(ctx, c)
	if perr != nil {
		return 0, engineIdentity{}, perr
	}
	return start.DurationSeconds + waited + pinned, engine, nil
}

func awaitReady(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"curl", "-sf", "-o", "/dev/null", serverURL + "/_cluster/health"},
			TimeoutSeconds: 10,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if fatal, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"grep", "-qE",
			"Elasticsearch died|fatal error|ElasticsearchUncaughtExceptionHandler", logPath}}); perr == nil && fatal.ExitCode == 0 {
			return 0, protoErr("engine_not_ready", true, "the node exited during startup")
		}
		if time.Since(begin) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"the node did not answer within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// describeStartFailure enriches a readiness timeout with the node's own
// last error line.
func describeStartFailure(ctx context.Context, c *core, logPath string, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "30", logPath}})
	if eperr != nil || val.ExitCode != 0 {
		return perr
	}
	line := lastErrorLine(stdout)
	if line == "" {
		return perr
	}
	return protoErr("restore_failed", false, "the node failed to start: %s", line)
}

func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "exception") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// readEngine asks the node what it is: the release from the root
// resource, the index version from the nodes API (measured: 8537000 on
// 8.19.20, 9111000 on 9.5.2). An answer that does not parse yields the
// zero value and the pre-check stays silent (the engine's own refusal
// remains the authority).
func readEngine(ctx context.Context, c *core) (engineIdentity, *protoError) {
	engine := engineIdentity{}
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-sf", serverURL + "/"}})
	if perr != nil {
		return engine, perr
	}
	if val.ExitCode == 0 {
		root := struct {
			Version struct {
				Number string `json:"number"`
			} `json:"version"`
		}{}
		if err := json.Unmarshal(stdout, &root); err == nil {
			engine.version = root.Version.Number
		}
	}
	val, stdout, _, perr = c.exec(ctx, execArgs{Argv: []string{"curl", "-sf",
		serverURL + "/_nodes?filter_path=nodes.*.index_version"}})
	if perr != nil {
		return engine, perr
	}
	if val.ExitCode == 0 {
		nodes := struct {
			Nodes map[string]struct {
				IndexVersion int64 `json:"index_version"`
			} `json:"nodes"`
		}{}
		if err := json.Unmarshal(stdout, &nodes); err == nil {
			for _, n := range nodes.Nodes {
				engine.indexVersion = n.IndexVersion
			}
		}
	}
	return engine, nil
}

// checkVersions refuses, before a byte is transferred, a repository
// whose own generation file claims a snapshot written by a newer
// Elasticsearch than the sandbox runs — the pairing the engine itself
// refuses at restore ("the snapshot was created with version [X] which
// is higher than the version of this node [Y]", measured, with X
// rendered from a number the older node cannot name); naming it here
// saves transferring the repository first. Positive evidence only:
// unknown versions stay silent.
func checkVersions(census repoCensus, engine engineIdentity) *protoError {
	for _, s := range census.snapshots {
		if indexVersionNewer(s.IndexVersion, engine.indexVersion) {
			return protoErr("invalid_request", false,
				"the repository's own metadata says snapshot %s was written at index version %d, and "+
					"the sandbox engine (Elasticsearch %s) is at %d: a snapshot does not restore on an "+
					"older engine — use an elasticsearch image at least as new as the backup's origin",
				s.Name, s.IndexVersion, engine.version, engine.indexVersion)
		}
	}
	return nil
}

// transferArtifact moves the artifact into the repository directory the
// node was started with: an archive is placed and unpacked, a tree is
// recreated file by file.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir, repoDir string) (transferSeconds, unpackSeconds float64, perr *protoError) {
	if src.archive {
		return unpackArchive(ctx, c, src.path, workDir, repoDir)
	}
	transferSeconds, perr = transferTree(ctx, c, src.path, repoDir)
	return transferSeconds, 0, perr
}

// rootScript locates the repository after unpacking — the archive holds
// it at the root or under one wrapping directory, decided by where
// index.latest sits — and copies its files into the directory path.repo
// already allows. set -e is the verdict's honesty: a failed copy must
// fail the step, not leave an empty repository the engine then lists as
// zero snapshots without a word (measured).
const rootScript = `set -e
d="$1"; dest="$2"
if [ ! -e "$d/index.latest" ]; then
  for sub in "$d"/*/; do
    if [ -e "${sub%/}/index.latest" ]; then d="${sub%/}"; break; fi
  done
fi
cp -a "$d/." "$dest/"`

func unpackArchive(ctx context.Context, c *core, hostPath, workDir, repoDir string) (transferSeconds, unpackSeconds float64, perr *protoError) {
	extractDir := path.Join(workDir, "extract")
	if perr := mkdirAll(ctx, c, extractDir); perr != nil {
		return 0, 0, perr
	}
	zipPath := path.Join(workDir, zipName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: zipPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	// unzip exits non-zero on a truncated or foreign archive (measured:
	// 9 on both verified images).
	unpack, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"unzip", "-q", zipPath, "-d", extractDir}})
	if perr != nil {
		return 0, 0, perr
	}
	if unpack.ExitCode != 0 {
		return 0, 0, protoErr("source_corrupt", false,
			"unzip could not unpack the archive: %s", firstLine(stderr))
	}
	locate, _, stderr2, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", rootScript, "bash", extractDir, repoDir}})
	if perr != nil {
		return 0, 0, perr
	}
	if locate.ExitCode != 0 {
		return 0, 0, protoErr("internal", false, "place repository: %s", firstLine(stderr2))
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, nil
}

// transferTree recreates the repository tree inside the sandbox: one
// mkdir for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, repoDir string) (float64, *protoError) {
	dirs := []string{repoDir}
	files := []string{}
	err := filepath.WalkDir(hostDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path.Join(repoDir, filepath.ToSlash(rel)))
		} else if d.Type().IsRegular() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if perr := mkdirAll(ctx, c, dirs...); perr != nil {
		return 0, perr
	}
	total := 0.0
	for _, rel := range files {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: filepath.Join(hostDir, filepath.FromSlash(rel)),
			DestPath:   path.Join(repoDir, rel),
			Mode:       "0600",
		})
		if perr != nil {
			return 0, perr
		}
		total += put.DurationSeconds
	}
	return total, nil
}

func mkdirAll(ctx context.Context, c *core, dirs ...string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: append([]string{"mkdir", "-p"}, dirs...)})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}

// restoreNewest registers the transferred repository read-only, selects
// the snapshot whose own metadata claims the newest instant, and
// restores it — reading the verdict from where it actually is.
func restoreNewest(ctx context.Context, c *core, src *resolvedSource,
	repoDir string, engine engineIdentity, logger *slog.Logger) (apiSnapshot, float64, *protoError) {
	total := 0.0
	seconds, perr := registerRepo(ctx, c, repoDir)
	if perr != nil {
		return apiSnapshot{}, 0, perr
	}
	total += seconds

	chosen, listed, seconds, perr := selectSnapshot(ctx, c, src)
	if perr != nil {
		return apiSnapshot{}, 0, perr
	}
	total += seconds
	if !listed {
		// Nothing the artifact claimed and nothing the engine lists: the
		// opaque-archive path with an empty answer — there is nothing to
		// restore and nothing to refuse (the sandbox is the authority).
		return apiSnapshot{}, total, nil
	}
	logger.Info("snapshot selected", "snapshot", chosen.Snapshot,
		"state", chosen.State, "index_version", chosen.VersionID)

	if chosen.State != "SUCCESS" {
		return apiSnapshot{}, 0, protoErr("source_corrupt", false,
			"the newest snapshot in the repository (%s) is %s, not SUCCESS — a partial or failed "+
				"snapshot is not a restorable claim; snapshot again and keep the repository intact",
			chosen.Snapshot, chosen.State)
	}
	if indexVersionNewer(chosen.VersionID, engine.indexVersion) {
		return apiSnapshot{}, 0, protoErr("invalid_request", false,
			"snapshot %s was written at index version %d, and the sandbox engine (Elasticsearch %s) "+
				"is at %d: a snapshot does not restore on an older engine — use an image at least as "+
				"new as the backup's origin", chosen.Snapshot, chosen.VersionID, engine.version,
			engine.indexVersion)
	}

	seconds, perr = runRestore(ctx, c, chosen)
	if perr != nil {
		return apiSnapshot{}, 0, perr
	}
	total += seconds

	seconds, perr = checkHealth(ctx, c)
	if perr != nil {
		return apiSnapshot{}, 0, perr
	}
	total += seconds
	return chosen, total, nil
}

func registerRepo(ctx context.Context, c *core, repoDir string) (float64, *protoError) {
	body := fmt.Sprintf(`{"type":"fs","settings":{"location":%q,"readonly":true}}`, repoDir)
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s", "-XPUT",
		serverURL + "/_snapshot/" + repoName, "-H", "Content-Type: application/json",
		"--data-binary", body}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the node stopped answering while registering the repository: %s", firstLine(stderr))
	}
	if reason, found := engineErrorReason(stdout); found {
		// A directory that is no repository registers silently (measured);
		// an engine that still refuses is reading damage.
		return 0, protoErr("source_corrupt", false,
			"the engine refused to register the repository: %s", reason)
	}
	return val.DurationSeconds, nil
}

// selectSnapshot lists the registered repository through the engine and
// picks the newest by the snapshots' own claimed instants. listed is
// false when the engine answered with nothing parseable and the
// artifact itself claimed nothing either.
func selectSnapshot(ctx context.Context, c *core, src *resolvedSource) (apiSnapshot, bool, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s",
		serverURL + "/_snapshot/" + repoName + "/_all"}})
	if perr != nil {
		return apiSnapshot{}, false, 0, perr
	}
	if val.ExitCode != 0 {
		return apiSnapshot{}, false, 0, protoErr("restore_failed", false,
			"the node stopped answering while listing snapshots: %s", firstLine(stderr))
	}
	if reason, found := engineErrorReason(stdout); found {
		// Listing is the first read of the repository's own metadata: an
		// engine refusal here is the engine reading damage.
		return apiSnapshot{}, false, 0, protoErr("source_corrupt", false,
			"the engine could not read the repository's snapshot list: %s", reason)
	}
	list := struct {
		Snapshots []apiSnapshot `json:"snapshots"`
	}{}
	if err := json.Unmarshal(stdout, &list); err != nil || len(list.Snapshots) == 0 {
		if len(src.census.snapshots) > 0 {
			return apiSnapshot{}, false, 0, protoErr("source_corrupt", false,
				"the repository's own files list %d snapshots (%s), but the engine lists none — "+
					"the repository copy is damaged", len(src.census.snapshots),
				strings.Join(src.census.names(), ", "))
		}
		return apiSnapshot{}, false, val.DurationSeconds, nil
	}
	sort.Slice(list.Snapshots, func(i, j int) bool {
		return list.Snapshots[i].EndTimeMs < list.Snapshots[j].EndTimeMs
	})
	return list.Snapshots[len(list.Snapshots)-1], true, val.DurationSeconds, nil
}

// runRestore restores every regular index and data stream the snapshot
// holds — `*` reaches hidden backing indices and leaves system indices
// to the feature states it does not ask for (measured: a data stream
// came back with its `.ds-` backing index and nothing collided) — with
// the cluster state left in the artifact (retention.go) and replicas
// dropped to the single node's honest zero. The verdict is the shard
// counts: the call returns 200 with failed shards when the repository's
// data is damaged (measured).
func runRestore(ctx context.Context, c *core, chosen apiSnapshot) (float64, *protoError) {
	body := `{"indices":"*","index_settings":{"index.number_of_replicas":0}}`
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-s", "-XPOST",
		serverURL + "/_snapshot/" + repoName + "/" + chosen.Snapshot + "/_restore?wait_for_completion=true",
		"-H", "Content-Type: application/json", "--data-binary", body}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the node stopped answering while restoring snapshot %s: %s",
			chosen.Snapshot, firstLine(stderr))
	}
	if reason, found := engineErrorReason(stdout); found {
		if strings.Contains(reason, "which is higher than the version of this node") {
			return 0, protoErr("invalid_request", false,
				"the engine refused the version pairing: %s — use an elasticsearch image at least "+
					"as new as the backup's origin", reason)
		}
		return 0, protoErr("restore_failed", false, "restoring snapshot %s failed: %s",
			chosen.Snapshot, reason)
	}
	result := struct {
		Snapshot struct {
			Indices []string `json:"indices"`
			Shards  struct {
				Total  int `json:"total"`
				Failed int `json:"failed"`
			} `json:"shards"`
		} `json:"snapshot"`
	}{}
	if err := json.Unmarshal(stdout, &result); err != nil {
		// Not the restore response's shape: the simulated-sandbox path.
		// The health gate below still runs on positive evidence only.
		return val.DurationSeconds, nil
	}
	if result.Snapshot.Shards.Failed > 0 {
		return 0, protoErr("source_corrupt", false,
			"%d of %d shards failed to restore from snapshot %s — the repository's data is "+
				"damaged, and the restore call itself returns 200 with the cluster red (measured), "+
				"which is why this drill reads the shard counts instead",
			result.Snapshot.Shards.Failed, result.Snapshot.Shards.Total, chosen.Snapshot)
	}
	return val.DurationSeconds, nil
}

// healthSettleTimeout bounds how long the health gate lets the engine
// finish starting the restored shards.
const healthSettleTimeout = 60 * time.Second

// checkHealth requires the restored cluster green: replicas are zero, so
// anything less means restored data is not fully served.
//
// It waits for green through the engine's own primitive rather than
// reading one instant: the restore call returns when its bookkeeping is
// complete, and the shards' started events land asynchronously after it
// — on a slower host the gap is wide enough to read, and a primary still
// initializing from a snapshot is reported *yellow*, not red (measured
// on a hosted runner, where a single-instant read failed a sound restore
// that this machine passed). A wait that expires answers HTTP 408 with
// the current status in the body (measured), so curl runs without -f
// here and the verdict is the body's: the gate fires on positive
// evidence only — an answer that is not the health API's shape is
// skipped.
func checkHealth(ctx context.Context, c *core) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"curl", "-s", fmt.Sprintf("%s/_cluster/health?wait_for_status=green&timeout=%ds",
			serverURL, int(healthSettleTimeout.Seconds()))},
		TimeoutSeconds: (healthSettleTimeout + 30*time.Second).Seconds(),
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return val.DurationSeconds, nil
	}
	health := struct {
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}{}
	if err := json.Unmarshal(stdout, &health); err != nil || health.Status == "" {
		return val.DurationSeconds, nil
	}
	if health.Status != "green" {
		waited := ""
		if health.TimedOut {
			waited = fmt.Sprintf(" %s after it", healthSettleTimeout)
		}
		return 0, protoErr("source_corrupt", false,
			"the cluster is %s%s the restore — with zero replicas anything below green means "+
				"restored data is not fully served", health.Status, waited)
	}
	return val.DurationSeconds, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored node still answers (§6.3). An
// unhealthy node is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"curl", "-sf",
		serverURL + "/_cluster/health"}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "node answers; " + firstLine(stdout)
	if !healthy {
		detail = fmt.Sprintf("curl exited %d: %s", val.ExitCode, firstLine(stderr))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The message crosses the protocol as a JSON string and lands in
	// evidence error fields: keep it single-line and quote-free.
	return strings.ReplaceAll(s, `"`, "'")
}
