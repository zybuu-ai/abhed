# Signing in

Every start of Abhed answers one question before anything else runs: who
is working? The answer is one of three:

- **an account on this machine**: a username and password Abhed keeps on
  this computer, for this OS login;
- **an account on an Abhed server**: your own server, or your
  organisation's, signed in with `abhed login`;
- **guest**: nothing is kept when the run ends.

The answer decides whose sessions, history, memory, secrets, sign-ins and
trust decisions are in play. It does not change what the agent may do:
policy, the sandbox, the egress allowlist and approvals are the same for
all three. A guest is never more privileged than an account.

No Zybuu service is involved. An account is on this machine, or on an
Abhed server that you or your organisation runs.

## The first start

In a terminal with no one signed in, Abhed asks:

```
Abhed — who is working?

  1) Sign in            an account on this machine
  2) Create an account  on this machine; nothing leaves it
  3) Sign in to a server your own or your organisation's Abhed server
  4) Guest              nothing is kept when you quit
```

Once you sign in, this machine remembers it until you sign out, change the
password, or an administrator removes you. Each later start says who is
signed in, under the banner, and `/account` signs out or switches (it ends
the session; the next start asks again). `/status` shows it too.

Studio shows the same choice in its own window. The engine lists the ways
in, and Studio signs you in over ACP (see the Studio contract).

### Moving data from 1.2.x

If `~/.abhed` holds sessions, history or memory from an earlier version,
the first choice is **Create my account and bring my data**. Abhed makes
the account and moves those files into it by rename. It verifies every
session's chain before and after, and the heads must match; if they do not,
everything moves back and nothing is lost. A crash part way through
resumes at the next start. Sessions recorded before sign-in stay yours.

To do it later, create the account and run `abhed account adopt`. To go
back to an earlier version, run `abhed account unmigrate` first: it moves
everything back to `~/.abhed`.

## Accounts on this machine

```bash
abhed account create            # asks for a username and a password, shows a recovery code
abhed account signin            # signs in; remembered until signout
abhed account whoami
abhed account list              # * marks the one signed in
abhed account passwd            # ends every remembered sign-in of the account
abhed account reset NAME        # with the recovery code
abhed account reset NAME -as-os-user
abhed account signout
abhed account rm NAME [-delete-data]
```

Accounts are kept in `~/.abhed/accounts/accounts.json`, mode 0600, in a
folder only you can open, inside the state the agent's tools and sandbox
cannot reach. Passwords are bcrypt-hashed (cost 12) and need at least 10
characters. After three wrong passwords in a row each try waits, from 30
seconds up to 15 minutes. Each account's data lives in a folder of its own,
`~/.abhed/accounts/<id>/`.

**An account is a separate space, not encryption.** Anyone who can act as
your OS user can read these files, as they always could. Accounts keep two
people who share one OS login out of each other's sessions and memory at a
glance; for real separation, give each person their own OS account, or use
a server.

**Recovery with no server.** Keep the recovery code shown when you create
the account: `abhed account reset NAME` takes it and sets a new password
(and shows a new code). Without it, `abhed account reset NAME -as-os-user`
sets a new password after you confirm. Whoever is the OS user can already
read the account's files, so this is not a way around anything, and the
account's next sign-in says it happened, when and from where.

### What belongs to the account

| The account's own | The machine's, shared |
|---|---|
| the local record (`records/`), history, drafts | `~/.abhed/config.json`, your model settings |
| memory: `ABHED.md` and auto memory | the managed configuration and the org policy |
| `secrets.json`, MCP sign-ins | TLS keys, the CLI token key, `pki/` |
| `profiles.json`: your server sign-ins | logs, the configuration change log |
| trust decisions, Studio folder grants | toolchain and package caches |
| skills, agents, commands and styles of your own | |

## Guest

Choose **Guest**, or run with `-guest` or `ABHED_GUEST=1`. A guest works in a
folder of its own, `~/.abhed/guest/<random>`, which holds the session's
record, history and anything else the run would keep. It is removed as the
run ends, by an interrupt or a stop signal too. If the process is killed
outright or the machine loses power, the next start of Abhed removes it.
Removal is ordinary deletion; on an SSD it is not a secure erase.

