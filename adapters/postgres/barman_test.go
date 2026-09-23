package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// barman_test.go builds Barman server directories in the shape a real one
// has, measured against Barman 3.20.0: a key=value manifest per backup
// under meta/, the cluster under base/<id>/data/, and plain uncompressed
// WAL segments under wals/<first sixteen>/.

const (
	barmanFull = "20260923T183446"
	barmanNext = "20260923T190000"
)

// barmanSpec describes a catalogue to write out for one test.
type barmanSpec struct {
	backups  []barmanBackup
	segments []string
	omitData []string // backup ids whose base/<id>/data is not on disk
	noMeta   bool
}

// twoBarmanBackups is a catalogue with a full and a later backup that
// names it as its parent — the shape an incremental catalogue has.
func twoBarmanBackups() []barmanBackup {
	return []barmanBackup{
		{
			id: barmanFull, status: barmanDone,
			beginWAL: "000000010000000000000005", endWAL: "000000010000000000000005",
			endTime: time.Date(2026, 9, 23, 18, 34, 46, 0, time.UTC),
		},
		{
			id: barmanNext, parent: barmanFull, status: barmanDone,
			beginWAL: "000000010000000000000009", endWAL: "00000001000000000000000A",
			endTime: time.Date(2026, 9, 23, 19, 0, 0, 0, time.UTC),
		},
	}
}

func barmanSegments() []string {
	return []string{
		"000000010000000000000005", "000000010000000000000006",
		"000000010000000000000009", "00000001000000000000000A",
	}
}

