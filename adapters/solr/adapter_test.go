package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func outExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)), DurationSeconds: 0.1}
}

func errExec(exit int, stderr string) any {
	return execValue{
		ExitCode:        exit,
		StderrB64:       base64.StdEncoding.EncodeToString([]byte(stderr)),
		DurationSeconds: 0.1,
	}
}

func parseExec(t *testing.T, call verbCall) execArgs {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args
}

// restoreRefused is what the restore step answers when the engine said
// no: 0 on stdout, its own words on stderr.
func restoreRefused(diagnosis string) any {
	return execValue{
		ExitCode: 0, DurationSeconds: 0.2,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte("0\n")),
		StderrB64: base64.StdEncoding.EncodeToString([]byte(diagnosis)),
	}
}

func okExec(exit int) any {
	return execValue{ExitCode: exit, DurationSeconds: 0.1, StdoutB64: "", StderrB64: ""}
}

// step names the provision step an exec call belongs to, by the script
// it carries. The adapter's steps are shell fragments, so the fragment is
// the only honest identifier — a step renamed in scripts.go renames
// itself here.
func step(t *testing.T, call verbCall) (string, execArgs) {
	t.Helper()
	args := parseExec(t, call)
	script := ""
	if len(args.Argv) > 2 {
		script = args.Argv[2]
	}
	switch {
	case strings.Contains(script, "SOLR_HOME"):
		return "home", args
	case strings.Contains(script, `"mode":"solrcloud"`):
		return "mode", args
	case strings.Contains(script, "admin/info/system"):
		return "ready", args
	case strings.Contains(script, "action=RESTORE"):
		return "restore", args
	case strings.Contains(script, "grep -cx"):
		return "live", args
	case strings.Contains(script, "action=LIST"):
		return "served", args
	case strings.Contains(script, "numFound"):
		return "health", args
	default:
		return "unknown:" + script, args
	}
}

const (
	sandboxHome = "/var/solr/data"
	// fixtureCollection is the collection every fixture in this file
	// holds; the adapter reads it out of the artifact's own layout.
	fixtureCollection = "orders"
)

// writeBackup lays out a Collections API backup the way the engine does.
func writeBackup(t *testing.T, files map[string]string) string {
	const collection = fixtureCollection
	t.Helper()
	root := filepath.Join(t.TempDir(), "nightly")
	base := filepath.Join(root, collection)
	all := map[string]string{
		"backup_0.properties":                    "backupName=nightly\ncollection=" + collection + "\nstartTime=2026-08-27T18\\:34\\:34.622561925Z\n",
		"shard_backup_metadata/md_shard1_0.json": "{}",
		"zk_backup_0/configs/c/solrconfig.xml":   "<config></config>",
		"index/segments_1":                       "index bytes",
	}
	for k, v := range files {
		all[k] = v
	}
	for name, content := range all {
		full := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func provisionPayload(path, options string) string {
	if options == "" {
		options = "{}"
	}
	return fmt.Sprintf(`{"source":{"kind":"solr_backup","path":%q},"sandbox":{"scratch_dir":"/tmp"},"options":%s}`,
		path, options)
}

// happyHandler answers every step the way a healthy Solr does.
func happyHandler(t *testing.T, seen *[]string) func(verbCall) (any, *protoError) {
	const collection = fixtureCollection
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*seen = append(*seen, "put_file")
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.DestPath != sandboxHome+"/"+transferDir {
				t.Errorf("put_file dest = %q, want the transfer directory under SOLR_HOME", args.DestPath)
			}
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
		}
		name, args := step(t, call)
		*seen = append(*seen, name)
		switch name {
		case "home":
			return outExec(sandboxHome), nil
		case "ready":
			return okExec(0), nil
		case "mode":
			return outExec("1"), nil
		case "restore":
			wantArgs(t, name, args, sandboxHome, transferDir, collection)
			return outExec("1"), nil
		case "live":
			wantArgs(t, name, args, collection)
			return outExec("1"), nil
		case "health":
			return outExec("250"), nil
		default:
			return okExec(0), nil
		}
	}
}

// wantArgs pins the positional parameters a step is given. bash -c
// <script> bash a b c puts the first argument at argv[4].
func wantArgs(t *testing.T, name string, args execArgs, want ...string) {
	t.Helper()
	got := args.Argv[min(4, len(args.Argv)):]
	if len(got) != len(want) {
		t.Fatalf("%s: argv tail = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: argv[%d] = %q, want %q", name, i+4, got[i], want[i])
		}
	}
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
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
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

