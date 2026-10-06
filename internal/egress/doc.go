// Package egress is the outbound proxy for sandboxed commands: one per
// session, on loopback, that lets a command reach only the destinations the
// administrator's rules allow and reports every decision.
//
// HTTPS goes through CONNECT and is decided by host and port; plain HTTP is
// decided by host, port, method and path. The proxy resolves each name
// itself, refuses loopback, private, link-local, multicast and metadata
// addresses unless a rule names them in allow_ips, and dials the address it
// checked, so a name that resolves differently later is never reached.
//
// The proxy is a policy point, not the boundary: what keeps a command from
// going around it is the sandbox tier blocking direct sockets. The process
// tier does that (a network namespace on Linux, Seatbelt on macOS); a tier
// that cannot refuses the allowlist setting.
package egress
