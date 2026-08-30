# Probavi evidence push — sending an evidence log to a URL

**Protocol version: `probavi-evidence-push/1`** (versioned independently
of the Probavi binary, like the adapter protocol, the evidence schema, and the
notification payload). This document is normative for `probavi push`: the
request it makes, the guarantees it gives the operator, and what a receiver
may rely on.

Status: implemented 2026-08-24.

---

## 1. Purpose and principles

A drill writes its evidence log on the host that ran it, and that is where
it stays. Getting it anywhere else has been the operator's problem, and the
usual answers need something not every estate has: a filesystem both sides
can reach, or an object store with credentials on every host. Hosts behind
NAT, in a DMZ, at another site, or in an organisation that will not run a
shared filesystem have neither. What they do have is the ability to make
one outbound HTTPS request.

Evidence that cannot leave the machine that produced it is evidence nobody
outside that machine can check — the opposite of what the format is for.
Moving it is therefore something the core does, freely, for anyone, on the
same reasoning that keeps `probavi evidence verify` in the core.

1. **The bytes on the wire are the log file, unchanged.** Push defines no
   new format and no envelope. The receiver stores what the host holds and
   runs the same verification anyone else would, against the same public
   key — including, byte for byte, any damage the host's own copy has.
2. **No receiver is defined, endorsed, or required.** Any endpoint that
   accepts the request of §4 works. Probavi ships no receiver, names none,
   and has no default destination: a push goes exactly where the operator
   points it, and nowhere else (AGENTS.md §3.3 — no telemetry, ever).
3. **A copy, never a move.** The log stays on the host, append-only,
   forever. `probavi push` opens it read-only; no flag offers otherwise.
4. **Stateless and self-healing.** Every push sends the whole file. There
   is no cursor file, no "last pushed" marker, nothing to lose or corrupt:
   whatever an earlier push missed, the next one repairs. Logs are small —
   a record is 1–2 KB, so a daily drill for a year is under a megabyte.
5. **A push never changes what a drill proved.** It is a separate command
   with its own exit code, deliberately not wired into `probavi run` (§8).

### 1.1 What this is not

Not a daemon and not a scheduler — a systemd timer or a cron entry runs it
(§8). Not a synchronisation protocol: there is no negotiation, no delta, no
acknowledgement beyond the HTTP status. Not a new evidence format.

## 2. Invocation

```
probavi push --log <file> (--to <url> | --to-env <VAR>)
             [--path <path>] [--token-env <VAR> | --allow-unauthenticated]
             [--secret-env <VAR>]
```

| Flag | Required | Meaning |
|---|---|---|
| `--log` | yes | The evidence log to send. Opened read-only. |
| `--to` / `--to-env` | exactly one | The destination. `--to` is a literal absolute `http(s)` URL; `--to-env` names an environment variable holding one. **A token-bearing URL is a credential and belongs in `--to-env`** — a command-line value is readable by every user on the host through the process list. |
| `--path` | no | The path this log is sent under (§5). Default: the base name of `--log`. |
| `--token-env` | no | Names the environment variable holding the bearer token. Default: `PROBAVI_PUSH_TOKEN`. |
| `--allow-unauthenticated` | no | Send no `Authorization` header. Mutually exclusive with `--token-env` (§6.1). |
| `--secret-env` | no | Names the environment variable holding the HMAC secret for body signing (§6.2). Absent means unsigned. |

Environment variables are resolved before the log is read and before
anything is sent: an unset or empty variable is a usage error (exit 3) that
names the *variable*, never its value. This is the fail-fast rule the
notification config already follows for `url_env` and `secret_env`
(`notifications.md` §2).

**Redaction (binding, unchanged from `notifications.md` §2).** The
destination URL is a credential regardless of where it came from: it is
never written to logs, to stdout, or into an error message. Go's
`*url.Error` embeds the full URL, so transport errors are unwrapped before
anything is printed; the underlying dial or DNS error may still name the
target *host*, never the path or query where tokens live. The token and the
HMAC secret are never printed at all.

## 3. What is sent

The exact bytes of the file, from the first to the last, read once. Nothing
is parsed, filtered, reordered, re-signed, or normalised, and no record is
selected: sending one record or only what is new would make the receiver
hold state and care about order.

- The file is read in a single pass, and precisely those bytes are what
  gets counted for `Content-Length`, signed for §6.2, and transmitted. A
  drill appending to the log during a push therefore cannot make the length,
  the signature, and the body disagree.
- An **empty log** is sent as an empty body. It is a truthful state — the
  log exists and holds no records — and `probavi push` says so on stderr,
  exactly as `probavi evidence verify` reports an intact but empty log.
- A log whose last line is **incomplete** (a drill's append caught
  mid-write) is sent as it is, torn tail included. The receiver's copy must
  be able to reproduce the host's own verification result; hiding damage
  from it would break that. The next push carries the completed record.
