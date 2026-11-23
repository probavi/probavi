package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// answerScript runs a scenario's handler — a bash function that sets out
// (the body) and code (the HTTP status) for a path and a body — and prints
// the status on its first line and the body after it.
const answerScript = `out= code=200
. "$1"
respond "$2" "$3"
printf '%s\n%s' "$code" "$out"`

// fakeEngine stands in for the engine's REST service: a real HTTP server on
// loopback, so the scripts' own client is what talks to it. It records the
// login each request carried and the request itself, and answers from the
// scenario's handler. It returns the port it listens on.
func fakeEngine(t *testing.T, dir string) string {
	t.Helper()
	record := func(name, line string) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("record %s: %v", name, err)
			return
		}
		_, err = f.WriteString(line + "\n")
		if err = errors.Join(err, f.Close()); err != nil {
			t.Errorf("record %s: %v", name, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		user, pass, _ := r.BasicAuth()
		record("config", user+":"+pass)
		record("requests", r.URL.Path+"\t"+string(body))
		answer, err := exec.CommandContext(r.Context(), "bash", "-c", answerScript, "bash",
			filepath.Join(dir, "handler"), r.URL.Path, string(body)).Output()
		if err != nil {
			t.Errorf("run the handler: %v", err)
		}
		status, out, _ := strings.Cut(string(answer), "\n")
		code, err := strconv.Atoi(status)
		if err != nil {
			t.Errorf("handler status %q: %v", status, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if _, err := io.WriteString(w, out); err != nil {
			t.Errorf("answer %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

// atPort points a script's client at port instead of the engine's.
func atPort(t *testing.T, script, port string) string {
	t.Helper()
	engine := "/dev/tcp/127.0.0.1/" + restPort
	if strings.Contains(script, "iotdb_post") && !strings.Contains(script, engine) {
		t.Fatalf("the script's client does not connect to %s", engine)
	}
	return strings.ReplaceAll(script, engine, "/dev/tcp/127.0.0.1/"+port)
}

// precisionIs answers show variables with the given precision; every
// scenario's handler starts with it.
func precisionIs(prec string) string {
	return `respond() {
  case "$2" in
    *'"show variables"'*) out='{"expressions":[],"column_names":["Variable","Value"],"data_types":["TEXT","TEXT"],"timestamps":[],"values":[["ClusterName","TimestampPrecision"],["defaultCluster","` + prec + `"]]}'; return ;;
  esac
  answer "$@"
}
`
}

type scriptRun struct {
	stdout, stderr string
	exit           int
	requests       string
	config         string
	// base64Args is every argument list base64 was run with, one per line.
	base64Args string
}

// recordingBase64 wraps the host's base64 to record the arguments it was
// given, which is where a password must never be.
const recordingBase64 = `#!/bin/bash
printf '%s\n' "$*" >> "$FAKE_DIR/base64-args"
exec "$REAL_BASE64" "$@"
`

// runScript runs one of this adapter's scripts on the host against the fake
// engine, with the scenario's handler.
func runScript(t *testing.T, script, handler string, env map[string]string, args ...string) scriptRun {
	t.Helper()
	for _, tool := range []string{"bash", "awk", "mktemp", "grep", "sed", "base64", "wc", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	realBase64, err := exec.LookPath("base64")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base64"), []byte(recordingBase64), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler"), []byte(handler), 0o600); err != nil {
		t.Fatal(err)
	}
	script = atPort(t, script, fakeEngine(t, dir))
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", script, "bash"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_DIR="+dir, "REAL_BASE64="+realBase64)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exit = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run script: %v", err)
	}
	run.requests, run.config = recorded(t, dir, "requests"), recorded(t, dir, "config")
	run.base64Args = recorded(t, dir, "base64-args")
	return run
}

// recorded reads what the fakes wrote, which is nothing when the script
// never reached them.
func recorded(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the fake's %s: %v", name, err)
	}
	return string(b)
}

func TestRunnerRendersTheTreeDialectExactly(t *testing.T) {
	handler := precisionIs("ms") + `answer() {
  out='{"expressions":["root.au.d.t","root.au.d.v"],"column_names":[],"data_types":["TEXT","DOUBLE"],"timestamps":[1000,2000,3000],"values":[["plain","q\"uote\\\\back ünï 😀",null],[1.0,2.0,3.0]]}'
}
`
	run := runScript(t, runnerScript, handler, nil, "", "root", "select * from root.au.d")
	if run.exit != 0 {
		t.Fatalf("exit %d: %s", run.exit, run.stderr)
	}
	want := "1970-01-01T00:00:01.000Z\tplain\t1.0\n" +
		"1970-01-01T00:00:02.000Z\tq\"uote\\\\back ünï 😀\t2.0\n" +
		"1970-01-01T00:00:03.000Z\t\t3.0\n"
	if run.stdout != want {
		t.Errorf("rows =\n%q\nwant\n%q", run.stdout, want)
	}
	if !strings.Contains(run.requests, "/rest/v2/query\t{\"sql\":\"select * from root.au.d\"}") {
		t.Errorf("requests = %q, want the tree service", run.requests)
	}
}

// TestRunnerRendersTimestampsAtTheClustersPrecision pins the string
// arithmetic: a nanosecond epoch does not fit a double exactly.
func TestRunnerRendersTimestampsAtTheClustersPrecision(t *testing.T) {
	for _, tc := range []struct {
		prec, raw, want string
	}{
		{"ms", "1789560001071", "2026-09-16T12:00:01.071Z"},
		{"us", "1789560001071123", "2026-09-16T12:00:01.071123Z"},
		{"ns", "1789560001071123456", "2026-09-16T12:00:01.071123456Z"},
		{"ms", "0", "1970-01-01T00:00:00.000Z"},
		{"ms", "-1", "1969-12-31T23:59:59.999Z"},
		{"ms", "951782400000", "2000-02-29T00:00:00.000Z"},
	} {
		t.Run(tc.prec+" "+tc.raw, func(t *testing.T) {
			handler := precisionIs(tc.prec) + `answer() {
  out='{"column_names":["_col0","big"],"data_types":["TIMESTAMP","INT64"],"values":[[` + tc.raw + `,9223372036854775807]]}'
}
`
			run := runScript(t, runnerScript, handler, nil, "rig_t", "root", `SELECT max("time"), big FROM "rig_t"."sensors"`)
			if run.exit != 0 {
				t.Fatalf("exit %d: %s", run.exit, run.stderr)
			}
			if want := tc.want + "\t9223372036854775807\n"; run.stdout != want {
				t.Errorf("row = %q, want %q", run.stdout, want)
			}
			if !strings.Contains(run.requests, `/rest/table/v1/query	{"database":"rig_t","sql":"SELECT max(\"time\"), big FROM \"rig_t\".\"sensors\""}`) {
				t.Errorf("requests = %q, want the table service in rig_t with the statement escaped", run.requests)
			}
		})
	}
}

func TestRunnerReadsA13AnswerAndAnEmptyOne(t *testing.T) {
	handler := precisionIs("ms") + `answer() {
  case "$2" in
    *count*) out='{"expressions":["count(root.rig.d2.v)"],"column_names":null,"data_types":["INT64"],"timestamps":null,"values":[[5000]]}' ;;
    *) out='{"expressions":[],"column_names":[],"data_types":[],"timestamps":[],"values":[]}' ;;
  esac
}
`
	if run := runScript(t, runnerScript, handler, nil, "", "root", "select count(v) from root.rig.d2"); run.exit != 0 || run.stdout != "5000\n" {
		t.Errorf("1.3 answer: exit %d stdout %q stderr %q", run.exit, run.stdout, run.stderr)
	}
	if run := runScript(t, runnerScript, handler, nil, "", "root", "select v from root.nosuch.d"); run.exit != 0 || run.stdout != "" {
		t.Errorf("empty answer: exit %d stdout %q stderr %q", run.exit, run.stdout, run.stderr)
	}
}

func TestRunnerFailsTheWayTheEngineDid(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		exit         int
		stderr       string
	}{
		{"a refused statement", `out='{"code":701,"message":"Table does not exist."}'`, 3, "701: Table does not exist."},
		{"a refused login", `out='{"code":801,"message":"WRONG_LOGIN_PASSWORD"}'; code=401`, 2, "WRONG_LOGIN_PASSWORD"},
		{"a dialect the engine lacks", `out='<html>404</html>'; code=404`, 4, "serves no /rest/table/v1/query"},
		{"an answer that is not JSON", `out='{"values":[[1,'`, 1, ""},
		{"a server error", `out='oops'; code=500`, 1, "HTTP 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := precisionIs("ms") + "answer() {\n  " + tc.answer + "\n}\n"
			run := runScript(t, runnerScript, handler, nil, "rig_t", "root", "select * from nosuch")
			if run.exit != tc.exit || !strings.Contains(run.stderr, tc.stderr) {
				t.Errorf("exit %d stderr %q, want exit %d mentioning %q", run.exit, run.stderr, tc.exit, tc.stderr)
			}
		})
	}
}

