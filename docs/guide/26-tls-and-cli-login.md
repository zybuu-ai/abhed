# TLS and command-line sign-in

`abhed serve` speaks plain HTTP unless told otherwise, and is normally put
behind a reverse proxy that terminates TLS. It can also terminate TLS itself,
ask for client certificates, and sign a command line in with
`abhed login`. All of this is in the Community Edition.

| You want | Set |
|---|---|
| HTTPS without a proxy | `server.tls.cert_file`, `server.tls.key_file` |
| An RSA certificate beside an ECDSA one, or the next certificate during a rotation | `server.tls.additional_certificates` |
| Only clients with a certificate from your CA | `server.tls.client_ca_file` |
| A client certificate to sign a local account in | `server.tls.client_cert_accounts` |
| `abhed login` for local accounts | `auth.cli.enabled` |
| A CA and certificates, if you have none | `abhed pki` |

Single sign-on from the command line (an identity provider's device flow) is
part of the Team edition, which adds its own method to the same `abhed login`.

## A private CA: `abhed pki`

For a deployment with no PKI of its own:

```bash
abhed pki init                                    # ~/.abhed/pki/ca.pem and ca-key.pem
abhed pki server -dns abhed.internal -ip 10.0.0.5 # abhed.internal.pem and -key.pem
abhed pki client -user ada                        # client-ada.pem and -key.pem, for account ada
```

Keys are ECDSA P-256 (`-rsa` gives RSA 3072), written mode 600 in a
directory only you can write, and never printed: the command prints the
paths, the subject, the expiry and the SHA-256 fingerprint. Nothing is
overwritten without `-force`. The CA signs leaf certificates only, never
another CA. A server certificate lasts 397 days (what browsers accept), a
client certificate a year, the CA five years; `-days` changes that, and no
leaf outlives its CA. `-dir` puts the files elsewhere; the default,
`~/.abhed/pki`, is a folder the agent's commands cannot reach.

Give each person their own client certificate and key, and nobody the CA key.

## Serving TLS

```json
{
  "server": {
    "tls": {
      "cert_file": "/etc/abhed/tls/abhed.internal.pem",
      "key_file": "/etc/abhed/tls/abhed.internal-key.pem"
    }
  }
}
```

The certificate file holds the server certificate followed by any
intermediates. With any `server.tls` setting made, serve listens with TLS on
its `-addr` (the port does not change) and the banner shows `https://`.

**It fails closed.** A certificate or key that cannot be read, a key that does
not match, a certificate that has expired or is not yet valid, a CA
certificate in place of a server one, or a client CA file holding no
certificate stops serve at startup with the reason. It never falls back to
plain HTTP. A key file other users can read is allowed, since a container
secret is often mounted that way, and named in a startup warning, as is a
certificate within 14 days of expiry.

**Keys are state.** The TLS keys and the client CA file, like the accounts
file and the secrets store, are refused to the agent's tools and sandbox, and serve refuses to
start with one inside the workspace, an added directory, a temporary folder
or a cache, where a command could read or replace it. Keep them under
`/etc/abhed`, `~/.abhed`, a mounted secret, or anywhere commands cannot write.

**Versions and suites.** TLS 1.2 and 1.3. TLS 1.2 offers only forward-secret
AEAD suites (ECDHE with AES-GCM or ChaCha20-Poly1305). Set `"min_version":
"1.3"` to refuse TLS 1.2. HTTP/2 is offered. Session cookies are sent
`Secure` whenever serve terminates TLS.

**Reloading.** The files are read again every `reload_seconds` (10 by
default). A change that loads is served from the next connection, without a
restart and without dropping open connections; it is logged with each
certificate's subject and expiry. A change that does not load (a file caught
half-written during a rotation, a key that does not match) is logged once and
the certificates already in use stay in service. SIGHUP does not reload; it
stops the server, as before.

**Two certificates.** List more in `additional_certificates`:

```json
"tls": {
  "cert_file": "/etc/abhed/tls/ecdsa.pem", "key_file": "/etc/abhed/tls/ecdsa-key.pem",
  "additional_certificates": [
    { "cert_file": "/etc/abhed/tls/rsa.pem", "key_file": "/etc/abhed/tls/rsa-key.pem" }
  ]
}
```

Each connection gets the first certificate that suits the name the client
asked for (SNI) and the signatures it can check, so an ECDSA and an RSA
certificate serve old and new clients side by side, and two names can be
served from one port. During a rotation, list the new certificate first and
the old one after, then remove the old one once clients have moved. With no
match the first certificate is served, so the client reports a name mismatch.

Relative paths are taken from the workspace, as `auth.users_file` is.

## Client certificates

```json
"tls": {
  "cert_file": "...", "key_file": "...",
  "client_ca_file": "/etc/abhed/tls/clients-ca.pem",
  "client_auth": "require"
}
```

With `client_ca_file` set, a client must present a certificate issued by that
CA, or the handshake fails before any HTTP is spoken. `"client_auth":
"optional"` lets a client without a certificate through to the other ways of
signing in, while a certificate that is offered must still verify. The client
CA file is reloaded like the certificates: replacing it refuses every
certificate the old CA issued on new connections (a connection already open
keeps the identity it was made with until it closes).

**Signing in with a certificate.** With local accounts
(`auth.mode: "local"`) and `"client_cert_accounts": true`, a verified client
certificate whose common name is a username signs that account in, with no
password. It owns the same sessions as a password sign-in. A browser that
also holds a session cookie is the cookie's account. A certificate for a name
with no account signs nobody in. The account is read again at least every two
seconds, so removing it ends the certificate's access, and the
`LocalAuth.Admit` hook of an edition is asked as at a password sign-in.

A certificate cannot be signed out. To end one, revoke it, remove the
account, or issue the client certificates from a new CA and replace
`client_ca_file`. A browser sends a client certificate on every request to
the site, so the same origin checks guard state-changing requests as for a
session cookie.

**Revoking a certificate.** `client_revoked_file` names the client
certificates refused although they verify:

```json
"tls": { "client_ca_file": "/etc/abhed/tls/clients-ca.pem",
         "client_revoked_file": "/etc/abhed/tls/clients-revoked" }
```

The file holds either PEM CRLs (`-----BEGIN X509 CRL-----`), each signed by a
certificate in `client_ca_file`, or serial numbers in hex, one to a line
(colons, a `0x` and openssl's `serial=` are allowed, `#` starts a
comment). A listed serial is
refused whichever CA in `client_ca_file` issued it. The file is reloaded like
the CA: a revoked certificate is refused on the next handshake, and a request
on a connection made with it before the change gets `401`. A file that is
missing or unreadable, a line that is not a serial, or a CRL no certificate
in the CA file signed stops `abhed serve` at startup; on a reload it is
reported and the revocations already in force stay. A CRL past its next
update still applies, and the banner warns that newer revocations are not in
it. `abhed pki` does not write CRLs; the serial list is the way to revoke a
certificate it issued (`openssl x509 -noout -serial -in alice.pem`). Keep the
file where commands cannot write it: like the CA, it is among the server's
private files that the agent's tools refuse.

## `abhed login`

```json
{ "auth": { "mode": "local", "cli": { "enabled": true } } }
```

Then, on the machine you work from:

```bash
abhed login https://abhed.internal
```

`abhed login` asks the server how it signs a command line in
(`GET /v1/cli`, public), then uses one of two OAuth flows. Either way you
approve the sign-in in a browser where you are already signed in to the
console, as the account the command line will act as.

- **Browser** (the default on a desktop): a browser opens on the server's
  approval page; approving sends a one-minute code back to a listener on
  `127.0.0.1` on your machine, which the command line exchanges with a proof
  only it holds (PKCE, S256). Use `-browser` to choose it.
- **Device code** (the default over SSH or without a display, or with
  `-device`): the command line prints a code such as `BCDF-GHJK` and a link;
  open `/cli/login` on any device, enter the code, check where the request came
  from and approve. The command line polls until you approve or deny, or the
  code expires after ten minutes.