func withoutBarmanSegment(drop string) []string {
	var out []string
	for _, s := range barmanSegments() {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// writeBarmanServer materialises a catalogue and returns its path.
func writeBarmanServer(t *testing.T, spec barmanSpec) string {
	t.Helper()
	dir := t.TempDir()

	skip := make(map[string]bool, len(spec.omitData))
	for _, id := range spec.omitData {
		skip[id] = true
	}

	for _, b := range spec.backups {
		if !spec.noMeta {
			var sb strings.Builder
			fmt.Fprintf(&sb, "backup_id=%s\nstatus=%s\n", b.id, b.status)
			fmt.Fprintf(&sb, "begin_wal=%s\nend_wal=%s\n", b.beginWAL, b.endWAL)
			if b.parent == "" {
				sb.WriteString("parent_backup_id=None\n")
			} else {
				fmt.Fprintf(&sb, "parent_backup_id=%s\n", b.parent)
			}
			if !b.endTime.IsZero() {
				// The measured rendering: microseconds and an explicit offset.
				fmt.Fprintf(&sb, "end_time=%s\n", b.endTime.Format("2006-01-02 15:04:05.000000-07:00"))
			}
			sb.WriteString("timeline=1\nsize=23394586\n")
			mustWriteFile(t, filepath.Join(dir, barmanMetaDir, b.id+"-backup.info"), sb.String())
		}
		if !skip[b.id] {
			mustWriteFile(t, filepath.Join(dir, barmanBaseDir, b.id, barmanDataDir, "PG_VERSION"), "16\n")
		}
	}
	for _, segment := range spec.segments {
		mustWriteFile(t, filepath.Join(dir, barmanWALDir, segment[:walPrefixLen], segment), "wal\n")
	}
	return dir
}

// assertChainVerdict reads a chain check's answer: want empty means the
// catalogue must be accepted, otherwise the refusal must be
// source_not_found and must name what is absent.
func assertChainVerdict(t *testing.T, perr *protoError, want string) {
	t.Helper()
	if want == "" {
		if perr != nil {
			t.Fatalf("refused a catalogue that restores: %s: %s", perr.Code, perr.Message)
		}
		return
	}
	if perr == nil {
		t.Fatalf("accepted a catalogue that cannot restore; want %q", want)
	}
	if perr.Code != "source_not_found" {
		t.Errorf("code = %q, want source_not_found", perr.Code)
	}
	if !strings.Contains(perr.Message, want) {
		t.Errorf("message = %q, want it to mention %q", perr.Message, want)
	}
}

// TestBarmanChainRefusals covers what a catalogue can be asked host-side,
// including the cases that must NOT be refused.
func TestBarmanChainRefusals(t *testing.T) {
	cases := []struct {
		name   string
		spec   barmanSpec
		target time.Time
		want   string
	}{
		{
			name: "an intact catalogue restores",
			spec: barmanSpec{backups: twoBarmanBackups(), segments: barmanSegments()},
		},
		{
			name: "the chosen backup's last segment is missing",
			spec: barmanSpec{backups: twoBarmanBackups(), segments: withoutBarmanSegment("00000001000000000000000A")},
			want: "missing 00000001000000000000000A",
		},
		{
			name: "the chosen backup's first segment is missing",
			spec: barmanSpec{backups: twoBarmanBackups(), segments: withoutBarmanSegment("000000010000000000000009")},
			want: "first segment",
		},
		{
			// The same bound chain.go proved for pgBackRest: an ancestor's
			// own WAL is not what carries this backup to consistency.
			name: "an ancestor's own WAL may be expired",
			spec: barmanSpec{backups: twoBarmanBackups(), segments: withoutBarmanSegment("000000010000000000000005")},
		},
		{
			name: "the parent backup is gone from disk",
			spec: barmanSpec{backups: twoBarmanBackups(), segments: barmanSegments(), omitData: []string{barmanFull}},
			want: "the repository does not hold",
		},
		{
			name: "the catalogue no longer describes the parent",
			spec: barmanSpec{backups: twoBarmanBackups()[1:], segments: barmanSegments()},
			want: "no longer describes",
		},
		{
			name: "an archive with no segments at all is not judged",
			spec: barmanSpec{backups: twoBarmanBackups()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBarmanServer(t, tc.spec)
			backups := readBarmanBackups(dir)
			chosen, ok, perr := selectBarmanBackup(backups, tc.target)
			if perr != nil || !ok {
				t.Fatalf("selection failed before the chain could be judged: ok=%v err=%v", ok, perr)
			}
			assertChainVerdict(t, checkBarmanChain(dir, chosen, backups), tc.want)
		})
	}
}

// TestSelectBarmanBackup pins the selection rule, including the two
// refusals only it can make.
func TestSelectBarmanBackup(t *testing.T) {
	waiting := []barmanBackup{{
		id: barmanFull, status: "WAITING_FOR_WALS",
		endTime: time.Date(2026, 9, 23, 18, 34, 46, 0, time.UTC),
	}}
	cases := []struct {
		name    string
		backups []barmanBackup
		target  time.Time
		want    string
		found   bool
		refusal string
	}{
		{name: "no target takes the newest", backups: twoBarmanBackups(), want: barmanNext, found: true},
		{
			name: "a target between the two takes the older", backups: twoBarmanBackups(),
			target: time.Date(2026, 9, 23, 18, 45, 0, 0, time.UTC), want: barmanFull, found: true,
		},
		{
			name: "a target before everything is refused", backups: twoBarmanBackups(),
			target: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), refusal: "recovery only rolls forward",
		},
		{
			// Barman's own word for "the WAL that makes this consistent has
			// not arrived": a drill must not restore from it.
			name:    "a catalogue holding only WAITING_FOR_WALS is refused",
			backups: waiting, refusal: "no backup in status DONE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, perr := selectBarmanBackup(tc.backups, tc.target)
			if tc.refusal != "" {
				if perr == nil {
					t.Fatalf("selected %q, want a refusal mentioning %q", got.id, tc.refusal)
				}
				if !strings.Contains(perr.Message, tc.refusal) {
					t.Errorf("message = %q, want %q", perr.Message, tc.refusal)
				}
				return
			}
			if perr != nil {
				t.Fatalf("refused: %s", perr.Message)
			}
			if ok != tc.found || (ok && got.id != tc.want) {
				t.Errorf("selected %q (ok=%v), want %q (ok=%v)", got.id, ok, tc.want, tc.found)
			}
		})
	}
}

