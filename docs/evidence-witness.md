# Witnessing the evidence log

Status: **Normative about practice, specified 2026-09-27; the
`--witness` flag it documents is not implemented yet**
(`evidence-schema.md` §9.2.5). The anchoring in §2 works today and needs
nothing new.

This document is the operator's half of `evidence-schema.md` §9.2: which
witness to use, how to obtain an attestation, where to keep it, and what
it is worth. The verifier's side — what `--witness` runs and what the
exit statuses mean — is normative there, and an independent verifier is
written from that section, not from this one.

## 1. What this is for

The hash chain and the signature defend the log against everyone who does
not hold the signing key. They do not defend it against whoever does.

That party can rebuild the whole log with any content they like and
re-sign it from record 1. The result passes every check in
`evidence-schema.md` §9: the sequence starts at 1, every `prev_hash`
follows, every signature verifies. Nothing in the format dates it, so
nothing inside the file can tell the rebuilt log from the original. The
threat model names an attacker who wants to forge *everything was fine*
(AGENTS.md §3.3), and this is the half of that attacker the local key
cannot reach.

An **anchor** — the chain head, kept outside the file — closes it for
whoever kept the anchor (§9.1). A **witness** closes it for everyone
else: a party other than the operator attests the head at a moment, and
the attestation is a thing a reader can be shown. A log rewritten
afterwards has a different head at that `seq`, and the comparison that
finds it is the anchored check that already exists.

**What makes a witness stronger is the second party, not a second
signature.** A head signed with the log's own key adds nothing: the
attacker holds that key. Everything below is about custody.

## 2. Anchoring, which works today

Every verification prints the head, so keeping one is a shell line. This
is the practice `--witness` later makes routine; it is not waiting on
anything.

```console
$ probavi evidence verify --log /var/lib/probavi/evidence.jsonl \
    --key /etc/probavi/ed25519.key.pub | jq -r '.head | "\(.seq):\(.hash)"'
3:sha256:6b3e356a9444cf3d7ca6bfdf7ee6bdf35d88928b2d66fcc28a5ceb033308b62d
```

That value goes wherever the drill host cannot rewrite it, and the next
verification is handed it back:

```console
$ probavi evidence verify --log /var/lib/probavi/evidence.jsonl \
    --key /etc/probavi/ed25519.key.pub \
    --anchor 3:sha256:6b3e356a9444cf3d7ca6bfdf7ee6bdf35d88928b2d66fcc28a5ceb033308b62d
```

Where "wherever the drill host cannot rewrite it" is: a ticket, a mail, a
commit in a repository the host has no credentials for, or a `probavi
push` receiver that retains what it was sent. **Not** beside the log —
that host is the one whose write access the attack assumes.

The core keeps none of this. It prints the value and stops: a store
inside the core would sit on the host the attacker writes to, and a store
outside it would be a daemon or a phone-home, which the non-goals close.

## 3. Choosing a witness

Two families answer the question, and **they are not
interchangeable.**

### 3.1 A timestamp authority (RFC 3161)

A TSA receives a hash and returns a signed token saying it saw that hash
at a time. It learns the hash and nothing else: not that the hash is a
Probavi head, not that your organisation runs drills, not how often.

Choose this when the fact that you test restores is itself not for
publication — which, for a self-hosted product bought by people who do
not want their recovery posture public, is the ordinary case. Many
organisations already run one for code signing or document retention, and
a head is one more hash to it.

The receipt is a file. Keep it beside the anchor, not beside the log.

### 3.2 A public transparency log

A public append-only log (a Certificate Transparency-style log, a
notary service, a blockchain) publishes what it is given and lets anyone
verify inclusion. It is strictly stronger where it applies: no single
party to trust, and the record is checkable by a reader who trusts
neither you nor your TSA.

**And it publishes more than the head.** Submitting heads on a schedule
publishes that this organisation runs drills, how often, and — from the
cadence of `seq` — how many. That is a statement about your recovery
posture, made to everyone, permanently. For some users that is the point.
For others it is a disclosure they never agreed to, and a default that
made it for them would be this project publishing something about its
users.

So: neither is the default, and this document does not pick one. What it
refuses to do is present them as two spellings of the same thing.

### 3.3 What is not a witness

A second copy you control. A signature made with the same key. A backup
of the log. All three move with the attacker's write access, and none of
them involves a second party.

## 4. The witness command

