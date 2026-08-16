package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// verbCall records one sandbox call the adapter issued.
type verbCall struct {
	Verb string
	Args json.RawMessage
}

// driveOp runs one full operation through run() with an in-process core
// simulator. handler returns the verb's value (or an error) for each
// sandbox call.
func driveOp(t *testing.T, op, payload string, handler func(call verbCall) (any, *protoError)) (finalLine []byte, calls []verbCall, exit int) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &bytes.Buffer{}

	exitCh := make(chan int, 1)
	go func() { exitCh <- run(stdinR, stdoutW, stderr) }()

	request := fmt.Sprintf(`{"protocol":"probavi-adapter/0","request_id":"r-test","op":%q,"payload":%s}`, op, payload)
	if _, err := io.WriteString(stdinW, request+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	sc := bufio.NewScanner(stdoutR)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		msg := struct {
			RequestID   string `json:"request_id"`
			SandboxCall *struct {
				CallID string          `json:"call_id"`
				Verb   string          `json:"verb"`
				Args   json.RawMessage `json:"args"`
			} `json:"sandbox_call"`
			OK *bool `json:"ok"`
		}{}
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("adapter emitted non-JSON: %s", line)
		}
		if msg.RequestID != "r-test" {
			t.Fatalf("adapter did not echo request_id: %s", line)
		}
		if msg.OK != nil {
			finalLine = line
			break
		}
		if msg.SandboxCall == nil {
			t.Fatalf("message is neither call nor final: %s", line)
		}
		call := verbCall{Verb: msg.SandboxCall.Verb, Args: msg.SandboxCall.Args}
		calls = append(calls, call)
		value, verr := handler(call)
		result := map[string]any{"call_id": msg.SandboxCall.CallID}
		if verr != nil {
			result["ok"] = false
			result["error"] = verr
		} else {
			result["ok"] = true
			result["value"] = value
		}
		reply, err := json.Marshal(map[string]any{
			"protocol": "probavi-adapter/0", "request_id": "r-test", "sandbox_result": result,
		})
		if err != nil {
			t.Fatalf("marshal sandbox_result: %v", err)
		}
		if _, err := stdinW.Write(append(reply, '\n')); err != nil {
			t.Fatalf("write sandbox_result: %v", err)
		}
	}
	if finalLine == nil {
		t.Fatal("adapter closed stdout without a final response")
	}
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	exit = <-exitCh
	return finalLine, calls, exit
}

// finalResponse unpacks a final response line.
type finalResponse struct {
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *protoError     `json:"error"`
}

func parseFinal(t *testing.T, line []byte) finalResponse {
	t.Helper()
	f := finalResponse{}
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("parse final %s: %v", line, err)
	}
	return f
}

func okExec() any { return execValue{ExitCode: 0} }

func outExec(stdout string) any { return outExecDur(stdout, 0) }

func outExecDur(stdout string, dur float64) any {
	return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)), DurationSeconds: dur}
}

func errExec(exit int, stderr string) any {
	return execValue{ExitCode: exit, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

func provisionPayload(t *testing.T, kind, path string, params map[string]string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path, "params": params},
		"sandbox": map[string]any{"scratch_dir": "/scratch"},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// argvOf decodes an exec call's argv.
func argvOf(t *testing.T, call verbCall) []string {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args.Argv
}

// fixtureList is the engine's answer to the snapshot listing, snap-2 the
// newest by its own instant but listed first — selection must go by the
// claimed instants, not file order.
const fixtureList = `{"snapshots":[` +
	`{"snapshot":"snap-2","state":"SUCCESS","version":"2.19.6","end_time_in_millis":1700000100000,"indices":["orders"]},` +
	`{"snapshot":"snap-1","state":"SUCCESS","version":"2.19.6","end_time_in_millis":1700000000000,"indices":["orders"]}]}`

const fixtureRestore = `{"snapshot":{"snapshot":"snap-2","indices":["orders"],"shards":{"total":1,"failed":0}}}`

// fixtureCreatedAt is snap-2's own instant rendered.
const fixtureCreatedAt = "2023-11-14T22:15:00.000Z"

// simulated answers per flow step; tests override single entries.
type simulated struct {
	ready    any
	version  any
	register any
	list     any
	restore  any
	health   any
	unpack   any
}

func defaultSimulated() simulated {
	return simulated{
		ready:    execValue{ExitCode: 0, DurationSeconds: 0.05},
		version:  outExec(`{"version":{"number":"2.19.6"}}`),
		register: outExecDur(`{"acknowledged":true}`, 0.1),
		list:     outExecDur(fixtureList, 0.05),
		restore:  outExecDur(fixtureRestore, 1.2),
		health:   outExec(`{"status":"green"}`),
		unpack:   okExec(),
	}
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string, sim simulated) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.25}, nil
		}
		label, value := classifyExec(argvOf(t, call), sim)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected.
