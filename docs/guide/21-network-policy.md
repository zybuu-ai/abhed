# Network policy for commands

The agent's shell commands have three network settings:

| Setting | What commands reach |
|---|---|
| `sandbox.allow_network: false` (the default) | nothing |
| `sandbox.network: "allowlist"` | only the destinations the `egress` rules allow, through a proxy that records every decision |
| `sandbox.allow_network: true` | any host, directly |

The allowlist is set by the administrator. `sandbox.network` and the whole
`egress` section are **managed only**: they are read from the managed
configuration (`/etc/abhed/config.json`) and set aside, with a warning, in
`~/.abhed/config.json`, `-settings` or a workspace's file. With the allowlist
on, `allow_network` is not read.

This is the first piece of the network policy layer. It covers the agent's
shell commands on the process tier. Abhed's own requests (the model,
`web_fetch`, `web_search`, MCP over HTTP) do not go through it yet; they keep
their own settings.

## Configuration

```json
{
  "sandbox": { "network": "allowlist" },
  "egress": {
    "default": "deny",
    "mode": "enforce",
    "rules": [
      { "host": "proxy.golang.org", "decision": "allow" },
      { "host": "*.githubusercontent.com", "ports": [443], "decision": "allow" },
      { "host": "registry.internal.example", "ports": [8080], "decision": "allow",
        "methods": ["GET", "HEAD"], "paths": ["/v2/*"], "allow_ips": ["10.20.0.0/16"] },
      { "host": "uploads.example.com", "decision": "deny" }
    ]
  }
}
```

Each rule has:

- `host`: an exact name or address, or `*.example.com` for any name under
  `example.com`. The wildcard matches on a label boundary: `a.example.com` and
  `a.b.example.com`, never `example.com` itself or `example.com.evil.net`. A
  wildcard needs at least two labels after `*.`, so `*.com` is refused.
- `ports`: the ports the rule covers. Left out, it covers 80 and 443.
- `methods` and `paths`: narrow the rule to plain HTTP requests. A path is
  exact (`/health`), or ends in `/*` for itself and everything below it
  (`/v2/*` covers `/v2`, `/v2/` and `/v2/x`).
- `decision`: `allow` or `deny`.
- `allow_ips`: internal addresses or prefixes this rule may reach (see
  Addresses below).

Deny rules win over allow rules. A request no rule matches gets `default`:
`deny` unless set to `allow`. `mode: "audit"` lets a denied request through and
records it as `would_deny`, so a rule set can be tried before it is enforced.

## How a decision is made

Each session gets its own proxy, started with its first command, listening on
a random loopback port and stopped when the session ends. A command is given
`HTTP_PROXY`, `HTTPS_PROXY` and their lower-case forms, pointing at it with the
call id as the user name and a per-session token as the password, and
`NO_PROXY=localhost,127.0.0.1,::1`, so a server the command starts inside its
own sandbox is reached directly. A request without the token is answered with
407.

- **HTTPS** goes through `CONNECT` and is decided by host and port only: the
  proxy does not see inside TLS. So a rule narrowed by `methods` or `paths`
  cannot allow a tunnel, and a deny rule narrowed by them refuses the tunnel to
  that host and port, since the proxy cannot check it.
- **Plain HTTP** is decided by host, port, method and path. The proxy strips
  its own credentials and the hop-by-hop headers before forwarding, refuses
  upgrades, and closes after each response.

A request target that is not one plain spelling is refused with 400 rather
than cleaned: a host with a trailing number (`2130706433`, `0x7f.1`), a
non-ASCII name (write the `xn--` form), an IPv4 address written as IPv6, a
path with `.` or `..` segments, `//`, a backslash, or an encoded `/`, `\`, `.`
or NUL. A rule on `/admin` cannot be stepped around with `/public/../admin`.

A denied request gets 403 with the rule and the reason in the body, which the
agent sees in the command's output.

## Addresses

The proxy resolves each name itself, checks every address, and dials the
address it checked, never the name again, so a name that resolves to
something else a moment later (DNS rebinding) is not reached. It refuses
loopback, private, link-local (cloud metadata included), multicast,
unspecified and other reserved or internal ranges, the same classes
`web_fetch` refuses, unless a rule that allows the request names the address
in `allow_ips`. A name with any refused address among its answers is refused
whole. The proxy's own port is never reachable through it.

Audit mode does not lift the address check: a request that resolves to an
internal address no allowing rule names is refused even in audit mode.

## The record

Every decision is written to the session's record as an `egress.decision`
event, once per connection (CONNECT) or request (plain HTTP), when it ends:

| Field | |
|---|---|
| `call_id` | the tool call whose command made it, from the proxy credentials Abhed set |
| `kind` | `connect`, `http`, or `auth` for a request without the token |
| `host`, `port`, `ip` | the target, and the address dialled |
| `method`, `path` | plain HTTP only; the path without its query |
| `decision` | `allow`, `deny` or `would_deny` |
| `rule`, `reason` | the rule that decided (`rules[2] host`, or `default`) and why |
| `bytes_in`, `bytes_out` | bytes received from and sent to the destination |

Bodies, header values, query strings and credentials are never recorded. The
call id is attribution, not authentication: a command can change its own
environment, so it could present another call's id.

## What each tier enforces

The proxy is the policy point; what keeps a command from going around it is
the sandbox blocking direct sockets. A tier that cannot do that refuses the
allowlist, naming why, rather than opening the network in its place.

| Tier | Under the allowlist |
|---|---|
| process, Linux (bubblewrap) | **Enforced.** The command has its own network namespace with loopback only. A small relay inside it, Abhed's own binary, listens where the proxy variables point and passes each connection to the proxy's unix socket, which is bound into the sandbox. Nothing else leaves: a client that ignores the proxy variables has no route out. |
| process, macOS (Seatbelt) | **Enforced.** The profile denies all network use except outbound to `localhost` on the proxy's port, which the proxy holds. A client that ignores the proxy variables is refused by the sandbox. Commands need no DNS of their own, since the proxy resolves names. Other loopback ports are refused, as with the network off, so `NO_PROXY` gives nothing here. |
| fence (Linux preview) | **Refused.** Landlock limits TCP connects by port, not by address, and seccomp cannot read the address a socket connects to, so allowing the proxy's port would allow that port on any host. Leave `sandbox.tier` unset to use the process tier. |
| container, vm | **Refused.** The proxy listens on the host's loopback, which the container's network cannot reach without opening more than the proxy. `Select` passes over them, so the process tier is chosen when `min_tier` allows it; with `min_tier` container or vm, the session does not start. |
| none | **Refused.** Nothing stops a command on the host from ignoring the proxy variables. |

Without a sandbox that blocks direct sockets, proxy variables are only
cooperative: a well-behaved client uses them and anything else goes around
them. That is why only the process tier accepts the allowlist.

`abhed doctor` shows the setting on its `egress` line (off, open, or the
allowlist with its rule count, default and mode), and the `sandbox` line
names the tier and says whether it holds it.

## Not covered yet

- Abhed's own requests (model calls, `web_fetch`, `web_search`, MCP over HTTP).
- Inspecting HTTPS: no TLS termination, so HTTPS rules are host and port only.
- UDP, raw TCP other than through `CONNECT`, and DNS policy; the proxy
  resolves names for the requests it carries.
- Asking a person to approve a destination; a request is allowed or denied.
- The `bash` tool's description still tells the model whether
  `allow_network` is on; under the allowlist it says the network is off.
