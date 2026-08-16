package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasSQLiteMagic(t *testing.T) {
	tests := []struct {
		name string
		head string
		want bool
	}{
		{"the magic alone", sqliteMagic, true},
		{"the magic plus header bytes", sqliteMagic + "\x10\x00\x01\x01", true},
		{"truncated magic", sqliteMagic[:10], false},
		{"sql text", "PRAGMA foreign_keys=OFF;\n", false},
		{"empty", "", false},
		{"the magic without its NUL terminator", "SQLite format 3 ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSQLiteMagic([]byte(tt.head)); got != tt.want {
				t.Errorf("hasSQLiteMagic = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasDumpSignature(t *testing.T) {
	tests := []struct {
		name string
		head string
		want bool
	}{
		{"a dump's exact opening", dumpSignature + "CREATE TABLE t(id);\n", true},
		{"the empty database's dump", dumpSignature + "COMMIT;\n", true},
		{"generic sql text", "BEGIN TRANSACTION;\nCREATE TABLE t(id);\n", false},
		{"crlf line endings are not the signature", "PRAGMA foreign_keys=OFF;\r\nBEGIN TRANSACTION;\r\n", false},
		{"a database file", sqliteMagic, false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasDumpSignature([]byte(tt.head)); got != tt.want {
				t.Errorf("hasDumpSignature = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsGzip(t *testing.T) {
	if !isGzip([]byte{0x1f, 0x8b, 0x08}) {
		t.Error("gzip header not recognised")
	}
	if isGzip([]byte{0x1f}) || isGzip([]byte(sqliteMagic)) || isGzip(nil) {
		t.Error("non-gzip bytes recognised as gzip")
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"a dump's trailer", dumpSignature + "INSERT INTO t VALUES(1);\nCOMMIT;\n", "COMMIT;"},
		{"trailing blank lines are looked past", "COMMIT;\n\n\n", "COMMIT;"},
		{"no final newline", "INSERT INTO t VALUES(1,'row", "INSERT INTO t VALUES(1,'row"},
		{"trailing spaces are trimmed", "COMMIT; \t\n", "COMMIT;"},
		{"empty file", "", ""},
		{"single line", "COMMIT;", "COMMIT;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dump.sql")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := lastNonEmptyLine(path)
			if err != nil {
				t.Fatalf("lastNonEmptyLine: %v", err)
			}
			if got != tt.want {
				t.Errorf("lastNonEmptyLine = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a final line longer than the window comes back as a fragment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dump.sql")
		long := "INSERT INTO t VALUES(X'" + strings.Repeat("ab", tailMax) + "'"
		if err := os.WriteFile(path, []byte(dumpSignature+long), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := lastNonEmptyLine(path)
		if err != nil {
			t.Fatalf("lastNonEmptyLine: %v", err)
		}
		if got == dumpTrailer || len(got) == 0 {
			t.Errorf("lastNonEmptyLine = %q, want a fragment that can never equal the trailer", got)
		}
	})

	t.Run("missing file reports the error", func(t *testing.T) {
		if _, err := lastNonEmptyLine(filepath.Join(t.TempDir(), "gone")); err == nil {
			t.Error("lastNonEmptyLine = nil error for a missing file")
		}
	})
}
