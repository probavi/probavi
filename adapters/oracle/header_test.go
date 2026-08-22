package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseHeaderReadsTheMeasuredShape(t *testing.T) {
	h := parseHeader([]byte(fixtureHeader))
	if !h.known || h.fileType != fileTypeDataPump {
		t.Fatalf("header = %+v", h)
	}
	for code, want := range map[string]string{
		itemFileVersion:   "6.1",
		itemDBVersion:     "23.06.00.00.00",
		itemCreationDate:  "Fri Aug 21 12:04:02 2026",
		itemJobName:       `"SYSTEM"."SYS_EXPORT_SCHEMA_01"`,
		itemInstance:      "012cb46a7cb7:FREE",
		itemGUID:          "598E6E466AB80430E063030015ACEC99",
		itemDataEncrypted: "0",
	} {
		if got := h.item(code); got != want {
			t.Errorf("item %s = %q, want %q", code, got, want)
		}
	}
	if h.encrypted() != "" {
		t.Errorf("encrypted() = %q for a plain dump", h.encrypted())
	}
	if perr := vetHeader(h, "23.26.3.0.0"); perr != nil {
		t.Errorf("vetHeader refused the measured header: %+v", perr)
	}
}

func TestParseHeaderStaysUnknownOnForeignOutput(t *testing.T) {
	for _, in := range []string{"", "1\n", "filetype=x\n", "ORA-39211: unable to retrieve dumpfile information\n"} {
		h := parseHeader([]byte(in))
		if h.known {
			t.Errorf("parseHeader(%q).known = true", in)
		}
		if perr := vetHeader(h, "23.26.3.0.0"); perr != nil {
			t.Errorf("vetHeader(%q) = %+v, want silence on an unknown header", in, perr)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	tests := []struct {
		dump, engine string
		want         bool
	}{
		{"23.06.00.00.00", "23.26.3.0.0", false},
		{"23.26.00.00.00", "23.26.3.0.0", false},
		{"23.26.04.00.00", "23.26.3.0.0", true},
		{"26.01.00.00.00", "23.26.3.0.0", true},
		{"19.00.00.00.00", "23.26.3.0.0", false},
		{"23.26.3", "23.26.3.0.0", false},
		{"23.26.3.0.1", "23.26.3", true},
		{"", "23.26.3.0.0", false},
		{"23.06.00.00.00", "", false},
		{"1", "23.26.3.0.0", false},
		{"abc", "23.26.3.0.0", false},
	}
	for _, tt := range tests {
		if got := versionNewer(tt.dump, tt.engine); got != tt.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", tt.dump, tt.engine, got, tt.want)
		}
	}
}

func TestParseIdentity(t *testing.T) {
	id := parseIdentity([]byte(fixtureIdentity))
	want := engineIdentity{version: "23.26.3.0.0", pdbs: []string{"FREEPDB1"},
		pins: map[string]string{"job_queue_processes": "0", "aq_tm_processes": "0"}}
	if !reflect.DeepEqual(id, want) {
		t.Errorf("parseIdentity = %+v, want %+v", id, want)
	}
	if id := parseIdentity([]byte("1\n")); id.version != "" || len(id.pdbs) != 0 || len(id.pins) != 0 {
		t.Errorf("parseIdentity(\"1\") = %+v, want the zero identity", id)
	}
}

func TestCreatedAt(t *testing.T) {
	budapest, err := time.LoadLocation("Europe/Budapest")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		clock string
		loc   *time.Location
		want  string
	}{
		{"Fri Aug 21 12:04:02 2026", time.UTC, "2026-08-21T12:04:02.000Z"},
		{"Fri Aug 21 12:04:02 2026", budapest, "2026-08-21T12:04:02.000+02:00"},
		{"Sat Jan  3 07:05:09 2026", budapest, "2026-01-03T07:05:09.000+01:00"},
		{"Fri Aug 21 12:04:02 2026", nil, ""},
		{"", time.UTC, ""},
		{"2026-08-21", time.UTC, ""},
	}
	for _, tt := range tests {
		got := createdAt(tt.clock, tt.loc)
		switch {
		case tt.want == "" && got != nil:
			t.Errorf("createdAt(%q) = %q, want nil", tt.clock, *got)
		case tt.want != "" && (got == nil || *got != tt.want):
			t.Errorf("createdAt(%q) = %v, want %q", tt.clock, got, tt.want)
		}
	}
}

func TestEncryptedNamesTheFirstClaim(t *testing.T) {
	h := parseHeader([]byte(strings.ReplaceAll(fixtureHeader, "item21=0", "item21=1")))
	if got := h.encrypted(); got != "metadata" {
		t.Errorf("encrypted() = %q, want metadata", got)
	}
}
