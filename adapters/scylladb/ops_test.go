package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// overrideStep replaces one step's simulated answer and leaves the rest
// of the happy path intact.
func overrideStep(t *testing.T, label string, value any) func(verbCall) (any, *protoError) {
	sim := defaultSimulated()
	switch label {
	case "schema":
		sim.schema = value
	case "stage":
		sim.stage = value
	case "refresh":
		sim.refresh = value
	case "probe":
		sim.probe = value
	case "ttl":
		sim.ttl = value
	case "discover":
		sim.discover = value
	case "unpack":
		sim.unpack = value
	default:
		t.Fatalf("no such step: %s", label)
	}
	var sequence []string
	return provisionHandler(t, &sequence, sim)
}

// provisionTree drives a provision of a one-table snapshot tree with one
// step's answer replaced, and returns the final response.
func provisionTree(t *testing.T, label string, value any) finalResponse {
	t.Helper()
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot", root, nil),
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

// TestRefreshIsNotTheVerdict is the measurement this adapter is built
// around: pointed at an empty upload directory, nodetool refresh loads
// nothing and exits 0. So an empty stage is refused before refresh is
// asked, and what the table serves is read back afterwards.
func TestRefreshIsNotTheVerdict(t *testing.T) {
	f := provisionTree(t, "stage", errExec(4, "no sstable files staged for shop.orders"))
	wantCode(t, f, "source_corrupt", "staging shop.orders")
}

// TestADamagedSSTableIsLoud: the engine validates compressed chunks as it
// reads them and names the file and offset, so refresh exits non-zero.
func TestADamagedSSTableIsLoud(t *testing.T) {
	f := provisionTree(t, "refresh", errExec(4,
		"Failed to load new sstables: sstables::malformed_sstable_exception (failed checksum)"))
	wantCode(t, f, "source_corrupt", "refused the sstables for shop.orders")
}

func TestAReadThatFailsIsCorruption(t *testing.T) {
	f := provisionTree(t, "probe", errExec(2, "ReadFailure"))
	wantCode(t, f, "source_corrupt", "reading restored table shop.orders failed")
}

// TestTheFenceNeedsBothHalves: an empty table is refused only when the
// artifact's own sstables declare a time-to-live. Without that, an empty
// read is legitimate and the drill proceeds.
func TestTheFenceNeedsBothHalves(t *testing.T) {
	t.Run("empty and expiring is refused", func(t *testing.T) {
		root := t.TempDir()
		writeSnapshot(t, root, oneTable())
		sim := defaultSimulated()
		sim.probe = outExec(emptyRead)
		sim.ttl = outExec("60\n")
		var sequence []string
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot", root, nil),
			provisionHandler(t, &sequence, sim))
		f := parseFinal(t, line)
		wantCode(t, f, "restore_failed", "time-to-live of 60 seconds")
		if !strings.Contains(f.Error.Message, "The backup is intact") {
			t.Errorf("message = %q, want it to say the backup is not at fault", f.Error.Message)
		}
		if !contains(sequence, "ttl") {
			t.Error("the fence never asked the artifact whether it declared a TTL")
		}
	})

	t.Run("empty and not expiring passes", func(t *testing.T) {
		root := t.TempDir()
		writeSnapshot(t, root, oneTable())
		sim := defaultSimulated()
		sim.probe = outExec(emptyRead)
		sim.ttl = outExec("0\n")
		var sequence []string
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot", root, nil),
			provisionHandler(t, &sequence, sim))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("an empty table with no declared TTL failed: %+v", f.Error)
		}
	})
}

// TestATTLTheToolCannotReportIsNotAnAccusation: an image without the
// tool, or a probe that fails, answers zero and the fence stands down.
func TestATTLTheToolCannotReportIsNotAnAccusation(t *testing.T) {
	for name, answer := range map[string]any{
		"the probe failed":      errExec(1, "no such tool"),
		"it printed nothing":    outExec(""),
		"it printed nonsense":   outExec("not a number\n"),
		"it printed a negative": outExec("-1\n"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSnapshot(t, root, oneTable())
			sim := defaultSimulated()
			sim.probe = outExec(emptyRead)
			sim.ttl = answer
			var sequence []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "scylladb_snapshot", root, nil),
				provisionHandler(t, &sequence, sim))
			if f := parseFinal(t, line); !f.OK {
				t.Fatalf("the fence fired on an unanswerable probe: %+v", f.Error)
			}
		})
	}
}

// TestASchemaFromANewerEngineNamesBothSides: a snapshot from a newer
// engine can state options an older one does not know, which is a drill
// config pairing a backup with an image that cannot restore it.
func TestASchemaFromANewerEngineNamesBothSides(t *testing.T) {
	for name, stderr := range map[string]string{
		"an unknown property":     "Unknown property 'tombstone_gc'",
		"a syntax error":          "SyntaxException: line 1:0 no viable alternative",
		"a refused configuration": "ConfigurationException: tablets not supported",
	} {
		t.Run(name, func(t *testing.T) {
			f := provisionTree(t, "schema", errExec(2, stderr))
			wantCode(t, f, "invalid_request", "at least as new as the backup's origin")
		})
	}
	// Anything else is a failed restore, not a mispaired image.
	f := provisionTree(t, "schema", errExec(2, "connection refused"))
	wantCode(t, f, "restore_failed", "applying the schema for shop.orders failed")
}