- The sender refuses a log larger than **64 MiB** (exit 3). At 1–2 KB per
  record that is tens of millions of drills, far outside what this format
  is for, and the refusal is preferable to an out-of-memory kill. A
  receiver's own limit may be lower and is its own answer to give (§7).

## 4. The request

```
POST <to>/<path> HTTP/1.1
Content-Type: application/x-ndjson
Content-Length: <exact byte count of the body>
Authorization: Bearer <token>                (unless --allow-unauthenticated)
User-Agent: probavi/<version>
X-Probavi-Evidence-Push-Version: probavi-evidence-push/1
X-Probavi-Signature-256: sha256=<hex>        (only when --secret-env is set)

<the evidence log, verbatim>
```

- **Method.** `POST`, always. The body's idempotence is a property this
  document states, not one a method is asked to imply.
- **Body.** The log file's bytes (§3). `Content-Length` is always present;
  the body is never chunked and never compressed by the sender.
- **`X-Probavi-Evidence-Push-Version`** carries this document's version, so
  a receiver can tell `probavi-evidence-push/1` from a future version
  without inspecting the body. Header name and value say the same word on
  purpose — one thing gets one name — and receivers should tolerate unknown
  *versions*, not unknown headers within a version.

## 5. The destination path

The receiver decides what to call the log from the token it was given and
from the path the sender chooses. The path is appended to the destination
URL's path component:

```
--to https://collector.example/ingest  --path prod-orders.jsonl
  → POST https://collector.example/ingest/prod-orders.jsonl
```

- Exactly one `/` joins the two, whatever trailing slashes the destination
  carries. A query string on the destination is preserved unchanged.
- **The default is the base name of `--log`** (`/var/lib/probavi/prod.jsonl`
  → `prod.jsonl`), which is stable across runs by construction. A path must
  identify the same log every time: nothing in it may be derived from the
  clock, the sequence number, or the run. A receiver that is handed a new
  path per push cannot maintain a history — it accumulates copies.
- **Grammar.** One to eight segments separated by `/`; each segment is 1–64
  characters from `A–Z a–z 0–9 . _ -` and **may not begin with a dot**; no
  empty segment, no leading or trailing `/`; at most 128 characters in
  total. The set is deliberately narrow: it needs no percent-encoding,
  cannot escape the destination's path prefix, and cannot smuggle a query
  or fragment. Two of the limits are there for the receiver as much as for
  the sender: the leading-dot ban rules out `..` and `.` without a separate
  rule and keeps hidden names free, because a receiver may write an
  incoming body to a dot-prefixed temporary file before renaming it into
  place; and the segment count matters because 128 characters would
  otherwise allow sixty single-character segments, which a stricter
  receiver refuses outright.
- A default derived from the log's base name that does not satisfy the
  grammar (an accented or spaced filename) is a usage error naming
  `--path`, never a silent transliteration.

