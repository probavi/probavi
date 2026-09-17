package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// answeredCore is a core whose next sandbox calls are answered as given,
// so a step can be driven straight to the branch under test. A core built
// with no answers is one whose stream is gone: the step then meets the
// harness's own failure, which is what it meets when the core goes away
// mid-operation.
func answeredCore(t *testing.T, answers ...any) *core {
	t.Helper()
	var lines strings.Builder
	for i, val := range answers {
		raw, err := json.Marshal(val)
		if err != nil {
			t.Fatal(err)
		}
		lines.WriteString(sandboxResultLine(
			`{"call_id":"c` + strconv.Itoa(i+1) + `","ok":true,"value":` + string(raw) + `}`))
	}
	return harnessCore(lines.String(), io.Discard)
}

// said is an exec answer carrying what the sandbox printed.
func said(exit int, stdout, stderr string) execValue {
	return execValue{
		ExitCode:        exit,
		StdoutB64:       base64.StdEncoding.EncodeToString([]byte(stdout)),
		StderrB64:       base64.StdEncoding.EncodeToString([]byte(stderr)),
		DurationSeconds: 0.25,
	}
}

// TestPreparingTheNodeAndTheImageAreReportedApart: the image is the drill
// config's business and the node preparation is the adapter's, so a
// failure in either says which it was.
func TestPreparingTheNodeAndTheImageAreReportedApart(t *testing.T) {
	t.Run("an image without the engine", func(t *testing.T) {
		perr := checkEngine(context.Background(), answeredCore(t, said(127, "", "bash: cqlsh: command not found\n")))
		if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "cqlsh") {
			t.Errorf("perr = %+v, want invalid_request naming what the image lacks", perr)
		}
	})
	t.Run("a node that could not be pinned to loopback", func(t *testing.T) {
		perr := prepareNode(context.Background(),
			answeredCore(t, said(1, "", "sed: /etc/cassandra/cassandra.yaml: Permission denied\n")))
		if perr == nil || perr.Code != "internal" || !strings.Contains(perr.Message, "Permission denied") {
			t.Errorf("perr = %+v, want internal carrying the sandbox's words", perr)
		}
	})
	t.Run("a prepared node", func(t *testing.T) {
		if perr := prepareNode(context.Background(), answeredCore(t, said(0, "", ""))); perr != nil {
			t.Errorf("perr = %+v, want a prepared node to pass", perr)
		}
	})
	t.Run("a core that went away mid-call", func(t *testing.T) {
		if perr := prepareNode(context.Background(), answeredCore(t)); perr == nil || perr.Code != "internal" {
			t.Errorf("perr = %+v, want the harness's own failure", perr)
		}
	})
}

// TestUnpackArchiveFindsTheKeyspacesWhereverTheArchiveNestedThem: a
// collected snapshot tars either the keyspaces at its root or one
// wrapping directory above them, so the sandbox is asked where they
// landed — and the extraction directory stands in when it cannot say.
func TestUnpackArchiveFindsTheKeyspacesWhereverTheArchiveNestedThem(t *testing.T) {
	const workDir = "/work"
	put := putFileValue{BytesCopied: 4096, DurationSeconds: 0.5}
	for name, tc := range map[string]struct {
		answers []any
		wantDir string
		code    string
		message string
	}{
		"keyspaces one level down": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(0, workDir+"/extract/snapshot\n", "")},
			workDir + "/extract/snapshot", "", "",
		},
		"an answer the sandbox could not give": {
			[]any{said(0, "", ""), put, said(0, "", ""), said(1, "", "bash: bad substitution\n")},
			workDir + "/extract", "", "",
		},
		"an archive tar refused": {
			[]any{said(0, "", ""), put, said(2, "", "tar: invalid tar magic\n")}, "", "source_corrupt", "invalid tar magic",
		},
		"a work directory that could not be made": {
			[]any{said(1, "", "mkdir: permission denied\n")}, "", "internal", "prepare work directory",
		},
		"a core that went away mid-call": {nil, "", "internal", "closed the stream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, unpack, dataRoot, perr := unpackArchive(context.Background(),
				answeredCore(t, tc.answers...), "/backups/snapshot.tar", workDir)
			if tc.code != "" {
				if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
					t.Fatalf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
				}
				return
			}
			if perr != nil || dataRoot != tc.wantDir || unpack <= 0 {
				t.Errorf("unpackArchive = %v, %q, %+v, want %q after a measured unpack", unpack, dataRoot, perr, tc.wantDir)
			}
		})
	}
}

// TestTransferTreeRecreatesTheSnapshotFileByFile: a directory artifact
// moves as its own files, and a tree the host cannot walk is the host's
// failure to report rather than the sandbox's.
func TestTransferTreeRecreatesTheSnapshotFileByFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "shop", "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"shop/orders/schema.cql", "shop/orders/nb-1-big-Data.db"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put := putFileValue{BytesCopied: 5, DurationSeconds: 0.25}
	seconds, perr := transferTree(context.Background(),
		answeredCore(t, said(0, "", ""), put, put), dir, "/work/snapshot")
	if perr != nil || seconds != 0.5 {
		t.Errorf("transferTree = %v, %+v, want the two transfers it measured", seconds, perr)
	}
	if _, perr = transferTree(context.Background(), answeredCore(t), filepath.Join(dir, "gone"), "/work/snapshot"); perr == nil ||
		perr.Code != "source_unreadable" {
		t.Errorf("perr = %+v, want source_unreadable", perr)
	}
}

