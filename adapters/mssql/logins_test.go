package main

import (
	"strings"
	"testing"
)

// The stderr fixtures below mirror the real server's shape, captured from
// a SQL Server 2022 instance: "Msg N, Level L, State S, Server x, Line 1"
// header lines with the message text after each, and header-less client
// lines for sqlcmd's own failures.

func header(msg string) string {
	return "Msg " + msg + ", Level 16, State 1, Server x, Line 1\n"
}

func TestLoginsFailureClassification(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		wantOK  bool
		wantHas string
	}{
		{"clean replay is silent", "", true, ""},
		{"whitespace only", "\n  \n", true, ""},
		{"sa collision tolerated",
			header("15025") + "The server principal 'sa' already exists.", true, ""},
		{"internal principal collision tolerated",
			header("15025") + "The server principal '##MS_PolicyEventProcessingLogin##' already exists.", true, ""},
		{"both sandbox collisions tolerated",
			header("15025") + "The server principal 'sa' already exists.\n" +
				header("15025") + "The server principal '##MS_PolicyTsqlExecutionLogin##' already exists.", true, ""},
		{"application login collision fails",
			header("15025") + "The server principal 'app_login' already exists.",
			false, "app_login"},
		{"half-wrapped name is not internal",
			header("15025") + "The server principal '##half' already exists.",
			false, "##half"},
		{"tolerated then real failure",
			header("15025") + "The server principal 'sa' already exists.\n" +
				header("102") + "Incorrect syntax near 'SID'.",
			false, "Incorrect syntax"},
		{"syntax error fails",
			header("102") + "Incorrect syntax near 'SID'.", false, "Incorrect syntax"},
		{"non-15025 message with exists-shaped text fails",
			header("15151") + "The server principal 'sa' already exists.", false, "already exists"},
		{"drop-shaped failure",
			header("15151") + "Cannot drop the login 'x', because it does not exist or you do not have permission.",
			false, "Cannot drop"},
		{"client error without header fails",
			"Sqlcmd: Error: Microsoft ODBC Driver 18 for SQL Server : Login failed for user 'sa'..",
			false, "Login failed"},
		{"invalid filename client line fails",
			"Sqlcmd: '/tmp/logins.sql': Invalid filename.", false, "Invalid filename"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loginsFailure([]byte(tt.stderr))
			if tt.wantOK {
				if got != "" {
					t.Errorf("loginsFailure = %q, want tolerated", got)
				}
				return
			}
			if got == "" || !strings.Contains(got, tt.wantHas) {
				t.Errorf("loginsFailure = %q, want a failure containing %q", got, tt.wantHas)
			}
		})
	}
}

func TestLoginsFailureIsProtocolSafe(t *testing.T) {
	stderr := header("102") + `Incorrect syntax near "0x0200feedface001122334455".` + "\nsecond line"
	got := loginsFailure([]byte(stderr))
	if strings.ContainsAny(got, "\"\n") {
		t.Errorf("loginsFailure = %q — must be single-line and quote-free for protocol embedding", got)
	}
	if strings.Contains(got, "feedface") {
		t.Errorf("loginsFailure = %q — hash material must be scrubbed", got)
	}
}

func TestScrubSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		bad  string
	}{
		// Measured: the parser echoes the offending token back lowercased,
		// hash included.
		{"hash literal echoed by syntax error",
			"Incorrect syntax near '0x0200feedface00112233445566778899aabb'.", "feedface"},
		{"uppercase hash literal",
			"near '0x0200FEEDFACE00112233445566778899AABB'", "FEEDFACE"},
		{"plaintext password literal",
			"CREATE LOGIN [x] WITH PASSWORD = 'S3cret!' failed", "S3cret!"},
		{"national password literal",
			"PASSWORD = N'S3cret!' rejected", "S3cret!"},
		{"spaced password literal",
			"password  =  'S3cret!'", "S3cret!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrubSecrets(tt.in); strings.Contains(got, tt.bad) {
				t.Errorf("scrubSecrets(%q) = %q — still carries %q", tt.in, got, tt.bad)
			}
		})
	}

	t.Run("short hex survives", func(t *testing.T) {
		in := "restore of file 0x01 failed"
		if got := scrubSecrets(in); got != in {
			t.Errorf("scrubSecrets(%q) = %q — short identifiers are not secrets", in, got)
		}
	})
	t.Run("firstLine scrubs on the shared path", func(t *testing.T) {
		got := firstLine([]byte("near '0x0200feedface00112233445566778899'\nrest"))
		if strings.Contains(got, "feedface") {
			t.Errorf("firstLine = %q — the shared message path must scrub", got)
		}
	})
	t.Run("verdictLine scrubs on the shared path", func(t *testing.T) {
		got := verdictLine([]byte(header("102") + "near '0x0200feedface00112233445566778899'"))
		if strings.Contains(got, "feedface") {
			t.Errorf("verdictLine = %q — the shared message path must scrub", got)
		}
	})
}
