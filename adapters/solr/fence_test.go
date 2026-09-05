package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const expiringConfig = `<config><processor class="solr.processor.` +
	expirationClass + `"/></config>`

// TestExpiringConfigsReadsOnlyTheArtifact pins what the directory pass
// may read. A backup file is attacker-shaped input (SECURITY.md), and a
// solrconfig.xml that is a symlink must not make the fence read whatever
// it points at: the archive pass over the same tree ignores every entry
// that is not a regular file, and the two must agree about one backup.
func TestExpiringConfigsReadsOnlyTheArtifact(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.xml")
	if err := os.WriteFile(outside, []byte(expiringConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		// link is the target of a solrconfig.xml planted as a symlink,
		// empty for none; relative names resolve inside the artifact.
		link string
		want []string
	}{
		"a configuration file the artifact holds itself": {
			want: []string{"orders/zk_backup_0/configs/expiring/solrconfig.xml"},
		},
		"a symlink out of the artifact": {
			link: outside,
		},
		"a symlink within the artifact": {
			link: "../c/solrconfig.xml",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := writeBackup(t, map[string]string{
				"zk_backup_0/configs/c/solrconfig.xml": expiringConfig,
			})
			planted := filepath.Join(artifact, "orders", "zk_backup_0", "configs", "expiring", "solrconfig.xml")
			if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.link == "" {
				if err := os.WriteFile(planted, []byte(expiringConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(tt.link, planted); err != nil {
				t.Fatal(err)
			}

			found, err := expiringConfigs(artifact)
			if err != nil {
				t.Fatalf("expiringConfigs: %v", err)
			}
			// The fixture's own configset carries the class too, so the
			// planted file is what each case is actually about.
			want := append([]string{"orders/zk_backup_0/configs/c/solrconfig.xml"}, tt.want...)
			if strings.Join(found, "|") != strings.Join(want, "|") {
				t.Errorf("expiringConfigs = %v, want %v", found, want)
			}
		})
	}
}

// TestExpiringConfigsBoundsWhatItReads keeps the directory pass and the
// archive pass reading the same amount of one configuration file. An
// artifact must not choose how much memory the drill host spends.
func TestExpiringConfigsBoundsWhatItReads(t *testing.T) {
	artifact := writeBackup(t, map[string]string{
		"zk_backup_0/configs/c/solrconfig.xml": strings.Repeat("<!-- padding -->", maxConfigBytes/16) +
			expiringConfig,
	})
	found, err := expiringConfigs(artifact)
	if err != nil {
		t.Fatalf("expiringConfigs: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("expiringConfigs = %v, want nothing: the class sits past the %d-byte bound",
			found, maxConfigBytes)
	}
}

// TestExpiringConfigsSkipsAnArchive leaves an artifact that is a file to
// the streaming pass, which reads a tar the fence cannot walk.
func TestExpiringConfigsSkipsAnArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "nightly.tar")
	if err := os.WriteFile(archive, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := expiringConfigs(archive)
	if err != nil {
		t.Fatalf("expiringConfigs: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("expiringConfigs = %v, want nothing for an archive", found)
	}
}
