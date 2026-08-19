package main

import (
	"strings"
	"testing"
)

// Measured INFO fragments, one per verified engine version plus the
// append-only shape. The field set grows between releases — 7.4 added
// `subexpiry`, Valkey 9.1 `keys_with_volatile_items` — which is why the
// parse anchors on names and never on positions.
const (
	censusRDB = "# Persistence\r\nrdb_last_load_keys_expired:50\r\nrdb_last_load_keys_loaded:51\r\n" +
		"# Keyspace\r\ndb0:keys=50,expires=0,avg_ttl=0\r\ndb3:keys=1,expires=0,avg_ttl=0\r\n"
	censusRDBNewer = "# Persistence\r\nrdb_last_load_keys_expired:50\r\nrdb_last_load_keys_loaded:51\r\n" +
		"# Keyspace\r\ndb0:keys=50,expires=0,avg_ttl=0,subexpiry=0\r\n" +
		"db3:keys=1,expires=0,avg_ttl=0,subexpiry=0\r\n"
	censusRDBValkey9 = "# Persistence\r\nrdb_last_load_keys_expired:50\r\nrdb_last_load_keys_loaded:51\r\n" +
		"# Keyspace\r\ndb0:keys=50,expires=0,avg_ttl=0,keys_with_volatile_items=0\r\n" +
		"db3:keys=1,expires=0,avg_ttl=0,keys_with_volatile_items=0\r\n"
	// The append-only shape: the base RDB is read as an AOF preamble, so
	// nothing is dropped at load and the counters say so — the keys go a
	// few seconds later, to the ordinary expiry cycle (measured).
	censusAOF = "# Persistence\r\nrdb_last_load_keys_expired:0\r\nrdb_last_load_keys_loaded:200\r\n" +
		"# Keyspace\r\ndb0:keys=100,expires=0,avg_ttl=0\r\n"
	// Everything the artifact held is gone: the drill serves an empty
	// server, which is what the fence exists for.
	censusEmptied = "# Persistence\r\nrdb_last_load_keys_expired:100\r\nrdb_last_load_keys_loaded:0\r\n" +
		"# Keyspace\r\n"
	// A backup of an empty server. Nothing was lost and nothing may be
	// refused.
	censusEmptyBackup = "# Persistence\r\nrdb_last_load_keys_expired:0\r\nrdb_last_load_keys_loaded:0\r\n" +
		"# Keyspace\r\n"
)

func TestParseKeyCensusReadsTheEnginesOwnAccount(t *testing.T) {
	tests := []struct {
		name                              string
		info                              string
		loaded, expired, serving, carried int
	}{
		{"an rdb load that dropped expired keys", censusRDB, 51, 50, 51, 101},
		{"the field set of the newer releases", censusRDBNewer, 51, 50, 51, 101},
		{"the field set Valkey 9.1 reports", censusRDBValkey9, 51, 50, 51, 101},
		{"an append-only load, where nothing is dropped at load", censusAOF, 200, 0, 100, 200},
		{"an artifact whose every key had expired", censusEmptied, 0, 100, 0, 100},
		{"a backup of an empty server", censusEmptyBackup, 0, 0, 0, 0},
		// A server that answers something else has said nothing about the
		// artifact, and a fence must rest on the engine having spoken.
		{"an answer that is not INFO", "PONG\r\n", 0, 0, 0, 0},
		{"an answer with no numbers in it", "db0:keys=lots\r\nrdb_last_load_keys_loaded:many\r\n", 0, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseKeyCensus([]byte(tc.info))
			if got.loaded != tc.loaded || got.expired != tc.expired || got.serving != tc.serving {
				t.Errorf("census = %+v, want loaded=%d expired=%d serving=%d",
					got, tc.loaded, tc.expired, tc.serving)
			}
			if got.carried() != tc.carried {
				t.Errorf("carried = %d, want %d", got.carried(), tc.carried)
			}
		})
	}
}

// TestProvisionRefusesAnEmptyRestoredServer is the fence: an artifact
// that carried keys and a server that serves none is a drill with nothing
// to prove anything with, and it now says so instead of reporting a
// successful restore.
func TestProvisionRefusesAnEmptyRestoredServer(t *testing.T) {
	rdb := writeRDB(t, t.TempDir(), "dump.rdb", "7.2.5", "1786289869")
	var sequence []string
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "valkey_rdb", rdb, nil),
		provisionHandlerCensus(t, &sequence, censusEmptied))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Fatalf("final = %+v, want restore_failed for a drill with nothing to prove", f)
	}
	for _, want := range []string{"holds no keys", "at least 100", "not damaged", "younger"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
}

// TestProvisionAcceptsWhatTheFenceMustNotRefuse keeps the fence from
// becoming a nuisance. A backup of an empty server carried nothing, and a
// backup that lost only some of its keys still proves the rest — the
// residual the README states rather than papers over.
func TestProvisionAcceptsWhatTheFenceMustNotRefuse(t *testing.T) {
	tests := []struct {
		name   string
		census string
	}{
		{"a backup of an empty server", censusEmptyBackup},
		{"an artifact that lost some of its keys", censusRDB},
		{"an append-only artifact the expiry cycle thinned", censusAOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdb := writeRDB(t, t.TempDir(), "dump.rdb", "7.2.5", "1786289869")
			var sequence []string
			line, _, exit := driveOp(t, "provision", provisionPayload(t, "valkey_rdb", rdb, nil),
				provisionHandlerCensus(t, &sequence, tc.census))
			f := parseFinal(t, line)
			if exit != 0 || !f.OK {
				t.Fatalf("exit=%d final=%+v", exit, f)
			}
		})
	}
}