// TestRunnerSendsTheLoginOnlyOnTheWire pins the password's route: the
// script's environment, base64's standard input and the request's Basic
// credentials, byte for byte, and never an argument of any process.
func TestRunnerSendsTheLoginOnlyOnTheWire(t *testing.T) {
	handler := precisionIs("ms") + `answer() { out='{"values":[[1]],"data_types":["INT32"]}'; }` + "\n"
	const secret = `p\a"ss: ünï`
	run := runScript(t, runnerScript, handler, map[string]string{passwordEnv: secret}, "", `au"d`, "select 1")
	if run.exit != 0 {
		t.Fatalf("exit %d: %s", run.exit, run.stderr)
	}
	if want := `au"d:` + secret + "\n"; !strings.HasPrefix(run.config, want) {
		t.Errorf("credentials the engine received = %q, want %q on every request", run.config, want)
	}
	if run.base64Args == "" || strings.Contains(run.base64Args, "p\\a") || strings.Contains(run.base64Args, "au\"d") {
		t.Errorf("base64 argument lists = %q, want it run with options only", run.base64Args)
	}
	defaulted := runScript(t, runnerScript, handler, map[string]string{passwordEnv: ""}, "", "root", "select 1")
	if !strings.HasPrefix(defaulted.config, "root:root\n") {
		t.Errorf("credentials the engine received = %q, want IoTDB's default password when none is set", defaulted.config)
	}
}

