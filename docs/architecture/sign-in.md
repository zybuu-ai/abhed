# Sign-in first, with a guest mode

Status: accepted 17 Nov 2026, being built for 1.3.0.
Written against the 1.3.0 development line.

The design below was written before the build; "As built" says where the build differs.

## Decisions (17 Nov 2026)

1. Everything here ships in 1.3.0.
2. A remembered sign-in lasts until sign-out or until an administrator
   removes the person. There is no idle expiry. `auth.client.remember_days`
   exists only as a managed or org-policy setting; unset means forever.
   Every start still shows who is signed in and offers a switch.
3. Community sign-in is local username and password only: an account on this
   machine, or on a self-hosted Community server. OIDC (Google, Microsoft and
   other providers) stays in Team (section 4.3).
4. No per-account encryption at rest. Accounts are separate spaces, and the
   docs say plainly that the OS account is the confidentiality boundary.
5. Enterprise/Team: sign-in is required; guest is allowed by default and an
   administrator can forbid it with `auth.client.guest: false`. Guest runs
   still feed the organisation's OCSF export and record shipping (6.4).
6. Accepted as proposed: the non-interactive rule and exit code 10 (section
   5), bcrypt cost 12 for the machine store, credentials in files under the
   guarded `~/.abhed`, and migration with a journal and `unmigrate`.

## As built (1.3.0)

Where the build differs from the sections below, the build is right:

- The setting is `auth.client.sign_in` (`""`, `"required"`, `"server"`),
  not `require`: the org-policy classification refuses any leaf named
  `require`. The org policy may set `sign_in`, `guest` (only `false`) and
  `remember_days`; never `server`.
- The remembered sign-in is a random token in
  `~/.abhed/accounts/remembered.json` whose SHA-256 the account keeps, with
  the revocation count it was issued under; it is not a `CLITokens` token,
  so the machine store needs no `LocalAuth` or signing key.
- Code: `internal/home` (machine and person folders), `internal/identity`
  (the gate, the accounts store, guest, migration), `app/signin*.go`,
  `app/cmd_account.go`, `app/rpc_auth.go`, `app/acp_auth.go`,
  `app/serve_signin.go`. Guide: `docs/guide/38-signing-in.md`.
- ACP credentials go in `_meta["zybuu.ai/abhed"]`, as every other `_meta`
  of the contract (studio-acp-contract §1.5).
- Server guest web sessions (2.5) are not in 1.3.0's Community build; a
  server's front door is unchanged apart from serve's first-start question.
- No variable hands a person's folder to a child process: every process
  answers who is working through the gate itself.
- A remembered server sign-in is checked at start; one with no credential,
  or that its server refuses, fails closed; only an unreachable server lets
  it through, and session.started then records `verified: false`.
- ACP: one lock orders authenticate against everything else, and who is
  working is fixed once anything has worked as someone.
- Migration: the first account is remembered only after its data has moved;
  a start settles any move first and never runs on half the data.
- A server's sign-in page is passed to a client only if it is https, or
  http on loopback or at that server.
- Each guest records as `guest:<id>`.
- A password reset is kept in `accounts.json` and told at the next sign-in
  on every surface (terminal, rpc `notice`, ACP `_meta.notice`); it is not
  appended to the record chain.
- `/config set`, `abhed mcp add` and the first-run model choice write the
  machine's `~/.abhed/config.json`, which every account and guest loads; the
  per-account `config.json` layer in 3.3 is not built.
- A guest's agent commands already run with `HISTFILE` empty; package and
  toolchain caches are the machine's, shared, and a guest's downloads stay
  in them.

---

## 0. The decision in one paragraph

Every start of Abhed resolves *who is at the keyboard* before anything else
runs: a **local account on this machine**, an **account on an Abhed server**
(the person's own or their organisation's), or **guest**. The answer picks a
*state root*: an account's own directory, or a throwaway guest directory
that is wiped on exit and swept after a crash. Policy, sandbox, egress and
approvals do not depend on the answer; only whose history, memory, record
and credentials are in play does. Non-interactive runs never prompt: they use
a stored sign-in, or an explicit guest flag, or default to guest with a
stderr note, unless managed policy requires sign-in, in which case they exit
with code 10. No Zybuu-hosted account service exists or is contacted.

What "local account" is, honestly: a separate, password-gated namespace for
Abhed's state under one OS login. It is not encryption at rest. Anyone who
can act as that OS user can read the files, as they can today. The OS
account stays the confidentiality boundary; section 7 says so, and the docs
must say so.

---

