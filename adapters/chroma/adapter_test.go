package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// okExec is the simulated sandbox's success: exit 0 with stdout.
func okExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

// codeExec is a command that failed with a chosen exit code and something
// on stderr — how the sandbox scripts report which verdict they reached.
func codeExec(code int, stderr string) any {
	return execValue{ExitCode: code, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// scriptOf returns the bash script an exec call carries, so a test can tell
// the adapter's steps apart by what they run.
func scriptOf(t *testing.T, call verbCall) string {
	t.Helper()
	argv := argvOf(t, call)
	if len(argv) < 3 || argv[0] != "bash" || argv[1] != "-c" {
		t.Fatalf("exec is not a bash script: %v", argv)
	}
	return argv[2]
}

// sandbox answers every step of a provision the way a healthy one would,
// with the verdict line the test chooses.
func sandbox(t *testing.T, verdict any) func(verbCall) (any, *protoError) {
	t.Helper()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.01}, nil
		}
		script := scriptOf(t, call)
		switch {
		case strings.Contains(script, "n_results"):
			return verdict, nil
		default:
			return okExec("ok\n"), nil
		}
	}
}

func TestProbeResponseMatchesGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", `{}`, func(verbCall) (any, *protoError) {
		t.Fatal("probe issued a sandbox call (§6.1 forbids it)")
		return nil, nil
	})
	if len(calls) != 0 || exit != 0 {
		t.Fatalf("probe issued %d sandbox calls, exit %d", len(calls), exit)
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update once): %v", err)
	}
	if strings.TrimSpace(string(want)) != string(line) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, strings.TrimSpace(string(want)))
	}
}