// verdictAnswers is a copy the verdict reads whole: a tree database of two
// devices and a table-model database of one table, no TTL.
func verdictAnswers(overrides string) string {
	return precisionIs("ms") + `answer() {
  case "$1 $2" in
` + overrides + `
    *'"show databases"'*) case "$1" in
        /rest/v2/query) out='{"expressions":[],"column_names":["Database","SchemaReplicationFactor"],"data_types":["TEXT","INT32"],"timestamps":[],"values":[["root.rig"],[1]]}' ;;
        *) out='{"column_names":["Database","TTL(ms)"],"data_types":["TEXT","TEXT"],"values":[["rig_t","INF"],["information_schema","INF"]]}' ;;
      esac ;;
    *'"show all ttl"'*) out='{"expressions":[],"column_names":["Device","TTL(ms)"],"data_types":["TEXT","TEXT"],"timestamps":[],"values":[["root.**"],["INF"]]}' ;;
    *'count(cast(* as TEXT)) from root.rig.** align by device'*|*'count(*) from root.rig.** align by device'*)
      out='{"expressions":["Device","count(v)"],"column_names":[],"data_types":["TEXT","INT64"],"timestamps":[],"values":[["root.rig.d1","root.rig.d2"],[10000,5000]]}' ;;
    *'"show tables details"'*) out='{"column_names":["TableName","TTL(ms)","Status"],"data_types":["TEXT","TEXT","TEXT"],"values":[["sensors","INF","USING"]]}' ;;
    *'"desc \"sensors\""'*) out='{"column_names":["ColumnName","DataType","Category"],"data_types":["TEXT","TEXT","TEXT"],"values":[["time","TIMESTAMP","TIME"],["region","STRING","TAG"],["temperature","FLOAT","FIELD"]]}' ;;
    *'select count(*) from \"sensors\"'*) out='{"column_names":["_col0"],"data_types":["INT64"],"values":[[5000]]}' ;;
    *'count(\"region\"), count(\"temperature\")'*|*'count(cast(\"region\" as STRING)), count(cast(\"temperature\" as STRING))'*)
      out='{"column_names":["_col0","_col1"],"data_types":["INT64","INT64"],"values":[[5000,4990]]}' ;;
    *) out='{"code":999,"message":"the fake has no answer for this"}' ;;
  esac
}
`
}