The token it receives is saved in a profile. It lasts `auth.cli.token_hours`
(168, a week, by default; at most 2,160). It ends early when you run
`abhed logout`, when the account signs out everywhere, changes its password or
has it reset, or is removed. `abhed logout` is kept in a file beside
`auth.cli.key_file`; servers that share that folder see it on their next
request. An account whose password must be changed gets
no token until it is changed in the browser.

```bash
abhed login -profile prod https://abhed.internal   # name it; default: host and port
abhed login -ca ~/.abhed/pki/ca.pem https://abhed.internal  # a private CA
abhed login -cert me.pem -key me-key.pem https://abhed.internal  # a server that asks for a client certificate
abhed profile list                                 # * marks the current profile
abhed profile use prod
abhed logout                                       # ends the token on the server too
abhed profile rm staging
```

**Profiles** are kept in `~/.abhed/profiles.json`, or `$ABHED_PROFILES_FILE`:
the server URL, the account, the token and its expiry, and the paths of any
CA or client certificate. The file is written mode 600; it is refused if
other users can read it or if it is a link, and refused inside the workspace
(other than its `.abhed`), where the agent's commands could read it. The
CA, client certificate and key a profile names (`-ca`, `-cert`, `-key`) are
state too: the agent's commands and file tools cannot read them. Keep them
outside the workspace.

**A sign-out is never quietly lost.** `abhed logout` stops using the token
at once. If the server cannot be reached, or answers that it could not
record the sign-out (a `5xx`, such as `503` while its database is down), it
tries again three more times, waiting half a second, then twice as long each
time. If the server still has not recorded it, `abhed logout` says so and
exits non-zero, `abhed profile list` shows the profile as signed out here but
not yet on the server, and the token is kept in the profiles file under
`pending_logouts` and sent again at the start of every `abhed login`,
`logout` and `profile` command until the server records it. Tokens
are never printed. `abhed login`, `abhed logout`, `abhed profile use`,
`abhed profile rm` and `abhed sandbox` are refused when run by the agent.

**Plain http** is refused for any host but this machine, since the token
would cross the network in clear; `-allow-http` overrides that on a network
you trust. With `-ca`, only that CA is trusted for the server.

### On the server

| Endpoint | Before sign-in | What it does |
|---|---|---|
| `GET /v1/cli` | yes | auth mode and the sign-in methods offered |
| `POST /v1/cli/device` | yes | starts a device sign-in (RFC 8628) |
| `POST /v1/cli/token` | yes | collects a token: device code, or authorization code with PKCE (RFC 7636) |
| `GET /cli/login` | no | the approval page |
| `GET`, `POST /v1/cli/approve` | no | shows and answers a device code; JSON from a browser session only |
| `POST /v1/cli/authorize` | no | issues a PKCE code for the signed-in person; JSON from a browser session only |
| `POST /v1/cli/logout` | bearer | ends the token it is sent with |
| `GET /v1/cli/whoami` | no | who the request is signed in as, bearer tokens included |
| `GET /v1/whoami` | yes | the same for a bearer token (`"credential": "token"`, with `scopes` when the token was narrowed), and for a browser session; `401` for a token that does not verify, `403` for one an edition's `Authorize` refuses |

Approving accepts only `application/json` from a page of the same origin, so
another site cannot approve a sign-in with a form, and only from a browser
session of a local account, never with a bearer token. A PKCE code is sent
only to a loopback redirect with a port (`http://127.0.0.1:PORT/...`,
`[::1]` or `localhost`) and is bound to its challenge and redirect; both
kinds of code are single use. The start, token, approve and authorize
endpoints share a limit of 30 requests every 30 seconds per address, and polling
faster than the interval is answered `slow_down`. At most 1,024 sign-ins wait
at once.

The token is signed with a key in `auth.cli.key_file`
(`~/.abhed/cli-token.key` by default), created mode 600 at the first start
and refused if other users can read it; tokens ended early by `abhed logout`
are listed beside it in `<key_file>.revoked` until they would have expired.
Both are state the agent cannot reach. Sign-ins in progress are held in
memory on the server that started them, so behind several servers a sign-in
must reach the same one for its few minutes; for tokens to work on every
server, they share the key file and the account store.

