# Network policy

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

Under the allowlist the same rules also judge Abhed's own requests: the
model client, `web_fetch`, `web_search`, MCP servers over HTTP, and the
network of stdio MCP servers. See "Abhed's own requests" below. Outside the
allowlist none of this changes how they connect.

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
  wildcard needs at least two labels after `*.`, so `*.com` is refused. An
  allow rule with a wildcard logs a warning once: a command can put data in
  the labels of names under that domain (`<data>.example.com`), and that
  domain's name servers see them when the proxy resolves the name. Prefer
  exact names where the domain is not yours.
- `ports`: the ports the rule covers. Left out, it covers 80 and 443.
- `methods` and `paths`: narrow the rule to plain HTTP requests. A path is
  exact (`/health`), or ends in `/*` for itself and everything below it
  (`/v2/*` covers `/v2`, `/v2/` and `/v2/x`). See "Path rules" below before
  writing a deny rule on a path.
- `decision`: `allow` or `deny`.
- `allow_ips`: internal addresses or prefixes this rule may reach (see
  Addresses below).

Deny rules win over allow rules. A request no rule matches gets `default`:
`deny` unless set to `allow`. `mode: "audit"` lets a denied request through and
records it as `would_deny`, so a rule set can be tried before it is enforced.

Two more settings, also managed only:

- `record_paths`: `true` (the default) records each plain HTTP request's
  path; `false` records the host, port and method only. A path can hold a
  token or other secret (`/reset/<token>`, `/hooks/<key>`), and the record
  keeps it; set `false` where clients put secrets in paths.
- `idle_seconds`: a tunnel or forwarded request that carries no bytes either
  way for this long is closed. Left out, 300 (five minutes). It applies after
  the request head, which has its own 30-second limit, so a command cannot
  hold the proxy's connections open by sending nothing.

## Path rules

