# Abhed — Enabling Authentication

Three modes in the Community Edition. Pick by how Abhed is exposed, not by how
much security sounds good.

| Mode | Who it is for | Identity comes from |
|---|---|---|
| `none` | Local development, single user | Nobody — everything is "anonymous/default" |
| `local` | A team with no identity provider | A username and password Abhed holds |
| `proxy` | Behind an authenticating reverse proxy | `X-Abhed-User` / `X-Abhed-Tenant` headers |

**Which edition has what.**

| | Community | Team | Enterprise |
|---|---|---|---|
| No sign-in, local accounts, or an authenticating proxy | yes | yes | yes |
| Change your own password at `/account` | yes | yes | yes |
| OIDC single sign-on (Keycloak, Okta, Entra ID, Auth0, Google and other OIDC providers) and API bearer tokens from the same provider | — | yes | yes |
| Invites, an admin page, access requests and grants with a recorded reason | — | yes | yes |
| Tenant mapping from the identity provider | — | — | yes |

The paid editions are built on this module and document their own setup.
SAML is not built in to any edition: put a SAML-speaking proxy in front and
use `proxy` mode. Users of the Community Edition are managed from the command
line (`abhed user add|list|passwd|remove`).

**Headers are not trusted unless you ask for it.** In `none` mode a caller cannot
choose its own tenant by setting a header — there is a test asserting exactly that.
`proxy` mode is safe only when the proxy is the *sole* route to the port.

## Local accounts (no identity provider)

The mode for a pilot, an air-gapped enclave, or a team standing Abhed up before
central IT is involved.

```json
{
  "auth": {
    "mode": "local",
    "session_hours": 12,
    "cookie_secure": true,
    "allow_signup": false
  },
  "storage": { "driver": "postgres", "dsn": "postgres://..." }
}
```

Create the first account from the CLI:

```bash
abhed -C /srv/abhed user add alice -email alice@corp.internal -name "Alice"
# generated password: 7Kq2mVx9pLd4  (change it after first sign-in)
```

Then open the server in a browser and sign in with it.

| Command | Does |
|---|---|
| `abhed user add <name>` | Create an account. `-password` sets one; omitted, one is generated. `-admin` puts it in the admin group |
| `abhed user list` | Show accounts, emails, tenants and groups |
| `abhed user passwd <name>` | Reset a forgotten password to a new generated one |
| `abhed user remove <name>` | Delete an account |

### Where accounts live

With `storage.driver: postgres`, accounts are a table in the same database as
the event store, which is what a multi-node deployment needs. Without it, they
go to `<workspace>/.abhed/users.json`, mode `0600`, written atomically.

The file store exists because the alternative was silently broken: an in-memory
store meant `abhed user add` created an account inside a CLI process that then
exited, reported success, and left the user unable to sign in.

### Moving accounts to Postgres

Switching `storage.driver` from `memory` to `postgres` does **not** carry
existing accounts across — Postgres simply starts empty, with no error. Import
them:

```bash
export ABHED_DATABASE_URL='postgres://abhed_runtime:...@db.internal:5432/abhed'
abhed -C /srv/abhed user import
```

It never overwrites an account that already exists in the target, so a re-run
is safe, and it leaves `users.json` in place: that file is the only copy of
those hashes until you have confirmed sign-in works. Delete it yourself
afterwards.

### Self-registration

`allow_signup` is **off by default**. On an internal tool, open registration is
a way in for anyone who can reach the port, not a convenience. Turn it on and
the sign-in card grows a "Create one" link; leave it off and the card says
accounts are created by an administrator, which is true and actionable.

### Password handling

- bcrypt at the library default cost. Sign-in happens once per session, so a few
  hundred milliseconds is invisible to a person and expensive to an attacker
  holding the hash file.
- Minimum ten characters, enforced on the server. The browser checks too, but
  only to save a round trip.
- A wrong password and an unknown username return the *same* error and take the
  *same* time — a missing user is still run through bcrypt against a dummy hash.
  Without that, response timing enumerates valid usernames. There is a test.
- `User.Hash` is tagged `json:"-"`, so a hash cannot fall out of an HTTP
  response. The on-disk store uses its own type to persist it, rather than
  relaxing that tag.
- A password set by an administrator (`user add`, `user passwd`) is flagged
  `must_change_password`, and until the user sets their own at `/account` the
  session reaches nothing else: only `/account`, `POST /v1/password`,
  `/v1/whoami`, sign-out and static files. A browser is sent to `/account`
  with a note; an API call gets `403 {"error":"password change required"}`.

### Administrators

`POST /v1/admin/users/admin` grants or removes the admin group. It refuses an
administrator removing their own rights, and refuses removing the last
administrator whoever asks, since nothing on the deployment could grant it
back. Every change under `/v1/admin/*` is written to the server log as
`admin action`, with the action, the target and who made it. An MCP server's
URL is recorded as scheme, host and path only, and its command as the program
alone, since either can carry a credential.

The last-administrator rule holds within one server process. Two nodes
sharing a Postgres account store can each remove the other's last
co-administrator at the same moment. If that happens, create a new
administrator from the command line with `abhed user add <new-name> -admin`.