func TestVerdictReadsEveryValueAndCountsThem(t *testing.T) {
	run := runScript(t, verdictScript, verdictAnswers(""), nil)
	if run.exit != 0 {
		t.Fatalf("exit %d: %s", run.exit, run.stderr)
	}
	if run.stdout != "24990\n" {
		t.Errorf("values read = %q, want 15000 tree values plus 9990 table values", run.stdout)
	}
	for _, want := range []string{"count(cast(* as TEXT)) from root.rig.** align by device", `count(cast(\"temperature\" as STRING))`} {
		if !strings.Contains(run.requests, want) {
			t.Errorf("requests lack the full read %q", want)
		}
	}
	if strings.Contains(run.requests, "information_schema.") {
		t.Error("the verdict read the engine's own information_schema as if it were data")
	}
}

func TestVerdictRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, overrides string
		env             map[string]string
		exit            int
		stderr          string
	}{
		{
			name:      "a page that does not decode",
			overrides: `    *'count(cast(* as TEXT)) from root.rig.**'*) out='{"code":724,"message":"Failed to decode page data."}' ;;`,
			exit:      exitUndecodable, stderr: "724: Failed to decode page data.",
		},
		{
			name:      "a file whose metadata does not read",
			overrides: `    *'count(*) from root.rig.**'*) out='{"code":724,"message":"Failed to read timeseries metadata."}' ;;`,
			exit:      exitUndecodable, stderr: "count root.rig: 724: Failed to read timeseries metadata.",
		},
		{
			name:      "a data file 1.3 cannot scan",
			overrides: `    *'count(cast(* as TEXT)) from root.rig.**'*) out='{"code":301,"message":"java.lang.RuntimeException: Error happened while scanning the file"}' ;;`,
			exit:      exitUndecodable, stderr: "Error happened while scanning the file",
		},
		{
			name:      "a truncated data file on 1.3",
			overrides: `    *'count(*) from root.rig.**'*) out='{"code":301,"message":"java.nio.BufferUnderflowException"}' ;;`,
			exit:      exitUndecodable, stderr: "BufferUnderflowException",
		},
		{
			name:      "a statement refused for another reason is not damage",
			overrides: `    *'count(*) from root.rig.**'*) out='{"code":301,"message":"EXECUTE_STATEMENT_ERROR"}' ;;`,
			exit:      exitQueryFailed, stderr: "count root.rig: 301: EXECUTE_STATEMENT_ERROR",
		},
		{
			name:      "a read that disagrees with the statistics",
			overrides: `    *'count(cast(* as TEXT)) from root.rig.**'*) out='{"expressions":["Device","count(v)"],"data_types":["TEXT","INT64"],"timestamps":[],"values":[["root.rig.d1","root.rig.d2"],[10000,4999]]}' ;;`,
			exit:      exitReadDisagrees, stderr: "root.rig",
		},
		{
			name: "a TTL that hides a whole scope",
			overrides: `    *'"show all ttl"'*) out='{"expressions":[],"data_types":["TEXT","TEXT"],"timestamps":[],"values":[["root.**","root.ttl.**"],["INF","300000"]]}' ;;
    *'count timeseries root.ttl.**'*) out='{"expressions":[],"data_types":["INT64"],"timestamps":[],"values":[[2]]}' ;;
    *'select count(*) from root.ttl.**"'*) out='{"expressions":["count(a)","count(b)"],"data_types":["INT64","INT64"],"timestamps":[],"values":[[0],[0]]}' ;;`,
			exit: exitTTLHidesAll, stderr: "tree\troot.ttl.**\t300000",
		},
		{
			name: "a table TTL that hides the table",
			overrides: `    *'"show tables details"'*) out='{"data_types":["TEXT","TEXT","TEXT"],"values":[["sensors","3600000","USING"]]}' ;;
    *'select count(*) from \"sensors\"'*) out='{"data_types":["INT64"],"values":[[0]]}' ;;`,
			exit: exitTTLHidesAll, stderr: "table\trig_t.sensors\t3600000",
		},
		{
			name: "a copy that holds nothing",
			overrides: `    *'align by device'*) out='{"expressions":["Device","count(v)"],"data_types":["TEXT","INT64"],"timestamps":[],"values":[["root.rig.d1"],[0]]}' ;;
    *'count(\"region\"), count(\"temperature\")'*|*'as STRING))'*) out='{"data_types":["INT64","INT64"],"values":[[0,0]]}' ;;`,
			exit: exitRestoredNothing,
		},
		{
			name: "a table database the copy does not hold", env: map[string]string{"IOTDB_DATABASE": "nosuch"},
			exit: exitNoSuchDatabase, stderr: "rig_t",
		},
		{
			name:      "any other refusal",
			overrides: `    *'"show all ttl"'*) out='{"code":301,"message":"EXECUTE_STATEMENT_ERROR"}' ;;`,
			exit:      exitQueryFailed, stderr: "list the TTLs: 301: EXECUTE_STATEMENT_ERROR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runScript(t, verdictScript, verdictAnswers(tc.overrides), tc.env)
			if run.exit != tc.exit || !strings.Contains(run.stderr, tc.stderr) {
				t.Errorf("exit %d stderr %q, want exit %d mentioning %q", run.exit, run.stderr, tc.exit, tc.stderr)
			}
		})
	}
}

