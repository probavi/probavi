//go:build !unix || aix || (solaris && !illumos)

// The evidence store has no single-writer lock on this platform, so this
// package does not build here — deliberately, and this file is where that
// is said.
//
// The lock is not a convenience. Two processes appending to one log
// interleave records and break the hash chain, which is the one thing an
// append-only evidence log exists to prevent, so a build that could not
// take the lock would be a build that cannot honestly write evidence. A
// no-op would be worse than no build: it would remove the guarantee
// without removing the claim.
//
// Probavi releases build linux and darwin. Every Unix-like platform Go
// supports that offers flock builds from source as well; solaris and aix
// do not offer it, and Windows has no equivalent this package uses.
//
// Verifying a log needs none of this and is not restricted to those
// platforms: the independent verifier in spec/evidence has no
// dependencies, takes no lock, and builds everywhere Go does — including
// Windows. An auditor handed a log and a public key uses that.
package evidence

// Referencing an undefined name is how this file states its constraint in
// the compiler's own voice: the build fails naming the reason rather than
// naming a syscall the reader did not write.
const _ = probaviNeedsAPlatformWithFlockToWriteEvidence
