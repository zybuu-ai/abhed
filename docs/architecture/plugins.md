# Plugin bundles

Status: Draft · 2026-11-17 · 1.3.0 track T8

A plugin bundle packages agents, skills, slash commands, simple hooks, MCP
server definitions and docs under one signed manifest, so a person can install
them together and an organisation can say which bundles may run.

## Goals and the rule

Everything a bundle can add already exists as a file Abhed loads under trust.
A bundle adds no new kind of power. The rule is the one extensions follow:
**a plugin adds content, never policy.**

- A bundle cannot set any configuration key. Its hooks are veto-only simple
  hooks. Its MCP servers face `mcp.allowed`/`mcp.denied`, the sandbox and the
  egress guard like any other server. Its agents and commands can only narrow
  tools. Deny rules, the sandbox tier and egress rules stay as they were.
- Nothing in a bundle runs at install. Install reads bytes, checks hashes and
  a signature, and copies files. Code runs only later, through the hook runner
  or the MCP gateway, under the session's sandbox.
- Installed content is inert until a person approves that exact content.
- A plugin never takes a name already in use. Organisation, user and workspace
  agents, skills, commands, hooks and servers keep their names.

## Format

A bundle is a directory, or a `.tar.gz`/`.tgz` of one, holding:

```
abhed-plugin.json                  manifest (what is signed)
abhed-plugin.json.sigstore.json    optional Sigstore bundle over the manifest
agents/*.md  skills/<name>/...  commands/*.md  hooks/...  mcp/...  docs/...
README*  LICENSE*
```

The manifest names the bundle (`name`, `version`, `publisher`, `description`),
the minimum Abhed version (`abhed`), every file with its SHA-256, the hooks and
MCP servers inline, and the `capabilities` it requests: `tools`, `egress`
hosts, hook `events` and `mcp_servers`. Loading refuses links, devices, `..`,
absolute paths, files not listed, listed files missing, a hash mismatch, and
any content that needs a capability the manifest does not request. The
bundle's identity is the SHA-256 of the manifest bytes; since the manifest
lists every file's hash, that one digest covers the whole bundle.

`${ABHED_PLUGIN_ROOT}` in a hook command or an MCP server's command and
arguments is replaced with the installed folder. The sandbox lets commands
read and run (never write) `~/.abhed/plugins`, as it does `~/.abhed/skills`.

## Verifying

Two ways to trust a bundle, and nothing else:

1. **Sigstore keyless signature.** `cosign sign-blob --bundle
   abhed-plugin.json.sigstore.json abhed-plugin.json`. Abhed verifies it
   offline: the leaf certificate chains to a certificate authority in a
   Sigstore trusted root (`trusted_root.json`), the transparency log's signed
   entry timestamp verifies against a log key in that root and falls inside the
   certificate's validity, the log entry covers this manifest's digest and
   signature, and the signature verifies over the manifest. The signer is the
   certificate's identity (SAN email or URI) and issuer (Fulcio OID
   1.3.6.1.4.1.57264.1.8 or .1.1). It must match an allowed publisher. No
   network is used; the trusted root is a file the person or the organisation
   provides. A bad signature is a refusal, never a fallback to unsigned.
2. **SHA-256 pin**, for air-gapped sites: `--sha256 <manifest digest>` given
   by the person at install, or the digest listed in the managed
   `plugins.allowed_bundles`.

## Install, approve, upgrade, remove

`abhed plugin install <path|url|name>` reads the bundle, verifies it, applies
policy, and prints what it adds (each agent, skill, command, hook and server)
and what it requests (tools, egress hosts, hook events). The files are copied to
`~/.abhed/plugins/<name>/<digest[:16]>/` and recorded as **pending**. Pending
content never loads. At a terminal the person is asked to approve now.
Otherwise `abhed plugin approve <name>` asks, or `-sha256 <digest>` approves
that exact content without a prompt. Approval and every state change are
refused inside an agent's command.

Installing a different version of an installed plugin is an upgrade. It shows
the diff (files, hooks, servers and capabilities added, removed or changed) and
is staged as pending. The approved version keeps loading until the new one is
approved, and then the old folder is removed. `abhed plugin remove` deletes the
folder and the state entry.

State lives in `~/.abhed/plugins.json`, which the person owns. On every load
each approved plugin's manifest and files are hashed again against the
approved digest. A plugin that changed on disk does not load, and the warning
says why.

## Policy

`plugins` is a managed-only configuration key, so the managed file or the
signed org policy sets it. A user file, `-settings` or a workspace setting it
is set aside:

```json
"plugins": {
  "disabled": false,
  "require_signature": true,
  "allowed_publishers": [{"identity": "release@acme.example", "issuer": "https://accounts.google.com"}],
  "allowed_bundles": ["<manifest sha256>"],
  "sources": [{"name": "acme", "url": "https://abhed.acme.example/v1/plugins/index"}],
  "org_source": true,
  "trusted_root": "/etc/abhed/sigstore/trusted_root.json"
}
```

An org policy may set `disabled` and `require_signature` (only to true),
`allowed_publishers`, `allowed_bundles` and `org_source`. It never sets
`sources` or `trusted_root`, because those name a server or a file. Those come
from the machine's managed file. `org_source` adds the org server this machine
takes its signed policy from as the source `org`.

`disabled` loads and installs nothing. When `allowed_publishers` or
`allowed_bundles` is set, a bundle must match one of them. `require_signature`
refuses pins. When any source is set, installs come only from a source: a
name is looked up in each index, the archive must sit under the source, and
its digest must be the one the index lists, which is the trust basis
`org-source` while that source stays configured. Policy is applied again at every load, so a later org change unloads a
plugin that no longer passes. A workspace has no way to enable a plugin:
installs and approvals live in the person's own state, and the policy is
managed.

Without managed lists, the person's own publishers (`abhed plugin publishers
add`) decide who may sign.

## Record

Install, upgrade, approve, remove, and changes to the person's publishers or
trusted root are appended to `~/.abhed/config-changes.jsonl`, as `abhed mcp
add` changes are, with the plugin's name, trust basis and digest. Refused
installs are logged too. The next session records each line as
`config.changed` in its hash-chained record. Each session also records
`plugin.loaded` once, before its first message, listing the plugins in force
and their digests. Both map to OCSF Device Config State Change (5019).
Hooks and MCP calls are recorded as they always are, under their prefixed
names (`<plugin>/<hook>`, `<plugin>-<server>`).

## Editions

Everything above is Community: one person installing, verifying and governing
plugins on their own machine, and an organisation's managed file or signed
policy constraining it. Team adds the **private plugin source**. The org server
keeps a list of bundles that admins upload or remove through an admin API. The
server checks each upload with the same Community reader (hashes, layout,
capabilities, and the signature against the server's trusted root when it has
one), then serves the index (`GET /v1/plugins/index`) and the archives to
signed-in laptops. The org policy turns on `plugins.org_source`. Community knows
only the index format and how to fetch it with the profile's token, so the
boundary between editions is a document format, not code.

## Out of scope for now

- Online Sigstore checks (TUF refresh of the trusted root, Rekor inclusion
  proofs, CT SCTs). Verification is offline, against the signed entry timestamp.
- Container and microVM tiers do not mount `~/.abhed/plugins`. A plugin hook
  or server that needs its own files runs on the process or fence tier.
- A public marketplace.
