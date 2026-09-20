package main

// retention.go carries this adapter's answer to the data-lifecycle
// question every engine in issue #166 raises: what does the engine do to
// the artifact, unbidden, that can only subtract from what the backup
// holds?
//
// For this engine the answer is the TTL index, and the shape is the
// **suspend** rather than the fence — which is the better outcome, and
// not the one its two nearest relatives could manage. A TTL index names a
// field and an `expireAfter`, and a background thread deletes documents
// whose field plus that interval has passed. It is a background deletion,
// not a read-time filter, and the vendor gives a switch for it:
// `--ttl.frequency` is documented as "the frequency (in milliseconds) for
// the TTL background thread invocation (0 = turn the TTL background
// thread off entirely)", default 30000.
//
// Measured 2026-09-20 on 3.12.4-3, restoring a dump of 200 documents each
// already an hour past a 60-second TTL, into two servers that differed in
// exactly that flag:
//
//	          default   --ttl.frequency 0
//	t+0s        200            200
//	t+20s       200            200
//	t+40s         0            200
//	t+60s         0            200
//
// Without the flag the drill restores every document and then watches the
// engine delete all of them — a drill reporting on data the engine
// silently removed while it ran. With it, nothing is removed.
//
// **Suspend, never rewrite.** The flag stops the thread; it does not touch
// the index. Verified in the same run: the restored collection still
// carries its TTL index with `expireAfter=60` on `fields:["stamp"]`,
// exactly as the operator declared it, so a check that reads the index
// sees what the backup held. Nothing here edits the artifact's schema.
//
// And the flag is passed explicitly rather than relied on: a default is
// not a guarantee, which is the lesson MySQL taught this project when it
// flipped `event_scheduler` to ON in 8.0 after years of the opposite.

// engineArgs are the server flags the adapter starts the engine with.
//
// Every one of them is load-bearing. The endpoint and the directories
// replace what the image's entrypoint would have set, since the sandbox
// runs `sleep infinity` and the adapter owns the engine. Authentication
// is off because a Probavi sandbox is zero-ingress — no published ports
// are expressible — which is the same reason the sibling adapters accept
// a known-public credential. And `--ttl.frequency 0` is the suspension
// above.
func engineArgs() []string {
	return []string{
		"--server.authentication", "false",
		"--server.endpoint", endpoint,
		"--database.directory", dataDirPath,
		"--javascript.app-path", appsDirPath,
		// Issue #166: hold the engine's own expiry thread back for the
		// drill. See this file's header for the measurement.
		"--ttl.frequency", "0",
	}
}
