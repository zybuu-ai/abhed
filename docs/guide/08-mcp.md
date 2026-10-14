# MCP

Abhed is an MCP client and gateway. A tool exposed by a Model Context Protocol
server appears in the model's tool list like any built-in one.

```json
"mcp": {
  "servers": [
    { "name": "github", "enabled": true,
      "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": ["GITHUB_TOKEN"] },
    { "name": "corpus", "enabled": true, "url": "https://retrieval.internal/mcp",
      "headers_env": { "Authorization": "CORPUS_TOKEN" } }
  ]
}
```

Both transports are supported: **stdio** for a local process, **HTTP** for a
remote service. A server runs only with `"enabled": true`. `headers_env`
names an environment variable to read a header from, so the secret stays out
of the file.

## Where a server runs

A stdio server is a process Abhed starts as you, outside the commands' own
sandbox but confined by the same tier. On the process (macOS Seatbelt, Linux
bubblewrap), fence, container and VM tiers its **files are confined**,
whatever the network setting: your
home folder is hidden from it, but for a home of its own,
`~/.abhed/mcp/<server>/home`, which is its `HOME`, which it may write and
which keeps its caches (`npx`, `uv`) between runs, and the folder it was
installed in when that is under your home (a node version's folder, a
toolchain's, never all of `~/.local` or `~/.cargo`), read-only. The workspace
stays readable and writable, with its `.abhed` hidden. So a server cannot read
`~/.ssh` or plant a LaunchAgent, a systemd user unit or a shell rc line.
Files outside your home that your user can write stay writable. A server that
needs your home (a credential file it reads, a tool installed elsewhere in
it) is set `"host_files": true`, which runs it with your files as before;
under the allowlist its network is still confined. Under bubblewrap the
server is refused where a private `/proc` cannot be mounted (set
`host_files`), and when Abhed runs as root, since bubblewrap keeps root's
capabilities over the host: run Abhed as an ordinary user, or set
`host_files` to run the server unconfined (refused under the allowlist).

On the fence tier the server runs under a fence of its own: Landlock grants
it the system folders, `read_only_paths` outside your home, the workspace,
its own home and its install folder, and nothing else; its launch is
recorded as a command's is, with `mcp/<server>` as its call id, and its
temporary folder is `~/.abhed/mcp/<server>/home/tmp`. On the container and
VM tiers it runs in a container of its own from the sandbox image (gVisor
on the VM tier), its stdio carried by the engine: the commands' mounts
(the workspace, its `.abhed` covered, `read_only_paths` outside your home),
its own home mounted as its `HOME` and its install folder read-only, nothing
else of your home, and its environment in a file only your user can read.
The container is removed when the server closes or the Abhed process that
started it exits, Ctrl-C included. For the terminal CLI and `abhed -p` that
is when the session ends; under `abhed serve` the server and its container
belong to the server process, shared by its sessions, and are removed when
`abhed serve` exits, not when a session ends. Each is labelled
`abhed.mcp.owner` with the Abhed process that started it (host, process id,
start time); one left by an Abhed that was killed outright (`kill -9`),
under the allowlist too, is removed the next time a server starts on these tiers, and by `abhed doctor`
when a stdio server is set to run in one (enabled, not `host_files`), which
say so. A container without that label is never touched, nor one whose
process cannot be checked from here (another user's, under `hidepid`): it is
kept, and `abhed doctor` names it.
So its program must run in that image: a `docker`-style server, or one
installed for macOS, does not. Before the server starts, Abhed looks for its
program in the image (with the server's mounts) and refuses a server whose
program is not there, or is a macOS binary, naming the program; the session's
warning, `/mcp` and `abhed doctor` give that reason. Set `ABHED_SANDBOX_IMAGE`
to an image with its runtime, or `host_files` to run it on the host. A tier that cannot
confine a server refuses it with that reason, and `host_files` is the way
out.

The terminal CLI, `abhed -p`, `abhed doctor`, `abhed rpc` and `abhed serve`
all start a server this way. On the none tier and on Windows a stdio
server's files are not confined: each such server is named at start-up and by
`abhed doctor`, with why the process tier is not available, until it is, or
until the server is set `"host_files": true`. Under `abhed serve` the
workbench's Extensions panel marks it *unconfined* with the same reason, as
`GET /v1/capabilities` does in the server's `unconfined` field. Under bubblewrap the temporary
folder is hidden from a confined server, so one whose program is installed
there is refused with that reason. Its network is open, whatever
`allow_network` says, except under `sandbox.network: "allowlist"`; see
below. A URL server runs wherever it is hosted.

`digest` pins a stdio server's program, as `"sha256:<64 hex digits>"`: just
before the server starts, the file its `command` resolves to (on the
server's own `PATH`, links followed) is hashed, a mismatch refuses the
server, and the resolved path is what starts. When the command is `docker`,
`podman` or `nerdctl` running an image (`run` or `container run`) and that
image, the first word after `run`'s options and their values, names an OCI
image by that digest (`ghcr.io/org/server@sha256:…`), the container engine
pulls by it and the program is not hashed. The digest anywhere else, as an
option's value or the image's own argument, pins nothing; an option the
check does not know is taken to have a value, which at worst leaves the
engine itself hashed and the server refused. Another program given such an
argument is hashed as usual. A digest pins the one program
file: an interpreter's script or packages it loads (`npx`, `python
server.py`) are not covered, so pin the image or a single binary. A URL
server with a `digest` is refused, since its program cannot be checked. A
malformed digest refuses the server too.
Add a server as you would install any program: only one you trust.

A stdio server does not inherit Abhed's environment, which holds model
provider keys and Abhed's own settings. It gets `PATH`, `HOME`, `USER`,
`LOGNAME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR` and `TZ` from it (on
Windows, also what a program needs to start, such as `SYSTEMROOT` and
`TEMP`), then the entries in its `env`, which win: `"KEY=VALUE"` sets a
value, and a bare `"KEY"` passes your own value of `KEY`, as `GITHUB_TOKEN`
above. Anything else a server needs, a proxy setting included, is listed
there.

### Under the egress allowlist

With `sandbox.network: "allowlist"` (see
[Network policy](21-network-policy.md)), MCP servers are held to the same
`egress` rules as the agent's commands:

- **URL servers** are judged in Abhed's own process: each request is
  decided by the rules and recorded as an `egress.decision` event with
  `kind: "mcp"`, in the record of the session whose tool call made it. A
  server no rule allows does not connect.
- **Stdio servers** start with their direct network sockets confined, on
  the process, fence, container and VM tiers. On the fence the server's own
  fence lets TCP reach only its proxy; on the container and VM tiers its
  container has no network but a relay to its proxy, as a command's has. On macOS a Seatbelt profile allows no network use
  but the server's own egress proxy and denies it LaunchServices. On Linux
  the server runs in its own network and process namespaces (bubblewrap)
  behind a relay to that proxy, with the session bus, `/run/user`, the
  container engines' sockets and a private temporary folder hidden, and
  without `DBUS_SESSION_BUS_ADDRESS`, `SSH_AUTH_SOCK` and similar
  variables. It is given `HTTP_PROXY` and `HTTPS_PROXY` for its proxy,
  with a credential of its own, and on Linux the session resolver for
  names. Under bubblewrap a stdio server is not started when Abhed runs as
  root.
  A server set to `host_files` is confined this way on the process tier
  only, and refused under the allowlist elsewhere. On Windows, and on any
  surface with no sandbox to confine it, a stdio server is not started
  under the allowlist; the warning at start-up and `/mcp` give the reason.

**This confines a server's direct network use, not a hostile server.** Add
only servers you trust, as before. What remains open to a server that
means to get out:

- **Files outside your home.** Its home is hidden (see above), but what
  your user can write elsewhere stays writable, and a server set to
  `host_files` reads and writes what your user can.
- **Unix sockets it can reach.** On Linux, any AF_UNIX socket in a folder
  that is not hidden (under your home, `/var/tmp`, `/var/run/postgresql`,
  and so on) can be connected to, and whatever listens there acts for it.
  On macOS the profile refuses AF_UNIX connections, but `launchctl` can
  still read launchd's state.
- **Requests system services make on its behalf.** On macOS, verifying a
  certificate asks `trustd`, which fetches the URLs a certificate names for
  its issuer (AIA) and revocation (OCSP) itself, outside the proxy and
  unrecorded: a server can present a certificate whose URL, path included,
  points at any host. `trustd` is not denied, since every program that
  verifies TLS through the system (Go programs among them) needs it. On
  Linux, a resolver reachable over a Unix socket in a folder that is not
  hidden would look names up for it, unrecorded; `/run/systemd/resolve` and
  `/run/nscd` are hidden.

A stdio server's decisions go to the record of the session whose call to
it is in flight, with that call's id, or with none in flight, the session
that called it last. Under `abhed serve`, where servers are shared, its
traffic while calls from several sessions are in flight cannot be put to
one of them, so it goes to each of their records, with no call id and
`shared_sessions` saying how many; its traffic before any call (the latest
64 decisions) goes to each session's record when that session first calls
it, marked `before_call`. A session therefore sees what the shared server
did while it was in use, including on another session's behalf.

## From the command line

`abhed mcp` changes the servers in your own `~/.abhed/config.json`:

```sh
abhed mcp add docs /usr/local/bin/docs-mcp --stdio
abhed mcp add -env TOKEN=abc -env HTTPS_PROXY -allow-tools search,read docs node server.js
abhed mcp add -header-env Authorization=CORPUS_TOKEN corpus https://retrieval.internal/mcp
abhed mcp list
abhed mcp remove docs
```

`add` shows the server, every value escaped and `-env` values hidden, and
asks (`-env`, `-header-env` and `-allow-tools` may be repeated, each
adding to the earlier ones); only a yes typed at a terminal adds it,
enabled, since it lets the agent start a process or reach an endpoint.
Without a terminal it is refused: edit the file instead. `add` and `remove`
are also refused inside an agent's command, where `ABHED_SANDBOX` is set or
an `abhed` process is above the command. Neither check is the boundary: a terminal can be faked,
and a detached daemon has no `abhed` above it. What keeps the agent from
adding a server is the sandbox, which denies it `~/.abhed`; with the
sandbox off (`none`) nothing does. `remove` narrows, so it does not ask. Each
change is appended to `~/.abhed/config-changes.jsonl` with the time, the
server's name, its transport and the SHA-256 of its entry, never its values.
`list` shows the servers in effect in this workspace and whether each came
from the managed file, your own or the workspace's.

For one run, `-mcp-config FILE` adds servers and `-strict-mcp-config` makes
them the only ones; see [The command line](18-cli.md#settings-servers-and-roles-for-one-run).

When the managed configuration sets the `mcp` section, `add` and `remove`
are refused: its list replaces yours, and only it changes it. A workspace's
servers still need workspace trust, as before.

## Prompts as commands

A connected server that offers prompts adds each one as a slash command,
`/mcp__<server>__<prompt>`, listed in `/help` with its arguments. The words
after the name fill the prompt's arguments in order, the last taking the
rest of the line; a required one left out refuses the command before the
server is asked.

A prompt is someone else's text, so it never reaches the model unasked. Only
when you type its command is it fetched (`prompts/get`) and shown to you
with hidden and control characters escaped, and only when you then answer
yes is it recorded as `command.invoked` with source `mcp` and the SHA-256 of
the text, and sent as your next message, exactly as shown, capped at 30,000
characters. No, or no answer, sends nothing. A prompt whose name or
argument names are not letters, digits, `_`, `.` and `-` is left out with a
warning. Built-in commands, and your own and the workspace's custom
commands, keep their names: an MCP prompt that would take one is left out,
and the session says so.

A remote tool's name may hold only letters, digits, `_`, `.` and `-`, up to
64 characters. The MCP specification allows names of up to 128, but model
providers refuse tool names over 64, so a longer one is left out: a warning
names it and gives its length, `/mcp` and Studio list it under its server,
and `abhed doctor` and the server's MCP status count the tools left out for
their names beside each server.
`abhed doctor` also says why each configured server that did not connect
was not: a program that does not match its `digest`, a URL server given a
`digest`, a server that would not start.

## Namespacing and policy

Remote tools are namespaced by server, so two servers offering `search` do not
collide, and a policy rule can name one precisely:

```json
"deny": ["mcp__github__create_issue"]
```

`mcp__github__*` names every tool of that server.

Every MCP tool routes through the policy engine, because Abhed cannot know what
someone else's tool does. A remote tool is subject to approval exactly as `bash`
is, and its result is recorded and tagged untrusted.

## Failure

A server that will not start is reported and skipped; the agent runs without it
rather than refusing to start. Tool lists are fetched lazily on first use, so an
unreachable server costs nothing until something needs it.

In a session, `/mcp` lists each enabled server, connected or with the reason
it is not, and its tools; `/mcp restart <server>` reconnects one, and the
tools already offered reach the new connection.

## Many tools

A server tool is registered only when its name is letters, digits, `_`, `.`
and `-`, up to 64 characters; any other is left out with a warning on
stderr, and `/mcp` lists it under its server, its hidden characters shown as
escapes.

With more than 40 MCP tools across the servers, the model is not given each
one. A server of three tools or fewer is still offered in full, smallest
first, up to twelve such tools in all; for the rest the model gets a
`tool_search` tool instead, whose description lists each
server and its tool names (names only; a name with anything but letters,
digits, `_`, `.` and `-`, or past about 2.5 KB of names, is counted rather
than shown). The system prompt tells the model to check those tools first:
for a request about live or current data, or one a listed tool could fit, it
calls `tool_search` before it answers from its own knowledge or searches the
web or the files, and falls back to the web or memory only when nothing fits.
`tool_search` finds tools
by the words in their names and descriptions and loads the ones it returns,
with their parameters, from the next step. A loaded tool is policed, approved and
recorded like any other, and a tool the model never searched for can still
only run through policy.
