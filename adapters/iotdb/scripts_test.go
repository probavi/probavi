package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCurl stands in for curl inside the scripts: it records the config
// line curl would read from its standard input and the request it would
// send, and answers from the scenario's handler — a bash function that sets
// out (the body) and code (the HTTP status) for a path and a body.
const fakeCurl = `#!/bin/bash
cfg=$(cat)
printf '%s\n' "$cfg" >> "$FAKE_DIR/config"
body= url=
while [ $# -gt 0 ]; do
  case "$1" in
    --data-binary) body=$2; shift 2 ;;
    -K|-H|-w) shift 2 ;;
    -s) shift ;;
    *) url=$1; shift ;;
  esac
done
path=${url#http://127.0.0.1:18080}
printf '%s\t%s\n' "$path" "$body" >> "$FAKE_DIR/requests"
out= code=200
. "$FAKE_DIR/handler"
respond "$path" "$body"
printf '%s\n%s' "$out" "$code"
`

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
}

// runScript runs one of this adapter's scripts on the host against the fake
// curl, with the scenario's handler.
func runScript(t *testing.T, script, handler string, env map[string]string, args ...string) scriptRun {
	t.Helper()
	for _, tool := range []string{"bash", "awk", "mktemp", "grep", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler"), []byte(handler), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", script, "bash"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+dir)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exit = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run script: %v", err)
	}
	run.requests, run.config = recorded(t, dir, "requests"), recorded(t, dir, "config")
	return run
}

// recorded reads what the fake curl wrote, which is nothing when the script
// never called it.
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

// TestRunnerQuotesTheLoginForCurlsConfig pins the password's route: curl's
// standard input, quoted by curl's own rules, and never an argument.
func TestRunnerQuotesTheLoginForCurlsConfig(t *testing.T) {
	handler := precisionIs("ms") + `answer() { out='{"values":[[1]],"data_types":["INT32"]}'; }` + "\n"
	run := runScript(t, runnerScript, handler, map[string]string{passwordEnv: `p\a"ss`}, "", `au"d`, "select 1")
	if run.exit != 0 {
		t.Fatalf("exit %d: %s", run.exit, run.stderr)
	}
	if want := `user = "au\"d:p\\a\"ss"`; !strings.Contains(run.config, want) {
		t.Errorf("curl config = %q, want %q", run.config, want)
	}
	defaulted := runScript(t, runnerScript, handler, map[string]string{passwordEnv: ""}, "", "root", "select 1")
	if !strings.Contains(defaulted.config, `user = "root:root"`) {
		t.Errorf("curl config = %q, want IoTDB's default password when none is set", defaulted.config)
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
