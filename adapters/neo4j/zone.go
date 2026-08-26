package main

// zone.go answers the question the other adapters answer with a parser:
// when was this backup taken?
//
// For Neo4j the answer is that it cannot be known from the backup. The
// engine's own reader for the artifact — `neo4j-admin database load
// --info` — reports the database name, the archive format, and the file
// and byte counts, and no timestamp at all (measured against Neo4j
// 5.26). The file's modification time is not a substitute: copying a
// backup without preserving timestamps resets it, and a month-old
// artifact then looks like last night's, so this adapter reports no
// creation time rather than one that dates a copy.
//
// The drill config key the other adapters use to place a backup's wall
// clock in a zone therefore has nothing to act on here, and a config that
// sets it is refused rather than silently ignored — an operator who wrote
// it is expecting an accuracy this kind cannot deliver.

// backupTimezoneParam names the IANA zone the backup host was in. The
// other adapters read it; this one only refuses it.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this adapter cannot honour.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: a Neo4j dump records no backup timestamp "+
			"(the engine's own reader reports the database, the archive format and the file and byte counts), "+
			"so backup.created_at stays empty",
		backupTimezoneParam)
}