Must-change is enforced on the node that holds the session. A reset reaches
the live sessions on the node that made it; on another node a session already
open stays unconfined until it signs in again.

## Behind a reverse proxy

```json
{ "auth": { "mode": "proxy" } }
```

The proxy authenticates the person and sets headers on every request it
forwards:

| Header | Carries | If absent |
|---|---|---|
| `X-Abhed-User` | the subject | `anonymous` |
| `X-Abhed-Email` | the email address | empty |
| `X-Abhed-Tenant` | the tenant | `default` |
| `X-Abhed-Groups` | comma-separated groups (the admin group among them, if any) | none |

A request with no `X-Abhed-User` is `anonymous`, and `anonymous` owns every
session in its tenant, now including each workbench shell: the proxy must set
the header on every request.

Abhed does no verification of its own in this mode, so the proxy must be the
only route to the port: bind Abhed to loopback or a private interface and let
nothing else reach it. Anything that can reach the port directly can claim any
identity by setting the headers itself.

`/v1/whoami` reports the identity the proxy supplied (`"auth_mode": "proxy"`,
the subject and groups), and the console and workbench show it. The proxy owns
sign-in, so there is no Switch link, and Sign out appears only when
`auth.proxy_logout_url` names the proxy's own sign-out; `/logout` then
redirects there:

```json
{ "auth": { "mode": "proxy", "proxy_logout_url": "/oauth2/sign_out" } }
```

## Allowed origins

The server refuses a state-changing request whose `Origin` matches neither its
own host nor `server.allowed_origins`. A proxy that rewrites `Host` therefore
needs the public origin listed, or every sign-in and every console action
answers `403 cross-origin request rejected`:

```json
{ "server": { "allowed_origins": ["https://abhed.internal"] } }
```

## When authentication is off

With `auth.mode: none` there are no user accounts, so `/login` and `/logout`
return a page explaining that and showing the config to enable sign-in — rather
than a 404, which reads as a fault. `/v1/whoami` answers
`{"authenticated": false, "reason": "..."}`, and the console removes the user
chip entirely rather than hiding it: a Sign out link that leads nowhere is worse
than no link.

## API clients

The Community Edition issues no API tokens. A script signs in with a
password and sends the session cookie back:

```bash
curl -c jar -H 'Content-Type: application/json' \
  -d '{"username":"ci","password":"..."}' https://abhed.internal/v1/signin
curl -b jar https://abhed.internal/v1/sessions
```

Behind `proxy` mode the proxy authenticates the script. The paid editions
also accept a bearer token from their OIDC provider; a session cookie, when
one is sent, is checked first.

A browser navigation with no session is redirected to sign in; an API call with
no token gets `401` with a reason. That distinction is `Accept: text/html`.

## Hooks for an edition built on this module

All of these are unset in the Community Edition, which behaves as described
above without them.

| Hook | Called | Effect of an error |
|---|---|---|
| `auth.LocalAuth.Admit(ctx, *User) error` | at sign-in, after the password checks out, before a session is issued | `403` with the error text; no session |
| `auth.Middleware.Check(ctx, *Identity) error` | on every request a provider session, a bearer token or a trusted proxy identifies, and in `/v1/whoami` and `/v1/overview` | the session is ended; a browser navigation goes to `/?refused=<reason>`, an API call gets `403 {"error":"forbidden","reason":…}`; whoami answers `authenticated: false` with the reason |
| `server.Options.AdminAudit(ctx, action, target, detail)` | after each `/v1/admin/*` change: `user.admin_granted`, `user.admin_revoked`, `skills.reloaded`, `mcp.added`, `index.rebuild_started` | none; it is told, and the server log line is written either way |

Notes for an edition setting them:

- **The reason is shown to the person.** Admit's and Check's error text
  appears in the sign-in form, in whoami, and on the front door after "Access
  refused:", cut to 200 characters. Write it for them; never pass through an
  internal error such as a database message.
- **Admit covers local accounts only.** A single sign-on session is issued at
  its callback, which is public, and is ended by Check on its first checked
  request. Refuse at the provider's own callback where it matters.
- **Check runs when a request arrives.** A stream already open (session
  events, a terminal) is not cut when access is withdrawn; it is refused when
  it reconnects.
- **AdminAudit may run while the admin-rights lock is held**, so it must not
  call an admin route itself.

`auth.LocalAuth.Sessions()` lists live local sessions — a digest of the
cookie as the ID, never the cookie, with the user, when it was created, last
seen and expires — and `EndSession(id)` ends one by that digest.

## Tenant

The Community Edition server is single-tenant. When `storage.driver` is
`postgres`, every session is written under `storage.tenant`, and Postgres
enforces that scope with row-level security. If an identity arrives carrying a
different tenant (a `proxy` header, for instance), the first write fails RLS;
Abhed reports the mismatch by name rather than passing Postgres's opaque error
through.

## Verifying

```bash
abhed doctor      # reports the auth mode in effect, alongside storage
```

The server fails at startup rather than on a user's first request, so a bad
storage DSN or an unreadable account file surfaces during deployment.