func classifyExec(argv []string, sim simulated) (string, any) {
	switch argv[0] {
	case "bash":
		return classifyShellExec(argv[2])
	case "mkdir":
		return "mkdir", okExec()
	case "tar":
		return "unpack", sim.unpack
	case "curl":
		return classifyCurlExec(strings.Join(argv, " "), sim)
	}
	return "", nil
}

func classifyShellExec(script string) (string, any) {
	switch {
	case strings.Contains(script, "command -v opensearch"):
		return "engine", okExec()
	case strings.Contains(script, "(opensearch "):
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.2}
	case script == rootScript:
		return "locate", outExec("/scratch/probavi-opensearch/extract\n")
	}
	return "", nil
}

func classifyCurlExec(joined string, sim simulated) (string, any) {
	switch {
	case strings.Contains(joined, "-o /dev/null"):
		return "ready", sim.ready
	case strings.Contains(joined, "-XPUT"):
		return "register", sim.register
	case strings.Contains(joined, "/_all"):
		return "list", sim.list
	case strings.Contains(joined, "_restore"):
		return "restore", sim.restore
	case strings.Contains(joined, "/_cluster/health"):
		return "health", sim.health
	case strings.HasSuffix(joined, ":9200/"):
		return "version", sim.version
	}
	return "", nil
}

func TestProbeGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("probe must not touch the sandbox")
		return nil, nil
	})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit=%d calls=%d", exit, len(calls))
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(golden, append(line, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -args -update once): %v", err)
	}
	if !bytes.Equal(append(line, '\n'), want) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, bytes.TrimSpace(want))
	}
}

// TestRunnerTemplateShape pins the declared dialect absorption: the
// OpenSearch SQL travels as one argv element into a bash pipeline that
// JSON-encodes it for the bundled SQL plugin and filters the plugin's
// raw format into undecorated tab-separated rows (measured shapes in
// runnerScript's comment).
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"bash", "-c", runnerScript, "bash", "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
}

// sequenceCounts tallies the labels of a recorded sequence.
func sequenceCounts(sequence []string) map[string]int {
	counts := map[string]int{}
	for _, label := range sequence {
		counts[label]++
	}
	return counts
}

func decodeProvision(t *testing.T, f finalResponse) (snapshot string, createdAt *string, timings map[string]float64) {
	t.Helper()
	got := struct {
		Connection struct {
			Scheme string `json:"scheme"`
			Host   string `json:"host"`
			Port   int    `json:"port"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   struct {
			Snapshot string `json:"snapshot"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Connection.Scheme != "http" || got.Connection.Host != "127.0.0.1" || got.Connection.Port != 9200 {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	return got.State.Snapshot, got.SourceIdentity.CreatedAt, got.Timings
}

func TestProvisionRestoresRepoDir(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "opensearch_repo", repo, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	counts := sequenceCounts(sequence)
	// The engine starts before a byte moves (path.repo is static): one
	// mkdir for the repo dir, one for the tree skeleton, three files
	// transferred, then the API drill.
	want := map[string]int{"engine": 1, "mkdir": 2, "start": 1, "ready": 1, "version": 1,
		"put_file": 3, "register": 1, "list": 1, "restore": 1, "health": 1}
	for label, n := range want {
		if counts[label] != n {
			t.Errorf("sequence has %d %s calls, want %d (sequence: %v)", counts[label], label, n, sequence)
		}
	}
	snapshot, createdAt, timings := decodeProvision(t, f)
	if snapshot != "snap-2" {
		t.Errorf("state.snapshot = %q, want the newest by its own instant", snapshot)
	}
	if createdAt == nil || *createdAt != fixtureCreatedAt {
		t.Errorf("created_at = %v, want the snapshot's own instant", createdAt)
	}
	if timings["transfer_seconds"] != 0.75 || timings["engine_ready_seconds"] <= 0 ||
		timings["restore_seconds"] < 1.2 {
		t.Errorf("timings = %+v, want measured phases", timings)
	}
}

func TestProvisionUnpacksArchive(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	path := treeToTar(t, repo, filepath.Join(t.TempDir(), "repo.tar.gz"), "backup", true)
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "opensearch_repo_tar", path, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	counts := sequenceCounts(sequence)
	// The archive is placed once, unpacked, and its root located; no
	// per-file transfer.
	want := map[string]int{"put_file": 1, "unpack": 1, "locate": 1, "register": 1,
		"list": 1, "restore": 1, "health": 1}
	for label, n := range want {
		if counts[label] != n {
			t.Errorf("sequence has %d %s calls, want %d (sequence: %v)", counts[label], label, n, sequence)
		}
	}
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.DestPath != "/scratch/probavi-opensearch/repo.tar" {
			t.Errorf("put_file dest = %q", args.DestPath)
		}
	}
	snapshot, createdAt, _ := decodeProvision(t, f)
	if snapshot != "snap-2" || createdAt == nil {
		t.Errorf("snapshot = %q created_at = %v, want the archive's own claims", snapshot, createdAt)
	}
}

// TestProvisionToleratesOpaqueArchive pins the simulated-sandbox path:
// an artifact the host cannot walk and an engine that lists nothing is
// an empty answer, not a verdict — there is nothing to restore and
// nothing to refuse.
func TestProvisionToleratesOpaqueArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	writeFixtureFile(t, path, []byte(strings.Repeat("x", 4096)))
	sim := defaultSimulated()
	sim.list = outExec("")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "opensearch_repo_tar", path, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — the sandbox is the authority on an opaque archive", f)
	}
	counts := sequenceCounts(sequence)
	if counts["restore"] != 0 || counts["health"] != 0 {
		t.Errorf("sequence = %v, want no restore of a snapshot nobody lists", sequence)
	}
	snapshot, createdAt, _ := decodeProvision(t, f)
	if snapshot != "" || createdAt != nil {
		t.Errorf("snapshot = %q created_at = %v, want no claims", snapshot, createdAt)
	}
}

