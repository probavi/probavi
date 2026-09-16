package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func okExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

func codeExec(code int, stderr string) any {
	return execValue{ExitCode: code, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// realProperties is what propertiesScript prints for a copy of a 2.0.11
// node addressed by loopback, in the image both suites run.
const realProperties = `cn.cn_internal_address=127.0.0.1
cn.cn_internal_port=10710
cn.cn_consensus_port=10720
cn.config_node_list=0,127.0.0.1\:10710,127.0.0.1\:10720
cn.iotdb_version=2.0.11
cn.timestamp_precision=ms
dn.dn_internal_address=127.0.0.1
dn.dn_internal_port=10730
dn.dn_rpc_address=127.0.0.1
dn.dn_rpc_port=6667
dn.iotdb_version=2.0.11
engine.home=/iotdb
engine.version=2.0.11
`

// fakeSandbox answers each script this adapter runs by what the script is,
// and records what it was asked.
type fakeSandbox struct {
	t          *testing.T
	properties string
	ready      any
	verdict    any
	start      any
	place      any
	calls      []string
	args       map[string][]string
	env        map[string]map[string]string
	putDest    string
}

func newFakeSandbox(t *testing.T) *fakeSandbox {
	return &fakeSandbox{
		t: t, properties: realProperties,
		ready: okExec(""), verdict: okExec("123\n"), start: okExec("started\n"), place: okExec(""),
		args: map[string][]string{}, env: map[string]map[string]string{},
	}
}

// scripts names every script by its text, so an assertion reads as a step.
var scripts = map[string]string{
	prepareScript: "prepare", placeDirScript: "place", placeTarScript(false): "place",
	placeTarScript(true): "place", propertiesScript: "properties", hostsScript: "hosts",
	configureScript: "configure", startScript: "start", readyScript: "ready",
	verdictScript: "verdict", healthScript: "health", startupErrorScript: "startup-error",
}

func (f *fakeSandbox) handle(call verbCall) (any, *protoError) {
	if call.Verb == "put_file" {
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			f.t.Fatalf("put_file args: %v", err)
		}
		f.putDest = args.DestPath
		f.calls = append(f.calls, "put_file")
		return putFileValue{BytesCopied: 10}, nil
	}
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		f.t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) < 4 || args.Argv[0] != "bash" || args.Argv[1] != "-c" || args.Argv[3] != "bash" {
		f.t.Fatalf("exec is not a positional bash script: %q", args.Argv)
	}
	name, ok := scripts[args.Argv[2]]
	if !ok {
		f.t.Fatalf("unknown script issued:\n%s", args.Argv[2])
	}
	f.calls = append(f.calls, name)
	f.args[name] = args.Argv[4:]
	f.env[name] = args.Env
	switch name {
	case "properties":
		return okExec(f.properties), nil
	case "ready":
		return f.ready, nil
	case "verdict":
		return f.verdict, nil
	case "start":
		return f.start, nil
	case "place":
		return f.place, nil
	case "startup-error":
		return okExec("ERROR o.a.i.d.s.DataNode - Create Data Region failed\n"), nil
	default:
		return okExec(""), nil
	}
}