A token is sent as `Authorization: Bearer abhcli_…` and is accepted by every
API route a session cookie is, except approving another sign-in;
`auth.require_group` applies to it. The `profiles` package holds the
profiles file and an HTTP client that sends a profile's token and TLS
settings, for commands that talk to a server.

### For an edition: adding a sign-in method

A `Provider` that also implements `auth.CLILoginAdvertiser` lists its own
methods in `GET /v1/cli` after the local one:

```go
type CLILoginAdvertiser interface {
	CLILoginMethods() []auth.CLILoginMethod
}
```

A method names a device authorization endpoint, an authorization endpoint,
a token endpoint, and optionally a revocation endpoint, a client id, scopes
and an issuer; endpoints may be absolute URLs on an identity provider (https
only). `abhed login -method NAME` uses it with the same device and PKCE
flows, sending standard form-encoded requests, and keeps a refresh token
when one is given. When the device page is on another host than the server,
the prompt says to open it and sign in there, to the identity provider. `Profile.Do` sends a request with the profile's token
and, when it holds a refresh token, refreshes it first if a tenth or less of
its life is left, and once more if the server answers `401`, then sends the
request again; `profiles.Refresh` does it by hand. The middleware's `Verifier` must accept the
tokens it leads to; `auth.ChainVerifier` joins a verifier to the command-line
token verifier, which recognises its own tokens by their prefix and passes
every other token on.

With `auth.mode: "oidc"` there are no local accounts, so `auth.cli.enabled`
is served by the edition's provider alone: serve refuses it unless a provider
advertises a method. `auth.cli.client_id` names the identity provider's client
for the command line (one that allows the device grant and loopback
redirects); the edition reads it. `auth.LoadCLIKey` reads or creates a token
key file with the same checks as `auth.cli.key_file`; an edition keeps state of
its own beside the key, in `<key_file>.edition`, which is refused to the agent
as the key is.

`auth.Middleware.Authorize`, when an edition sets it, runs on every request
after the caller is identified and `Check` has passed, with the request
itself, and again whenever a stream rechecks its caller. An error answers
`403` with its text and ends no session. It is for a credential that is good
for some requests only, such as a token scoped to one sandbox.

Public endpoints an edition serves under `/v1/cli/`, and `POST /v1/token`,
share the sign-in endpoints' rate limit.

## Settings

| Key | Default | |
|---|---|---|
| `server.tls.cert_file`, `key_file` | — | the server certificate chain and key; both or neither |
| `server.tls.additional_certificates` | — | `[{cert_file, key_file}]`, chosen by SNI and key type |
| `server.tls.min_version` | `"1.2"` | or `"1.3"` |
| `server.tls.client_ca_file` | — | ask for client certificates from this CA |
| `server.tls.client_auth` | `"require"` once a client CA is set | or `"optional"` |
| `server.tls.client_cert_accounts` | `false` | a client certificate's common name signs that local account in |
| `server.tls.reload_seconds` | `10` | how often the files are checked, 1 to 3600 |
| `auth.cli.enabled` | `false` | serve `abhed login` for local accounts |
| `auth.cli.token_hours` | `168` | 1 to 2160 |
| `auth.cli.key_file` | `~/.abhed/cli-token.key` | the token signing key |
| `auth.cli.client_id` | `auth.client_id` | with `auth.mode` `oidc`: the identity provider's client the command line signs in through (Team edition) |
| `auth.workload.*` | — | workload identity and token exchange (Enterprise edition); the Community Edition refuses `enabled` |
| `auth.workload.grants[].allow_reusable_svid` | `false` | accept that workload's JWT-SVIDs without a `jti` (Enterprise edition) |

`server` and `auth` are deployment settings: an untrusted workspace file that
sets them stops `abhed serve` rather than being ignored, and the managed
`/etc/abhed/config.json` overrides both.
