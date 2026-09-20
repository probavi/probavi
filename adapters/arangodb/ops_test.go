package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func overrideStep(t *testing.T, label string, value any) func(verbCall) (any, *protoError) {
	sim := defaultSimulated()
	switch label {
	case "restore":
		sim.restore = value
	case "count":
		sim.count = value
	case "ready":
		sim.ready = value
	case "unpack":
		sim.unpack = value
	case "database":
		sim.database = value
	default:
		t.Fatalf("no such step: %s", label)
	}
	var sequence []string
	return provisionHandler(t, &sequence, sim)
}

func provisionDump(t *testing.T, label string, value any) finalResponse {
	t.Helper()
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		overrideStep(t, label, value))
	return parseFinal(t, line)
}

func wantCode(t *testing.T, f finalResponse, code, phrase string) {
	t.Helper()
	if f.OK {
		t.Fatalf("the operation succeeded, want %s", code)
	}
	if f.Error.Code != code {
		t.Errorf("code = %q, want %q (message: %s)", f.Error.Code, code, f.Error.Message)
	}
	if !strings.Contains(f.Error.Message, phrase) {
		t.Errorf("message = %q, want it to mention %q", f.Error.Message, phrase)
	}
}

// TestTheRestoreExitCodeIsNotTheVerdict is the measurement this adapter
// is built around: pointed at an empty dump directory, arangorestore
// reports "Processed 0 collection(s)" and exits 0. What the restored
// database holds is the verdict.
func TestTheRestoreExitCodeIsNotTheVerdict(t *testing.T) {
	f := provisionDump(t, "count", outExec("0\n"))
	wantCode(t, f, "source_corrupt", "the tool reports that as success")
}

// TestADamagedDumpKeepsItsDocumentsOutOfTheRecord: the tool's error
// quotes the request payload — hundreds of restored production documents
// — and a record must be shareable as it stands.
func TestADamagedDumpKeepsItsDocumentsOutOfTheRecord(t *testing.T) {
	leak := `got invalid response from server: HTTP 400: 'received invalid JSON data for ` +
		`collection 'orders' on line 243' while executing restoring data with this request ` +
		`payload: '{"_id":"orders/k1","_key":"k1","v":"row1"}'`
	f := provisionDump(t, "restore", errExec(1, leak))
	wantCode(t, f, "source_corrupt", "the engine refused the dump")
	for _, secret := range []string{"_key", "row1", "request payload"} {
		if strings.Contains(f.Error.Message, secret) {
			t.Errorf("the message carries %q out of the engine's own diagnostics: %s",
				secret, f.Error.Message)
		}
	}
	if !strings.Contains(f.Error.Message, "kept out of this record deliberately") {
		t.Errorf("message = %q, want it to say where the detail went", f.Error.Message)
	}
}

func TestAReadBackThatFailsIsAFailedRestore(t *testing.T) {
	f := provisionDump(t, "count", errExec(1, "not connected"))
	wantCode(t, f, "restore_failed", "reading back database shop failed")

	f = provisionDump(t, "count", outExec("not a number\n"))
	wantCode(t, f, "restore_failed", "did not report a collection count")
}

// TestAServerThatNeverAnswersIsNamedByItsOwnLog rather than by a bare
// timeout.
func TestAServerThatNeverAnswersIsNamedByItsOwnLog(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	sim := defaultSimulated()
	sim.ready = errExec(1, "not connected")
	var sequence []string
	base := provisionHandler(t, &sequence, sim)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				switch argvOf(t, call)[0] {
				case "grep":
					// The log carries a fatal line, so the wait ends now
					// rather than at the end of the budget.
					return execValue{ExitCode: 0}, nil
				case "tail":
					return outExec("FATAL cannot open database directory\n"), nil
				}
			}
			return base(call)
		})
	f := parseFinal(t, line)
	wantCode(t, f, "restore_failed", "cannot open database directory")
}

func TestAnImageWithoutTheToolchainIsNamed(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				return errExec(127, "arangorestore: not found"), nil
			}
			return putFileValue{}, nil
		})
	wantCode(t, parseFinal(t, line), "invalid_request", "lacks the ArangoDB toolchain")
}

// TestTheArchivePathUnpacksAndLocatesTheDump, and reads the database name
// out of the unpacked dump when the host could not walk the archive.
func TestTheArchivePathUnpacksAndLocatesTheDump(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump_tar", tarOf(t, dir, ""), nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("archive provision failed: %+v", f.Error)
	}
	for _, want := range []string{"put_file", "unpack", "locate"} {
		if !contains(sequence, want) {
			t.Errorf("the archive flow never did %q; it did %v", want, sequence)
		}
	}
}

func TestAnOpaqueArchiveAsksTheSandboxForTheDatabase(t *testing.T) {
	opaque := t.TempDir() + "/opaque.tar"
	writeFile(t, opaque, "not tar at all\n")

	t.Run("named", func(t *testing.T) {
		var sequence []string
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "arangodb_dump_tar", opaque, nil),
			provisionHandler(t, &sequence, defaultSimulated()))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("provision failed: %+v", f.Error)
		}
		if !contains(sequence, "database") {
			t.Errorf("the flow never asked the sandbox for the database: %v", sequence)
		}
	})

	t.Run("nameless", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "arangodb_dump_tar", opaque, nil),
			overrideStep(t, "database", outExec("\n")))
		wantCode(t, parseFinal(t, line), "source_corrupt", "states no database")
	})

	t.Run("a name that cannot be used", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "arangodb_dump_tar", opaque, nil),
			overrideStep(t, "database", outExec("1shop\n")))
		wantCode(t, parseFinal(t, line), "invalid_request", "not one this adapter can put into")
	})
}

func TestAnArchiveTarCannotUnpackIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump_tar", tarOf(t, dir, ""), nil),
		overrideStep(t, "unpack", errExec(2, "unexpected EOF")))
	wantCode(t, parseFinal(t, line), "source_corrupt", "tar could not unpack")
}

func TestProvisionRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())
	noSandbox := func(verbCall) (any, *protoError) {
		t.Fatal("a refused request must not touch the sandbox")
		return nil, nil
	}
	t.Run("malformed payload", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", `"not an object"`, noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "malformed provision payload")
	})
	t.Run("pitr", func(t *testing.T) {
		payload := `{"source":{"kind":"arangodb_dump","path":"` + dir +
			`"},"pitr":{"target_time":"2026-09-20T10:00:00Z"}}`
		line, _, _ := driveOp(t, "provision", payload, noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "does not support pitr")
	})
	t.Run("a declared timezone", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "arangodb_dump", dir, map[string]string{"backup_timezone": "UTC"}),
			noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "has no effect for this adapter")
	})
}

// TestTheEngineIsStartedWithItsExpiryThreadHeldBack is issue #166: the
// flag is passed explicitly rather than relied on as a default.
func TestTheEngineIsStartedWithItsExpiryThreadHeldBack(t *testing.T) {
	args := strings.Join(engineArgs(), " ")
	if !strings.Contains(args, "--ttl.frequency 0") {
		t.Errorf("engineArgs = %q, want the TTL background thread turned off", args)
	}
	for _, want := range []string{"--server.authentication false", "--database.directory", endpoint} {
		if !strings.Contains(args, want) {
			t.Errorf("engineArgs = %q, want it to carry %q", args, want)
		}
	}
}

func TestHealthcheckReportsRatherThanFails(t *testing.T) {
	answer := func(value any) map[string]any {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return value, nil })
		if exit != 0 {
			t.Fatalf("healthcheck exit = %d, want 0 — an unhealthy server is a result", exit)
		}
		f := parseFinal(t, line)
		if !f.OK {
			t.Fatalf("healthcheck failed: %+v", f.Error)
		}
		out := map[string]any{}
		if err := json.Unmarshal(f.Payload, &out); err != nil {
			t.Fatalf("decode healthcheck: %v", err)
		}
		return out
	}
	if got := answer(execValue{ExitCode: 0, DurationSeconds: 0.01}); got["healthy"] != true {
		t.Errorf("a serving node reported %v", got)
	}
	got := answer(execValue{ExitCode: 2,
		StderrB64: base64.StdEncoding.EncodeToString([]byte("not connected\nsecond line"))})
	if got["healthy"] != false {
		t.Errorf("a dead server reported %v", got)
	}
	detail, ok := got["detail"].(string)
	if !ok {
		t.Fatalf("detail = %v, want a string", got["detail"])
	}
	if !strings.Contains(detail, "arangosh exited 2") || strings.Contains(detail, "second line") {
		t.Errorf("detail = %q, want one line naming the exit code", detail)
	}
}

func TestHealthcheckRefusesAMalformedPayload(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `"not an object"`,
		func(verbCall) (any, *protoError) {
			t.Fatal("a malformed payload must not touch the sandbox")
			return nil, nil
		})
	wantCode(t, parseFinal(t, line), "invalid_request", "malformed healthcheck payload")
}

func TestTeardownReleasesNothingItDoesNotOwn(t *testing.T) {
	line, calls, exit := driveOp(t, "teardown", `{"state":{}}`,
		func(verbCall) (any, *protoError) {
			t.Fatal("teardown must not touch the sandbox: the provider destroys it")
			return nil, nil
		})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("teardown exit = %d with %d calls, want 0 and none", exit, len(calls))
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("teardown failed: %+v", f.Error)
	}
}

func TestAnUnknownOpIsRefused(t *testing.T) {
	line, _, _ := driveOp(t, "reticulate", `{}`, func(verbCall) (any, *protoError) {
		t.Fatal("an unknown op must not touch the sandbox")
		return nil, nil
	})
	wantCode(t, parseFinal(t, line), "invalid_request", "unknown op: reticulate")
}

func TestAMessageCrossesTheProtocolAsOneQuoteFreeLine(t *testing.T) {
	got := firstLine([]byte("  first \"quoted\" line \nsecond line\n"))
	if strings.Contains(got, "\n") || strings.Contains(got, `"`) {
		t.Errorf("firstLine = %q, want one line with no double quotes", got)
	}
	if firstLine(nil) != "" {
		t.Errorf("firstLine(nil) = %q, want empty", firstLine(nil))
	}
}

func TestShellQuoteSurvivesASingleQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %q", got)
	}
}
