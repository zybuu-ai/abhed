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
toolchain's, never all of `~/.local` or `~/.cargo`), read-only. So is the
folder of each file its configured `args` name by absolute path under your
home, such as the script an interpreter runs (`"command": "python3",
"args": ["/Users/me/tools/mcp/tickets.py"]` shows `~/tools/mcp`), on every
tier. Where that folder, or the install folder, is a folder directly in
your home (`~/Downloads`, `~/Documents`, `~/Projects`, `~/bin`,
`~/.local`), `~/.local/bin` or `~/.cargo/bin`, or a cloud drive's root or a
folder directly in it (iCloud Drive and other apps' folders in
`~/Library/Mobile Documents`, the drives in `~/Library/CloudStorage`), only
the file itself is shown, not the folder; paths are compared without
regard to case, as macOS's disk does. A file in your home itself,
in a folder that holds a credential path (`~/.config`, `~/Library`) or
inside one (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gcloud` and the
rest) is never shown, and a program installed in a credential folder is
refused. A server
whose program sits in `~/.abhed`, or directly in your home, is refused: that
folder would show it Abhed's state. A server that fails to start says why:
the end of what it wrote to stderr (its last lines, up to 1,000 characters)
follows the error in `/mcp`, Studio and `abhed doctor`, so a missing file or
module is named instead of only a closed pipe. It is redacted first: the
values of the server's own configured environment (on the container and VM
tiers too, where they travel in a file), a value its arguments name as a
secret (`--api-token VALUE`, `-e API_KEY=VALUE`, `--header
"Authorization: Bearer VALUE"`, `NAME_TOKEN=VALUE`), and anything shaped
like a key or token. The workspace
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
warning, `/mcp` and `abhed doctor` give that reason. Set `sandbox.image` or `ABHED_SANDBOX_IMAGE`
to an image with its runtime ([the container tier's image](02-configuration.md#the-container-tiers-image)), or `host_files` to run it on the host. A tier that cannot
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
abhed mcp login corpus
abhed mcp logout corpus
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

Studio adds servers through this command (**Abhed: Add MCP Server…**): the
engine checks the server and describes it, Studio asks in a modal whose
default is No, and on a yes runs `abhed mcp add -confirm-fd 3 -sha256 …`,
giving the yes on an inherited descriptor in place of a terminal and holding
it to exactly the entry you were shown. Studio passes environment variables
by name only, so the server gets your own value and no value is written. The
same refusals apply, and a refused add is logged as an attempt in
`config-changes.jsonl` and recorded as `config.refused`. The next session
records the change log's new entries as `config.changed`, so an add, from
Studio or a terminal, reaches a hash-chained record. Studio removes a
server of your own with `_abhed/mcp/remove`, recorded as `mcp.status`.

For one run, `-mcp-config FILE` adds servers and `-strict-mcp-config` makes
them the only ones; see [The command line](18-cli.md#settings-servers-and-roles-for-one-run).

When the managed configuration sets `mcp.servers`, `add` and `remove`
are refused: its list replaces yours, and only it changes it. A workspace's
servers still need workspace trust, as before.

## Signing in with OAuth

A URL server that wants a sign-in answers `401` until you sign in to it:

```sh
abhed mcp login corpus     # opens your browser; prints the address too
abhed mcp login -no-browser corpus  # only prints the address
abhed mcp logout corpus    # revokes the login where the server offers that, and forgets it
```

The browser is the one `BROWSER` names when it is set (commands separated
by `:`, each given the address in place of `%s` or at the end, run without
a shell), else the system's. Over SSH no browser is opened, since the
sign-in returns to a listener on this machine.

`login` follows the MCP authorization specification. It asks the server for
its protected-resource metadata (from its `401` challenge, or the well-known
address), then its authorization server's metadata, registers Abhed as a
client where that server offers dynamic registration, and opens the sign-in
page in your browser. The page comes back to a listener on `127.0.0.1` that
Abhed opens for this sign-in alone; the code is exchanged with PKCE (S256)
and the server's URL as the `resource`. It refuses:

- a server or authorization server that is not `https`, so a token is never
  sent in the clear; `http` is taken only on this machine, and only when the
  MCP server itself is there, so a remote server cannot point a sign-in at a
  local service;
- metadata that names another resource than the server, or an issuer not
  exactly the authorization server it was fetched from;
- an authorization server without PKCE S256;
- a redirect that does not carry this sign-in's state, or whose `iss`, when
  it has one, is another issuer.

For an authorization server that registers no clients, name a client
registered ahead of time:

```json
{ "name": "corpus", "enabled": true, "url": "https://retrieval.internal/mcp",
  "oauth": { "client_id": "abhed-cli", "client_secret_env": "CORPUS_CLIENT_SECRET", "scopes": ["read"] } }
```

`client_secret_env` is read at `login` only. `scopes` defaults to what the
server says it supports.

The tokens are kept in the secret store (`~/.abhed/secrets.json`) under
names beginning `MCP_OAUTH_`, which Abhed holds for itself: they are not
offered to the model, a command or tool is never given one, `abhed secret
set` and `abhed secret rm` refuse the prefix (as does the server's
`/v1/secrets` API, and Studio does not list them), and their values are
redacted from the record like any stored secret. A login is bound to the server's name and URL: a server
whose URL changes is not sent it, and asks for `login` again. Its access
token goes on requests to the server's own origin only, never on a redirect
or to an endpoint on another host, and a server that also has an
`Authorization` header configured is refused. It is refreshed before it
ends, and once more when the server refuses it. A refresh holds a lock
beside the store, given up after the refresh's own time limit, so two Abhed
processes never spend one refresh token: the second takes up the first's
tokens. The lock spans processes where the store is a file on macOS or
Linux; on Windows, and for a store a secrets driver keeps, it spans one
process only. A running session reads the store again at most every five
seconds as it sends: a login removed while it runs stops being used within
that time (a request already sent completes), is never written back by its
refresh, and the session says it was signed out; a login made again says
that `/mcp restart` takes it up. Every request a sign-in or refresh makes, the
browser's apart, goes under the egress allowlist (`sandbox.network:
"allowlist"`, where the authorization server needs a rule too) and the
corporate upstream proxy. `login` and `logout` are refused inside an
agent's command, and `login` needs a person at a terminal; each is written
to `~/.abhed/config-changes.jsonl`, never a token. `abhed mcp list` says
which servers are signed in.

The login is read from the operator's store, by the terminal, an editor's
sessions, the SDK and an `abhed serve` that no accounts sign in to. OAuth
logins aren't available per account yet: where accounts sign in
(`auth.mode` other than `none`), the server's sessions share one gateway,
and the operator's login would let every account act as the operator, so
it is not used. A server with a login is not connected there, and says why;
`abhed serve` warns at start-up and `abhed doctor` names each one.

## Resources

A server that offers resources adds two tools under its name:
`mcp__<server>__list_resources` lists them (each URI, name and type; what
the server says of them is treated as a tool description is) and
`mcp__<server>__read_resource` reads one by URI. A read returns at most
30,000 characters; binary content is described, not sent. Both go through
policy as the server's tools do: `mcp__<server>__*` covers them, a rule can
name a URI (`"deny": ["mcp__docs__read_resource(docs://private/*)"]`), and
when `allow_tools` is set it must name them. A server's own tool of either
name keeps it, and its resources are then not offered under that name.

A URI is read only in its canonical form, the form policy judges and the
server is sent: scheme and host in lower case, the host without a trailing
dot, no default port for `http` and `https`, `file:///` rather than
`file://localhost/`, an IPv6 or IPv4 address in its one spelling, no `.`
or `..` segments and no empty ones, unreserved characters not
percent-encoded, printable non-ASCII characters written as themselves in
NFC rather than escaped, and other escapes in upper case. A URI written
otherwise (`DOCS://private/a`, `docs://private./a`, `https://h:443/a`,
`docs://private/%61`, `docs://private/caf%C3%A9`, `docs://127.1/a`) is
refused, naming the form to write. One with a host not in ASCII (write it in its punycode, `xn--`,
form), an escape in its host, an encoded `/` or `\`, a backslash, a
fragment, user information or a rooted path without an authority
(`docs:/private/a`) is refused outright, and a listing leaves those out and
says how many. Since a server may still read paths without regard to case,
or normalise Unicode, deny and ask rules on a read also match the URI
without regard to case, in NFC and with its escapes decoded, as rules on
file paths do: a deny on `docs://private/*` holds for `docs://PRIVATE/a`,
and one on `docs://private/café/*` for `docs://private/caf%C3%A9/x`. They
also match the URI decoded twice and in NFKC, with its dot-segments then
resolved, as a server that decodes twice or folds compatibility characters
reads it: a deny on `docs://h/private/*` holds for
`docs://h/pub%252F..%252Fprivate/a`, `docs://h/pub／..／private/a` and
`docs://h/ｐrivate/a`, while a name such as `TM™.md`, `x².md` or
`100%25AB.md` is read as written. Allow rules match only as
written. What one URI names on the server beyond that is the server's.
Sessions in one process (an editor's, an SDK embedder's) each read a
server's resources through their own connection. A server whose tools
change across `/mcp restart` keeps its resources read as the session
started; once a later listing in that session's gateway offers the
server's own tool under the read's name, the session's read refuses to run.

In the terminal or an editor's prompt, `@server:uri` attaches a resource:
`@docs:file:///guide.md`, `@tickets:ticket://42` (write a space as `%20`:
`@docs:file:///my%20notes.md`). It is read as your own call through the
server's `read_resource` tool, so a deny rule refuses it and the message is
not sent; the read and the attachment are recorded as an `@` file's are.
`/mcp resources <server>` lists a server's resources.

## Allowed and denied servers

The managed file can say which servers may ever start, with `mcp.allowed`
and `mcp.denied`. Each entry matches by `name`, by the `command` path the
program resolves to, by a pinned `digest`, or by a remote server's `host`:

```json
"mcp": {
  "allowed": [{"command": "/opt/mcp/bin/*"}, {"host": "*.mcp.corp.example"}],
  "denied": [{"name": "shell-*"}]
}
```

A server any `mcp.denied` entry matches never starts. When `mcp.allowed` is
set, a server no entry matches never starts either. That holds wherever the
server comes from:

- the user's, a trusted workspace's or the managed file
- `-mcp-config` (the run stops, naming the server and the list)
- `abhed mcp add` (refused before anything is written when the managed lists
  refuse it; one only your own lists refuse is written, then set aside when
  the configuration loads)
- the console's add (403, audited as `mcp.refused`)
- Studio
- a restart

Each server left out is recorded as `config.refused` and shown as refused,
with why, by `/mcp`, Studio's MCP panel and `abhed doctor`. Setting the
lists does not make `mcp.servers` the organisation's. A lower layer's lists
can only narrow. See
[Allowed models and MCP servers](02-configuration.md#allowed-models-and-mcp-servers)
for every field and the startup checks.

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

A prompt's name and its arguments' names may hold only letters, digits, `_`,
`.` and `-`, up to 64 characters, since a person types them; a server may
list up to 200 prompts, over up to 10 pages, and a warning names a server
that lists more. A remote tool's name may hold the same characters, up to
128, as the MCP specification allows; any other is left out: a warning
names it and gives its length, `/mcp` and Studio list it under its server,
and `abhed doctor` and the server's MCP status count the tools left out for
their names beside each server. A full name a model provider would refuse,
longer than 64 or holding `.`, still works: see
[Names on the wire](#names-on-the-wire).
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

### readOnlyHint

A server may mark a tool `readOnlyHint: true` in its tool list. Abhed
ignores that unless the operator opts the server in: the managed file
(`/etc/abhed/config.json`) defines the server, with
`"trust_read_only_hint": true` on its entry, and the entry in force is
exactly that one.

```json
{"mcp": {"servers": [{"name": "docs", "url": "https://docs.corp.example/mcp",
  "enabled": true, "trust_read_only_hint": true}]}}
```

A tool such an opted-in server marks read-only is then a read, as `read`
and `grep` are: it runs without asking in the default mode and in plan
mode, and deny rules, ask rules, hooks and the record apply as before. It
is off by default, so a managed server that does not opt in, and every
server you, a `-mcp-config` file or a workspace added (with the key or
without it), asks as above.

Why only the operator: the hint is the server's own word about itself. A
server that wants to run unasked would say it, and nothing checks it. An
operator who opts a server in has vouched both that it may run and that its
reads need no question; a server that came with a repository, or that a
person added for themselves, has no one who has.

## Failure

A server that will not start is reported and skipped; the agent runs without it
rather than refusing to start. Servers start when the session does, eight at
a time, each within its own startup deadline: 60 seconds by default, set per
server with `startup_timeout_seconds`. The deadline covers the handshake and
reading its tool, prompt and resource lists. A server that has not started
by then, one that hangs say, is reported as failed with the deadline it
missed, its tools are absent, and the other servers start as they would
without it; `/mcp restart <server>` tries it again. After the start, each
request to a server (a tool call, a list page) waits at most 60 seconds.

In a session, `/mcp` lists each enabled server, connected or with the reason
it is not, and its tools; `/mcp restart <server>` reconnects one, and the
tools already offered reach the new connection. A server that refuses the
connection for want of a sign-in says to run `abhed mcp login <server>`.

## Many tools

A server tool is registered only when its name is letters, digits, `_`, `.`
and `-`, up to 128 characters; any other is left out with a warning on
stderr, and `/mcp` lists it under its server, its hidden characters shown as
escapes. A tool list in pages is read to its end, following `nextCursor`, up
to 100 pages and 2,000 tools a server; past either, the rest is left out and
a warning names the server.

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

At most 100 tools stay loaded. Loading one more unloads the one loaded or
called least recently, and `tool_search` says how many it unloaded; a search
loads one again. A request also never offers the provider more tools than it
takes (`max_tools`, 128 by default; see
[Configuration](02-configuration.md)): past that, the loop unloads loaded
tools, least recently used first, before it sends. A built-in tool, an
extension's, and an MCP tool offered in full are never unloaded, so when they
alone pass the limit the request is refused before it is sent, with an error
that names the count, the limit and what to narrow (an MCP server's
`allow_tools`, a server or extension to disable, or `max_tools`), rather than
a provider's 400.

## Names on the wire

Providers accept a tool name of letters, digits, `_` and `-`, up to 64
characters. `mcp__<server>__<tool>` can be longer, and a tool's own name may
hold `.`. Such a name is sent to the provider under a short wire name: its
plain characters, up to 53, then `_` and ten hex digits of the full name's
SHA-256, so `mcp__docs__search.pages` goes as
`mcp__docs__search_pages_` followed by its hash. The wire name is the same in
every request, so the prompt prefix stays cacheable, and a call by it is
mapped back to the real name before anything else sees it. `tool_search`
gives the wire name beside the real one.

Only the request to the provider carries the wire name. Permission rules,
approval cards, `always allow`, hooks and the record all use the real
`mcp__<server>__<tool>` name, so a rule written for `mcp__docs__search.pages`
matches it however it travelled.

## Limits

| What | Limit | Past it |
|---|---|---|
| A tool's name on its server | 128 characters, letters, digits, `_ . -` | left out, with a warning |
| A tool list | 100 pages, 2,000 tools a server | the rest left out, with a warning |
| Prompts | 200 a server, 10 pages; names up to 64 | the rest left out, with a warning |
| Resources listed | 500 a server, 10 pages | the rest not listed, with a warning |
| A tool's description | 1,024 bytes, cut on a character | cut, with `…` |
| A tool's result, a prompt's text, a resource read | 30,000 bytes, cut on a character | cut, marked `[truncated]` |
| One message on a stdio server's stdout | 16 MiB | the connection ends, with an error naming the server |
| Starting a server | 60 s (`startup_timeout_seconds`), 8 servers at once | reported as failed, its tools absent |
| One request to a server | 60 s | the call fails with a timeout |
| MCP tools offered in full | 40 across the servers, then `tool_search`; servers of 3 tools or fewer, 12 tools in all, stay in full | deferred |
| Tools loaded by `tool_search` | 100 | least recently used unloaded |
| Tools in one request | the provider's `max_tools`, 128 by default | loaded tools unloaded, then refused with a clear error |

Skills and agent types have budgets of their own in the prompt; see
[Skills](06-skills.md#why-only-the-description-is-in-the-prompt) and
[Agent definitions](17-agent-definitions.md#many-definitions). A workspace
reads at most 64 skills, 64 agent definitions and 128 custom commands a
directory, and says so when there are more.

## Scale

What was tested, with stub servers on this machine and no network, in the
`TestScaleIntegrations` check (`make test-scale`): 50 stdio servers of 20
tools each (1,000 tools, some with dotted names and names past 64
characters), 300 skills and 100 agent types in one session. All the servers
start; calls on a sample of the servers' tools reach the right server with
their arguments unchanged, through the registry and through the loop and an
OpenAI-compatible adapter by their wire names; after 150 tools are searched
for, 100 stay loaded and no request offers more than 128 tools, every name
one a provider accepts; and the skill listing and the agent types each stay
within their 8 KB budgets. Routing tests besides it cover three servers
offering a tool of one name, a deferred tool once loaded, pagination, an
extension tool named like an MCP tool (refused), a server that hangs at
start, and a long and a dotted name through each provider's wire format.

Nothing caps how many servers you configure. What grows with them is
startup, eight at a time, and the tool definitions in each request, which
the limits above keep within the provider's.