## 1. What exists today (the base we build on)

| Piece | Where | What it gives us |
|---|---|---|
| Local accounts, bcrypt (library default cost), must-change flag, revocation count, sign-out everywhere | CE `auth/local.go`, `auth/filestore.go` (JSON file, atomic write, file lock) | the account model and store for machine accounts, reused as is |
| Signed CLI tokens tied to account, revocation count and password stamp | CE `auth/clitoken.go` | the "remembered sign-in" token for machine accounts, reused |
| `abhed login` (RFC 8628 device, RFC 7636 PKCE loopback) against a server's local accounts; discovery `GET /v1/cli`; `CLILoginAdvertiser` for editions | CE `auth/clilogin.go`, `profiles/`, `app/cmd_login.go`, guide 26 | server sign-in on the client, unchanged |
| Profiles file `~/.abhed/profiles.json`, 0600, link refused, refused inside a workspace | CE `profiles/profiles.go` | where server tokens live today (file, not keychain) |
| Owners: `local:<user>`, verified email, `oidc:<sub>`, `anonymous`, `agent`, `unclaimed:`, `nobody:` | CE `auth/identity.go` | we add a reserved `guest` owner |
| CLI record owner is `$USER` (sanitised), tenant `default`; `ownedHere` already accepts `local:<user>` after an "owner migration" | CE `app/cli_sessions.go` (`cliUser`, `ownedHere`, `openRecordWith`) | the hook for per-account ownership and migration |
| First-run model onboarding, TTY only, writes only `~/.abhed/config.json` on confirm | CE `app/onboard.go` (`firstRunNeeded`, `firstRun`), called from `app/cli_run.go:48` | identity gate goes before this |
| Managed file `/etc/abhed/config.json`, signed org policy (tighten-only, cached in `~/.abhed/org-policy.json`, fetched with a login profile) | CE `config/managed.go`, `config/orgpolicy.go`, `app/orgpolicy.go`, `orgpolicy/` | how a laptop learns "sign-in required" |
| ACP `initialize` returns `authMethods: []`; `authenticate` is a no-op | CE `app/acp_init.go:94`, `app/acp.go:844` | the standard ACP hook Studio will use |
| OIDC browser sign-in with Google and Microsoft presets, GitHub OAuth, CLI SSO (`/v1/cli/sso/*`, server keeps the client secret, issues its own token) | Team edition | Team today; stays Team (section 4.3) |
| SCIM 2.0 with deprovisioning that signs out CLI tokens and marks `access_removed` for laptops | Team edition (SCIM guide) | laptop behaviour on deprovisioning |
| Sign-in endpoints throttled per caller outside auth | CE `server/server.go:1052` | no per-account lockout exists yet |