// TestVerdictOnA13CopyReadsTheTreeAlone: 1.3 answers the table dialect's
// path with 404, which means no table model, not a failure.
func TestVerdictOnA13CopyReadsTheTreeAlone(t *testing.T) {
	overrides := `    */rest/table/v1/query*) out='<html>404</html>'; code=404 ;;`
	run := runScript(t, verdictScript, verdictAnswers(overrides), nil)
	if run.exit != 0 || run.stdout != "15000\n" {
		t.Errorf("exit %d stdout %q stderr %q, want the tree's 15000 values", run.exit, run.stdout, run.stderr)
	}
}

func TestReadinessWantsEveryNodeAndRegionRunning(t *testing.T) {
	cluster := `    *'"show cluster"'*) out='{"expressions":[],"data_types":["INT32","TEXT","TEXT"],"timestamps":[],"values":[[0,1],["ConfigNode","DataNode"],["Running","RUNSTATUS"]]}' ;;`
	regions := `    *'"show regions"'*) out='{"expressions":[],"data_types":["INT32","TEXT","TEXT"],"timestamps":[],"values":[[0],["DataRegion"],["STATUS"]]}' ;;`
	base := func(clusterStatus, regionStatus string) string {
		return precisionIs("ms") + `answer() {
  case "$2" in
` + strings.Replace(cluster, "RUNSTATUS", clusterStatus, 1) + "\n" + strings.Replace(regions, "STATUS", regionStatus, 1) + `
  esac
}
`
	}
	for _, tc := range []struct {
		name, cluster, region string
		exit                  int
	}{
		{"serving", "Running", "Running", 0},
		{"a node still starting", "Unknown", "Running", exitNotReady},
		{"a region still being led", "Running", "Unknown", exitNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if run := runScript(t, readyScript, base(tc.cluster, tc.region), nil); run.exit != tc.exit {
				t.Errorf("exit %d (%s), want %d", run.exit, run.stderr, tc.exit)
			}
		})
	}
	refused := precisionIs("ms") + `answer() { out='{"code":801,"message":"WRONG_LOGIN_PASSWORD"}'; code=401; }` + "\n"
	if run := runScript(t, readyScript, refused, nil); run.exit != exitLoginRefused {
		t.Errorf("a refused login: exit %d, want %d", run.exit, exitLoginRefused)
	}
}