func TestProvisionRefusals(t *testing.T) {
	base := t.TempDir()
	repo := writeRepo(t, filepath.Join(base, "repo"))
	liveDir := filepath.Join(base, "live")
	if err := os.MkdirAll(filepath.Join(liveDir, "nodes"), 0o755); err != nil {
		t.Fatal(err)
	}
	emptyRepo := writeRepoWithIndex(t, filepath.Join(base, "empty"), `{"snapshots":[],"indices":{}}`)
	liveTarDir := filepath.Join(base, "livetar")
	if err := os.MkdirAll(filepath.Join(liveTarDir, "nodes", "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(liveTarDir, "nodes", "0", "node.lock"), []byte(""))
	liveTar := treeToTar(t, liveTarDir, filepath.Join(base, "live.tar"), "", false)

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "opensearch_backup", repo, nil), "unsupported_source"},
		{"missing archive", provisionPayload(t, "opensearch_repo_tar", filepath.Join(base, "gone.tar"), nil), "source_not_found"},
		{"a directory for the tar kind", provisionPayload(t, "opensearch_repo_tar", repo, nil), "invalid_request"},
		{"a raw data-directory copy", provisionPayload(t, "opensearch_repo", liveDir, nil), "unsupported_source"},
		{"a raw copy inside a tar", provisionPayload(t, "opensearch_repo_tar", liveTar, nil), "unsupported_source"},
		{"a repository listing no snapshots", provisionPayload(t, "opensearch_repo", emptyRepo, nil), "source_corrupt"},
		{"backup_timezone has nothing to declare over epoch instants",
			provisionPayload(t, "opensearch_repo", repo, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"opensearch_repo","path":"` + repo + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
			"invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tc.payload, func(verbCall) (any, *protoError) {
				t.Fatal("a refusal must be decided before any sandbox call")
				return nil, nil
			})
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tc.want {
				t.Errorf("final = %+v, want code %s", f, tc.want)
			}
			if len(calls) != 0 {
				t.Errorf("%d sandbox calls issued before the refusal", len(calls))
			}
		})
	}
}

func TestSandboxPreconditions(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		func(call verbCall) (any, *protoError) {
			return errExec(127, "bash: opensearch: command not found"), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "toolchain") {
		t.Errorf("final = %+v, want invalid_request naming the missing toolchain", f)
	}
}