State today is spread across about 45 call sites that build
`filepath.Join(os.UserHomeDir(), ".abhed", ...)` directly (records, history,
drafts, projects/…/memory, exports, secrets.json, profiles.json, trust.json,
command-trust.json, state/*.json, index/, push/, ui.json, org-policy.json,
config.json, pki/, cli-token.key, workload-token.key, logs/, …). The single
biggest piece of work in this track is giving those sites one place to ask
"which root am I under".

---

## 2. First-start flow

### 2.1 The identity gate (shared by every front end)

A new package `internal/identity` (CE) runs before configuration beyond the
managed layer is read, before the sandbox is chosen and before the model
onboarding. It returns a `Principal`:

```
Principal{Kind: account|server|guest, Account: "yuvraj", Server: "prod" (profile), Owner: "device:yuvraj" | "<server owner>" | "guest",
          Root: "~/.abhed/accounts/<id>" | "~/.abhed/guest/<rand>", Remembered: bool}
```

Order of resolution:

1. Explicit choice: `-guest` / `ABHED_GUEST=1`, or `-account NAME`, or
   `-profile NAME` (server).
2. A remembered sign-in on this machine (section 2.6) that is still valid.
3. Interactive: the choice screen.
4. Non-interactive: the rule in section 5.

Managed policy filters every step: `auth.client.guest: false` removes guest;
`auth.client.require: "server"` removes machine accounts (EE, section 6).

### 2.2 TUI (`abhed`, interactive)

No remembered sign-in:

```
Abhed — who is working?

  1) Sign in            local account on this machine
  2) Create an account  on this machine; nothing leaves it
  3) Sign in to a server   your own or your organisation's Abhed server
  4) Guest              nothing is kept when you quit

Choose [1]:
```

- 1: username (default: the only account, else the last one used), password.
  Three wrong answers add a delay (section 7.2).
- 2: register (section 2.7). Default username is `$USER`.
- 3: lists existing server profiles, or asks for a URL and runs the existing
  `abhed login` flow (PKCE in a browser, device code over SSH). The server
  decides the method (local, or SSO where it offers it).
- 4: guest; one line says what is not kept.

With a remembered sign-in, no prompt: the banner gains
`Signed in as yuvraj (this machine) · /account to switch`. Inside the TUI,
`/account` shows who, switches (ends the conversation first) or signs out.

Only after the gate: the existing model onboarding (`firstRunNeeded`) runs
against the chosen root, so a guest with no machine model configured is
offered the same choice, written nowhere unless they pick an account.

### 2.3 Studio (engine host asks Studio to show the choice)

Studio starts the engine over ACP; stdin is a pipe, so the engine never
prompts. It uses ACP's own authentication handshake:

- `initialize` returns `authMethods` instead of `[]`:
  `abhed.account` (sign in), `abhed.register`, `abhed.server` (one entry per
  profile plus "another server"), `abhed.guest`, filtered by managed policy.
  `_meta.abhed.identity` carries the remembered principal when there is one
  (`{kind, display, remembered:true}`), so Studio can skip the screen.
- Until a principal is set, `session/new`, `session/load` and every
  `_abhed/*` method that touches state answer -32000 "Authentication
  required" with `data.authRequired: true`.
- Studio shows its own sign-in view and calls `authenticate {methodId,
  _meta:{username,password}}` for a machine account. The password crosses the
  parent-child pipe once; it is never logged, recorded or echoed in a reply,
  and the redactor gets it for the life of the call. For `abhed.server` the
  engine runs PKCE itself and returns the browser URL through
  `_abhed/auth/openUrl` for Studio to open; device code is the fallback.
- Studio's own storage (its global and workspace storage, chat views,
  terminal scrollback) must also be guest-aware: in guest, Studio keeps
  nothing in its user-data dir. This is a Studio work item.

### 2.4 `abhed rpc` and other ACP clients

- `abhed rpc` gains `auth.status`, `auth.signin`, `auth.register`,
  `auth.guest`. A 1.2.9 client that never calls them gets the
  non-interactive rule (section 5) at the first state-touching call, so it
  never hangs and keeps working.
- Third-party ACP clients that ignore `authMethods` get the same: stored
  sign-in, else guest by default with a `_meta` notice on the first session,
  else (sign-in required by policy) the auth-required error with a message
  naming `abhed login` / `abhed account signin`.

### 2.5 `abhed serve`

`serve` is a daemon run by an operator; it does not get the personal choice
screen. What changes:

- Interactive first start with no `auth.mode` and no accounts: offer
  "create the first admin account" (local mode, writes `auth.users_file` out
  of every workspace) or "run without sign-in on loopback only". Non-TTY
  starts behave exactly as 1.2.9 (bootstrap admin with must-change, or
  `auth.mode: none` on loopback).
- The console's front door gains the same three doors: sign in (local or
  SSO), register (`auth.allow_signup`, unchanged, off by default) and guest.
  **Server guest is off by default** (`auth.guest: false`): an operator's
  server running an unauthenticated agent that executes commands is not
  something to enable silently. When on, a guest web session gets owner
  `guest:<random>`, its sessions are deleted when it signs out or its cookie
  expires, and it is listed for administrators as guest.

### 2.6 Remembered sign-in

A successful machine sign-in issues a `CLITokens` token (reused code, key at
`~/.abhed/accounts/.key`, 0600). It lasts until `abhed account signout`, a
password change, a reset, or account removal (the token already checks
revocation count and password stamp). There is no idle expiry; a managed or
org-policy `auth.client.remember_days` bounds it (0 asks every start). Server
sign-ins are remembered as today by their profile token and refresh token,
until sign-out or until the server's administrator removes the person.
Every start shows who is signed in and how to switch.

### 2.7 Register (machine account)

`abhed account create` or the screen: username, optional email/name,
password twice (at least 10 characters, as `ErrWeakPassword` already says).
Creates the account record, its root directory, and shows a **recovery
code** once (section 3.2). If 1.2.9 data exists and no account has taken it,
registration offers to bring it in (section 3.4).

---

## 3. Local account on this machine

### 3.1 Store, permissions, hashing

- Directory `~/.abhed/accounts/`, 0700. Accounts file
  `~/.abhed/accounts/accounts.json`, 0600, the existing `FileUserStore` format
  (bcrypt hash, revocation count; atomic replace; file lock), with two added
  fields: `recovery_hash` (bcrypt of the recovery code) and
  `failed`/`locked_until` for backoff. Link or group/world-readable file is
  refused, as `profiles.Load` already refuses.
- Each account gets `~/.abhed/accounts/<id>/` (0700) where `<id>` is a random
  stable id, not the username, so a rename is a field change and usernames
  never become paths.
- Hashing: bcrypt via the existing `LocalAuth.CreateUser`/`Authenticate`.
  For the machine store, raise the cost to 12 (about 250 ms on an M-class
  CPU) because the file is offline-attackable by design; server cost is
  unchanged.
- The accounts directory sits inside `~/.abhed`, which the sandbox and the
  agent's tools already treat as state they cannot read or write. The guard
  list in `internal/sandboxconfig` gains the managed `record.dir`-equivalent
  per-account paths only when they are moved out of `~/.abhed`.
- `abhed account create|list|signin|signout|passwd|reset|rename|rm` are
  refused when run by the agent, as `abhed login`/`logout` already are.

### 3.2 Recovery and reset with no server

Two ways, both recorded:

1. **Recovery code** shown once at registration (16 random bytes, base32,
   grouped). `abhed account reset NAME` with the code sets a new password.
2. **OS-owner reset**: `abhed account reset NAME --as-os-user` sets a new
   password with no code, after a typed confirmation. It is honest about the
   boundary: whoever is the OS user can already read the files. It appends
   `account.password_reset{by:"os-user", host, os_user}` to that account's
   record chain, and the next sign-in shows "Your password was reset on
   <date> from this machine without the recovery code".

Either reset raises the revocation count, ending every remembered sign-in.
There is no encryption at rest (decision 4), so the OS-owner reset loses
nothing.

### 3.3 What signing in unlocks, and the state split

The new `internal/home` package answers two roots: `home.Machine()`
(`~/.abhed`) and `home.Person()` (the principal's root). Every one of the ~45
call sites moves to one of them:

| Per person (moves under the account or guest root) | Stays per machine |
|---|---|
| `records/` (the hash-chained record, its index, heads, blobs) | `config.json` (machine settings, the layer 1.2.9 scripts rely on) |
| `history/` (prompt history), `drafts/` | `org-policy.json` (the org's tighten-only policy binds everyone on the machine, guest included) |
| `projects/<id>/memory/MEMORY.md` (auto memory), `ABHED.md` (user memory) | `pki/`, `cli-token.key`, `workload-token.key`, server state (`controlplane/`, `fence/`, `run/`) |
| `secrets.json`, `secrets.d/` (incl. MCP OAuth tokens) | `logs/`, `config-changes.jsonl`, `admin.jsonl` (machine audit) |
| `profiles.json` (each account's server sign-ins), `startup-files.json` | |
| `trust.json`, `command-trust.json`, `state/workspace-grants.json` | the guest sweep lock and `accounts/` itself |
| `state/record-ship.json`, `exports/`, `push/`, `quarantine/` | |
| `index/` (vector caches), `ui.json`, `skills/`, `skills-active/`, `agents/`, `commands/`, `styles/` | |
| an optional per-account `config.json` layered above the machine one | |

Trust and grants move per person on purpose: a guest or another account
must not inherit the owner's decision to trust a workspace's hooks or a
Studio folder grant. Secrets move per person so a guest can never use the
owner's MCP tokens; a machine-level secret store remains for the owner to
deliberately share a model provider key (`abhed secret set -machine`), and
that is the only machine secret a guest can resolve.

The local record's `User` becomes the account owner (`device:<username>`, a
new reserved namespace so it can never equal a server owner) and `Tenant`
stays `default` unless `storage.tenant` says otherwise. When managed
`record.dir` is set (organisation-owned record location), the record stays
there, unchanged in layout: sessions are told apart by their recorded
owner, and the org's collectors keep their cursors.

### 3.4 Migrating a 1.2.9 user's data into their first account

On the first interactive 1.3.0 start, if `~/.abhed/records` (or any
per-person item in the table) exists and no account has adopted it:

```
Abhed 1.3 adds sign-in. You have 214 sessions, prompt history and memory
from earlier versions. Create your account on this machine and they move
into it; nothing is deleted.

  1) Create my account and bring my data   (recommended)
  2) Sign in to a server instead           (your local data stays as it is, asked again next time)
  3) Guest for now                         (your local data stays as it is, asked again next time)
```

Migration:

1. Take the machine lock (`~/.abhed/.migrate.lock`) and stop if any other
   Abhed process holds a record session lock.
2. Verify every session chain (`abhed record verify` logic) and write the
   heads to `~/.abhed/accounts/<id>/migration.json` with a journal of the
   planned moves.
3. `rename(2)` each item into the account root (same filesystem, so each
   move is atomic). A crash resumes from the journal on next start.
4. Verify again; the heads must match. Mark the account
   `legacy_user: $USER`, so `ownedHere` treats old rows recorded as
   `$USER` as this account's (it already accepts `local:<user>` after an
   owner migration; the new rule is the same idea).
5. Write `~/.abhed/MOVED-TO-ACCOUNTS.txt` naming where things went.

No links are left behind (state paths refuse links). Rollback:
`abhed account unmigrate` moves everything back for a downgrade to 1.2.9,
and the Upgrading notes say to run it before downgrading.

A second machine account starts empty; only the first adopts legacy data.

---

## 4. Sign-in against a self-hosted Abhed server

### 4.1 What works with no change

`abhed login URL` against a CE server with `auth.mode: local` (accounts on
that server, bcrypt, PKCE or device code, signed CLI tokens). In 1.3.0 the
identity gate simply lists profiles as choice 3 and makes the chosen
profile the principal. The profile's server owner is the owner of the
person's sessions; their local state root is
`~/.abhed/accounts/server-<profile-id>/` so each server identity gets its own
history and memory on the laptop.

### 4.2 Google and Microsoft ("gmail / outlook / manual") — Team

"Manual" is the local account (on the machine or the server). Google and
Microsoft are standard OIDC against the operator's own server; the EE code
already treats them so (presets: Google issuer with
`hd`, Microsoft `common` with `tid`). No Zybuu service is involved: the
operator registers their own OAuth client with Google or Microsoft and puts
its id in the server config; the laptop talks only to the operator's server
(the server fronts the provider, as EE CLI SSO already does).

### 4.3 Which edition signs in with an identity provider

Community signs in with local usernames and passwords: an account on this
machine, or on a self-hosted Community server. Signing in through an
identity provider (OIDC, including the Google and Microsoft presets, and
GitHub) is part of the Team edition, as is everything that governs who may
sign in: group roles, SCIM provisioning and deprovisioning, and requiring
sign-in to the organisation's server on every machine (section 6). In
Community, "Google" or "Outlook" means a local account whose email is such
an address.

---

## 5. Non-interactive use (`-p`, rpc, ACP, CI, scripts)

Never prompt when stdin or stderr is not a terminal (the test
`firstRunNeeded` already uses). The rule, in order:

1. `-guest` or `ABHED_GUEST=1` → guest.
2. `-account NAME` / `ABHED_ACCOUNT=NAME` or `-profile NAME` → that principal,
   if a remembered sign-in for it is valid; otherwise exit **10** ("sign-in
   required: run `abhed account signin NAME` in a terminal").
3. A valid remembered sign-in (the last used) → that principal.
4. **1.2.9 compatibility**: per-person legacy data exists in `~/.abhed` and no
   account has adopted it → run exactly as 1.2.9 (legacy root, owner
   `$USER`), with one stderr line:
   `abhed: sign-in is not set up; running as before. Start abhed in a terminal once to create your account.`
5. Otherwise → guest, with one stderr line:
   `abhed: no sign-in; running as guest, nothing is kept (use -guest to silence, or sign in once in a terminal)`.

Managed policy overrides 4 and 5: with `auth.client.require` set, steps 4
and 5 exit **10** instead; with `auth.client.guest: false`, steps 1 and 5
exit 10.

Exit code 10 is new and unused (guide 10 lists 0–9, 129, 130, 143). It goes
in the result line as `"error":"signin_required"`.

How 1.2.9 scripts keep working:

- A developer laptop: step 4 until they first start the TUI; after migration
  the remembered sign-in (step 3) keeps `-p`, `-continue`, `abhed logs` and
  `abhed record` working on the same sessions, now under the account.
  A remembered sign-in has no idle expiry (decision 2); it ends only on
  sign-out or removal, after which `-p` falls to step 5 (or exits 10 with
  `-account NAME`). This is the one behaviour change a script can notice;
  it is in the Upgrading notes.
- CI and containers (fresh home, no data): step 5, guest. CI runs are
  ephemeral already; `-continue` there had nothing to continue. Record
  shipping in CI (EE) needs a server profile, as today.
- A CI wrapper should pass `-guest` explicitly to silence the note.
- `abhed rpc` 1.2.9 clients: the same rule at the first state-touching call.

Guest refuses what needs persistence, with a clear message rather than
silently doing less: `-continue`/`-resume`, `abhed logs`, background
(detached) jobs that outlive the process, `abhed secret set`,
`abhed mcp login`, `/memory` writes beyond the session.

---

## 6. Enterprise / Team

### 6.1 Required sign-in with SSO, guest allowed unless policy forbids it

New managed keys (CE parses them; the `server` value needs an edition
server to mean SSO):

```json
{ "auth": { "client": {
    "require": "server",              // "" (default) | "any" | "server"
    "server": "https://abhed.corp",   // the only server whose sign-in counts
    "guest": true,                    // EE default true, overridable
    "remember_days": 7
} } }
```

- `require: "any"`: some sign-in (machine account or server) before work.
- `require: "server"`: a valid sign-in to `auth.client.server` (local or SSO
  there, whatever that server offers; existing CLI SSO, unchanged). Machine
  accounts are not offered.
- `guest`: when false, guest is removed everywhere (screen, ACP methods,
  `-guest`, the non-interactive default).

Defaults per edition (decided):

| | CE | EE (machine enrolled with an EE server) |
|---|---|---|
| sign-in required | no | yes: the EE server's org policy carries `require: "server"` unless its administrator turns it off |
| guest | yes | yes, unless `guest: false` |

The org policy can also set these (tightening only: it can turn
`require` on and `guest` off, never the reverse), so a fleet enrolled with
`abhed login --enrol` picks them up without touching `/etc/abhed`.

### 6.2 How the laptop knows

The managed file `/etc/abhed/config.json` (MDM), or the cached signed org
policy in `~/.abhed/org-policy.json`. Both are machine level and bind every
principal on the machine including guest. Because the org policy is fetched
with a person's profile and profiles move per account, the enrolment records
which account holds the fetching profile; when that account is not signed in
the cached policy keeps applying until `max_age_hours`, then its fallback
(the existing behaviour for a stale policy, which is restrictive).

### 6.3 SCIM deprovisioning effects on the laptop

Already: deprovisioning signs out every CLI token, and the laptop sees
`access_removed` on its next request (record shipping, policy refresh,
login). New in 1.3.0:

- The identity gate checks the server principal at start (a cheap whoami
  with the existing refresh) when online; `access_removed` ends the
  remembered sign-in on the laptop and the gate says "Your organisation
  removed your access" and offers only what policy still allows (guest, if
  allowed; nothing, under `require: "server"`).
- The person's local per-server root (history, memory, local record copy) is
  **kept, not deleted**: it is evidence and may be under legal hold. It is
  readable again only by signing in to that server. An EE managed option
  `auth.client.on_removed: "wipe"` can delete the local root after the
  record is shipped; not built in 1.3.0.
- A run in progress continues until it next needs the server (approval,
  shipping), as guide 28 already describes.

### 6.4 Audit and compliance when a guest runs

"Guest keeps nothing" is about the person's retained state. It does not
switch off the organisation's audit, which is the organisation's data:

- A guest run writes its record into the guest root (needed during the run
  for rewind, verify and approvals) and the record is wiped at exit.
- Where managed config turns on OCSF export (`record.ocsf`) or record
  shipping, they still apply to guest runs, with owner `guest:<id>`, the OS user
  and host, and `principal.kind: guest` on every event, so an administrator
  can see guest use and who on the machine did it.
- Shipping needs a credential. A guest has none, so under required shipping
  guest is refused where shipping is required.
- Admin views (EE dashboard, audit queries) gain a "guest runs" filter.

---

## 7. Threat model

### 7.1 Credential storage: keychain versus file

- 1.2.9 keeps server bearer tokens in `~/.abhed/profiles.json` (0600, link
  refused, refused inside a workspace). The agent's commands and tools cannot
  reach `~/.abhed` (the state guard). This is proven and tested.
- An OS keychain is not automatically safer here: on macOS, an item created
  through `/usr/bin/security` trusts that tool, so any process of the user,
  including an agent command, can read it with `security
  find-generic-password`; on Linux, Secret Service is reachable over the
  session D-Bus from any process of the user. The sandbox does not guard
  either today.
- **Recommendation for 1.3.0: file storage under the guarded `~/.abhed`** for
  the remembered machine token and server tokens (as today). Keychain in a
  later release only with an item ACL scoped to the signed Abhed binary
  (macOS Security framework via cgo, not the `security` CLI, which also puts
  the secret in argv) and a process-tier profile that denies agent commands
  the keychain/D-Bus. A managed `auth.client.credential_store` can choose
  then.

### 7.2 Brute force

- Offline: the accounts file is readable by the OS user, so a password is
  only as strong as bcrypt cost 12 and its length. Minimum 10 characters,
  checked against a short list of common passwords **[new]**.
- Online (machine): after 3 failures, a delay doubling to 15 minutes, kept in
  the accounts file. Honest note: the OS user can edit the file; this stops
  a passer-by, not the OS owner.
- Online (server): the existing per-caller throttle on sign-in endpoints,
  plus a per-account backoff **[new; today there is none per account]**.

### 7.3 Shared machines

- Different people, different OS logins: each has their own `~/.abhed`; no
  change, full separation by the OS.
- Different people, one OS login (lab machine, family laptop): machine
  accounts separate state and stop casual access; they do not stop someone
  who reads files directly as that OS user. Docs say this plainly and
  recommend separate OS accounts or a server for confidentiality. There is no
  encryption at rest per account (decision 4).

### 7.4 Guest data leakage

- Guest root is `~/.abhed/guest/<random>/` (0700): inside the guarded state
  directory (so the agent cannot read other guests or accounts) and on the
  same volume. Not `/tmp`, which the sandbox may expose and other users can
  list.
- Environment for every command the guest's agent runs points `HISTFILE`,
  `XDG_STATE_HOME`, `XDG_CACHE_HOME` and tool caches under the guest root,
  so shell history and tool caches go with it **[check the fence and
  process tiers already set these; fence sets XDG_CACHE_HOME per run]**.
- Wipe: on normal exit and on SIGINT/SIGTERM/SIGHUP the guest root is
  removed. Each guest process holds an `flock` on `<root>/.lock`; every
  Abhed start sweeps guest roots whose lock is free (crash, power loss,
  `kill -9`). `abhed doctor` reports any it found.
- Deletion is ordinary unlinking: on SSDs and APFS it is not secure erase,
  and the docs say so. Guest also never writes to the person's model
  provider caches, Studio storage (Studio item) or the machine
  `config-changes.jsonl` beyond machine audit lines.
- Files the guest's agent changes in the workspace are the work itself and
  stay; Abhed's own state is what is not kept. Agent worktrees created
  outside the workspace by a guest are removed at exit.

### 7.5 Downgrade to dodge policy or audit

- Policy (managed file, org policy, workspace config, sandbox minimum,
  egress, approvals, budgets) is resolved identically for every principal.
  Guest gets the machine config and defaults, never a signed-in account's
  per-person allow rules, trust decisions or grants. So guest is never more
  privileged than any account on the machine.
- Guest cannot be chosen by a workspace or user config file: only by the
  person (screen, flag, environment). The keys under `auth.client` are
  managed-only.
- Audit: section 6.4. In EE, `guest: false` closes the door entirely.
- Switching principal mid-conversation is not possible: `/account` ends the
  conversation first, and every record event carries the principal kind.

### 7.6 Offline and air-gapped

- Machine accounts and guest need no network.
- Server sign-in: remembered tokens work offline until they expire; refresh
  needs the server. Under `require: "server"`, an expired token with the
  server unreachable means only guest (if allowed). Air-gapped servers use
  explicit OIDC endpoints (already supported by `Endpoints`).
- Nothing contacts a Zybuu service, in any mode.

---

## 8. Upgrade and migration

### What an existing user sees on first 1.3.0 start

- Interactive with 1.2.9 data: the migration screen (section 3.4).
- Interactive with no data: the choice screen.
- Scripts: unchanged until the first interactive start (section 5, step 4).

### CHANGELOG `[Unreleased]` (draft)

- **Added**: sign-in first. Every interactive start asks who is working: a
  local account on this machine, an account on an Abhed server, or guest.
  Guest keeps nothing. `abhed account` manages local accounts; `-guest`,
  `-account`, `ABHED_GUEST`, `ABHED_ACCOUNT`; exit code 10 when sign-in is
  required and none is held. Studio and ACP clients sign in through ACP
  `authenticate`.
- **Added**: managed and org-policy keys `auth.client.require`, `.server`,
  `.guest`, `.remember_days`.
- **Changed**: per-person state (records, history, memory, secrets, profiles,
  trust) now lives under `~/.abhed/accounts/<id>/`. The first account adopts
  existing data.

### Upgrading notes (draft)

1. Start `abhed` once in a terminal and create your account; your data
   moves into it, verified before and after.
2. Scripts keep working: before that first start they run as before; after
   it, they use your remembered sign-in. If you sign out, `-p` runs as guest with a note; pass `-account NAME` to fail
   with code 10 instead.
3. CI needs nothing; add `-guest` to silence the note.
4. To downgrade to 1.2.9, run `abhed account unmigrate` first.
5. Fleets: set `auth.client` in the managed file or the org policy.

---

## 9. Work items and tests

Community:

- C1 `internal/home`: machine and person roots; every state path moves to
  one of them; the state guard covers the new roots.
- C2 the identity gate (`internal/identity`): principal, resolution order,
  managed keys `auth.client.*`.
- C3 machine accounts: store in `~/.abhed/accounts`, register, sign-in,
  remembered token, recovery code, OS-owner reset, backoff, `abhed account`.
- C4 guest: ephemeral root, environment redirection, wipe on exit and
  signal, crash sweep, refusal of persistent features, reserved `guest`
  owner.
- C5 migration from 1.2.9 with a journal, chain verification before and
  after, and `unmigrate`.
- C6 the TUI choice screen, `/account`, the banner line; model onboarding
  after the gate.
- C7 the non-interactive rule, flags and environment, exit code 10, the
  result-line error, rpc `auth.*` methods.
- C8 ACP `authMethods`/`authenticate`, gating until sign-in,
  `_abhed/auth/openUrl`.
- C9 `serve`: first admin when interactive, the console's front door with
  guest (off by default), guest web sessions.
- C11 documentation, CHANGELOG and Upgrading notes.

Studio: the sign-in view, storage that keeps nothing for a guest, opening
the browser for server sign-in.

Team/Enterprise:

- E2 `require: "server"` on the machine: a live check at start, offline
  behaviour, `access_removed` handled by the gate.
- E3 SCIM effects: the local root kept and locked, optional
  `on_removed: "wipe"`.
- E4 guest audit: principal kind on events, OCSF owner `guest`, refusal
  when shipping is required with no credential, an administrator filter.
- E5 the end-to-end sign-in rig below.

### Test plan

Unit and package tests (each must fail without the change):

- Gate resolution table: every combination of flag, env, remembered token,
  legacy data, TTY, managed `require`/`guest`, giving the expected principal
  or exit 10.
- Accounts: create race (two processes, one wins), wrong password backoff,
  recovery code reset, OS-owner reset records the event and ends remembered
  tokens, link/mode refusals on the accounts file.
- Home roots: a table test over every state path asserting machine versus
  person root; a guard test that the agent's tools and each sandbox tier
  cannot read `accounts/` or another guest root.
- Guest: nothing remains under `~/.abhed` after exit, after SIGTERM, and
  after `kill -9` plus next start (sweep); refusals of `-continue`, `mcp
  login`, `secret set`; guest cannot see an account's trust, grants or
  secrets; guest under managed `guest:false` exits 10.
- Migration: a 1.2.9 fixture home (records with chains, history, memory,
  secrets, profiles) migrates, chains verify with the same heads, old
  sessions resume under the account; crash at each journal step resumes;
  `unmigrate` round-trips.
- Non-interactive: `-p` with no TTY never reads stdin for sign-in (a test
  with a stdin that blocks forever must finish).
- ACP: `session/new` before `authenticate` fails with auth-required;
  password never appears in the record, logs or replies (grep test).

End to end (through the eval gate, CE then EE, CLI and Studio, twice each):

- CE: register, quit, restart (remembered), sign out, sign in, guest run,
  1.2.9 upgrade on a copy of a real home. Never on a real
  `~/.abhed`; a throwaway `HOME` per run.
- SSO: a throwaway **dex** container (small, starts in seconds, supports
  device code and PKCE, static users and groups) for every EE CI run, on an
  engine-assigned port, removed with `podman rm -f -v`. A **Keycloak**
  container for a fuller manual/nightly run: groups, `require_group`, a
  public CLI client and a confidential web client, token expiry, logout.
  Google and Microsoft presets (Team): one manual smoke each with test
  OAuth clients registered for the purpose, outside CI.
- SCIM: the Team edition's SCIM test suite drives the EE server as an IdP
  would (dex and Keycloak do not push SCIM); deprovision a user who is
  signed in on a test laptop home and check: next start says access removed,
  remembered sign-in gone, local root kept and locked, guest still offered
  unless `guest:false`.
- Red team: try to reach the accounts file, another guest root and the
  remembered token from the agent on each sandbox tier; try to select guest
  from workspace config; try to keep data across a guest crash; try to run
  under `require` with an expired token offline.

---

## Questions that were open

All five were decided on 17 Nov 2026; see Decisions at the top.