A path rule matches the path exactly as the request spells it, after
percent-decoding. The proxy refuses the spellings servers commonly rewrite
before routing: `.` and `..` segments, `//`, a backslash, an encoded `/`,
`\`, `.` or NUL, and any `;`. The `;` is refused with 400 because servers
that read path parameters (many Java servers among them) route `/admin;x`
as `/admin` and `/secret;x/a` as `/secret/a`, which would step around a
deny rule on `/admin` or `/secret/*`. Refusing it is simpler and safer than
guessing how each server reads parameters; few clients of a command's
allowlist need path parameters.

Other spellings are not refused, and a server may treat them as the same
path as the one a deny rule names: `/ADMIN` (a case-insensitive server),
`/admin/` (trailing slash), `/admin.` or `/admin.json` (suffix matching),
`/admin%20` (trailing space). A deny rule on a path is therefore a speed
bump, not a boundary. To keep commands away from part of a service, allow
the paths they need and let the default deny the rest: an allow-list fails
closed when the server spells a path in a way the rule did not foresee, and
a deny-list fails open.

## How a decision is made

Each session gets its own proxy, started with its first command, listening on
a random loopback port. A session's subagents share its
proxy. Under `abhed serve`, every session of the server has its own: the
agent's commands, the workbench terminal and `!` commands each run as one of
the session's calls, so their decisions go to that session's record and no
other. The proxy stops, and its socket folder is removed, when the session
leaves the server (it is deleted, or another node takes it), when the
server shuts down, and when the session has had no command in flight for 30
seconds (a workbench terminal left open counts as one); the next command
starts a new proxy. On the command line the proxies stop when Abhed exits.
A command is given `HTTP_PROXY`, `HTTPS_PROXY` and their lower-case forms,
pointing at it with the call id as the user name and a token of its own as
the password, and `NO_PROXY=localhost,127.0.0.1,::1`, so a server the
command starts inside its own sandbox is reached directly. A request without
a valid token is answered with 407.

### Per-call credentials

Each command gets a token issued for its call alone, and the proxy and the
resolver take the call id from the token, never from the user name or
anything else the client sends. A command that writes another call's id
into its proxy URL is still recorded under its own call. The token is
revoked when the command's call ends:

- A process a command left running after the command ended is refused from
  then on (407, or REFUSED for a lookup), and the refusal is recorded under
  the ended call with rule `auth`. Requests it made with the token while the
  call was live are stopped when the call ends: a connection still being
  dialled is cancelled, and both sides of an open one are closed. Bytes
  already sent upstream before then cannot be recalled. On Linux the
  sandbox's PID namespace usually ends such a process first.
- Run a server the agent needs with `run_in_background`, or move a command
  to the background with Ctrl-B: the call stays live while it runs, so its
  token does too, and it is revoked when the process exits.
- A workbench terminal and each `!` command are calls of their own, with
  their own tokens, live while they run.
- Any launch bound to a session gets a token the same way, an MCP server
  started as `mcp/<name>` included, for as long as it runs.

The proxy remembers the last 1,024 ended tokens, so their late traffic is
recorded under the right call; an older one, or one never issued, is
refused with no call id. A token is given only in its command's environment,
though a command can pass it on: one written to the workspace and read by a
concurrent call makes that call's traffic recorded as the first call's, until
the first call ends.
On Linux each command has its own PID namespace, so no command can see
another's environment. On macOS, `ps` is refused inside the sandbox and the
system does not return another process's environment, so the same holds.

- **HTTPS** goes through `CONNECT` and is decided by host and port only: the
  proxy does not see inside TLS. So a rule narrowed by `methods` or `paths`
  cannot allow a tunnel, and a deny rule narrowed by them refuses the tunnel to
  that host and port, since the proxy cannot check it.
- **Plain HTTP** is decided by host, port, method and path. The proxy strips
  its own credentials and the hop-by-hop headers before forwarding, refuses
  upgrades, and closes after each response.

A request target that is not one plain spelling is refused with 400 rather
than cleaned: a host with a trailing number (`2130706433`, `0x7f.1`), a
non-ASCII name (write the `xn--` form), an IPv4 address written as IPv6, or
a path with `.` or `..` segments, `//`, a backslash, a `;`, or an encoded
`/`, `\`, `.` or NUL. A rule on `/admin` cannot be stepped around with
`/public/../admin` or `/admin;x`; see "Path rules" for what a deny rule on a
path still cannot catch.

The proxy serves at most 256 connections at once per session; one more is
closed unread. Clients that keep connections open should be within it.

### Clients that do not send proxy credentials

The proxy asks for its credentials (the user and password in the proxy
variables) on every request, `CONNECT` included, and answers 407 without
them. Most clients send them: curl, wget, git, pip, npm, Go and Python
`requests`. Java's `HttpURLConnection`, which Maven and Gradle use, does
not send Basic credentials on a `CONNECT` by default, so HTTPS from a JVM
gets 407. The JVM's own flag turns that default off:

```
-Djdk.http.auth.tunneling.disabledSchemes=
```

(an empty value; set it with `JAVA_TOOL_OPTIONS`, `MAVEN_OPTS` or
`org.gradle.jvmargs`), together with `-Dhttps.proxyHost`, `-Dhttps.proxyPort`,
`-Dhttps.proxyUser` and `-Dhttps.proxyPassword` from the proxy URL, since the
JVM does not read `HTTPS_PROXY`.

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

## Names (DNS)

Commands under the allowlist never send a DNS query to the network.

On **Linux** each command's network namespace has the session's resolver
on `127.0.0.1:53`, served by the relay and answered by the session's proxy
over a unix socket, the same way as the proxy itself. A generated
`resolv.conf` naming only that resolver is bound over `/etc/resolv.conf`
inside the sandbox; the host's file is not touched. The resolver:

- answers a name only if an allow rule's host could match it, on any port
  (or any name, with `default: allow`). Ports, methods, paths and deny rules
  are left to the proxy, which sees the request;
- answers `A` with an address from `198.18.0.0/15` (benchmarking space,
  never routed), one per name, held for the session, with a 30-second TTL.
  An address stays mapped for five minutes after its name was last
  answered, so a client that caches past the TTL still connects. After
  that the address may be given to another name; a client still holding
  it then reaches that name, judged as that name. The pool holds 131,070
  addresses per session; when every one is mapped, a new name gets
  SERVFAIL, recorded, until a mapping expires; `AAAA` and
  other types of an allowed name get no data, so clients use the `A` answer;
- answers every other name with NXDOMAIN (REFUSED for a class other than
  IN) and records it as an `egress.decision` with `kind: "dns"`;
- answers `localhost` with loopback, since no `/etc/hosts` is bound;
- never forwards a query anywhere, so a lookup alone carries nothing out;
- takes each lookup with the call's own proxy credential, which the relay
  reads from the command's `HTTP_PROXY`, so it is recorded against the call.
  A command that reaches the resolver's socket itself without the
  credential is refused and recorded with rule `auth`. At most 64 lookups
  are served at once; one more is closed unread and recorded with rule `cap`.

The address the command gets is only a token for the name. A `CONNECT` or
plain request to it is judged on the name it stands for, and the proxy
resolves that name itself, checks every address, and dials the one it
checked, as for any request, so the rebinding protection below is the same.
The request to the origin carries the name in its `Host` header. An address
in `198.18.0.0/15` the resolver did not give out, or whose mapping has
expired, is refused with rule `dns`. Each session's proxy has its own
mapping, so a new proxy (after the session was quiet, say) starts afresh.

Plain UDP and TCP other than the resolver and the proxy still have no
route, so QUIC and DNS to any other server fail. To bind port 53 the relay
starts as root of the sandbox's user namespace with
`CAP_NET_BIND_SERVICE` (and `CAP_SETFCAP`, to map ids), makes itself
undumpable, runs the command in a user namespace of its own as the user
Abhed runs as, and drops every capability; the command holds none. When
Abhed runs as root there is no nesting: bwrap drops every capability but
`CAP_NET_BIND_SERVICE` and `CAP_SETPCAP`, and the relay empties all its
sets, the bounding set included, before the command starts, so a root
command gains none at exec. Before using the resolver Abhed checks once
that a command started this way runs as its user with no capability. Where
that fails (bwrap installed setuid, no private `/proc`, nested user
namespaces refused, or a build with cgo, where capabilities cannot be
dropped on every thread), Abhed logs a warning once and commands resolve
nothing, as before; the proxy still resolves the names it carries.

On **macOS** Seatbelt cannot redirect DNS, so there is no in-sandbox
resolver and no `dns` record. The profile already refuses both ways a
command could resolve: a UDP or TCP socket to any DNS server, and the
system resolver's socket (`/var/run/mDNSResponder`, which `getaddrinfo`
uses), so lookups fail inside the sandbox. Commands reach hosts by name
through the proxy variables, and the proxy resolves them. A client that
resolves a name before using the proxy fails there; one that hands the
proxy the name works. Lookups a command attempts are refused by the sandbox,
not recorded.

## The record

Every decision is written to the session's record as an `egress.decision`
event, once per connection (CONNECT) or request (plain HTTP), when it ends:

| Field | |
|---|---|
| `call_id` | the tool call whose command made it, from the proxy credentials Abhed set |
| `kind` | `connect`, `http`, `dns` for a name lookup (Linux), `auth` for a request without a valid token, or `request` for one refused before it was read; for Abhed's own requests `model`, `web_fetch`, `web_search` or `mcp` |
| `host`, `port`, `ip` | the target, and the address dialled; for `dns`, the name, port 0, and the synthetic address given |
| `synthetic` | the resolver's address the command connected to, when it stood for `host` |
| `method`, `path` | plain HTTP only; the path without its query, left out with `record_paths: false` |
| `decision` | `allow`, `deny` or `would_deny` |
| `rule`, `reason` | the rule that decided (`rules[2] host`, or `default`), or `parse` (a malformed or over-large request head), `cap` (over the connection bound), `path` (a `;` in the path), `auth`, `dns` (a lookup refused before any rule, or a synthetic address not given out); for Abhed's own requests also `model`, `policy`, or `web_fetch` (its own address check); and why. A `dns` lookup can also be refused with `auth` or `cap` |
| `bytes_in`, `bytes_out` | bytes received from and sent to the destination |
| `repeats` | on a summary, how many denials like it were counted rather than recorded |
| `session` | Abhed's own requests and stdio MCP servers: the session's id |
| `mcp_server` | a stdio MCP server's decisions: the server's name |

Bodies, header values, query strings and credentials are never recorded. A
path can hold a secret; see `record_paths`.

The call id comes from the call's own token (see Per-call credentials), and
each session has its own proxy, so a decision is recorded under the call
that made it and never in another session's record. A command run outside
any session has no record; its decisions are dropped and logged as a
warning.

Decisions are rate-limited, so a command looping on a request cannot flood
the record; `dns` lookups count with the rest. A refused name is recorded
each time it is asked (rate-limited); an allowed name is recorded when it is
first given an address, not on each lookup. In each one-minute interval the first 10 denials of a kind (same
`kind`, `decision`, `rule`, `host` and `port`) are recorded one by one, and at
most 100 of all kinds; the rest are counted, and when the interval ends each
kind with denials left out gets one summary event with `repeats` set to the
count and the first denial's reason. Allowed decisions have a budget of
their own: the first 200 in each interval are recorded one by one, whatever
their kind, and the rest are counted the same way, each kind's summary
carrying `repeats` and the bytes each way of what it counts. At most 512
kinds of denials are counted in an interval, and apart from them 512 of
allowed decisions, so allowed traffic never takes a denial's place; past
that, the rest share one summary per decision. Summaries still owed are written when the proxy stops.

## Abhed's own requests

Under the allowlist, Abhed's own HTTP clients connect through a guard in
its own process that applies the same `egress` rules and records each
request as an `egress.decision` event:

| Client | `kind` | Judged |
|---|---|---|
| the model provider | `model` | its configured endpoint (`base_url`, and watsonx's IAM URL) is allowed without a rule, by the rule `model`, so the agent keeps working; any other host the model client is sent to is judged by the rules |
| `web_fetch` | `web_fetch` | by the rules, then by `web_fetch`'s own checks as before (its `allowed_hosts`, and internal addresses, which it never reaches even when an egress rule names them in `allow_ips`) |
| `web_search` | `web_search` | by the rules |
| MCP over HTTP | `mcp` | by the rules |

The `model` rule follows whatever provider configuration is in force: it
skips the address check, so a `base_url` or IAM URL on loopback, a private
address or a metadata address such as `169.254.169.254` is reached without
a rule. The model cannot change it, but anyone who can set the provider can.

Each tool set has its own guard: the rules of the configuration it was
built from judge its tools' requests and its sessions' model requests,
and a set outside the allowlist is left alone, even with another set in the
same process under it (as with several SDK agents). A request that names
no set is judged by the one guard in force, and refused if several are.

Each request, not each connection, is decided, since Abhed builds the
request itself: the host, port, method and path, for HTTPS too. A rule
narrowed by `methods` or `paths` therefore can allow one of Abhed's HTTPS
requests, where for a command's tunnel it cannot. The guard resolves the
name once, refuses the same internal addresses the proxy refuses unless the
allowing rule names them in `allow_ips`, and connects only to an address it
checked; a pooled connection is checked again against each request's rule
before it is reused. It never uses `HTTP_PROXY` or `HTTPS_PROXY` from
Abhed's environment, so it sees where each connection goes. A denied
request is never sent: the tool gets an error naming the rule and the
reason, and the model client's caller sees the same error.

The record entry is the one described below, with `session` set to the
session's id, `call_id` to the tool call (empty for the model's own
requests), and `method` and `path` for HTTPS as well. `ip` is the address
connected to; `bytes_out` is the request body's declared length, and
`bytes_in` is not counted. The same per-interval limits apply, counted
apart from the commands' proxy: each session's own requests have their own
10-a-kind denials and 200 allowed one by one a minute, with summaries past
that, written when the interval ends, the session ends or Abhed exits.

The guard fails closed. If it cannot compile the `egress` section (the
sandbox refuses such a configuration first, so this is a second line),
every one of Abhed's own requests is refused by the rule `policy`, except
the model's endpoint. It never falls back to the open network.

Requests made outside any session, such as an MCP server's connection when
Abhed starts and the legacy SSE stream it keeps open, are judged the same
way but written to Abhed's log rather than a record.

### Stdio MCP servers

A stdio MCP server is started with its network confined to an egress proxy
of its own, apart from the sessions' proxies, with its own `mcp/<name>`
credential, revoked when the server stops:

| Tier | Stdio MCP server under the allowlist |
|---|---|
| process, Linux (bubblewrap) | **Direct sockets confined.** It runs in its own network namespace with loopback only, behind the same relay commands use, and in its own process namespace with a private `/proc`. `/run/user` (the session bus and user services), `XDG_RUNTIME_DIR`, `/run/dbus`, the podman, docker, containerd and snapd sockets under `/run`, the resolvers' `/run/systemd/resolve` and `/run/nscd`, `/run/cups`, `/run/avahi-daemon`, and the temporary folder (other sessions' egress sockets) are hidden; bus, agent and container variables are left out of its environment. Its filesystem is otherwise not confined. Names resolve through the proxy's resolver, as for commands, where the relay can serve it. When Abhed runs as root the server is refused, with that reason, and not started; so is it where bubblewrap cannot mount a private `/proc`. |
| process, macOS (Seatbelt) | **Direct sockets confined.** The profile denies all network use, AF_UNIX included, but outbound to the proxy's port on `localhost`, and denies the LaunchServices and Apple event services (`com.apple.coreservices.*`, `com.apple.CoreServices.*` and `com.apple.lsd.*`, the `coreservices` and `lsd` part matched in any case; the whole family is denied, which also cuts Handoff's clipboard and the shared file lists), which would open a URL or an app outside the sandbox. `trustd` is reachable: TLS verification needs it, and it fetches a certificate's AIA and OCSP URLs outside the proxy. Its filesystem is not confined. |
| fence, container, vm, none | Not reached: these tiers refuse the allowlist, so no session starts on them. |
| Windows, or a surface with no process-tier sandbox | **Not started.** It cannot be confined, so it is refused with that reason. |

This confines a server's direct network use, **not a hostile server**. A
server that means to get out still can: its files are not confined, so it
can plant a LaunchAgent, a systemd user unit or a shell rc line that runs
later, outside any sandbox; and on Linux it can connect to any AF_UNIX
socket in a folder that is not hidden, whose listener then acts for it.
System services also make requests on its behalf, outside the proxy and
unrecorded: on macOS `trustd` fetches the issuer (AIA) and revocation
(OCSP) URLs of a certificate it is asked to verify, so a crafted
certificate reaches any host and path; on Linux a resolver reachable over
a Unix socket in a folder that is not hidden would look names up for it
(`/run/systemd/resolve` and `/run/nscd` are hidden). Add only servers you
trust.

The server is given `HTTP_PROXY` and `HTTPS_PROXY` pointing at its proxy.
Its decisions are recorded in the record of the session whose tool call to
it is in flight, with that call's id, or with none in flight, of the
session that called it last, with `mcp_server` naming it. Decisions made
before any call, or while calls from two sessions are in flight at once,
are logged instead. See [MCP](08-mcp.md).

## What each tier enforces

The proxy is the policy point; what keeps a command from going around it is
the sandbox blocking direct sockets. A tier that cannot do that refuses the
allowlist, naming why, rather than opening the network in its place.

| Tier | Under the allowlist |
|---|---|
| process, Linux (bubblewrap) | **Enforced.** The command has its own network namespace with loopback only. A small relay inside it, Abhed's own binary, listens where the proxy variables point and passes each connection to the proxy's unix socket, which is bound into the sandbox, and serves the session's resolver (see Names). Nothing else leaves: a client that ignores the proxy variables has no route out. |
| process, macOS (Seatbelt) | **Enforced.** The profile denies all network use except outbound to `localhost` on the proxy's port. Seatbelt names no loopback address but `localhost`, which is both `127.0.0.1` and `::1`, so the proxy holds the port on both and no other process can take the `::1` half. A client that ignores the proxy variables is refused by the sandbox. So are the LaunchServices and Apple event services (`com.apple.coreservices.*`, `com.apple.CoreServices.*` and `com.apple.lsd.*`, the `coreservices` and `lsd` part matched in any case; the whole family is denied, which also cuts Handoff's clipboard and the shared file lists), on every network setting: `open` still runs but exits non-zero without handing the URL to a browser, and `osascript` cannot send Apple events to another app. Other system services are not: `trustd` fetches the issuer (AIA) and revocation (OCSP) URLs of a certificate a command asks it to verify, itself, outside the proxy and unrecorded, so a crafted certificate reaches any host and path. Commands cannot resolve names themselves (see Names); the proxy resolves them. Other loopback ports are refused, as with the network off, so `NO_PROXY` gives nothing here. |
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

- Abhed's other own connections: the start-up check that the model endpoint
  is up (a TCP connect to it), embeddings for retrieval and `rag` corpora,
  the Kubernetes and SSH tools, the forge client, extensions' processes,
  and the checks `abhed doctor` and onboarding make. These keep their own
  settings.
- The filesystem of a stdio MCP server: only its network is confined.
- Inspecting HTTPS: no TLS termination, so HTTPS rules are host and port only.
- UDP and raw TCP other than through `CONNECT`, QUIC among them.
- A resolver inside the macOS sandbox; see Names.
- Asking a person to approve a destination; a request is allowed or denied.

Under the allowlist the `bash` tool's description tells the model that the
network is limited to the allowed destinations, through the proxy, names up
to 20 of the hosts the allow rules name, and says a refusal is a 403 from the
proxy.