// TestTheArchiveKindLeads pins an ordering the §10 conformance suite
// depends on: it provisions the first declared kind from 64 KiB of random
// bytes, so a leading kind that reads magic host-side would refuse them and
// fail check 9. The archive kind defers to tar and then to the engine, so
// it can accept those bytes honestly and fail inside the sandbox, where the
// verdict belongs.
func TestTheArchiveKindLeads(t *testing.T) {
	line, _, _ := driveOp(t, "probe", `{}`, nil)
	probe := struct {
		Sources []struct {
			Kind string `json:"kind"`
		} `json:"sources"`
	}{}
	if err := json.Unmarshal(parseFinal(t, line).Payload, &probe); err != nil {
		t.Fatalf("probe payload: %v", err)
	}
	if len(probe.Sources) == 0 || probe.Sources[0].Kind != kindDataTar {
		t.Fatalf("first declared kind is %+v, want %s — the conformance suite hands it random bytes",
			probe.Sources, kindDataTar)
	}
	// And the leading kind must in fact accept them.
	random := filepath.Join(t.TempDir(), "random")
	if err := os.WriteFile(random, []byte(strings.Repeat("\x00\xff", 4096)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := inspect(kindDataTar, random); perr != nil {
		t.Errorf("the leading kind refused arbitrary bytes host-side: %+v", perr)
	}
}

// TestRunnerTemplateShape holds the declared check runner to §6.1: one argv,
// no shell, the placeholders each their own element.
func TestRunnerTemplateShape(t *testing.T) {
	line, _, _ := driveOp(t, "probe", `{}`, nil)
	probe := struct {
		SQLRunner struct {
			Argv []string `json:"argv"`
		} `json:"sql_runner"`
	}{}
	if err := json.Unmarshal(parseFinal(t, line).Payload, &probe); err != nil {
		t.Fatalf("probe payload: %v", err)
	}
	argv := probe.SQLRunner.Argv
	if len(argv) < 4 || argv[0] != "bash" || argv[1] != "-c" {
		t.Fatalf("runner argv is not a bash script: %v", argv)
	}
	if !reflect.DeepEqual(argv[len(argv)-2:], []string{"{{database}}", "{{sql}}"}) {
		t.Errorf("runner does not end with the placeholders: %v", argv)
	}
	for _, a := range argv {
		if strings.Contains(a, "{{password}}") {
			t.Error("the runner carries {{password}} in argv; §6.1 permits it only in env")
		}
	}
}

func TestProvisionRestoresAndReportsRealTimings(t *testing.T) {
	dir := writeDir(t, nil)
	line, calls, exit := driveOp(t, "provision", provisionPayload(t, kindData, dir), sandbox(t, okExec("1\n")))
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v (exit %d)", final.Error, exit)
	}
	res := struct {
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			SizeBytes int64   `json:"size_bytes"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   map[string]any     `json:"state"`
	}{}
	if err := json.Unmarshal(final.Payload, &res); err != nil {
		t.Fatalf("provision payload: %v", err)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum %q", res.SourceIdentity.Checksum)
	}
	if res.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null: nothing in a persistence directory dates the backup",
			*res.SourceIdentity.CreatedAt)
	}
	for _, name := range []string{"engine_ready_seconds", "transfer_seconds", "restore_seconds"} {
		if _, ok := res.Timings[name]; !ok {
			t.Errorf("timings is missing %s (§7)", name)
		}
	}
	var puts int
	for _, call := range calls {
		if call.Verb == "put_file" {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("provision issued %d put_file calls, want 1", puts)
	}
}

// TestProvisionRefusesAnIndexThatDidNotSurvive is the adapter's reason to
// exist. Measured on chromadb/chroma:1.5.9: with the write queue purged, a
// persistence directory whose HNSW segment directory is gone reports every
// record present and answers no nearest-neighbour query at all — silently.
// The drill must fail, and must say why in terms an operator can act on.
func TestProvisionRefusesAnIndexThatDidNotSurvive(t *testing.T) {
	dir := writeDir(t, nil)
	verdict := codeExec(exitIndexLost,
		"collection c9da8d60-2e4d-411a-a641-e5201a0da941 holds 2200 records and answered a "+
			"nearest-neighbour query with 0 of the 5 asked for\n")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, kindData, dir), sandbox(t, verdict))
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("provision passed a restore whose vector index was gone")
	}
	if final.Error.Code != "source_corrupt" {
		t.Errorf("code = %q, want source_corrupt", final.Error.Code)
	}
	for _, want := range []string{"2200", "nearest-neighbour", "count is not the proof", "server stopped"} {
		if !strings.Contains(final.Error.Message, want) {
			t.Errorf("message does not mention %q: %s", want, final.Error.Message)
		}
	}
}

func TestProvisionRefusesARestoreThatServesNothing(t *testing.T) {
	dir := writeDir(t, nil)
	line, _, _ := driveOp(t, "provision", provisionPayload(t, kindData, dir),
		sandbox(t, codeExec(exitNoCollections, "no collections\n")))
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "restore_failed" {
		t.Fatalf("got %+v, want restore_failed", final.Error)
	}
	if !strings.Contains(final.Error.Message, "backup of nothing") {
		t.Errorf("message: %s", final.Error.Message)
	}
}

func TestProvisionRefusesAnUndeclaredParam(t *testing.T) {
	dir := writeDir(t, nil)
	payload := provisionPayload(t, kindData, dir)
	payload = strings.Replace(payload, `"kind":"`+kindData+`"`,
		`"kind":"`+kindData+`","params":{"backup_timezone":"UTC"}`, 1)
	line, calls, _ := driveOp(t, "provision", payload, sandbox(t, okExec("1\n")))
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Fatalf("got %+v, want invalid_request", final.Error)
	}
	if len(calls) != 0 {
		t.Errorf("the refusal cost %d sandbox calls; it is answerable before any", len(calls))
	}
}

func TestHealthcheckReportsAVerdictRatherThanFailing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       any
		wantHealthy bool
	}{
		{"answering", okExec(""), true},
		{"silent", codeExec(1, ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`,
				func(verbCall) (any, *protoError) { return tc.value, nil })
			final := parseFinal(t, line)
			if !final.OK {
				t.Fatalf("healthcheck returned a final error: %+v", final.Error)
			}
			res := struct {
				Healthy bool    `json:"healthy"`
				Latency float64 `json:"latency_seconds"`
			}{}
			if err := json.Unmarshal(final.Payload, &res); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if res.Healthy != tc.wantHealthy {
				t.Errorf("healthy = %v, want %v", res.Healthy, tc.wantHealthy)
			}
		})
	}
}