// sentRequest is what the client put on the wire, and whether the fake
// managed to answer it.
type sentRequest struct {
	proto, auth string
	body        []byte
	err         error
	served      error
}

// rawEngine answers every connection with response, byte for byte, after
// reading one HTTP request from it, and closes the connection: the answers
// a well-behaved server never gives are exactly the ones the client has to
// refuse. It returns the port and what each request carried.
func rawEngine(t *testing.T, response string) (string, <-chan sentRequest) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("close the fake engine: %v", err)
		}
	})
	sent := make(chan sentRequest, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			got := sentRequest{err: err}
			if err == nil {
				got.proto, got.auth = req.Proto, req.Header.Get("Authorization")
				got.body, got.err = io.ReadAll(req.Body)
			}
			_, err = io.WriteString(conn, response)
			got.served = errors.Join(err, conn.Close())
			select {
			case sent <- got:
			default:
			}
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port, sent
}

// postTo runs the client alone against port and returns what it printed and
// how it exited.
func postTo(t *testing.T, port, body string) (string, int) {
	t.Helper()
	for _, tool := range []string{"bash", "awk", "base64", "wc", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	script := atPort(t, "set -u\n"+restFn+"\niotdb_post /rest/v2/query \"$1\"", port)
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", body)
	cmd.Env = append(os.Environ(), "IOTDB_USER=root", passwordEnv+"=Drill-Pass-9")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run the client: %v", err)
	}
	return string(out), 0
}

// received waits for the fake engine's account of one request, and fails
// the test unless the request parsed as HTTP and was answered.
func received(t *testing.T, sent <-chan sentRequest) sentRequest {
	t.Helper()
	select {
	case got := <-sent:
		if got.err != nil || got.served != nil {
			t.Fatalf("the fake engine could not read the request (%v) or answer it (%v)", got.err, got.served)
		}
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("the fake engine saw no request")
	}
	return sentRequest{}
}

// answer is an HTTP response with the given status line, headers and body.
func answer(status, headers, body string) string {
	return status + "\r\n" + headers + "\r\n" + body
}