// TestAnImageWithoutTheToolchainIsNamed, including the trap that makes
// this engine different: `sleep infinity` does not idle it.
func TestAnImageWithoutTheToolchainIsNamed(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot", root, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				return errExec(127, "cqlsh: not found"), nil
			}
			return putFileValue{}, nil
		})
	f := parseFinal(t, line)
	wantCode(t, f, "invalid_request", "lacks the ScyllaDB toolchain")
	if !strings.Contains(f.Error.Message, "never `sleep infinity`") {
		t.Errorf("message = %q, want it to name the command that breaks this image", f.Error.Message)
	}
}

// TestTheArchivePathUnpacksAndDiscovers drives the kind whose census the
// host could not read, so the tables come from the unpacked tree.
func TestTheArchivePathUnpacksAndDiscovers(t *testing.T) {
	path := tarOf(t, func() string {
		root := t.TempDir()
		writeSnapshot(t, root, oneTable())
		return root
	}(), "")

	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("archive provision failed: %+v", f.Error)
	}
	for _, want := range []string{"put_file", "unpack", "locate"} {
		if !contains(sequence, want) {
			t.Errorf("the archive flow never did %q; it did %v", want, sequence)
		}
	}
}

func TestAnArchiveTarCannotUnpackIsCorrupt(t *testing.T) {
	path := tarOf(t, func() string {
		root := t.TempDir()
		writeSnapshot(t, root, oneTable())
		return root
	}(), "")
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot_tar", path, nil),
		overrideStep(t, "unpack", errExec(2, "unexpected EOF")))
	wantCode(t, parseFinal(t, line), "source_corrupt", "tar could not unpack")
}

// TestAnOpaqueArchiveDiscoversItsTablesInTheSandbox: where the host's
// pass said nothing, the unpacked tree is the authority — and a tree with
// no keyspace directories is not a collected snapshot.
func TestAnOpaqueArchiveDiscoversItsTablesInTheSandbox(t *testing.T) {
	opaque := t.TempDir() + "/opaque.tar"
	writeFile(t, opaque, "not tar at all\n")

	t.Run("discovered", func(t *testing.T) {
		sim := defaultSimulated()
		sim.discover = outExec("shop/orders/\n")
		var sequence []string
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot_tar", opaque, nil),
			provisionHandler(t, &sequence, sim))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("discovery failed: %+v", f.Error)
		}
		if !contains(sequence, "discover") {
			t.Errorf("the flow never discovered tables; it did %v", sequence)
		}
	})

	t.Run("nothing to discover", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot_tar", opaque, nil),
			overrideStep(t, "discover", errExec(2, "no such directory")))
		wantCode(t, parseFinal(t, line), "source_corrupt", "not a collected snapshot")
	})

	t.Run("a discovered name that cannot go into CQL", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot_tar", opaque, nil),
			overrideStep(t, "discover", outExec("shop/Orders;drop/\n")))
		wantCode(t, parseFinal(t, line), "invalid_request", "not an unquoted CQL identifier")
	})
}

func TestProvisionRefusesWhatItCannotDo(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())
	noSandbox := func(verbCall) (any, *protoError) {
		t.Fatal("a refused request must not touch the sandbox")
		return nil, nil
	}
	t.Run("malformed payload", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", `"not an object"`, noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "malformed provision payload")
	})
	t.Run("pitr", func(t *testing.T) {
		payload := `{"source":{"kind":"scylladb_snapshot","path":"` + root +
			`"},"pitr":{"target_time":"2026-09-20T10:00:00Z"}}`
		line, _, _ := driveOp(t, "provision", payload, noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "does not support pitr")
	})
	t.Run("a declared timezone", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision",
			provisionPayload(t, "scylladb_snapshot", root, map[string]string{"backup_timezone": "UTC"}),
			noSandbox)
		wantCode(t, parseFinal(t, line), "invalid_request", "has no effect for this adapter")
	})
}

func TestHealthcheckReportsRatherThanFails(t *testing.T) {
	answer := func(value any) map[string]any {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return value, nil })
		if exit != 0 {
			t.Fatalf("healthcheck exit = %d, want 0 — an unhealthy node is a result", exit)
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
		StderrB64: base64.StdEncoding.EncodeToString([]byte("connection refused\nsecond line"))})
	if got["healthy"] != false {
		t.Errorf("a dead node reported %v", got)
	}
	detail, ok := got["detail"].(string)
	if !ok {
		t.Fatalf("healthcheck detail = %v, want a string", got["detail"])
	}
	if !strings.Contains(detail, "cqlsh exited 2") || strings.Contains(detail, "second line") {
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

// TestAMessageCrossesTheProtocolAsOneQuoteFreeLine: it lands in an
// evidence error field, which must stay shareable as it stands.
func TestAMessageCrossesTheProtocolAsOneQuoteFreeLine(t *testing.T) {
	got := firstLine([]byte("  first \"quoted\" line \nsecond line\n"))
	if strings.Contains(got, "\n") || strings.Contains(got, `"`) {
		t.Errorf("firstLine = %q, want one line with no double quotes", got)
	}
	// The guarantee is one line with no double quotes, not a trimmed one
	// — identical to every sibling adapter's.
	if !strings.HasPrefix(got, "first 'quoted' line") {
		t.Errorf("firstLine = %q", got)
	}
	if firstLine(nil) != "" {
		t.Errorf("firstLine(nil) = %q, want empty", firstLine(nil))
	}
}