`--witness <command>` is how a verifier asks (`evidence-schema.md` §9.2).
The verifier runs `<command> <head>` — the head as one argument, no shell
— and reads the exit status: `0` attested, `1` not attested, anything
else "could not answer", which is a failure to run rather than a verdict
about the log.

The command is the operator's, and so is the trust in it. A verifier
reports which command answered precisely so a reader can see whose
judgement they are reading.

### 4.1 A worked example: an RFC 3161 receipt

The witness has one job: decide whether it holds an attestation for the
head it was handed. With receipts kept one per head, that is a lookup and
a verification.

```sh
#!/bin/sh
# probavi-witness-tsa — answer whether a receipt attests this head.
#
# argv[1] is <seq>:sha256:<hex>, exactly as `probavi evidence verify`
# prints it. Receipts are kept as <dir>/<seq>.tsr.
#
# Exit 0 attested, 1 not attested, 3 cannot answer. The third is not a
# detail: a misconfigured or unreachable witness must not read as "this
# log was never attested" (evidence-schema.md §9.2.2).
set -u

fail() { echo "$*" >&2; exit 3; }

head=${1:-}
[ -n "$head" ] || fail "no head given"
[ -n "${PROBAVI_WITNESS_DIR:-}" ] || fail "PROBAVI_WITNESS_DIR is not set"
[ -n "${PROBAVI_WITNESS_CA:-}" ] || fail "PROBAVI_WITNESS_CA is not set"

seq=${head%%:*}
hash=${head#*:sha256:}
receipt=$PROBAVI_WITNESS_DIR/$seq.tsr

[ -f "$receipt" ] || exit 1          # no attestation for this seq

# The token attests a digest, and the head carries it as hex — so it is
# checked directly, with no temporary file to get wrong and nothing
# stored alongside the receipt for an attacker to rewrite.
openssl ts -verify -digest "$hash" -in "$receipt" \
    -CAfile "$PROBAVI_WITNESS_CA" >/dev/null 2>&1 || exit 1

# Attested. Print the instant the TSA recorded, which the verifier
# carries into its result as attested_at. A receipt that verifies but
# whose time cannot be read is still an attestation, so this does not
# fail the script.
openssl ts -reply -in "$receipt" -text 2>/dev/null \
    | sed -n 's/^Time stamp: *//p' | head -1
exit 0
```

Obtaining a receipt is the other half, and it belongs in the same cron
entry as the drill:

```sh
head=$(probavi evidence verify --log "$LOG" --key "$PUB" \
         | jq -r '.head | "\(.seq):\(.hash)"')
seq=${head%%:*}
openssl ts -query -digest "${head#*:sha256:}" -sha256 -cert -out /tmp/req.tsq
curl -sS -H 'Content-Type: application/timestamp-query' \
     --data-binary @/tmp/req.tsq "$TSA_URL" > "$WITNESS_DIR/$seq.tsr"
```

Nothing here is inside Probavi, and that is deliberate: pushing a head to
a remote from inside the core would add a dependency and an egress path
for what one cron line already does.

### 4.2 The auditor's run

```console
$ PROBAVI_WITNESS_DIR=/srv/receipts PROBAVI_WITNESS_CA=/etc/ssl/tsa-ca.pem \
    probavi evidence verify --log evidence.jsonl --key signer.pub \
      --witness /usr/local/bin/probavi-witness-tsa
```

One run, two findings: the log is internally intact, and an outside party
attested its content. Without the flag those are two jobs and a hex
comparison by hand, which is the difference between a check that is
available and one that is used.

## 5. What it is worth, exactly

It proves a party other than the operator attested **this head**, so the
log held this content when the attestation was made. A later rewrite
produces a different head and is detectable by anyone holding the
receipt — including a reader who trusts nothing you say.

It does not prove the log is complete now: records appended after the
attestation are outside it, which is the ordinary case rather than a
finding. It does not prove the drills happened or that their verdicts are
true. And the verifier does not check the receipt — the command does.

The honest sentence, which is the one to put in front of an auditor:
*this log has not been rewritten since the moment a third party attested
it, and here is the receipt.*

## 6. Related

- `evidence-schema.md` §9 — the verification algorithm.
- `evidence-schema.md` §9.1 — anchored verification, and where an anchor
  lives.
- `evidence-schema.md` §9.2 — the normative `--witness` contract.
- `evidence-schema.md` §12 — the standing commitment that verification is
  free and independent.