// TestTheClientReadsWhatTheServerSaidAndNothingElse pins the HTTP client the
// scripts carry instead of curl: the shapes both verified engines answer
// with are read whole, and an answer that is not a whole HTTP response fails
// the request rather than reaching the JSON reader.
func TestTheClientReadsWhatTheServerSaidAndNothingElse(t *testing.T) {
	long := `{"values":[[` + strings.Repeat("1.0,", 40000) + `1.0]]}`
	short := `{"code":801,"message":"WRONG_LOGIN_PASSWORD"}`
	for _, tc := range []struct {
		name, response, want string
		fails                bool
	}{
		{"an answer with its length", answer("HTTP/1.1 200 OK", "Content-Type: application/json\r\nContent-Length: "+
			strconv.Itoa(len(short))+"\r\n", short), short + "\n200", false},
		{"a length in lower case", answer("HTTP/1.1 401 Unauthorized", "content-length:"+strconv.Itoa(len(short))+"\r\n",
			short), short + "\n401", false},
		{"a long answer ended by the close", answer("HTTP/1.1 200 OK", "Content-Type: application/json\r\n", long),
			long + "\n200", false},
		{"trailing newlines kept", answer("HTTP/1.0 200 OK", "Content-Length: 4\r\n", "{}\n\n"), "{}\n\n\n200", false},
		{"a body short of its length", answer("HTTP/1.1 200 OK", "Content-Length: "+strconv.Itoa(len(short)+1)+"\r\n",
			short), "", true},
		{"two lengths", answer("HTTP/1.1 200 OK", "Content-Length: 2\r\nContent-Length: 2\r\n", "{}"), "", true},
		{"headers that never end", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n", "", true},
		{"not HTTP", "SSH-2.0-OpenSSH_9.6\r\n\r\n{}", "", true},
		{"a status that is not one", answer("HTTP/1.1 20 OK", "", "{}"), "", true},
		{"nothing at all", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, sent := rawEngine(t, tc.response)
			out, exit := postTo(t, port, `{"sql":"show cluster"}`)
			received(t, sent)
			if tc.fails {
				if exit == 0 {
					t.Errorf("exit 0 printing %q, want the request to fail", out)
				}
				return
			}
			if exit != 0 || out != tc.want {
				t.Errorf("exit %d printing %.200q, want exit 0 printing %.200q", exit, out, tc.want)
			}
		})
	}
}

// TestTheClientSendsAWholeRequest pins the request half: HTTP/1.0, so no
// server answers in chunks; a Content-Length in bytes, not characters, so a
// statement outside ASCII arrives whole; and the login as Basic credentials.
func TestTheClientSendsAWholeRequest(t *testing.T) {
	port, sent := rawEngine(t, answer("HTTP/1.1 200 OK", "Content-Length: 2\r\n", "{}"))
	body := `{"sql":"select v from root.m.d where note = 'ünï 😀'"}`
	if out, exit := postTo(t, port, body); exit != 0 || out != "{}\n200" {
		t.Fatalf("exit %d printing %q", exit, out)
	}
	got := received(t, sent)
	if got.proto != "HTTP/1.0" || string(got.body) != body {
		t.Errorf("sent %s with body %q, want HTTP/1.0 with %q", got.proto, got.body, body)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("root:Drill-Pass-9")); got.auth != want {
		t.Errorf("Authorization = %q, want %q", got.auth, want)
	}
}

// TestTheClientFailsWhenNothingListens is the engine still starting: the
// readiness loop polls on exactly this failure.
func TestTheClientFailsWhenNothingListens(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if out, exit := postTo(t, port, "{}"); exit == 0 || out != "" {
		t.Errorf("exit %d printing %q, want a silent failure", exit, out)
	}
}

// TestTheClientCheckPassesOnThisHost runs the provision's client check where
// the tests run: a host whose bash opens /dev/tcp and which has every tool.
func TestTheClientCheckPassesOnThisHost(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "bash", "-c", clientScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("client check: %v: %s", err, out)
	}
}

// TestTheClientCheckNamesWhatIsMissing runs it with a PATH that lacks
// base64, the way an image without coreutils' encoder would look.
func TestTheClientCheckNamesWhatIsMissing(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	dir := t.TempDir()
	for _, tool := range []string{"wc", "cat", "awk"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
		if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), bash, "-c", clientScript)
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "the image has no base64") {
		t.Errorf("client check = %v: %q, want a failure naming base64", err, out)
	}
}
