package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// rdbFixture composes an RDB head the way the server writes one: magic,
// aux fields, then the first data opcode.
func rdbFixture(auxPairs ...[2]string) []byte {
	b := []byte("REDIS0011")
	for _, kv := range auxPairs {
		b = append(b, rdbOpcodeAux)
		b = appendRDBString(b, kv[0])
		b = appendRDBString(b, kv[1])
	}
	return append(b, 0xFE, 0x00) // SELECTDB: data begins, aux fields over
}

// appendRDBString writes the 6-bit length form, which covers every
// fixture value.
func appendRDBString(b []byte, s string) []byte {
	b = append(b, byte(len(s)))
	return append(b, s...)
}

func TestParseRDBMeta(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want rdbMeta
	}{
		{"version and ctime as strings",
			rdbFixture([2]string{"redis-ver", "7.2.5"}, [2]string{"ctime", "1786289869"}),
			rdbMeta{valid: true, redisVer: "7.2.5", ctime: 1786289869}},
		{"aux order does not matter",
			rdbFixture([2]string{"ctime", "1786289869"}, [2]string{"redis-ver", "7.2.5"}),
			rdbMeta{valid: true, redisVer: "7.2.5", ctime: 1786289869}},
		{"unknown aux fields are skipped",
			rdbFixture([2]string{"redis-bits", "64"}, [2]string{"redis-ver", "8.0.3"}),
			rdbMeta{valid: true, redisVer: "8.0.3"}},
		{"no aux fields at all",
			[]byte("REDIS0011\xFE\x00"),
			rdbMeta{valid: true}},
		{"unstable-build version keeps its shape",
			rdbFixture([2]string{"redis-ver", "255.255.255"}),
			rdbMeta{valid: true, redisVer: "255.255.255"}},
		{"non-version redis-ver is discarded",
			rdbFixture([2]string{"redis-ver", "whatever"}),
			rdbMeta{valid: true}},
		{"implausible ctime is discarded",
			rdbFixture([2]string{"ctime", "42"}),
			rdbMeta{valid: true}},
		{"wrong magic", []byte("RESP30110000"), rdbMeta{}},
		{"non-digit format version", []byte("REDISv011\xFE"), rdbMeta{}},
		{"truncated mid-value ends the parse cleanly",
			rdbFixture([2]string{"redis-ver", "7.4.2"})[:20],
			rdbMeta{valid: true}},
		{"empty input", nil, rdbMeta{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRDBMeta(tt.head); got != tt.want {
				t.Errorf("parseRDBMeta = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseRDBMetaIntEncodedCtime pins the encoding redis actually uses
// for numeric aux values: an int32-encoded string, little-endian.
func TestParseRDBMetaIntEncodedCtime(t *testing.T) {
	head := []byte("REDIS0012")
	head = append(head, rdbOpcodeAux)
	head = appendRDBString(head, "ctime")
	head = append(head, 0xC2) // 32-bit integer encoding
	head = binary.LittleEndian.AppendUint32(head, 1786289869)
	got := parseRDBMeta(head)
	if !got.valid || got.ctime != 1786289869 {
		t.Errorf("parseRDBMeta = %+v, want the int-encoded ctime", got)
	}
}

// TestParseRDBMetaStopsAtCompression pins the bail-out: an LZF-compressed
// aux value is out of scope, and the parse must end cleanly with what it
// has rather than misread lengths.
func TestParseRDBMetaStopsAtCompression(t *testing.T) {
	head := []byte("REDIS0011")
	head = append(head, rdbOpcodeAux)
	head = appendRDBString(head, "redis-ver")
	head = appendRDBString(head, "7.2.5")
	head = append(head, rdbOpcodeAux)
	head = appendRDBString(head, "ctime")
	head = append(head, 0xC3, 0x05) // LZF marker
	got := parseRDBMeta(head)
	if !got.valid || got.redisVer != "7.2.5" || got.ctime != 0 {
		t.Errorf("parseRDBMeta = %+v, want the pre-compression fields only", got)
	}
}

// TestParseRDBMetaFourteenBitLength covers the two-byte length form.
func TestParseRDBMetaFourteenBitLength(t *testing.T) {
	value := make([]byte, 100) // needs the 14-bit form once above 63
	for i := range value {
		value[i] = 'x'
	}
	head := []byte("REDIS0011")
	head = append(head, rdbOpcodeAux)
	head = appendRDBString(head, "some-field")
	head = append(head, 0x40, byte(len(value))) // 14-bit: 0b01, high 0, low 100
	head = append(head, value...)
	head = append(head, rdbOpcodeAux)
	head = appendRDBString(head, "redis-ver")
	head = appendRDBString(head, "7.4.2")
	if got := parseRDBMeta(head); got.redisVer != "7.4.2" {
		t.Errorf("parseRDBMeta = %+v, want the field after the long value", got)
	}
}

func TestReadRDBMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.rdb")
	if err := os.WriteFile(path, rdbFixture(
		[2]string{"redis-ver", "7.2.5"}, [2]string{"ctime", "1786289869"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRDBMeta(path); got.redisVer != "7.2.5" || got.ctime != 1786289869 {
		t.Errorf("readRDBMeta = %+v", got)
	}
	if got := readRDBMeta(filepath.Join(t.TempDir(), "gone.rdb")); got != (rdbMeta{}) {
		t.Errorf("missing file: %+v, want zero value", got)
	}
}

func TestOriginSeries(t *testing.T) {
	tests := []struct {
		version string
		major   int
		minor   int
		ok      bool
	}{
		{"7.2.5", 7, 2, true},
		{"8.0", 8, 0, true},
		{"255.255.255", 0, 0, false}, // unstable-build marker
		{"1.3.6", 0, 0, false},       // predates RDB as this adapter knows it
		{"whatever", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		major, minor, ok := originSeries(tt.version)
		if major != tt.major || minor != tt.minor || ok != tt.ok {
			t.Errorf("originSeries(%q) = %d.%d/%v, want %d.%d/%v",
				tt.version, major, minor, ok, tt.major, tt.minor, tt.ok)
		}
	}
}

func TestCheckEngineVersion(t *testing.T) {
	const engine72 = "Redis server v=7.2.5 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64 build=abc"
	tests := []struct {
		name     string
		redisVer string
		engine   string
		refused  bool
	}{
		{"newer backup on older engine refused", "7.4.2", engine72, true},
		{"newer major refused", "8.0.3", engine72, true},
		{"same series passes", "7.2.4", engine72, false},
		{"older backup on newer engine is the supported path", "6.2.14", engine72, false},
		{"no backup version skips", "", engine72, false},
		{"unstable marker skips", "255.255.255", engine72, false},
		{"unparseable engine output skips", "7.4.2", "1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perr := checkEngineVersion(tt.redisVer, tt.engine)
			if (perr != nil) != tt.refused {
				t.Fatalf("checkEngineVersion = %+v, refused=%v", perr, tt.refused)
			}
			if perr != nil && perr.Code != "invalid_request" {
				t.Errorf("code = %s, want invalid_request", perr.Code)
			}
		})
	}
}
