package main

import (
	"time"
)

// zone.go turns a backup's wall clock into an instant.
//
// backup.created_at in an evidence record is an absolute instant, and no
// backup format this adapter reads records one: they store the backup
// host's local time with no offset (measured). The offset is a fact only
// the operator has, so the drill config supplies it — by name rather than
// as a number, because the offset depends on the date of the backup: a
// January backup in Europe/Budapest is +01:00 and a July one +02:00, and
// a fixed number in a config file would be wrong for half of every year.
//
// Nothing is guessed. Without the declaration the adapter reports no
// creation time, and the record's created_at is null — which the evidence
// schema provides for precisely because a backup's own creation time is
// not always derivable.

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

// formatCreatedAt renders an instant for source_identity.created_at.
func formatCreatedAt(t time.Time) *string {
	s := t.Format(createdAtLayout)
	return &s
}
