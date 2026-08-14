package main

import "time"

// zone.go turns the backup's wall clock into an instant.
//
// backup.created_at in an evidence record is an absolute instant. What a
// ClickHouse backup manifest records is the server's local time with no
// offset (see backupmeta.go), and the offset is a fact only the operator
// has — so the drill config supplies it, by zone name rather than as a
// number: the offset depends on the date of the backup, and a January
// backup in Europe/Budapest is +01:00 while a July one is +02:00.
//
// Nothing is guessed. Without the declaration the adapter reports no
// creation time and the record's created_at is null — which the evidence
// schema provides for precisely because a backup's own creation time is
// not always derivable. Reporting the wall clock as if it were UTC would
// be worse than reporting nothing: it would be a specific, signed, wrong
// instant rather than an honest gap.

// backupTimezoneParam names the IANA zone the backup host was in.
const backupTimezoneParam = "backup_timezone"

// createdAtLayout keeps the offset in the value; the core normalizes it to
// UTC when it writes the record (adapter protocol §6.2).
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// backupLocation resolves the declared zone, or nil when none is declared.
// An unknown name fails the drill immediately rather than silently
// dropping the timestamp it was supposed to make exact.
func backupLocation(params map[string]string) (*time.Location, *protoError) {
	name := params[backupTimezoneParam]
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, protoErr("invalid_request", false,
			"source.params.%s must be an IANA time zone name such as Europe/Budapest or UTC: %s is not one",
			backupTimezoneParam, name)
	}
	return loc, nil
}

// createdAt renders the archive's wall clock in the declared zone. A nil
// location, or a backup whose manifest carries no timestamp, yields nil.
func createdAt(wallClock time.Time, loc *time.Location) *string {
	if loc == nil || wallClock.IsZero() {
		return nil
	}
	s := time.Date(wallClock.Year(), wallClock.Month(), wallClock.Day(),
		wallClock.Hour(), wallClock.Minute(), wallClock.Second(), wallClock.Nanosecond(),
		loc).Format(createdAtLayout)
	return &s
}