A guest cannot resume a session, keep a secret, add an MCP server or sign
one in, sign in to a server, or change this machine's configuration. A
guest uses the machine's configuration and defaults, and never an
account's own trust decisions, grants or secrets. The machine's
configuration (`~/.abhed/config.json`) is shared: what any account sets
with `/config set` or `abhed mcp add` applies to every account and guest
on this OS login. Files the agent
changes in your workspace are the work itself, and stay.

Every session's `session.started` says who was working: `principal`, with
its `kind` (`account`, `server`, `guest` or `legacy`) and `owner`
(`device:<username>`, or `guest`), and `by_default` when no one chose it.
Where the managed configuration writes the record's OCSF copy
(`record.ocsf`), a guest's sessions are written there too, as the
organisation's, though nothing of them is kept in the guest's folder.

## Runs that cannot ask

`abhed -p`, `abhed rpc`, `abhed acp`, CI and scripts never wait for an
answer. In order:

1. `-guest` or `ABHED_GUEST=1`: guest.
2. `-account NAME` or `ABHED_ACCOUNT=NAME`: that account, if it is signed
   in on this machine; otherwise exit code **10**.
3. The remembered sign-in.
4. Data from an earlier version that no account has taken over: the run
   works exactly as it did before sign-in, with one line on stderr.
5. Otherwise: guest, with one line on stderr.

Where the organisation requires sign-in, steps 4 and 5 exit **10** instead,
and where it has turned guest off, so do 1 and 5. Exit code 10 means
"sign-in required"; the result line's error is `signin_required`, and
stderr says what to run.

`abhed rpc` and ACP clients can sign the person in over the protocol:
rpc's `auth.status`, `auth.signin`, `auth.register` and `auth.guest`, and
ACP's `authenticate`, before the first session. Where sign-in is required
they wait for that instead of exiting.

So a 1.2.x script keeps working: before your first interactive start it
runs as before, and after it, as your account. On a fresh CI machine it
runs as guest; add `-guest` to silence the note. If you sign out, `-p`
runs as guest; pass `-account NAME` to fail with code 10 instead.

## For organisations

The managed configuration (`/etc/abhed/config.json`) says who may work on
the machine. The signed org policy may set the same keys, but only to
tighten them.

```json
{ "auth": { "client": {
    "sign_in": "required",
    "guest": false,
    "remember_days": 30
} } }
```

| Key | Meaning |
|---|---|
| `auth.client.sign_in` | unset: sign-in is optional. `"required"`: an account on this machine or a server. `"server"`: a sign-in to `auth.client.server`, or to the org policy's server |
| `auth.client.server` | the one server whose sign-in counts under `"server"` (managed file only) |
| `auth.client.guest` | `false` removes guest everywhere |
| `auth.client.remember_days` | bounds a remembered sign-in; unset, it lasts until sign-out. `0` asks at every start |

None of these can be set in your own `~/.abhed/config.json` or a
workspace's.

Under `"server"` with no `auth.client.server`, the server is the one the
machine takes its org policy from. On such a machine `abhed login SERVER`
is how a person signs in, even though nothing else runs before it.

Where the managed configuration ships every session's record to the org
server (`record.ship`), guest is refused: a guest holds no sign-in to ship
it with.

When the organisation removes a person's access (an administrator's
revocation, or SCIM deprovisioning on a Team server), the next start that
can reach the server ends the remembered sign-in and says so; signing in
again does not restore it. The person's data on the machine is kept. A
server that cannot be reached does not end a sign-in: the machine may be
offline, and the session's `session.started` then says
`"verified": false`. A remembered server sign-in that holds no credential
(no profile, no token) or that the server refuses is not a sign-in: it is
forgotten, and where a sign-in is required the run exits 10.

Signing in through an identity provider (OIDC, including Google and
Microsoft, and GitHub), group roles, SCIM deprovisioning and requiring a
sign-in to the organisation's server are Team features, documented with the
Team and Enterprise edition. In the Community Edition, a server's accounts
are local usernames and passwords; see [TLS and command-line
sign-in](26-tls-and-cli-login.md).

## `abhed serve`

A server signs its own users in (`auth.mode`), as before. In a terminal,
`abhed serve` with no sign-in on an address other machines can reach now
asks first: set up local accounts (it shows how and serves nothing), serve
on this machine only, or serve as configured. Under a supervisor, with no
terminal, it serves as it always has.