// TestStartEngineCarriesTheNodesOwnReason: the node starts on an empty
// data directory — the backup is streamed into it afterwards, by
// sstableloader — so a node that will not come up is never the backup's
// fault, and every refusal here says restore_failed with the node's own
// last line rather than blaming the artifact.
func TestStartEngineCarriesTheNodesOwnReason(t *testing.T) {
	const logPath = "/work/cassandra.log"
	for name, tc := range map[string]struct {
		answers []any
		code    string
		message string
	}{
		"a node that came up": {[]any{said(0, "", ""), said(0, "", "")}, "", ""},
		"a node that would not launch": {
			[]any{said(1, "", "bash: cassandra: command not found\n")}, "restore_failed", "failed to launch",
		},
		"a node that died during startup": {
			[]any{said(0, "", ""), said(1, "", ""), said(0, "", ""), said(0,
				"INFO  [main] CassandraDaemon.java:1 - Starting\n"+
					"ERROR [main] CassandraDaemon.java:900 - Exception encountered during startup: "+
					"Port already in use\n", "")},
			"restore_failed", "Port already in use",
		},
		"a node whose log says nothing": {
			[]any{said(0, "", ""), said(1, "", ""), said(0, "", ""), said(0, "INFO  [main] Starting\n", "")},
			"engine_not_ready", "exited during startup",
		},
		"a log that could not be read": {
			[]any{said(0, "", ""), said(1, "", ""), said(0, "", ""), said(1, "", "tail: no such file\n")},
			"engine_not_ready", "exited during startup",
		},
	} {
		t.Run(name, func(t *testing.T) {
			seconds, perr := startEngine(context.Background(), answeredCore(t, tc.answers...), logPath)
			if tc.code == "" {
				if perr != nil || seconds < 0 {
					t.Errorf("startEngine = %v, %+v, want the wait it measured", seconds, perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestRestoreTablesReportsWhichTableAndWhy: a restore that stops says
// which table it stopped on, and a schema an older engine cannot parse is
// a drill configuration problem rather than a damaged backup.
func TestRestoreTablesReportsWhichTableAndWhy(t *testing.T) {
	tables := []tableRef{{keyspace: "shop", table: "orders"}}
	for name, tc := range map[string]struct {
		answers []any
		code    string
		message string
	}{
		"a table restored": {
			[]any{said(0, "", ""), said(0, "", ""), said(0, "", "")}, "", "",
		},
		"a keyspace the engine refused": {
			[]any{said(1, "", "ConfigurationException: bad replication\n")}, "restore_failed", "creating keyspace shop",
		},
		"a schema this engine does not know": {
			[]any{said(0, "", ""), said(2, "", "ConfigurationException: Unknown property 'allow_auto_snapshot'\n")},
			"invalid_request", "use a cassandra image at least as new",
		},
		"a schema refused for another reason": {
			[]any{said(0, "", ""), said(2, "", "AuthenticationException: role has no permission\n")},
			"restore_failed", "applying the schema for shop.orders",
		},
		"sstables the loader would not stream": {
			[]any{said(0, "", ""), said(0, "", ""), said(1, "", "Error: could not connect to 127.0.0.1\n")},
			"restore_failed", "sstableloader failed for shop.orders",
		},
	} {
		t.Run(name, func(t *testing.T) {
			seconds, perr := restoreTables(context.Background(), answeredCore(t, tc.answers...), "/work/snapshot", tables)
			if tc.code == "" {
				if perr != nil || seconds != 0.75 {
					t.Errorf("restoreTables = %v, %+v, want the three steps it measured", seconds, perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestTheDeclaredTTLIsReadAsTheArtifactsOwnClaim: the fence reads the
// table's own TTL out of the artifact, and a probe that could not answer
// is not an accusation — the table passes.
func TestTheDeclaredTTLIsReadAsTheArtifactsOwnClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		answer any
		want   int
	}{
		"a table that expires its rows":  {said(0, "86400\n", ""), 86400},
		"a table that does not":          {said(0, "0\n", ""), 0},
		"a probe that could not run":     {said(3, "", "sstableutil: not found\n"), 0},
		"an answer that is not a number": {said(0, "forever\n", ""), 0},
	} {
		t.Run(name, func(t *testing.T) {
			ttl, _, perr := declaredTTL(context.Background(), answeredCore(t, tc.answer), "/work/snapshot/shop/orders")
			if perr != nil {
				t.Fatalf("perr = %+v", perr)
			}
			if ttl != tc.want {
				t.Errorf("declaredTTL = %d, want %d", ttl, tc.want)
			}
		})
	}
}
