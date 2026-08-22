package main

import (
	"strconv"
	"strings"
)

// header.go reads what a dump file states about itself — through the
// engine, never by parsing the bytes: the dump format is Oracle's own,
// and the documented reader is DBMS_DATAPUMP.GET_DUMPFILE_INFO, which
// answers a file type and a table of numbered items (headerScript prints
// them as key=value lines). The item codes below are the package's
// documented KU$_DFHDR_* constants; the values beside them were measured
// on a dump the verified image exported.

// Item codes of GET_DUMPFILE_INFO, as the package documents them.
const (
	itemFileVersion       = "1"  // dump file format version, "6.1" measured
	itemGUID              = "3"  // export job GUID
	itemCreationDate      = "6"  // export wall clock, asctime form, no zone
	itemJobName           = "8"  // e.g. "SYSTEM"."SYS_EXPORT_SCHEMA_01"
	itemInstance          = "10" // host:SID the export ran on
	itemDBVersion         = "15" // the source's compatible setting, "23.06.00.00.00" measured
	itemDataEncrypted     = "20"
	itemMetadataEncrypted = "21"
	itemColumnsEncrypted  = "22"
)

// File types GET_DUMPFILE_INFO answers.
const (
	fileTypeUnknown  = 0
	fileTypeDataPump = 1
	fileTypeOriginal = 2 // an original Export (exp) file
)

// dumpHeader is what the engine read out of the file. known reports that
// the reader answered in its shape at all — the simulated sandbox answers
// every exec with "1", and every verdict below needs positive evidence.
type dumpHeader struct {
	known    bool
	fileType int
	items    map[string]string
}

func (h dumpHeader) item(code string) string { return h.items[code] }

// parseHeader reads the headerScript's key=value lines.
func parseHeader(stdout []byte) dumpHeader {
	h := dumpHeader{items: map[string]string{}}
	for _, line := range strings.Split(string(stdout), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch {
		case key == "filetype":
			n, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			h.known = true
			h.fileType = n
		case strings.HasPrefix(key, "item"):
			h.items[strings.TrimPrefix(key, "item")] = value
		}
	}
	return h
}

// encrypted names the first encryption the header claims, or "".
func (h dumpHeader) encrypted() string {
	for _, f := range []struct{ code, name string }{
		{itemDataEncrypted, "table data"},
		{itemMetadataEncrypted, "metadata"},
		{itemColumnsEncrypted, "encrypted columns"},
	} {
		if h.item(f.code) == "1" {
			return f.name
		}
	}
	return ""
}

// vetHeader turns the header's own claims into verdicts, each on positive
// evidence only: an unknown header (the reader did not answer in its
// shape) passes, and the engine's own refusal at import stays the
// authority.
func vetHeader(h dumpHeader, engineVersion string) *protoError {
	if !h.known {
		return nil
	}
	switch h.fileType {
	case fileTypeDataPump:
	case fileTypeOriginal:
		return protoErr("unsupported_source", false,
			"the file is an original Export (exp) dump, which Data Pump cannot import — this "+
				"adapter restores expdp dumps; re-export with expdp")
	default:
		return protoErr("source_corrupt", false,
			"the engine does not recognise the file as a Data Pump dump (file type %d)", h.fileType)
	}
	if what := h.encrypted(); what != "" {
		return protoErr("unsupported_source", false,
			"the dump's own header says its %s is encrypted: importing it needs the encryption "+
				"password, which would have to cross the protocol inside a payload — export with "+
				"ENCRYPTION=NONE for drills", what)
	}
	if versionNewer(h.item(itemDBVersion), engineVersion) {
		return protoErr("invalid_request", false,
			"the dump's own header says it was written at Oracle version %s, and the sandbox engine "+
				"is %s: Data Pump does not import a dump into an older release (the engine's own "+
				"refusal is ORA-39142) — use an image at least as new as the backup's origin",
			h.item(itemDBVersion), engineVersion)
	}
	return nil
}

// versionNewer reports that the dump's version is newer than the engine's
// when both parse as dotted numbers; an unparseable side compares as not
// newer. Segments are compared numerically, so 23.06 precedes 23.26 (the
// header zero-pads, the engine does not, measured).
func versionNewer(dump, engine string) bool {
	d, dok := parseVersion(dump)
	e, eok := parseVersion(engine)
	if !dok || !eok {
		return false
	}
	for i := 0; i < len(d) || i < len(e); i++ {
		var dv, ev int
		if i < len(d) {
			dv = d[i]
		}
		if i < len(e) {
			ev = e[i]
		}
		if dv != ev {
			return dv > ev
		}
	}
	return false
}

func parseVersion(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// engineIdentity is what the started instance states about itself, read
// in one call after the launch (identityScript).
type engineIdentity struct {
	version string   // version_full, "23.26.3.0.0" measured
	pdbs    []string // pluggable databases open read write, the seed excluded
	pins    map[string]string
}

// parseIdentity reads the identityScript's key=value lines.
func parseIdentity(stdout []byte) engineIdentity {
	id := engineIdentity{pins: map[string]string{}}
	for _, line := range strings.Split(string(stdout), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "version":
			id.version = value
		case "pdb":
			id.pdbs = append(id.pdbs, value)
		case "job_queue_processes", "aq_tm_processes":
			id.pins[key] = value
		}
	}
	return id
}
