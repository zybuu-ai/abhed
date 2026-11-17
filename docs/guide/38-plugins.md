# Plugins

A plugin bundles [agent definitions](17-agent-definitions.md),
[skills](06-skills.md), [custom commands](19-input-and-memory.md),
[simple hooks](07-extensions.md#simple-hooks) and [MCP servers](08-mcp.md)
under one manifest, so they install, upgrade and come out together.

A plugin adds content. It never adds policy:

- A bundle sets no configuration key. Your deny rules, the sandbox tier and
  the egress policy stay as they are.
- Its hooks can only block, never approve, and run in the session's sandbox.
- Its MCP servers face `mcp.allowed` and `mcp.denied`, run confined, and never
  get the host's files.
- Its agents and commands can only narrow the tools a turn may use.
- Nothing in a bundle runs when you install it.
- Installed content stays off until you approve that exact content.
- A plugin never takes a name already in use. Your skills, agents, commands,
  hooks and servers, and the organisation's, keep theirs.

The design is in [Plugin bundles](../architecture/plugins.md).

## Installing

```sh
abhed plugin install ./acme-review                # a folder
abhed plugin install ./acme-review-1.0.0.tgz      # an archive
abhed plugin install https://plugins.acme.example/acme-review-1.0.0.tgz
abhed plugin install acme-review                  # by name, from a plugin source
```

Install reads the bundle and checks every file against the manifest. It then
checks the signature, or your pin, and the policy, and prints what the bundle
adds and requests:

```
acme-review 1.0.0 from Acme
  digest   5b0c…
It adds:
  agent    reviewer
  skill    lint
  command  /review
  hook     acme-review/no-rm on tool_call: sh ${ABHED_PLUGIN_ROOT}/hooks/no-rm.sh
  mcp      acme-review-tickets: https://tickets.acme.example/mcp
It requests:
  tools    grep, read
  egress   tickets.acme.example
  hooks    tool_call
  mcp      tickets
Trusted by: a Sigstore signature from release@acme.example (https://token.actions.githubusercontent.com)
Installed acme-review 1.0.0, waiting for approval: it does not load until approved.
```

At a terminal you are asked to approve it now. Elsewhere, approve it later:

```sh
abhed plugin approve acme-review                   # shows it again and asks
abhed plugin approve -sha256 5b0c… acme-review     # approves this exact content, no prompt
```

The plugin loads from the next session. `abhed plugin list` shows each plugin
and its state: `loaded`, `pending`, or `not loaded` with the reason, for
example a policy that changed since. `abhed plugin show NAME` prints the
summary again.

`install`, `approve`, `remove`, `publishers` and `trusted-root` are refused
inside an agent's command. Plugins change only from your own terminal.

## Trusting a bundle

A bundle is trusted in one of two ways, and nothing else:

**A Sigstore signature from a publisher you trust.** The publisher signs the
manifest with `cosign sign-blob --bundle abhed-plugin.json.sigstore.json
abhed-plugin.json`. Abhed checks the signature offline against a Sigstore
trusted root, which is a `trusted_root.json` file you or your organisation
provide:

```sh
abhed plugin trusted-root ./trusted_root.json      # kept as ~/.abhed/plugin-trusted-root.json
abhed plugin publishers add -issuer https://token.actions.githubusercontent.com \
  'https://github.com/acme/plugins/.github/workflows/release.yml@refs/tags/*'
abhed plugin publishers list
```

The check is offline. The certificate must chain to the trusted root at the
time the transparency log took the entry. The log's signed timestamp must
verify, and the entry must be for this manifest and this signature. The
signature must verify too. The signer is the certificate's identity (an email
or a URI) and its OIDC issuer. An identity that ends in `*` matches any
identity it starts with, such as a workflow at any tag. A signature that does
not verify is refused; it is never treated as unsigned.

**A SHA-256 pin**, for air-gapped sites. Check where the bundle came from,
then pin its manifest's digest:

```sh
abhed plugin install -sha256 5b0c… ./acme-review
```

An unsigned bundle without a pin is refused, and the error gives the digest
to pin.

## Upgrading and removing

Installing another version of an installed plugin is an upgrade. It prints
what changed: files added, removed or changed, hooks and servers, and
requests. The new version waits for approval, and the approved version keeps
loading until you approve the new one.

```sh
abhed plugin remove acme-review        # asks first; -yes to skip
```

`remove` deletes every file of the plugin and its entry.

## Where things live

| Path | What it holds |
|---|---|
| `~/.abhed/plugins/<name>/<digest>/` | the installed files; commands may read and run them (as `~/.abhed/skills`), never write |
| `~/.abhed/plugins.json` | your decisions: approved and pending versions, your publishers |
| `~/.abhed/plugin-trusted-root.json` | your Sigstore trusted root |