// TestProvisionHappyPath pins the order the steps run in. The order is
// the design: the artifact is read and fenced host-side, the engine is
// found before anything is written, and nothing is called a success
// until the server says it serves the collection.
func TestProvisionHappyPath(t *testing.T) {
	backup := writeBackup(t, nil)
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(backup, ""), happyHandler(t, &seen))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	want := []string{"home", "ready", "mode", "put_file", "restore", "live"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", seen, want)
	}

	payload := struct {
		Connection struct {
			Database string `json:"database"`
			Port     int    `json:"port"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		State struct {
			Collection string `json:"collection"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Database != "orders" || payload.Connection.Port != defaultPort {
		t.Errorf("connection = %+v", payload.Connection)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", payload.SourceIdentity.Checksum)
	}
	// The engine records when the backup began; the adapter reports that
	// rather than a directory's mtime.
	if payload.SourceIdentity.CreatedAt == nil || *payload.SourceIdentity.CreatedAt != "2026-08-27T18:34:34.622561925Z" {
		t.Errorf("created_at = %v, want the unescaped instant from backup_0.properties", payload.SourceIdentity.CreatedAt)
	}
	if payload.State.Collection != "orders" {
		t.Errorf("state = %+v", payload.State)
	}
}

// TestProvisionRefusesBeforeTouchingTheSandbox covers everything the
// adapter can decide from the request and the artifact alone. None of
// these may reach the engine: a drill that cannot be honest should cost
// nothing.
func TestProvisionRefusesBeforeTouchingTheSandbox(t *testing.T) {
	good := writeBackup(t, nil)
	expiring := writeBackup(t, map[string]string{
		"zk_backup_0/configs/c/solrconfig.xml": `<config><processor class="solr.processor.DocExpirationUpdateProcessorFactory"/></config>`,
	})
	twoCollections := writeBackup(t, nil)
	if err := os.MkdirAll(filepath.Join(twoCollections, "customers"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct{ payload, code, message string }{
		"a backup that deletes its own documents": {
			provisionPayload(expiring, ""), "unsupported_source", "DocExpirationUpdateProcessorFactory"},
		"a backup of several collections": {
			provisionPayload(twoCollections, ""), "unsupported_source", "single collection"},
		"a source that does not exist": {
			provisionPayload(filepath.Join(t.TempDir(), "gone"), ""), "source_not_found", "does not exist"},
		"an unsupported kind": {
			`{"source":{"kind":"solr_core","path":"/tmp"},"sandbox":{"scratch_dir":"/tmp"}}`,
			"unsupported_source", "solr_backup"},
		"a collection name Solr would not accept": {
			provisionPayload(good, `{"collection":"no spaces"}`), "invalid_request", "Solr accepts"},
		"a point-in-time request": {
			`{"source":{"kind":"solr_backup","path":"` + good + `"},"sandbox":{"scratch_dir":"/tmp"},` +
				`"pitr":{"target_time":"2026-07-30T14:32:00Z"}}`, "invalid_request", "point-in-time"},
		"a declared backup zone": {
			`{"source":{"kind":"solr_backup","path":"` + good + `","params":{"backup_timezone":"Europe/Budapest"}},` +
				`"sandbox":{"scratch_dir":"/tmp"}}`, "invalid_request", "records its own start time"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, _ := driveOp(t, "provision", tt.payload, func(verbCall) (any, *protoError) {
				t.Fatal("the sandbox must not be touched")
				return nil, nil
			})
			if len(calls) != 0 {
				t.Fatalf("touched the sandbox %d times", len(calls))
			}
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.code {
				t.Fatalf("final = %+v, want %s", f, tt.code)
			}
			if !strings.Contains(f.Error.Message, tt.message) {
				t.Errorf("message = %q, want it to carry %q", f.Error.Message, tt.message)
			}
		})
	}
}

// TestProvisionRefusesACollectionTheServerDoesNotServe is the gate
// between the API's status 0 and a drill that has something to check.
func TestProvisionRefusesACollectionTheServerDoesNotServe(t *testing.T) {
	backup := writeBackup(t, nil)
	var seen []string
	handler := happyHandler(t, &seen)
	line, _, _ := driveOp(t, "provision", provisionPayload(backup, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				name, _ := step(t, call)
				switch name {
				case "live":
					return outExec("0"), nil
				case "served":
					return outExec("films\nusers\n"), nil
				}
			}
			return handler(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Fatalf("final = %+v, want restore_failed", f)
	}
	for _, want := range []string{"does not serve", "films", "users"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
}

// TestProvisionClassifiesTheEnginesRefusals separates a damaged backup
// from a drill that asked for something impossible.
func TestProvisionClassifiesTheEnginesRefusals(t *testing.T) {
	backup := writeBackup(t, nil)
	tests := map[string]struct{ diagnosis, code, message string }{
		"a backup the engine cannot read": {
			`HTTP 500 {"error":{"msg":"Could not restore core"}}`, "source_corrupt", "Could not restore core"},
		"a collection that already exists": {
			`HTTP 400 {"error":{"msg":"collection already exists: orders"}}`, "restore_failed", "already exists"},
		"an engine that answered nothing": {"", "restore_failed", "said nothing"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var seen []string
			handler := happyHandler(t, &seen)
			line, _, _ := driveOp(t, "provision", provisionPayload(backup, ""),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "exec" {
						if name, _ := step(t, call); name == "restore" {
							return restoreRefused(tt.diagnosis), nil
						}
					}
					return handler(call)
				})
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.code {
				t.Fatalf("final = %+v, want %s", f, tt.code)
			}
			if !strings.Contains(f.Error.Message, tt.message) {
				t.Errorf("message = %q, want it to carry %q", f.Error.Message, tt.message)
			}
		})
	}
}

// TestProvisionFallsBackWhenTheSandboxCannotNameItsSolrHome pins the
// choice behind an answer the adapter cannot use: it writes to the
// documented default and lets Solr refuse the path in its own words,
// rather than picking a different one quietly. Solr's refusal names the
// setting, which a silent guess never would.
func TestProvisionFallsBackWhenTheSandboxCannotNameItsSolrHome(t *testing.T) {
	backup := writeBackup(t, nil)
	for _, answer := range []string{"", "relative/path", "/var/$(id)", "/var/a b"} {
		t.Run(answer, func(t *testing.T) {
			var seen []string
			handler := happyHandler(t, &seen)
			var restoreHome string
			line, _, _ := driveOp(t, "provision", provisionPayload(backup, ""),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "exec" {
						name, args := step(t, call)
						switch name {
						case "home":
							return outExec(answer), nil
						case "restore":
							restoreHome = args.Argv[4]
							return outExec("1"), nil
						}
					}
					return handler(call)
				})
			if f := parseFinal(t, line); !f.OK {
				t.Fatalf("final = %+v", f)
			}
			if restoreHome != sandboxHome {
				t.Errorf("restored from %q, want the documented default %q", restoreHome, sandboxHome)
			}
		})
	}
}

func TestHealthcheck(t *testing.T) {
	tests := map[string]struct {
		reply   any
		healthy bool
	}{
		"a collection that answers a count": {outExec("250"), true},
		"a collection that answers nothing": {outExec("not a number"), false},
		"a server that has gone":            {errExec(7, "curl: (7) Failed to connect"), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, _, exit := driveOp(t, "healthcheck", `{"state":{"collection":"orders"}}`,
				func(verbCall) (any, *protoError) { return tt.reply, nil })
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if !f.OK {
				t.Fatalf("final = %+v", f)
			}
			got := struct {
				Healthy bool `json:"healthy"`
			}{}
			if err := json.Unmarshal(f.Payload, &got); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if got.Healthy != tt.healthy {
				t.Errorf("healthy = %v, want %v", got.Healthy, tt.healthy)
			}
		})
	}
}

