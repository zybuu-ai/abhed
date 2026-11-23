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
  hooks and servers, and the organisation's, keep theirs. A plugin ranks right
  after your own files: before a workspace's skills and commands, and after a
  workspace's agent definitions, as your own definitions are.
- A server's headers from the environment are listed, and a variable named
  like a credential is never one of them.

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
  command  /acme-check
  hook     acme-review/no-rm on tool_call: sh ${ABHED_PLUGIN_ROOT}/hooks/no-rm.sh
  mcp      acme-review-tickets: https://tickets.acme.example/mcp (header Authorization <- $ACME_TICKETS_HEADER)
It requests:
  tools    grep, read
  egress   tickets.acme.example
  hooks    tool_call
  mcp      tickets
  env      ACME_TICKETS_HEADER (sent as headers to its servers)
Trusted by: a Sigstore signature from release@acme.example (https://token.actions.githubusercontent.com)
Installed acme-review 1.0.0, waiting for approval: it does not load until approved.
```

At a terminal you are asked to approve it now. Elsewhere, approve it later:

```sh
abhed plugin approve acme-review                   # shows it again and asks
abhed plugin approve -sha256 5b0c… acme-review     # approves this exact content, no prompt
```

When an agent or command lists no tools, the `tools` line says it gets all
the session's tools. Every string in the manifest is printed as it is; a
manifest holding a control character, an escape sequence or an invisible
format character is refused when it is read, so nothing can redraw the lines
above the prompt.

The plugin loads from the next session. `abhed plugin list` shows each plugin
and its state: `loaded`, `pending`, or `not loaded` with the reason, for
example a policy that changed since. `abhed plugin show NAME` prints the
summary again.

`install`, `approve`, `remove`, `publishers` and `trusted-root` are refused
inside an agent's command. Plugins change only from your own terminal.

## Trusting a bundle

A bundle is trusted in one of two ways, and nothing else:

**A Sigstore signature from a publisher you trust.** The publisher signs the
manifest keylessly with cosign 3:

```sh
cosign sign-blob --yes --bundle abhed-plugin.json.sigstore.json abhed-plugin.json
```

This is the command Abhed's own release workflow runs (cosign v3.1.3). The tests
check a bundle the cosign v3.1.4 release made the same way, against the
public-good trusted root. With cosign 2.4 to 2.x, add `--new-bundle-format`;
older cosign writes its legacy bundle, which is refused with a message saying
so. Sigstore bundles 0.2 and 0.3 are read. The entry must come from a Rekor v1
log and carry its signed entry timestamp, which is what makes an offline check
possible. An entry from a Rekor v2 log has none, and is refused with a message
saying so.

Abhed checks the signature offline against Sigstore trusted roots
(`trusted_root.json`). There are two, and which one a signature verifies
against decides what it counts for:

- **The org root** is the managed file's `plugins.trusted_root`. If the managed
  file names none, it is the public-good root this build carries: a snapshot
  of `sigstore/root-signing`'s `targets/trusted_root.json`. Only a signature
  that verifies against the org root counts toward `allowed_publishers` and
  `require_signature`. A root you choose never does, so a CA you made cannot
  issue the identity an organisation's list names.
- **Your root** is the one you keep with `abhed plugin trusted-root`, or the
  org root when you keep none. A signature that verifies only against it
  counts for your own publishers alone.

Roots are kept, not given per install, because each load checks the
signature again.

The built-in root is a snapshot, taken on the date `abhed plugin list` shows
once it is more than a year old; loading warns then too. Sigstore rotates its
keys. A bundle signed under a key this snapshot does not know fails to
verify. A key Sigstore later revokes stays trusted by this build. Either
change reaches you only through an Abhed upgrade, or through a managed
`plugins.trusted_root` your organisation keeps current.

```sh
abhed plugin trusted-root ./trusted_root.json      # kept as ~/.abhed/plugin-trusted-root.json
abhed plugin publishers add -issuer https://token.actions.githubusercontent.com \
  'https://github.com/acme/plugins/.github/workflows/release.yml@refs/tags/*'
abhed plugin publishers list
```

The check is offline. The certificate must chain to the trusted root at the
time the transparency log took the entry. The log's signed timestamp must
verify, and the entry, a `hashedrekord` 0.0.1, must be for this manifest and
this signature. The signature must verify too. Not checked: the certificate's
CT SCTs, and the log's inclusion proof and checkpoint.

The signer is the certificate's identity (an email or a URI) and its OIDC
issuer. A certificate that names more than one identity, or two different
issuers in its v1 (`1.3.6.1.4.1.57264.1.1`) and v2 (`.1.8`) extensions, or a
v2 issuer that does not read, is refused. An identity with a `.` or `..` path
segment, or an escaped `.`, `/` or `\`, matches no publisher. A URI identity may end in `*` right after a `/` or `@`, to match any
identity it starts with, such as a workflow at any tag. An email identity is
always named whole. A signature that does not verify is refused at install;
it is never treated as unsigned.

At every load the staged signature is verified again against the roots in
force. The signer that counts, and that the record names, is the one that
verifies then. What `~/.abhed/plugins.json` says about the signer is for you
to read, and nothing trusts it. A signature that no longer verifies counts as
none, and the bundle then loads only on a pin.

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

Plugins are each person's, as skills are. Signed in, the paths below are in
your account's folder under `~/.abhed/accounts/` instead of `~/.abhed`; a
guest can list plugins but not install, approve or remove one, and keeps
none (see [Signing in](38-signing-in.md)).

| Path | What it holds |
|---|---|
| `~/.abhed/plugins/<name>/<digest>/` | the installed files; commands may read and run them (as `~/.abhed/skills`), never write |
| `~/.abhed/plugins.json` | your decisions: approved and pending versions, your publishers |
| `~/.abhed/plugin-trusted-root.json` | your Sigstore trusted root |

On every load, each approved plugin is hashed again against what you
approved. A plugin whose files changed on disk does not load, and a warning
says which file. The check runs when a session starts. Hook scripts, server
programs and a skill's helper files are run from disk later. No sandboxed
command can write `~/.abhed/plugins` on any tier, but a program running as you
outside Abhed could change a file mid-session. The next session would refuse
the plugin.

Which sandboxes see the plugin folder:

- macOS process tier and the Linux fence tier: commands can read and run
  `~/.abhed/plugins`.
- Linux process tier (bubblewrap) and the container and microVM tiers: the
  folder is not mounted, as `~/.abhed/skills` is not. A plugin hook or server
  that runs its own script needs the fence tier there. A command written as
  is, such as `npx`, `uvx` or a binary on `PATH`, runs on every tier.

A plugin does not load in `abhed serve` sessions. A server's sessions are
other people's, and the plugins you approved are your own.

## Writing a plugin

```
acme-review/
  abhed-plugin.json
  agents/reviewer.md
  skills/lint/SKILL.md
  skills/lint/run.sh
  commands/acme-check.md
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
     "headers_env": {"Authorization": "ACME_TICKETS_HEADER"}}
  ],
  "files": [],
  "capabilities": {}
}
```

`abhed plugin pack DIR` fills in `files` with every file's SHA-256, and fills
in any empty `capabilities` list with what the content uses. Those are the
tools its agents and commands list, the hosts its hooks and URL servers
reach, its hook events, its server names, and the environment variables its
servers send as headers (`env`). A bundle whose content needs a capability the
manifest does not request is refused. Then sign the manifest with cosign.

A server's `headers_env` maps a header to an environment variable you set for
that plugin. These names are refused:

- names that contain `KEY`, `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`,
  `PASSPHRASE`, `CREDENTIAL`, `PRIVATE`, `COOKIE`, `SESSION`, `WEBHOOK`,
  `DATABASE_URL`, `CONN_STR`, `CONNSTR`, `CONNECTION_STRING`, `BEARER`,
  `NETRC` or `AUTH`;
- names with one of these words between underscores: `PASS`, `PWD`, `PAT`,
  `JWT`, `DSN`, `OTP`, `PIN`, `SIG`, `CERT`;
- a `…_URL` or `…_URI` named for a database, cache, broker or hook, such as
  `REDIS_URL` or `APP_DB_URL`;
- names that start with `ABHED_`, `AWS_`, `GITHUB_`, `GH_`, `GITLAB_`, `GL_`,
  `GOOGLE_`, `GCP_`, `GCLOUD_`, `AZURE_`, `AZ_`, `ANTHROPIC_`, `OPENAI_`,
  `CLAUDE_`, `GEMINI_`, `MISTRAL_`, `COHERE_`, `GROQ_`, `HF_`, `HUGGING`,
  `OLLAMA_`, `VAULT_`, `SSH_`, `GPG_`, `NPM_`, `DOCKER_`, `KUBE`, `CI_`,
  `SLACK_` or `SENTRY_`.

This list is best-effort: a credential can be named anything. What a plugin
may send is the `env` it requests, which the approval shows by name. An upgrade that adds a header or a variable lists it among the changes,
and it waits for your approval like the rest.

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
| `allowed_publishers`, `allowed_bundles` | when either is set, a bundle must be signed by a listed publisher or have a listed digest |
| `sources` | plugin indexes; when set, installs come only from them: by name, or a URL under one |
| `org_source` | adds the org server this machine takes its signed policy from as the source `org` |
| `trusted_root` | the org root: the only Sigstore root a signature may verify against to count toward `allowed_publishers` and `require_signature` (otherwise the built-in public-good root); yours still serves your own publishers |

The policy is applied again on every load, so a change by the organisation
unloads a plugin that no longer passes. An org policy may set `disabled` and
`require_signature` only to `true`, plus `allowed_publishers`,
`allowed_bundles` and `org_source`. `sources` and `trusted_root` name a server
or a file, so only the machine's managed file sets them.

The machine's lists and the org policy's lists are separate layers, and a
bundle must pass every layer that sets a list. If the machine allows
publisher A and the org allows bundle X, a bundle must be signed by A *and*
be X. Neither widens the other.

Passing a list is enough to trust a bundle when it is the machine's list, or
the list of an org policy whose keys the machine pins (`org_policy.keys`).
An org whose keys are not pinned only narrows. The trust must then come from
you, through a publisher you added or a pin. Your own publishers count only
for bundles every list allows. An org policy grants only while it is fresh
and verified. A stale, refused, wrong-tenant or fallback policy keeps its
lists, which still narrow, but they never grant.

The `org` source is trusted on the same terms: when the machine's file turns
it on, or the org's keys are pinned. Otherwise a bundle from it needs a
signature or a pin like any other.

When a trusted source's index vouches for a digest at install, that is
recorded as your approval of exactly that digest through the source (basis
`org-source`). It stands at every later load, even under
`require_signature`, as long as the bundle's signature verifies against the
org root. A load never asks the source again, and never takes a source name
from `plugins.json`. So turning `org_source` off, removing a source, or
removing the bundle from the index does not unload a plugin already installed
from it. Only the allow lists or `disabled` do that.

Install judges the bundle as every later load will, from what it keeps, and
refuses one that would not load. Approval does the same.

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

The package `plugin` (the reader, the verifier, the store and the index types)
is public so that other programs can check bundles the same way. It is an
experimental API until 2.0, and may change in a minor release.
