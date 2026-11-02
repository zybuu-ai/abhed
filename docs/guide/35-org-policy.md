# Signed org policy

An organisation can give every laptop the same managed configuration without
pushing a file to `/etc/abhed`. After `abhed login` to a server that serves a
signed org policy, Abhed fetches the policy, checks its signature against a
key pinned at sign-in, keeps it under `~/.abhed`, and applies it as the
managed layer: above your own `~/.abhed/config.json`, `-settings` and the
workspace's `.abhed/config.json`, and below this machine's
`/etc/abhed/config.json`.

The policy is the same document as [the managed file](02-configuration.md#the-managed-file),
and binds the same way: it is tighten-only for flags and SDK options,
`bypass` is refused, a managed `model.default` pins the model, and so on.
The command line, Abhed Studio, `acp`, `rpc` and the SDK all read it.

Any server that answers the two requests below with a policy signed by an
Ed25519 key can be the source. The Enterprise server is one: it authors,
versions and signs policy per tenant and group and records each publish in
its admin audit.

## Signing in

```sh
abhed login https://abhed.example.com
```

When the server serves an org policy, the sign-in ends with:

```
Pinned https://abhed.example.com's policy signing key sha256:3f…; check it with your administrator.
Applying the org policy "default" (version 7) from https://abhed.example.com; abhed policy status shows it.
```

The key the server offers is pinned on first use. To pin a key you were
given out of band instead, pass it; a server that does not offer it is
refused:

```sh
abhed login -policy-key 'MCowBQYDK2VwAyEA…' https://abhed.example.com
```

`-policy-key` takes a base64 Ed25519 key or a PEM `PUBLIC KEY`, and may be
repeated. Signing in again to the same server keeps the keys already pinned.
One server's policy applies at a time: signing in to a second server that
serves one leaves the first in place and says so.

## What applies, and in what order

From lowest to highest:

1. built-in defaults
2. `~/.abhed/config.json`
3. `-settings`
4. the workspace's `.abhed/config.json`, once trusted
5. **the signed org policy**
6. `/etc/abhed/config.json`, this machine's managed file

Where the org policy and `/etc/abhed/config.json` both make a setting, the
machine's file wins, setting by setting: a map entry such as one provider
under `model.providers` is taken whole from the file. `permissions.deny` and
`permissions.ask` are joined, so a rule from either stays. Both together are
"the managed configuration" for every rule that page states.

`model.allowed`, `mcp.allowed` and `mcp.denied` are kept as a layer of their
own rather than merged: a model or MCP server must pass the machine file's
list and the org policy's, so an org list narrows the machine's and never
widens it, and an `mcp.denied` entry from either refuses. `abhed doctor` and
the policy views show the org's lists with the layer `org`.

An org policy may not reach into the machine. These settings in it are set
aside, with a warning and a `config.refused` event in each session's record:
`org_policy`, `additional_dirs`, `extensions`, `mcp.servers`, `statusline`,
`commands.dirs`, `rules.dirs`, `skills.dirs`, `agents.dirs`,
`context.memory_files`, `k8s`, `storage`, `server`, `auth`, `schedules`,
`control_plane`, `record.dir`, `record.ocsf.path`, `record.witness.file`,
`sandbox.microvm_socket`, `model.providers.<name>.api_key_env` and
`network.upstream_proxy`. They name a program to run, a file or folder, a
stored secret or an environment variable on this machine, or configure a
server; the machine's own managed file may set them. The proxy is the
machine's because it names a local CA file and secret, and because the
policy itself is fetched through it: a policy that pointed it elsewhere
could cut the laptop off from the next one.

## Refresh and age

The policy is fetched again as each session starts (at most once a minute)
and every `org_policy.refresh_minutes` (15) while an interactive session or
Studio runs. A new version applies to the sessions that start after it; a
running session keeps the policy it started with, and the command line says
when it changed.

Each statement carries when it was signed and how long it may be applied
(`max_age_seconds`, set by the server; 72 hours when it gives none). A server
that cannot be reached leaves the cached policy in force until that age.

## Fail closed

The **fallback** applies when:

- the cached policy is older than its limit and no fresh one could be fetched;
- a fetched policy fails verification: not signed by a pinned key, issued for
  another person, from an earlier epoch than one already held (a rollback),
  or a document that does not load;
- the cached policy does not verify, or the state file cannot be read;
- the machine's managed file requires an org policy and none is held.

The fallback is plan mode, no allow rules, and web search and web fetch off:

```json
{"permissions": {"mode": "plan", "allow": []}, "web_search": {"enabled": false}, "web_fetch": {"enabled": false}}
```

It is laid over the cached policy, so the cached deny and ask rules and the
rest still bind beneath it. Every surface says it applies, and why, and each
session's `session.started` event records `org_policy.status: "fallback"`
with the reason. A later fetch that verifies ends it.

## Key rotation

A statement lists the keys the organisation signs with now and next. Once a
statement verifies, those become the pinned keys. To rotate, the server first
announces the new key beside the old one (signed by the old), then signs with
the new one; a client that refreshed in between follows. A client that missed
the overlap falls back, and `abhed login` again pins the new key.

## Binding a machine its owner controls

The state file lives in `~/.abhed`, which the agent's tools and sandbox cannot
read or write, so the agent cannot remove or change the policy. The person who
owns the laptop can (`abhed policy leave`, or deleting the file). To bind them
too, set `org_policy` in `/etc/abhed/config.json` with your device management:

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
| `url` | the only server whose policy this machine takes; `abhed login` to another does not enrol |
| `keys` | the only keys trusted, base64 Ed25519 or PEM; statements cannot rotate them, so rotate here |
| `required` | with no verified policy, the fallback applies; `abhed policy leave` is refused |
| `max_age_hours` | caps the statement's own age limit |
| `refresh_minutes` | how often a running Abhed fetches again |

`org_policy` is managed-only: in your own or a workspace's file it is set
aside, and an org policy cannot set it. A person with root on the machine can
still change `/etc/abhed`, or the clock the age is measured by.

## Commands

```sh
abhed policy status    # what applies, the server, the pinned keys, the keys it sets
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
Package `github.com/zybuu-ai/abhed/orgpolicy` signs and checks this format.