func TestTeardownIsIdempotent(t *testing.T) {
	for range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{"collection":"orders"}}`,
			func(verbCall) (any, *protoError) {
				t.Fatal("teardown owns nothing outside the sandbox")
				return nil, nil
			})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit=%d calls=%d", exit, len(calls))
		}
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
	}
}

// writeTar packs a directory the way an operator archiving a backup
// would, with the backup directory itself as the top-level entry.
func writeTar(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.tar")
	f, err := os.Create(path) //#nosec G304 -- a path this test just made.
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	root := filepath.Dir(dir)
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, berr := os.ReadFile(p) //#nosec G304 -- inside the fixture.
		if berr != nil {
			return berr
		}
		_, err := tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The three tests below pin the division of labour for the archive kind:
// the host reads the stream once — which is what lets the fence refuse
// before a byte moves — and the sandbox does the unpacking.

func TestTarArtifactIsTransferredWholeAndUnpackedBySandbox(t *testing.T) {
	{
		archive := writeTar(t, writeBackup(t, nil))
		var seen []string
		handler := happyHandler(t, &seen)
		var extracted bool
		line, _, _ := driveOp(t, "provision",
			fmt.Sprintf(`{"source":{"kind":"solr_backup_tar","path":%q},"sandbox":{"scratch_dir":"/tmp"}}`, archive),
			func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					args := putFileArgs{}
					if err := json.Unmarshal(call.Args, &args); err != nil {
						t.Fatal(err)
					}
					if !strings.HasSuffix(args.DestPath, ".tar") {
						t.Errorf("put_file dest = %q, want the archive itself", args.DestPath)
					}
					return putFileValue{BytesCopied: 2048, DurationSeconds: 0.3}, nil
				}
				if args := parseExec(t, call); len(args.Argv) > 2 && strings.Contains(args.Argv[2], "tar -xf") {
					extracted = true
					return outExec("orders"), nil
				}
				return handler(call)
			})
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if !extracted {
			t.Error("the sandbox was never asked to unpack the archive")
		}
	}
}

func TestTarArtifactThatDeletesItsOwnDocumentsNeverReachesTheSandbox(t *testing.T) {
	{
		archive := writeTar(t, writeBackup(t, map[string]string{
			"zk_backup_0/configs/c/solrconfig.xml": `<config><processor class="solr.processor.DocExpirationUpdateProcessorFactory"/></config>`,
		}))
		line, calls, _ := driveOp(t, "provision",
			fmt.Sprintf(`{"source":{"kind":"solr_backup_tar","path":%q},"sandbox":{"scratch_dir":"/tmp"}}`, archive),
			func(verbCall) (any, *protoError) {
				t.Fatal("the sandbox must not be touched")
				return nil, nil
			})
		if len(calls) != 0 {
			t.Fatalf("touched the sandbox %d times", len(calls))
		}
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "unsupported_source" {
			t.Fatalf("final = %+v, want unsupported_source", f)
		}
		if !strings.Contains(f.Error.Message, expirationClass) {
			t.Errorf("message = %q, want it to name the processor", f.Error.Message)
		}
	}
}

func TestTarKindRefusesADirectory(t *testing.T) {
	{
		dir := writeBackup(t, nil)
		line, _, _ := driveOp(t, "provision",
			fmt.Sprintf(`{"source":{"kind":"solr_backup_tar","path":%q},"sandbox":{"scratch_dir":"/tmp"}}`, dir),
			func(verbCall) (any, *protoError) { return okExec(0), nil })
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" {
			t.Fatalf("final = %+v, want invalid_request", f)
		}
		if !strings.Contains(f.Error.Message, "solr_backup") {
			t.Errorf("message = %q, want it to name the kind that takes a directory", f.Error.Message)
		}
	}
}

// TestScanBackupTarRefusesUnboundedNames pins the retention bound. The
// fence keeps a name per collection it meets and per configuration file
// carrying the expiration class, and a tar entry is a 512-byte header
// that compresses to almost nothing — so without a bound a small archive
// decides how much memory the drill host spends. A backup file is
// attacker-controlled input (SECURITY.md). The bound is tight because
// what the fence retains is inherently tiny: this adapter restores one
// collection.
func TestScanBackupTarRefusesUnboundedNames(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := 0; i <= keptMaxEntries; i++ {
		name := fmt.Sprintf("c%d/backup_x.properties", i)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	collection, expiring, perr := scanBackupTar(bytes.NewReader(buf.Bytes()), "backup.tar")
	if perr == nil {
		t.Fatalf("scanBackupTar = %q, expiring %v; want a refusal", collection, expiring)
	}
	if perr.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", perr.Code)
	}
	if !strings.Contains(perr.Message, "memory") {
		t.Errorf("message %q must say why the walk stopped", perr.Message)
	}
}