// TestVersionPrecheckRefusesBeforeTransfer pins the pre-transfer half of
// the pairing gate: the repository's own metadata names a writing engine
// newer than the sandbox, and the refusal lands before a byte moves.
func TestVersionPrecheckRefusesBeforeTransfer(t *testing.T) {
	index := strings.ReplaceAll(fixtureIndex, "2.19.6", "99.9.9")
	repo := writeRepoWithIndex(t, t.TempDir(), index)
	var sequence []string
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "99.9.9") || !strings.Contains(f.Error.Message, "2.19.6") {
		t.Errorf("final = %+v, want invalid_request naming both sides", f)
	}
	if counts := sequenceCounts(sequence); counts["put_file"] != 0 {
		t.Errorf("sequence = %v, want the refusal before any transfer", sequence)
	}
}

// overrideStep answers the happy path except one labelled step.
func overrideStep(t *testing.T, label string, value any) func(verbCall) (any, *protoError) {
	sim := defaultSimulated()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		got, v := classifyExec(argvOf(t, call), sim)
		if got == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		if got == label {
			return value, nil
		}
		return v, nil
	}
}

// TestEngineListingNothingContradictsTheCensus pins the false green the
// census closes: the engine registers a damaged copy silently and lists
// zero snapshots (measured), while the repository's own files claim two.
func TestEngineListingNothingContradictsTheCensus(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		overrideStep(t, "list", outExec(`{}`)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" ||
		!strings.Contains(f.Error.Message, "2 snapshots") || !strings.Contains(f.Error.Message, "snap-1") {
		t.Errorf("final = %+v, want source_corrupt naming the contradiction", f)
	}
}

func TestNewestSnapshotMustBeSuccess(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	partial := strings.Replace(fixtureList, `"snapshot":"snap-2","state":"SUCCESS"`,
		`"snapshot":"snap-2","state":"PARTIAL"`, 1)
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		overrideStep(t, "list", outExec(partial)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "PARTIAL") {
		t.Errorf("final = %+v, want source_corrupt refusing the newest by name, not skipping it", f)
	}
}

// TestRestoreRefusalMapsToInvalidRequest pins the fallback half of the
// pairing gate: when the metadata carried no parseable version, the
// engine's own refusal at restore is surfaced as the pairing problem it
// is — readable only because the API calls run curl without -f.
func TestRestoreRefusalMapsToInvalidRequest(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	blank := strings.ReplaceAll(fixtureList, `"version":"2.19.6"`, `"version":""`)
	sim := defaultSimulated()
	sim.list = outExec(blank)
	sim.restore = outExec(`{"error":{"root_cause":[{"type":"snapshot_restore_exception",` +
		`"reason":"[probavi:snap-2] the snapshot was created with OpenSearch version [3.8.0] ` +
		`which is higher than the version of this node [2.19.6]"}]},"status":500}`)
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			label, v := classifyExec(argvOf(t, call), sim)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argvOf(t, call))
			}
			return v, nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "created with OpenSearch version [3.8.0]") {
		t.Errorf("final = %+v, want invalid_request carrying the engine's refusal", f)
	}
}

// TestShardFailuresAreTheVerdict pins the HTTP-200 trap: the restore
// call returns 200 with failed shards when the repository's data is
// damaged (measured), so the verdict is read from the shard counts.
func TestShardFailuresAreTheVerdict(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		overrideStep(t, "restore",
			outExec(`{"snapshot":{"snapshot":"snap-2","indices":["orders"],"shards":{"total":2,"failed":1}}}`)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "1 of 2 shards") {
		t.Errorf("final = %+v, want source_corrupt read from the shard counts", f)
	}
}

func TestClusterBelowGreenIsTheVerdict(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		overrideStep(t, "health", outExec(`{"status":"red"}`)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "red") {
		t.Errorf("final = %+v, want source_corrupt naming the cluster state", f)
	}
}

