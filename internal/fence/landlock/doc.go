// Package landlock is the fence tier's filesystem layer: it confines a
// command Abhed starts to an allowlist of paths with Linux Landlock, and can
// refuse it TCP when the network is off.
//
// The API is small:
//
//	abi, err := landlock.Detect()        // the kernel's Landlock ABI; 0 and an error when there is none
//	rs, err := landlock.New(spec, abi)   // validate spec against abi; fails closed
//	err = rs.Restrict()                  // confine the calling thread, then exec the command from it
//
// A Spec names four kinds of grant (Exec, Read, Write, Devices) and the paths
// of Abhed's own state that must stay out of reach (Deny). Landlock only
// grants, it cannot carve a path out of a grant above it, so a Spec whose
// denied path sits under any grant, or whose grant sits inside denied state,
// is refused rather than applied. Paths are compared both as written and with
// symbolic links resolved.
//
// New refuses, and so does Restrict, when the kernel's ABI is older than
// MinABI (Linux 6.2), when DenyTCP is asked of an ABI without network rules
// (before 4, Linux 6.7), and when running as root. Nothing falls back to a
// weaker domain: every step either succeeds or the command must not run.
//
// Restrict binds the calling OS thread only. It locks the goroutine to that
// thread for good, sets no_new_privs, builds the ruleset and enters it; the
// caller then execs the command from the same goroutine, and the command
// inherits the domain. Other threads of the calling process stay unconfined,
// which is why Abhed re-executes a small launcher to call it.
//
// ABI.Describe and Ruleset.Describe give the one-line accounts that abhed
// doctor and the record show.
//
// What it does not cover: UDP and other socket families (the fence's seccomp
// layer refuses those), a hard link to denied state made before the command
// started, the same file reached through another mount, and, below ABI 6
// (Linux 6.12), signals to processes outside the domain.
//
// This package only builds and applies the ruleset. Choosing the fence tier
// and launching commands under it happen elsewhere.
package landlock