// writeCopy lays out the smallest tree inspect accepts: both nodes' system
// properties, under prefix inside a fresh directory.
func writeCopy(t *testing.T, prefix string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{confignodeProperties, datanodeProperties} {
		p := filepath.Join(dir, prefix, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("iotdb_version=2.0.11\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func provisionWith(t *testing.T, kind, path string, options map[string]string, credentialEnv []string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path, "credential_env": credentialEnv},
		"sandbox": map[string]any{"scratch_dir": testScratch},
		"options": options,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProbeResponseMatchesGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", `{}`, nil)
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("probe exit=%d calls=%d", exit, len(calls))
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(line)) {
		t.Errorf("probe response differs from golden:\ngot  %s\nwant %s", line, want)
	}
}

// probed is the probe payload as the core reads it.
type probed struct {
	Sources []struct {
		Kind string `json:"kind"`
	} `json:"sources"`
	SQLRunner struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
	} `json:"sql_runner"`
}

func readProbe(t *testing.T) probed {
	t.Helper()
	raw, err := json.Marshal(probePayload())
	if err != nil {
		t.Fatal(err)
	}
	p := probed{}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheArchiveKindLeads pins the order the conformance suite depends on:
// it provisions the first declared kind from random bytes.
func TestTheArchiveKindLeads(t *testing.T) {
	sources := readProbe(t).Sources
	if len(sources) != 2 || sources[0].Kind != kindDataTar || sources[1].Kind != kindData {
		t.Errorf("sources = %v, want %s then %s", sources, kindDataTar, kindData)
	}
}

// TestRunnerKeepsThePasswordOffTheArgumentList pins §6.1: {{sql}} is its
// own argv element and the password is an environment value only.
func TestRunnerKeepsThePasswordOffTheArgumentList(t *testing.T) {
	runner := readProbe(t).SQLRunner
	if !slices.Contains(runner.Argv, "{{sql}}") {
		t.Errorf("argv %q lacks a {{sql}} element", runner.Argv)
	}
	for _, a := range runner.Argv {
		if strings.Contains(a, "{{password}}") {
			t.Errorf("argv carries {{password}}: %q", a)
		}
	}
	if runner.Env[passwordEnv] != "{{password}}" {
		t.Errorf("env = %v, want the password under %s", runner.Env, passwordEnv)
	}
}

func TestProvisionRunsTheStepsInOrder(t *testing.T) {
	f := newFakeSandbox(t)
	line, _, exit := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, "data"), nil, nil), f.handle)
	if final := parseFinal(t, line); exit != 0 || !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	want := []string{"prepare", "put_file", "place", "properties", "configure", "start", "ready", "verdict"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("steps = %v, want %v", f.calls, want)
	}
	if f.putDest != testScratch+"/probavi-iotdb/staging" {
		t.Errorf("put_file destination = %s, want the staging directory under scratch_dir", f.putDest)
	}
	settings := strings.Join(f.args["configure"], " ")
	for _, want := range []string{"cn_internal_address=127.0.0.1", "dn_rpc_port=6667", "cn_seed_config_node=127.0.0.1:10710"} {
		if !strings.Contains(settings, want) {
			t.Errorf("configure args %q lack %s", settings, want)
		}
	}
	if f.args["configure"][1] != "/iotdb" {
		t.Errorf("configure reads the engine from %s, want the home the sandbox reported", f.args["configure"][1])
	}
}