On every load, each approved plugin is hashed again against what you
approved. A plugin whose files changed on disk does not load, and a warning
says which file.

Container and microVM sandboxes do not mount `~/.abhed/plugins`. A plugin hook
or server that runs its own script needs the process or fence tier.

## Writing a plugin

```
acme-review/
  abhed-plugin.json
  agents/reviewer.md
  skills/lint/SKILL.md
  skills/lint/run.sh
  commands/review.md
  hooks/no-rm.sh
  docs/usage.md
  README.md
```

A bundle may hold `agents/*.md`, `skills/<name>/` (each with a `SKILL.md`),
`commands/**/*.md`, `hooks/`, `mcp/`, `docs/`, `README*` and `LICENSE*`, and
nothing else. Links and special files are refused. Hooks and MCP servers are
declared in the manifest, so the signature covers them:

```json
{
  "schema": 1,
  "name": "acme-review",
  "version": "1.0.0",
  "publisher": "Acme",
  "description": "Review helpers",
  "abhed": "1.3.0",
  "hooks": [
    {"name": "no-rm", "events": ["tool_call"], "match": ["bash(rm *)"],
     "command": "sh ${ABHED_PLUGIN_ROOT}/hooks/no-rm.sh"}
  ],
  "mcp_servers": [
    {"name": "tickets", "url": "https://tickets.acme.example/mcp",
     "headers_env": {"Authorization": "ACME_TICKETS_TOKEN"}}
  ],
  "files": [],
  "capabilities": {}
}
```

`abhed plugin pack DIR` fills in `files` with every file's SHA-256, and fills
in any empty `capabilities` list with what the content uses. Those are the
tools its agents and commands list, the hosts its hooks and URL servers
reach, its hook events and its server names. A bundle whose content needs a
capability the manifest does not request is refused. Then sign the manifest
with cosign.

`${ABHED_PLUGIN_ROOT}` in a hook command or a server's command and arguments
becomes the installed folder. Run scripts through an interpreter
(`sh …/x.sh`), because installed files are not executable. A server's
credentials come only from the environment (`headers_env`). Hooks are named
`<plugin>/<hook>` and servers `<plugin>-<server>`, so a permission rule written
for one of your own servers never matches a plugin's.

## Policy

The plugin policy is `plugins` in the managed configuration, or in the signed
[org policy](35-org-policy.md). Your own file, `-settings` and a workspace's
file never set it: what they write there is set aside.

```json
"plugins": {
  "disabled": false,
  "require_signature": true,
  "allowed_publishers": [{"identity": "release@acme.example", "issuer": "https://accounts.google.com"}],
  "allowed_bundles": ["5b0c…"],
  "sources": [{"name": "acme", "url": "https://abhed.acme.example/v1/plugins/index"}],
  "org_source": true,
  "trusted_root": "/etc/abhed/sigstore/trusted_root.json"
}
```

| Key | Effect |
|---|---|
| `disabled` | nothing installs and nothing loads |
| `require_signature` | a pin alone is refused |
| `allowed_publishers`, `allowed_bundles` | when either is set, a bundle must be signed by a listed publisher or have a listed digest; your own publishers are then not consulted |
| `sources` | plugin indexes; when set, installs come only from them: by name, or a URL under one |
| `org_source` | adds the org server this machine takes its signed policy from as the source `org` |
| `trusted_root` | the Sigstore trusted root signatures are checked against; `-trusted-root` and yours are then not taken |

The policy is applied again on every load, so a change by the organisation
unloads a plugin that no longer passes. An org policy may set `disabled` and
`require_signature` only to `true`, plus `allowed_publishers`,
`allowed_bundles` and `org_source`. `sources` and `trusted_root` name a server
or a file, so only the machine's managed file sets them.

A source is a JSON index:

```json
{"plugins": [{"name": "acme-review", "version": "1.0.0",
  "url": "bundles/acme-review-1.0.0.tgz", "sha256": "5b0c…",
  "publisher": "Acme", "description": "Review helpers"}]}
```

`url` is resolved against the index and must stay under its folder. The
archive fetched must have the digest the index lists. A server you are signed
in to (`abhed login`) is asked with your profile's token. Hosting a private
source on the org server, with an admin API, is a Team feature, documented
with the Team and Enterprise edition.

## The record

Each change (install, upgrade, approve, remove, publishers, trusted root) and
each refused install is appended to `~/.abhed/config-changes.jsonl`. The next
session records each one as `config.changed`, with the plugin's name, trust
basis and digest. Every session records `plugin.loaded` before its first
message, naming each plugin in force with its version, digest, basis and
signer. Both map to OCSF Device Config State Change (5019) —
[OCSF and logs](30-ocsf-and-logs.md).

A bare run (`-bare`) loads no plugins.