func TestTeardownIsIdempotentAndTouchesNothing(t *testing.T) {
	for range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"completed"}`,
			func(verbCall) (any, *protoError) {
				t.Fatal("teardown issued a sandbox call; the provider destroys the sandbox")
				return nil, nil
			})
		if !parseFinal(t, line).OK || len(calls) != 0 || exit != 0 {
			t.Fatalf("teardown: ok=%v calls=%d exit=%d", parseFinal(t, line).OK, len(calls), exit)
		}
	}
}

// unpackSandbox answers an archive provision and records the script that
// ran tar, so a test can assert which flags it chose.
func unpackSandbox(t *testing.T, unpack *string) func(verbCall) (any, *protoError) {
	t.Helper()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			want := newSandboxPaths(testScratch).archive
			if args.DestPath != want {
				t.Errorf("archive went to %s, want %s", args.DestPath, want)
			}
			// Whatever the adapter composes, it composes it inside the
			// directory the provider guaranteed: / is not writable on a
			// bare host (#287).
			if !strings.HasPrefix(args.DestPath, testScratch+"/") {
				t.Errorf("archive went to %s, want it under the scratch directory %s", args.DestPath, testScratch)
			}
			return putFileValue{BytesCopied: 16}, nil
		}
		script := scriptOf(t, call)
		if strings.Contains(script, "tar ") {
			*unpack = script
		}
		if strings.Contains(script, "n_results") {
			return okExec("1\n"), nil
		}
		return okExec("ok\n"), nil
	}
}

// TestProvisionUnpacksAnArchive walks the other source kind end to end and
// pins the extraction flags to the artifact's own bytes.
func TestProvisionUnpacksAnArchive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		head     []byte
		wantFlag string
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00, 'x'}, "-xzf"},
		{"plain", []byte("ustar\x00 and the rest"), "-xf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup.tar")
			if err := os.WriteFile(path, tc.head, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			var unpack string
			line, calls, _ := driveOp(t, "provision", provisionPayload(t, kindDataTar, path),
				unpackSandbox(t, &unpack))
			if final := parseFinal(t, line); !final.OK {
				t.Fatalf("provision failed: %+v", final.Error)
			}
			if !strings.Contains(unpack, "tar "+tc.wantFlag+" ") {
				t.Errorf("unpack script does not use %s:\n%s", tc.wantFlag, unpack)
			}
			if len(calls) == 0 {
				t.Error("provision issued no sandbox calls")
			}
		})
	}
}

// TestArchiveWithoutADatabaseIsRefusedByName keeps the two unpack failures
// apart: an archive that held no metadata database is a different sentence
// from a shell that broke.
func TestArchiveWithoutADatabaseIsRefusedByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.tar")
	if err := os.WriteFile(path, []byte("ustar\x00"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, tc := range []struct {
		name   string
		exit   int
		wantIn string
	}{
		{"no database inside", exitNoDatabase, "at its root or under a single wrapping directory"},
		{"the unpack itself broke", 1, "unpack the backup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", provisionPayload(t, kindDataTar, path),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "put_file" {
						return putFileValue{BytesCopied: 6}, nil
					}
					if strings.Contains(scriptOf(t, call), "tar ") {
						return codeExec(tc.exit, "tar: broken\n"), nil
					}
					return okExec("ok\n"), nil
				})
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != "source_corrupt" {
				t.Fatalf("got %+v, want source_corrupt", final.Error)
			}
			if !strings.Contains(final.Error.Message, tc.wantIn) {
				t.Errorf("message %q does not mention %q", final.Error.Message, tc.wantIn)
			}
		})
	}
}

// TestATransferredDirectoryThatArrivedShortIsNamedForWhatItIs pins the
// directory kind's own unpack failure: it cannot be "the archive holds no
// database", because there is no archive.
func TestATransferredDirectoryThatArrivedShortIsNamedForWhatItIs(t *testing.T) {
	line, _, _ := driveOp(t, "provision", provisionPayload(t, kindData, writeDir(t, nil)),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{BytesCopied: 10}, nil
			}
			if strings.Contains(scriptOf(t, call), "mv ") {
				return codeExec(exitNoDatabase, ""), nil
			}
			return okExec("ok\n"), nil
		})
	final := parseFinal(t, line)
	if final.OK || !strings.Contains(final.Error.Message, "did not arrive whole") {
		t.Fatalf("got %+v, want the directory wording", final.Error)
	}
}

// TestEngineThatNeverAnswersSaysWhatItSaid keeps a readiness timeout from
// reporting the budget instead of the engine's own reason.
func TestEngineThatNeverAnswersSaysWhatItSaid(t *testing.T) {
	line, _, _ := driveOp(t, "provision", provisionPayload(t, kindData, writeDir(t, nil)),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{BytesCopied: 10}, nil
			}
			script := scriptOf(t, call)
			switch {
			case strings.Contains(script, "nohup"):
				return codeExec(1, "chroma: cannot bind 8000\n"), nil
			default:
				return okExec("ok\n"), nil
			}
		})
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "engine_not_ready" {
		t.Fatalf("got %+v, want engine_not_ready", final.Error)
	}
	if !strings.Contains(final.Error.Message, "cannot bind") {
		t.Errorf("message drops what the engine said: %s", final.Error.Message)
	}
}

// TestSandboxPreparationFailureIsTheSandbox's, not the backup's.
func TestSandboxPreparationFailureIsNotTheBackupsFault(t *testing.T) {
	line, _, _ := driveOp(t, "provision", provisionPayload(t, kindData, writeDir(t, nil)),
		func(call verbCall) (any, *protoError) {
			return codeExec(1, "mkdir: read-only file system\n"), nil
		})
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "sandbox_error" {
		t.Fatalf("got %+v, want sandbox_error", final.Error)
	}
}

func TestMalformedPayloadsAreInvalidRequests(t *testing.T) {
	for _, op := range []string{"provision", "healthcheck"} {
		t.Run(op, func(t *testing.T) {
			line, _, exit := driveOp(t, op, `"not an object"`, nil)
			final := parseFinal(t, line)
			if final.OK || final.Error.Code != "invalid_request" || exit != 0 {
				t.Fatalf("got %+v exit %d, want invalid_request with exit 0", final.Error, exit)
			}
		})
	}
}