// TestParseBarmanInfo reads the manifest as Barman writes it, including
// the literal "None" it uses for an unset field.
func TestParseBarmanInfo(t *testing.T) {
	raw := `backup_id=20260923T183446
backup_label='START WAL LOCATION: 0/5000028 (file 000000010000000000000005)\n'
begin_time=2026-09-23 18:34:46.451076+00:00
begin_wal=000000010000000000000005
children_backup_ids=None
end_time=2026-09-23 18:34:46.592775+00:00
end_wal=000000010000000000000005
parent_backup_id=None
status=DONE
timeline=1
`
	got := parseBarmanInfo(barmanFull, raw)
	if got.parent != "" {
		t.Errorf("parent = %q, want empty — None is not a value", got.parent)
	}
	if got.status != barmanDone || got.beginWAL != "000000010000000000000005" {
		t.Errorf("parsed %+v, want status DONE and the begin_wal from the manifest", got)
	}
	want := time.Date(2026, 9, 23, 18, 34, 46, 592775000, time.UTC)
	if !got.endTime.Equal(want) {
		t.Errorf("end_time = %s, want %s", got.endTime, want)
	}
}

// TestBarmanCreatedAtIgnoresUnfinishedBackups keeps the recorded
// backup.created_at to a backup a drill could actually have restored.
func TestBarmanCreatedAtIgnoresUnfinishedBackups(t *testing.T) {
	backups := twoBarmanBackups()
	backups[1].status = "WAITING_FOR_WALS"
	dir := writeBarmanServer(t, barmanSpec{backups: backups, segments: barmanSegments()})

	got := barmanCreatedAt(dir)
	if got == nil {
		t.Fatal("dated nothing, but the full backup is DONE")
	}
	if !strings.HasPrefix(*got, "2026-09-23T18:34:46") {
		t.Errorf("created_at = %q, want the completed backup's end_time, not the waiting one's", *got)
	}
}

// TestBarmanRestoreScriptStaysInsideItsOwnEscapes keeps two things the
// measurement day settled from drifting.
func TestBarmanRestoreScriptStaysInsideItsOwnEscapes(t *testing.T) {
	script := barmanRestoreScript("/scratch/probavi-barman", barmanFull, "/var/lib/postgresql/data", false, time.Time{})

	// PostgreSQL rejects a restore_command containing a % escape it does
	// not know, so the WAL directory is derived with cut and the only
	// percent signs left are %f and %p.
	for _, forbidden := range []string{"%.", "%s", "%-"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("restore_command carries %q, which PostgreSQL reads as an unknown escape", forbidden)
		}
	}
	for _, want := range []string{"cut -c1-16", "archive_mode = off", "recovery.signal", "chmod 0700"} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q", want)
		}
	}
	if strings.Contains(script, "recovery_target_time") {
		t.Error("a drill that asked for no point in time must set no recovery target")
	}

	pitr := barmanRestoreScript("/scratch/probavi-barman", barmanFull, "/pgdata", true,
		time.Date(2026, 9, 23, 18, 45, 0, 0, time.UTC))
	if !strings.Contains(pitr, "recovery_target_time = '2026-09-23 18:45:00.000000+00'") {
		t.Errorf("pitr script does not carry the target in the form PostgreSQL parses:\n%s", pitr)
	}
}

// TestProbeDeclaresEveryPITRKind holds the provision gate and the probe's
// declaration to each other: a kind that accepts a point in time must say
// so, and one that says so must accept it.
func TestProbeDeclaresEveryPITRKind(t *testing.T) {
	declared := map[string]bool{}
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("the probe payload is no longer a map, which this gate reads")
	}
	payload, ok := probe["sources"].([]map[string]any)
	if !ok {
		t.Fatal("the probe payload no longer lists sources as this gate reads them")
	}
	for _, kind := range payload {
		name, named := kind["kind"].(string)
		caps, hasCaps := kind["capabilities"].(map[string]bool)
		if !named || !hasCaps {
			t.Fatalf("a source entry is shaped unexpectedly: %+v", kind)
		}
		if caps["pitr"] {
			declared[name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no source kind declares pitr — this gate would pass vacuously")
	}
	for kind := range declared {
		if !pitrKinds[kind] {
			t.Errorf("the probe declares pitr for %s, but provision refuses a target for it", kind)
		}
	}
	for kind := range pitrKinds {
		if !declared[kind] {
			t.Errorf("provision accepts a target for %s, but the probe does not declare pitr", kind)
		}
	}
}
