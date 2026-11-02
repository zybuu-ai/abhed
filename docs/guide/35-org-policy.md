# Signed org policy

An organisation can give every laptop the same managed settings without
pushing a file to `/etc/abhed`. After `abhed login` to a server that serves a
signed org policy, and with your consent, Abhed fetches the policy, checks
its signature against a key pinned at sign-in, keeps it under `~/.abhed`,
and applies the settings an org policy may make as a managed layer: above
your own `~/.abhed/config.json`, `-settings` and the workspace's
`.abhed/config.json`, and below this machine's `/etc/abhed/config.json`.

The policy is written like [the managed file](02-configuration.md#the-managed-file),
but an org policy may make only some settings, and most of them only in the
direction that tightens (see [What an org policy may set](#what-an-org-policy-may-set)).
What it does set binds as managed settings do: flags and SDK options may only
tighten it, `bypass` is refused. The command line, Abhed Studio, `acp`, `rpc`
and the SDK all read it.

Any server that answers the two requests below with a policy signed by an
Ed25519 key can be the source. The Enterprise server is one: it authors,
versions and signs policy per tenant and group and records each publish in
its admin audit.

## Signing in

```sh
abhed login https://abhed.example.com
```

When the server serves an org policy, the sign-in asks before taking it:

```
https://abhed.example.com serves an org policy signed by sha256:3f….
Taking it lets that server govern this machine's Abhed settings as managed configuration,
and it may turn on sending your session records to it. Take it? [y/N]
```

With no terminal to ask on, it is not taken and the sign-in says how to take
it. `-org-policy` takes it without asking; so does `-policy-key`, which pins a
key you were given out of band (a server that does not offer it is refused):

```sh
abhed login -org-policy https://abhed.example.com
abhed login -policy-key 'MCowBQYDK2VwAyEA…' https://abhed.example.com
```

No question is asked when `/etc/abhed/config.json` names the server in
`org_policy.url`: the machine's administrator has chosen it.

Without `-policy-key`, or keys in `/etc/abhed`, the key the server offers is
pinned on first use, and its id is printed so you can check it with your
administrator. Over plain `http` to another machine that is refused: whoever
is on the path could offer their own key, so `-policy-key` is required there.

`-policy-key` takes a base64 Ed25519 key or a PEM `PUBLIC KEY`, and may be
repeated. Signing in again to the same server keeps the keys already pinned
and asks nothing. One server's policy applies at a time: signing in to a
second server that serves one leaves the first in place and says so.

The first statement that verifies also fixes who it is for: a later one for
another person or tenant is refused, on fetch and on every load.

## What applies, and in what order

From lowest to highest:

1. built-in defaults
2. `~/.abhed/config.json`
3. `-settings`
4. the workspace's `.abhed/config.json`, once trusted
5. **the signed org policy**
6. `/etc/abhed/config.json`, this machine's managed file

Where the org policy and `/etc/abhed/config.json` both make a setting, the
machine's file wins, setting by setting. `permissions.deny` and
`permissions.ask` are joined, so a rule from either stays.

`model.allowed`, `mcp.allowed` and `mcp.denied` are kept as a layer of their
own rather than merged: a model or MCP server must pass the machine file's
list and the org policy's, so an org list narrows the machine's and never
widens it, and an `mcp.denied` entry from either refuses. `abhed doctor` and
the policy views show the org's lists with the layer `org`.

## What an org policy may set

Every setting is classified, and a new one is refused until it is. A setting
the policy may not make is set aside, with a warning and a `config.refused`
event in each session's record.

**Never**, whatever the keys: anything that names a program, a local file or
folder, an environment variable, an endpoint or a `base_url`, or configures a
server. That covers model providers and `custom_providers`, `web_search`'s
provider, endpoint and key, the supervisor's judge (`monitor.endpoint`,
`api_key_env`, model), `rag`, `retrieval`, `telemetry`, `network` (the proxy
names a local CA file and secret, and the policy is fetched through it), ssh
hosts and `ssh.read_commands`, `k8s` clusters, `sandbox.read_only_paths`,
`sandbox.language_servers`, `sandbox.image`, `verify.contract.require`,
`verify.critic`, extensions, `mcp.servers`, statusline, the `dirs` settings,
`context`, `additional_dirs`, record paths and retention, storage, server,
auth, schedules, the control plane and `org_policy` itself.

**Only where it tightens** what the layers below already say, by the same rule
an untrusted workspace's file follows: deny and ask rules, honeytokens and `verify.contract.protect` (added to the
person's own, never replacing them),
`permissions.mode` and `mode.default` (only plan or default, and only
narrower), `model.allowed` and the `mcp` lists, the web tools turned off,
lower limits, a stronger `sandbox.min_tier`, `allow_network` and
`readable_home` off, stricter toolchains and listen, `monitor.enabled` on and
`on_unavailable: deny`, more supervisor reviews and honeytokens, stricter
syntax checks and checkpoints, and the like. A value that would loosen is set
aside.

**Only when the machine pins the keys**: `permissions.allow`,
`git_extensions`, `sandbox.tier`, `sandbox.network`, `protect_git`,
`listen_ports`, preapproved toolchains, `fence`, `egress` and `hooks`, and the
loosening values of the tighten-only settings above. These change what runs
or reaches out unasked, so they need keys `/etc/abhed/config.json` pins out
of band (`org_policy.keys`); a key pinned on first use is not enough.
`auto` and `bypass` modes are never set by an org policy.

**Always**: `record.ship.enabled` and `record.ship.max_backlog_mb` (records
go only to the server this machine enrolled with), `hooks.managed_only:
true`, `studio.disable_host_terminal: true`, `model.allowed_loopback: false`,
and `model.default` pinned to a provider the machine's managed file defines
(never one the person configured with their own key). Under the org's `model.allowed`,
endpoints are bound as under the managed file's, but only the managed file
may define one (a provider, a custom provider type, `monitor.endpoint`).

## Refresh and age

The policy is fetched again as each session starts (at most once a minute)
and every `org_policy.refresh_minutes` (15) while an interactive session or
Studio runs. A new version applies to the sessions that start after it; a
running session keeps the policy it started with, and the command line says
when it changed.

Each statement carries when it was signed and how long it may be applied
(`max_age_seconds`, set by the server; 72 hours when it gives none, and never
more than 30 days). A server that cannot be reached, or that stops serving a
policy, leaves the cached policy in force until that age; the fallback reason
then says what the last fetch found.

## Fail closed

The **fallback** applies when:

- the cached policy is older than its limit and no fresh one could be fetched;
- a fetched policy fails verification: not signed by a pinned key, issued for
  another person or tenant, from an earlier epoch than one already held (a
  rollback), with keys that leave out the one that signed it, or a document
  that does not load;
- the cached policy does not verify, is for another person or tenant, or is
  older than an epoch already held, or the state file cannot be read;
- the machine's managed file requires an org policy and none is held.

The fallback is plan mode, no allow rules, and web search and web fetch off:

```json
{"permissions": {"mode": "plan", "allow": []}, "web_search": {"enabled": false}, "web_fetch": {"enabled": false}}
```

Of the cached policy only what tightens stays beneath it: its deny and ask
rules and its allow lists. Everything else it set is dropped. Every surface
says the fallback applies, and why, and each session's `session.started`
event records `org_policy.status: "fallback"` with the reason. A later fetch
that verifies ends it.

`/etc/abhed/config.json` still wins over the fallback where it sets the same
key, such as `permissions.mode` or the web tools: the machine's administrator
decides what the machine does.

## Key rotation

A statement lists the keys the organisation signs with now and next, and must
include the one that signed it. Once a statement verifies, those become the
pinned keys. To rotate, the server first announces the new key beside the old
one (signed by the old), then signs with the new one; a client that refreshed
in between follows. A client that missed the overlap falls back, and `abhed
login` again pins the new key.

Rotation by statement trusts the current key: whoever holds it can announce
keys of their own. Keys pinned in `/etc/abhed` are never moved by a statement,
which is the way to make a stolen signing key not enough.

## What `required` does and does not stop

The state file lives in `~/.abhed`, which the agent's tools and sandbox cannot
read or write, so the agent cannot remove or change the policy. The person who
owns the laptop can (`abhed policy leave`, or editing the file). Set
`org_policy` in `/etc/abhed/config.json` with your device management:

```json
{
  "org_policy": {
    "url": "https://abhed.example.com",
    "keys": ["MCowBQYDK2VwAyEA…"],
    "required": true,
    "max_age_hours": 24,
    "refresh_minutes": 15
  }
}
```

| Key | Effect |
|---|---|
| `url` | the only server whose policy this machine takes; signing in to it takes the policy without asking, and `abhed login` to another does not enrol |
| `keys` | the only keys trusted, base64 Ed25519 or PEM; statements cannot rotate them, so rotate here; also what lets the policy loosen settings |
| `required` | with no verified policy, the fallback applies; `abhed policy leave` is refused |
| `max_age_hours` | caps the statement's own age limit |
| `refresh_minutes` | how often a running Abhed fetches again |

`required` without `keys` is warned about at every start and by `abhed
doctor`: the key is then whatever the server answered at the first sign-in.

What this does not stop: the owner can still edit `~/.abhed`, for example
putting back an older statement of their own within its age, or removing the
file; with `required` that means the fallback or the older statement, never
content the org did not sign for them. A person with root on the machine can
change `/etc/abhed`, or the clock the age is measured by.

An `abhed` started inside an agent's command with another `HOME` reads no org
state, so it runs without the org layer; its commands still run in the
parent session's sandbox. Set `required` to have such a run fall back instead.

## Commands

```sh
abhed policy status    # what applies, the server, the pinned keys, the keys it sets and those set aside
abhed policy show      # the verified policy document
abhed policy refresh   # fetch it now
abhed policy leave     # stop taking it (refused under a managed org_policy)
```

`refresh` and `leave` are refused inside an agent's command.

## The server side of the protocol

A source answers, to a signed-in request with the profile's bearer token:

- `GET /v1/org/policy/keys`: `{"keys": ["<base64 Ed25519>", …]}`
- `GET /v1/org/policy`: `{"envelope": <DSSE envelope>}`

The envelope's `payloadType` is `application/vnd.abhed.org-policy+json`, its
signatures are Ed25519 over the DSSE pre-authentication encoding, and each
`keyid` is `sha256:` and the hex SHA-256 of the key's PKIX DER. The payload is:

```json
{
  "v": 1,
  "tenant": "acme",
  "subject": "alice@example.com",
  "name": "engineering",
  "version": 4,
  "epoch": 31,
  "issued_at": "2026-11-02T09:30:00Z",
  "max_age_seconds": 86400,
  "keys": ["<base64 Ed25519>"],
  "policy": { "permissions": { "mode": "default" } }
}
```

`epoch` must never go down for a client: a server counts every publish.
`subject` must be the signed-in person. Package
`github.com/zybuu-ai/abhed/orgpolicy` signs and checks this format.
