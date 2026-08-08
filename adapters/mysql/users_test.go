package main

import (
	"strings"
	"testing"
)

// The stderr fixtures below mirror the real client's shape, captured from
// a MySQL 8.4 instance: one "ERROR NNNN (state)" line per failure, warning
// lines prefixed "mysql:", and an optional "in file: ..." fragment in
// source-command mode.

func TestUsersFailureClassification(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		wantOK  bool
		wantHas string
	}{
		{"clean replay is silent", "", true, ""},
		{"whitespace only", "\n  \n", true, ""},
		{"client warning is not a failure",
			"mysql: [Warning] Using a password on the command line interface can be insecure.", true, ""},
		{"root collision tolerated",
			"ERROR 1396 (HY000) at line 1: Operation CREATE USER failed for 'root'@'localhost'", true, ""},
		{"root collision in source mode tolerated",
			"ERROR 1396 (HY000) at line 1 in file: '/tmp/users.sql': Operation CREATE USER failed for 'root'@'localhost'", true, ""},
		{"reserved system account collision tolerated",
			"ERROR 1396 (HY000) at line 1: Operation CREATE USER failed for 'mysql.sys'@'localhost'", true, ""},
		{"all sandbox collisions tolerated",
			"ERROR 1396 (HY000) at line 1: Operation CREATE USER failed for 'root'@'localhost'\n" +
				"ERROR 1396 (HY000) at line 2: Operation CREATE USER failed for 'mysql.infoschema'@'localhost'", true, ""},
		{"application account collision fails",
			"ERROR 1396 (HY000) at line 3: Operation CREATE USER failed for 'app'@'%'",
			false, "app"},
		{"tolerated then real failure",
			"ERROR 1396 (HY000) at line 1: Operation CREATE USER failed for 'root'@'localhost'\n" +
				"ERROR 1064 (42000) at line 2: You have an error in your SQL syntax",
			false, "1064"},
		{"syntax error fails",
			"ERROR 1064 (42000) at line 1: You have an error in your SQL syntax; check the manual", false, "1064"},
		{"grant-without-user fails",
			"ERROR 1410 (42000) at line 1: You are not allowed to create a user with GRANT", false, "1410"},
		{"bad hash format fails",
			"ERROR 1827 (HY000) at line 1: The password hash doesn't have the expected format.", false, "1827"},
		{"non-1396 error naming root still fails",
			"ERROR 1064 (42000) at line 1: Operation CREATE USER failed for 'root'@'localhost'", false, "1064"},
		{"unopenable file fails",
			"ERROR at line 1: Failed to open file '/tmp/never.sql', error: 2", false, "Failed to open"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usersFailure([]byte(tt.stderr))
			if tt.wantOK {
				if got != "" {
					t.Errorf("usersFailure = %q, want tolerated", got)
				}
				return
			}
			if got == "" || !strings.Contains(got, tt.wantHas) {
				t.Errorf("usersFailure = %q, want a failure containing %q", got, tt.wantHas)
			}
		})
	}
}

func TestUsersFailureIsProtocolSafe(t *testing.T) {
	stderr := `ERROR 1064 (42000) at line 1: syntax error near "0x2441243030352434feedface1122334455667788"` + "\nsecond line"
	got := usersFailure([]byte(stderr))
	if strings.ContainsAny(got, "\"\n") {
		t.Errorf("usersFailure = %q — must be single-line and quote-free for protocol embedding", got)
	}
	if strings.Contains(got, "feedface") {
		t.Errorf("usersFailure = %q — hash material must be scrubbed", got)
	}
}

func TestScrubSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		bad  string
	}{
		// Measured: the server quotes the offending token back in syntax
		// errors, and a users script's tokens include password hashes.
		{"hex hash literal echoed by syntax error",
			"near '0x2441243030352434FEEDFACE1122334455667788' at line 1", "FEEDFACE"},
		{"lowercase hex hash literal",
			"near '0x2441243030352434feedface1122334455667788'", "feedface"},
		{"identified with ... as hex",
			"CREATE USER `app`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x2441FEEDFACE11223344556677", "FEEDFACE"},
		{"identified by plaintext",
			"in 'CREATE USER x IDENTIFIED BY 'Sup3rS3cret!' BOGUS' at line 1", "Sup3rS3cret!"},
		{"caching_sha2 dollar hash literal",
			"AS '$A$005$saltFEEDFACEhashmaterial' REQUIRE NONE", "FEEDFACE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrubSecrets(tt.in); strings.Contains(got, tt.bad) {
				t.Errorf("scrubSecrets(%q) = %q — still carries %q", tt.in, got, tt.bad)
			}
		})
	}

	t.Run("short hex survives", func(t *testing.T) {
		in := "table page 0x1A2B is corrupt"
		if got := scrubSecrets(in); got != in {
			t.Errorf("scrubSecrets(%q) = %q — short identifiers are not secrets", in, got)
		}
	})
	t.Run("firstLine scrubs on the shared path", func(t *testing.T) {
		got := firstLine([]byte("near '0x2441243030352434feedface11223344'\nrest"))
		if strings.Contains(got, "feedface") {
			t.Errorf("firstLine = %q — the shared message path must scrub", got)
		}
	})
}

func TestNameList(t *testing.T) {
	if got := nameList([]string{"a@x", "b@y"}, 5); got != "a@x, b@y" {
		t.Errorf("nameList = %q", got)
	}
	got := nameList([]string{"a", "b", "c", "d", "e", "f", "g"}, 5)
	if got != "a, b, c, d, e and 2 more" {
		t.Errorf("nameList = %q, want the capped form", got)
	}
}