Hierarchy is allowed and is the way one host sends several logs, or several
hosts send to one receiver: `--path db01/orders.jsonl`. What a receiver
does with the shape — accept it, reject it, map it to a name — is its own
policy (§7); a receiver may, for instance, accept only file names matching
`*.jsonl`, and the default path (the log's base name) usually satisfies
that on its own. The sender enforces nothing beyond the grammar above: what
a destination accepts is the destination's answer to give, and hard-coding
one receiver's rules here would be endorsing it.

## 6. Authenticity

### 6.1 The bearer token

A push carries `Authorization: Bearer <token>`, read from the environment
variable named by `--token-env` (default `PROBAVI_PUSH_TOKEN`). The token
is what identifies the sender to the receiver; it never appears on the
command line, in logs, or in any diagnostic.

Sending without a token requires `--allow-unauthenticated`, spelled out in
full. The flag exists for receivers that genuinely do not authenticate — a
closed network, a test — but it must be typed deliberately: were the token
merely optional, a single mistyped variable name would be enough to send
evidence unauthenticated to a public address, and a silent outcome of that
shape is exactly what this product cannot afford. Giving both
`--allow-unauthenticated` and `--token-env` is a usage error.

### 6.2 Body signing (optional)

When `--secret-env` is set, the request carries

```
X-Probavi-Signature-256: sha256=<lowercase hex of HMAC-SHA256(secret, body)>
```

over the exact request-body bytes — the same header, the same scheme, and
the same GitHub-shaped convention as a notification (`notifications.md`
§6), so a receiver already verifying webhook payloads verifies pushes with
the code it has. Receivers compare **constant-time**. Signing proves origin
and integrity of the *transfer*; the evidence's own authenticity is the
ed25519 signature inside every record, which is what an auditor checks and
what no transport can substitute for.

## 7. Delivery

The rules are the notification rules of `notifications.md` §3, unchanged,
and the constants are pinned to those by a test so the two cannot drift
apart unnoticed:

- **3 attempts**, 10 s per attempt, 1 s then 2 s of backoff, under a total
  budget of 60 s. Cancellation (Ctrl-C, SIGTERM) is honoured between and
  during attempts.
- Retries happen on **transport errors and 5xx only**. Any **2xx** is
  success. Redirects are **never followed** — a redirect could hand a
  token-bearing URL or a signed body to an unintended host — and, like any
  other non-2xx, they end the attempt loop.
- **A refusal that names a reason is printed as it arrives.** A receiver may
  answer that it is out of licence, that the log is too large, or that the
  path is not one it accepts, and an operator reads that in a cron job's
  mail. At most 4 KiB of the response body is read; from it, characters
  that are not printable are removed, runs of whitespace collapse to single
  spaces, and the result is truncated to 500 characters before being
  written to stderr. Nothing else about the response influences Probavi:
  its body is never parsed, and no receiver-defined field has meaning here.

## 8. Exit codes and where to run it

| Code | Meaning |
|---|---|
| 0 | The log was accepted (a 2xx response). |
| 2 | Delivery failure: the attempts were exhausted, or the receiver refused. |
| 3 | Usage, configuration, or I/O error — nothing was sent. |

`probavi push` prints a one-line JSON summary on stdout
(`{"status":200,"bytes":12480,"path":"prod-orders.jsonl"}`), which, like
every machine output in this project, is never translated (`i18n.md` §1).
The destination is not in it: it may be a credential.

The recommended arrangement is a **timer of its own**, separate from the
drill:

```ini
# /etc/systemd/system/probavi-push.service
[Service]
Type=oneshot
User=probavi
# push.env holds PROBAVI_PUSH_URL and PROBAVI_PUSH_TOKEN, one KEY=value per
# line, mode 0600 and owned by the user above: both are credentials.
EnvironmentFile=/etc/probavi/push.env
ExecStart=/usr/bin/probavi push --log /var/lib/probavi/prod.jsonl --to-env PROBAVI_PUSH_URL

# /etc/systemd/system/probavi-push.timer
[Timer]
OnCalendar=hourly
Persistent=true

[Install]
WantedBy=timers.target
```

```
# /etc/cron.d/probavi-push — hourly, after the daily drill has had its say.
# The same push.env as above; `set -a` exports what it defines.
17 * * * * probavi set -a; . /etc/probavi/push.env; set +a; /usr/bin/probavi push --log /var/lib/probavi/prod.jsonl --to-env PROBAVI_PUSH_URL
```

A push next to a drill must never change the drill's exit code: `probavi
run` reports what it proved, and a receiver being unreachable is not a
recoverability failure. Because every push sends the whole file, an hour
(or a day) of failed pushes costs nothing but the delay — the next
successful one carries everything.

## 9. Security considerations

- The destination URL and the token are credentials; §2's redaction rules
  and `--to-env` exist for them. Prefer `https`; `http` is accepted for
  air-gapped or internal receivers and exposes both the body and any URL
  token on the wire.
- **The body is drill metadata, not database contents** — the same content
  the evidence schema already permits: drill names, config hashes, adapter
  and sandbox parameters, check names and verdicts, timings, the
  environment fingerprint. Sandbox parameters enter signed records, which is
  why connection details are kept out of them (`evidence-schema.md` §8);
  that rule is what makes a log safe to hand to a third party at all.
- **Pushing extends no trust to the receiver.** It cannot forge, alter, or
  reorder records undetectably: the hash chain and the ed25519 signatures
  are verified against the operator's public key, which the receiver never
  holds. A receiver that returns 200 has proven nothing about the evidence,
  and Probavi records nothing about the push — a delivery is not evidence.
- **A retained copy turns a receiver into a truncation anchor.** The
  chain proves what a log contains, not that its end is still there:
  deleting the newest records leaves a shorter log that verifies VALID
  (`evidence-schema.md` §1, §9). Because every push carries the whole file
  (§1.4), a receiver that keeps what it was sent — rather than overwriting
  its copy — can see a log grow shorter, or recognise a shorter log as the
  prefix of one it already holds. The head of the copy it kept is the
  anchor of `evidence-schema.md` §9.1, which `probavi evidence verify
  --anchor` checks directly. Probavi neither requires this of a receiver
  nor can check it, and a delivery still proves nothing (previous bullet);
  it is stated here because an operator who wants the property needs to
  know it is theirs to build.
- Receivers are untrusted. Their responses are read for the status code and
  for the bounded, sanitised reason of §7, and for nothing else.

## 10. Versioning

`probavi-evidence-push/1` covers the request of §4, the path rules of §5, the
authentication of §6, and the delivery semantics of §7. Additive,
receiver-visible changes (a new header, a new accepted status class) bump
the version; so does any change to what the body contains. Receivers should
tolerate unknown versions rather than guess.