func TestProvisionReportsWhatItMeasured(t *testing.T) {
	f := newFakeSandbox(t)
	line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, "data"), nil, nil), f.handle)
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	payload := struct {
		Connection     map[string]any `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   map[string]any     `json:"state"`
	}{}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Connection["user"] != defaultUser || payload.Connection["database"] != "" {
		t.Errorf("connection = %v, want the tree dialect as %s", payload.Connection, defaultUser)
	}
	if _, has := payload.Connection["password_env"]; has {
		t.Errorf("connection names a password_env the drill never declared: %v", payload.Connection)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") || payload.SourceIdentity.CreatedAt != nil {
		t.Errorf("source_identity = %+v, want a sha256 and no creation time", payload.SourceIdentity)
	}
	for _, k := range []string{"engine_ready_seconds", "transfer_seconds", "restore_seconds"} {
		if v, ok := payload.Timings[k]; !ok || v < 0 {
			t.Errorf("timing %s = %v", k, v)
		}
	}
	if payload.State["values_read"] != float64(123) {
		t.Errorf("state = %v, want the verdict's count", payload.State)
	}
}

func TestProvisionUnpacksAnArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "copy.tar.gz")
	if err := os.WriteFile(archive, []byte{0x1f, 0x8b, 8, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeSandbox(t)
	line, calls, _ := driveOp(t, "provision", provisionWith(t, kindDataTar, archive, nil, nil), f.handle)
	if final := parseFinal(t, line); !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	if f.putDest != testScratch+"/probavi-iotdb/backup.tar" {
		t.Errorf("put_file destination = %s, want the archive path under scratch_dir", f.putDest)
	}
	for _, call := range calls {
		if call.Verb == "exec" && scripts[argvOf(t, call)[2]] == "place" && argvOf(t, call)[2] != placeTarScript(true) {
			t.Error("a gzip archive was placed with the plain-tar script")
		}
	}
}

func TestPlacementRefusalsNameWhatWasWrong(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		place      any
		code, says string
	}{
		{"no data directory in an archive", kindDataTar, codeExec(exitNoData, ""), "source_corrupt", "holds no IoTDB data directory"},
		{"a directory that arrived short", kindData, codeExec(exitNoData, ""), "source_corrupt", "did not arrive whole"},
		{"two nodes in one archive", kindDataTar, codeExec(exitAmbiguous, ""), "source_corrupt", "more than one"},
		{"an archive tar cannot read", kindDataTar, codeExec(exitUnpack, "tar: Unexpected EOF\n"), "source_corrupt", "Unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCopy(t, "")
			if tc.kind == kindDataTar {
				path = filepath.Join(t.TempDir(), "copy.tar")
				if err := os.WriteFile(path, []byte("not really a tar"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f := newFakeSandbox(t)
			f.place = tc.place
			line, _, _ := driveOp(t, "provision", provisionWith(t, tc.kind, path, nil, nil), f.handle)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != tc.code || !strings.Contains(final.Error.Message, tc.says) {
				t.Errorf("got %+v, want %s saying %q", final.Error, tc.code, tc.says)
			}
		})
	}
}

// TestCopiesTheSandboxCannotStartAreRefusedBeforeItTries pins the two
// fences that stand in front of a readiness timeout.
func TestCopiesTheSandboxCannotStartAreRefusedBeforeItTries(t *testing.T) {
	for _, tc := range []struct {
		name, properties, says string
	}{
		{"a newer line's copy", strings.ReplaceAll(realProperties, "engine.version=2.0.11", "engine.version=1.3.7"),
			"written by IoTDB 2.0.11 and the sandbox runs 1.3.7"},
		{"a node on a routable address", strings.ReplaceAll(realProperties, "cn.cn_internal_address=127.0.0.1", "cn.cn_internal_address=10.9.8.7"),
			"listen on 10.9.8.7 (cn_internal_address)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSandbox(t)
			f.properties = tc.properties
			line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil), f.handle)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != "restore_failed" || !strings.Contains(final.Error.Message, tc.says) {
				t.Errorf("got %+v, want restore_failed saying %q", final.Error, tc.says)
			}
			if slices.Contains(f.calls, "start") {
				t.Error("the engine was started on a copy that was already refused")
			}
		})
	}
}

// TestANamedNodeIsMappedToLoopback pins the host-name route: the name is
// mapped before the configuration that names it is written.
func TestANamedNodeIsMappedToLoopback(t *testing.T) {
	f := newFakeSandbox(t)
	f.properties = strings.ReplaceAll(realProperties, "127.0.0.1", "iotdb-prod-1")
	line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil), f.handle)
	if final := parseFinal(t, line); !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	if !slices.Equal(f.args["hosts"], []string{"iotdb-prod-1"}) {
		t.Errorf("hosts mapped = %v, want the one name the copy records", f.args["hosts"])
	}
	if slices.Index(f.calls, "hosts") > slices.Index(f.calls, "configure") {
		t.Errorf("steps = %v: the name was mapped after the configuration naming it was written", f.calls)
	}
	if !slices.Contains(f.args["configure"], "dn_seed_config_node=iotdb-prod-1:10710") {
		t.Errorf("configure args = %v, want the seed addressed by name", f.args["configure"])
	}
}

// TestConformanceStandInsAreNotJudged pins the order that lets the §10
// suite's simulated sandbox reach a verdict: output that is not the
// property script's own carries no version and no address to refuse.
func TestConformanceStandInsAreNotJudged(t *testing.T) {
	f := newFakeSandbox(t)
	f.properties = "1"
	line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil), f.handle)
	if final := parseFinal(t, line); !final.OK {
		t.Fatalf("provision refused a stand-in answer: %+v", final.Error)
	}
	if len(f.args["configure"]) != 3 {
		t.Errorf("configure args = %v, want only the paths when nothing was recorded", f.args["configure"])
	}
}

func TestVerdictsBecomeTheErrorsTheyAre(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict any
		code    string
		says    string
	}{
		{"restores nothing", codeExec(exitRestoredNothing, ""), "restore_failed", "restores to nothing"},
		{"a page that does not decode", codeExec(exitUndecodable, "root.rig: 724: Failed to decode page data\n"), "source_corrupt", "724: Failed to decode"},
		{"a TTL hiding a scope", codeExec(exitTTLHidesAll, "tree\troot.ttl.**\t300000\n"), "restore_failed", "tree root.ttl.** holds data and reads no row: the copy carries a TTL of 5m0s"},
		{"a read that disagrees", codeExec(exitReadDisagrees, "root.rig\n"), "source_corrupt", "reading every value of root.rig"},
		{"a missing table database", codeExec(exitNoSuchDatabase, "adb information \n"), "invalid_request", "(it holds: adb information)"},
		{"a 1.3 copy has no table model", codeExec(exitNoSuchDatabase, ""), "invalid_request", "no table model"},
		{"any other refusal", codeExec(exitQueryFailed, "list the TTLs: 301: EXECUTE_STATEMENT_ERROR\n"), "restore_failed", "list the TTLs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSandbox(t)
			f.verdict = tc.verdict
			line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil), f.handle)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != tc.code || !strings.Contains(final.Error.Message, tc.says) {
				t.Errorf("got %+v, want %s saying %q", final.Error, tc.code, tc.says)
			}
		})
	}
}

func TestLoginRefusalSaysWhichPasswordWasTried(t *testing.T) {
	t.Setenv("IOTDB_PROD_PASSWORD", "hunter2")
	for _, tc := range []struct {
		name     string
		options  map[string]string
		declared []string
		says     string
	}{
		{"the default", nil, nil, "IoTDB's default password"},
		{"a declared one", map[string]string{"password_env": "IOTDB_PROD_PASSWORD"}, []string{"IOTDB_PROD_PASSWORD"}, "the password in IOTDB_PROD_PASSWORD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSandbox(t)
			f.ready = codeExec(exitLoginRefused, "")
			line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), tc.options, tc.declared), f.handle)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != "invalid_request" || !strings.Contains(final.Error.Message, tc.says) {
				t.Errorf("got %+v, want invalid_request saying %q", final.Error, tc.says)
			}
			if strings.Contains(string(line), "hunter2") {
				t.Error("the password value reached a protocol message")
			}
		})
	}
}

// TestThePasswordTravelsInTheEnvironment pins where the secret goes: into
// the environment of the scripts that read the engine, and nowhere else.
func TestThePasswordTravelsInTheEnvironment(t *testing.T) {
	t.Setenv("IOTDB_PROD_PASSWORD", "hunter2")
	f := newFakeSandbox(t)
	options := map[string]string{"password_env": "IOTDB_PROD_PASSWORD", "user": "auditor", "database": "rig_t"}
	line, calls, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), options, []string{"IOTDB_PROD_PASSWORD"}), f.handle)
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	for _, step := range []string{"ready", "verdict"} {
		env := f.env[step]
		if env[passwordEnv] != "hunter2" || env["IOTDB_USER"] != "auditor" || env["IOTDB_DATABASE"] != "rig_t" {
			t.Errorf("%s env = %v, want the login and the database", step, env)
		}
	}
	for _, call := range calls {
		if call.Verb == "exec" && slices.Contains(argvOf(t, call), "hunter2") {
			t.Errorf("the password reached an argument list: %q", argvOf(t, call))
		}
	}
	connection := struct {
		Connection map[string]any `json:"connection"`
	}{}
	if err := json.Unmarshal(final.Payload, &connection); err != nil {
		t.Fatal(err)
	}
	if connection.Connection["password_env"] != "IOTDB_PROD_PASSWORD" || connection.Connection["database"] != "rig_t" {
		t.Errorf("connection = %v, want the variable's name and the database", connection.Connection)
	}
	if strings.Contains(string(line), "hunter2") {
		t.Error("the password value reached the final response")
	}
}

func TestDrillConfigMistakesAreRefusedBeforeAnythingMoves(t *testing.T) {
	t.Setenv("IOTDB_EMPTY", "")
	for _, tc := range []struct {
		name     string
		options  map[string]string
		declared []string
		payload  string
		says     string
	}{
		{"an undeclared password variable", map[string]string{"password_env": "IOTDB_X"}, nil, "", "does not list it"},
		{"an empty password variable", map[string]string{"password_env": "IOTDB_EMPTY"}, []string{"IOTDB_EMPTY"}, "", "is empty"},
		{"an option nothing reads", map[string]string{"sql_dialect": "table"}, nil, "", "options.sql_dialect has no effect"},
		{"a source parameter", nil, nil, `{"source":{"kind":"iotdb_data","path":"/x","params":{"backup_timezone":"UTC"}}}`, "source.params.backup_timezone has no effect"},
		{"a point-in-time target", nil, nil, `{"source":{"kind":"iotdb_data","path":"/x"},"pitr":{"target_time":"2026-09-16T00:00:00Z"}}`, "no point-in-time recovery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.payload
			if payload == "" {
				payload = provisionWith(t, kindData, writeCopy(t, ""), tc.options, tc.declared)
			}
			line, calls, _ := driveOp(t, "provision", payload, nil)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != "invalid_request" || !strings.Contains(final.Error.Message, tc.says) {
				t.Errorf("got %+v, want invalid_request saying %q", final.Error, tc.says)
			}
			if len(calls) != 0 {
				t.Errorf("%d sandbox calls issued before the config was judged", len(calls))
			}
		})
	}
}

// TestEngineThatNeverStartsSaysWhatItSaid keeps a start failure from
// reporting a generic timeout instead of the engine's own reason.
func TestEngineThatNeverStartsSaysWhatItSaid(t *testing.T) {
	f := newFakeSandbox(t)
	f.start = codeExec(1, "start-confignode.sh: command not found\n")
	line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil), f.handle)
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "engine_not_ready" || !strings.Contains(final.Error.Message, "command not found") {
		t.Errorf("got %+v, want engine_not_ready carrying what the start said", final.Error)
	}
}

func TestSandboxPreparationFailureIsNotTheBackupsFault(t *testing.T) {
	line, _, _ := driveOp(t, "provision", provisionWith(t, kindData, writeCopy(t, ""), nil, nil),
		func(call verbCall) (any, *protoError) { return codeExec(1, "mkdir: read-only file system\n"), nil })
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "sandbox_error" || !final.Error.Retryable {
		t.Errorf("got %+v, want a retryable sandbox_error", final.Error)
	}
}

func TestHealthcheckReportsAVerdictRatherThanFailing(t *testing.T) {
	t.Setenv("IOTDB_PROD_PASSWORD", "hunter2")
	for _, tc := range []struct {
		name    string
		answer  any
		healthy bool
	}{
		{"serving", okExec("2.0.11\t7c0de06\n"), true},
		{"refusing", codeExec(1, "the engine's REST service did not answer\n"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env map[string]string
			line, _, _ := driveOp(t, "healthcheck",
				`{"connection":{"user":"auditor","password_env":"IOTDB_PROD_PASSWORD"},"state":{}}`,
				func(call verbCall) (any, *protoError) {
					args := execArgs{}
					if err := json.Unmarshal(call.Args, &args); err != nil {
						t.Fatal(err)
					}
					env = args.Env
					return tc.answer, nil
				})
			final := parseFinal(t, line)
			result := struct {
				Healthy bool    `json:"healthy"`
				Latency float64 `json:"latency_seconds"`
			}{}
			if err := json.Unmarshal(final.Payload, &result); err != nil || !final.OK {
				t.Fatalf("healthcheck = %s (%v)", line, err)
			}
			if result.Healthy != tc.healthy || result.Latency < 0 {
				t.Errorf("result = %+v, want healthy=%v", result, tc.healthy)
			}
			if env[passwordEnv] != "hunter2" || env["IOTDB_USER"] != "auditor" {
				t.Errorf("healthcheck env = %v, want the connection's login", env)
			}
		})
	}
}

func TestTeardownIsIdempotentAndTouchesNothing(t *testing.T) {
	for i := range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"completed"}`, nil)
		if final := parseFinal(t, line); exit != 0 || !final.OK || len(calls) != 0 {
			t.Fatalf("teardown %d: exit=%d ok=%v calls=%d", i, exit, final.OK, len(calls))
		}
	}
}

func TestMalformedPayloadsAreInvalidRequests(t *testing.T) {
	for _, op := range []string{"provision", "healthcheck"} {
		t.Run(op, func(t *testing.T) {
			line, _, exit := driveOp(t, op, `"not an object"`, nil)
			if final := parseFinal(t, line); exit != 0 || final.OK || final.Error.Code != "invalid_request" {
				t.Errorf("got exit=%d %+v, want invalid_request", exit, final.Error)
			}
		})
	}
}