// TestStartFailureCarriesTheNodesOwnLine pins the fatal-line watch: a
// node that dies during startup is reported with its own last error
// line, not a bare timeout.
func TestStartFailureCarriesTheNodesOwnLine(t *testing.T) {
	repo := writeRepo(t, t.TempDir())
	sim := defaultSimulated()
	sim.ready = errExec(7, "")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "opensearch_repo", repo, nil),
		func(call verbCall) (any, *protoError) {
			argv := argvOf(t, call)
			switch argv[0] {
			case "grep":
				return okExec(), nil
			case "tail":
				return outExec("fatal error in thread [main]\n" +
					"java.lang.IllegalStateException: failed to obtain node locks\n"), nil
			}
			label, v := classifyExec(argv, sim)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argv)
			}
			return v, nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" ||
		!strings.Contains(f.Error.Message, "IllegalStateException") {
		t.Errorf("final = %+v, want restore_failed carrying the node's own line", f)
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	t.Run("unsupported protocol", func(t *testing.T) {
		stdout := &bytes.Buffer{}
		exit := run(strings.NewReader(`{"protocol":"probavi-adapter/9","request_id":"r-1","op":"probe"}`+"\n"), stdout, io.Discard)
		f := parseFinal(t, stdout.Bytes())
		if exit != 0 || f.OK || f.Error.Code != "unsupported_protocol" {
			t.Errorf("exit=%d final=%+v", exit, f)
		}
		supported, ok := f.Error.Detail["supported"].([]any)
		if !ok || len(supported) == 0 || supported[0] != "probavi-adapter/0" {
			t.Errorf("detail.supported = %v, want the spoken versions (§3.1)", f.Error.Detail)
		}
	})
	t.Run("unknown op", func(t *testing.T) {
		line, _, exit := driveOp(t, "backup", "{}", nil)
		f := parseFinal(t, line)
		if exit != 0 || f.OK || f.Error.Code != "invalid_request" {
			t.Errorf("exit=%d final=%+v", exit, f)
		}
	})
	t.Run("garbage stdin is a crash", func(t *testing.T) {
		stdout := &bytes.Buffer{}
		if exit := run(strings.NewReader("not json\n"), stdout, io.Discard); exit != 1 {
			t.Error("garbage stdin must exit non-zero so the core records adapter_crash")
		}
	})
	t.Run("teardown succeeds on empty state", func(t *testing.T) {
		line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"failed"}`, nil)
		f := parseFinal(t, line)
		if exit != 0 || !f.OK || len(calls) != 0 {
			t.Errorf("exit=%d ok=%v calls=%d", exit, f.OK, len(calls))
		}
	})
}

func TestHealthcheck(t *testing.T) {
	t.Run("an answering node", func(t *testing.T) {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) {
				return outExec(`{"cluster_name":"docker-cluster","status":"green"}`), nil
			})
		f := parseFinal(t, line)
		if exit != 0 || !f.OK {
			t.Fatalf("exit=%d final=%+v", exit, f)
		}
		got := struct {
			Healthy bool    `json:"healthy"`
			Latency float64 `json:"latency_seconds"`
			Detail  string  `json:"detail"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !got.Healthy || got.Latency < 0 || !strings.Contains(got.Detail, "green") {
			t.Errorf("payload = %+v", got)
		}
	})

	t.Run("an unanswering node is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) {
				return errExec(7, "curl: (7) Failed to connect to 127.0.0.1 port 9200"), nil
			})
		f := parseFinal(t, line)
		if !f.OK {
			t.Fatalf("an unhealthy verdict must still be ok:true (§6.3): %+v", f)
		}
		got := struct {
			Healthy bool `json:"healthy"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if got.Healthy {
			t.Error("healthy = true for a node that answered nothing")
		}
	})
}

func TestEngineErrorReason(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  string
		found bool
	}{
		{"root cause wins", `{"error":{"reason":"outer","root_cause":[{"reason":"inner"}]},"status":500}`, "inner", true},
		{"reason alone", `{"error":{"reason":"repository_exception"},"status":500}`, "repository_exception", true},
		{"bare status", `{"error":{},"status":503}`, "status 503", true},
		{"a health answer is no error", `{"status":"green"}`, "", false},
		{"a success is no error", `{"acknowledged":true}`, "", false},
		{"garbage is no error", `not json`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := engineErrorReason([]byte(tc.body))
			if found != tc.found || !strings.Contains(got, tc.want) {
				t.Errorf("engineErrorReason(%q) = %q, %v", tc.body, got, found)
			}
		})
	}
}

func TestLastErrorLine(t *testing.T) {
	log := []byte("[2026-08-16T10:00:00] [INFO ] starting ...\n" +
		"fatal error in thread [main]\n" +
		"java.lang.IllegalStateException: failed to obtain node locks\n")
	if got := lastErrorLine(log); !strings.Contains(got, "IllegalStateException") {
		t.Errorf("lastErrorLine = %q, want the node's own failure report", got)
	}
	if lastErrorLine([]byte("[INFO ] started\n")) != "" {
		t.Error("a healthy log must yield nothing")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine([]byte("  curl: (7) \"refused\"\nsecond\n")); got != "curl: (7) 'refused'" {
		t.Errorf("firstLine = %q, want the first line, trimmed and quote-free", got)
	}
	if firstLine(nil) != "" {
		t.Error("empty input must yield an empty line")
	}
}
