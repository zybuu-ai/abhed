# Abhed — Clusters, Machines, and External Retrieval

Four capabilities that let Abhed work on infrastructure rather than only on
files. All are **off by default**: reaching a cluster, a VM, or a corpus is an
authorization decision, and credentials already sitting on the machine are not
a reason to hand them to an agent unasked.

## Kubernetes

```json
{
  "k8s": {
    "enabled": true,
    "context": "prod-eu",
    "namespace": "payments",
    "allow_writes": false
  }
}
```

Two tools, deliberately not one:

| Tool | Approval | What it does |
|---|---|---|
| `k8s_get` | never prompts | list, describe, pod logs |
| `k8s_apply` | **always prompts** | apply, delete, scale, restart |

Splitting them is what makes the safety real. Abhed's permission engine decides
by tool name; a single tool with a `verb` argument would force it to parse an
opaque string to tell `list pods` from `delete namespace`. As separate tools the
read path is genuinely non-mutating, and every write goes through approval by
construction rather than by correctly interpreting text.

`allow_writes` decides whether `k8s_apply` exists at all. Even with it on, there
is no permission mode that auto-approves a cluster write: a mistaken delete in
production is not recoverable the way a file edit is.

### Logging in during a conversation

A cluster token expires, and a stale one in a kubeconfig produces a 401 that
reads like a permissions problem. `k8s_login` takes a fresh one, but only for
a cluster the operator declared:

```json
{
  "k8s": {
    "enabled": true,
    "ca_file": "/etc/abhed/cluster-ca.pem",
    "clusters": [
      { "name": "prod", "server": "https://api.prod.example.com:6443" },
      { "name": "lab",  "server": "https://api.lab.example.com:6443",
        "ca_file": "/etc/abhed/lab-ca.pem" }
    ]
  }
}
```

Store the token on the machine Abhed runs on, allow it, and tell the agent
the cluster and the secret's name:

```
abhed secret set OCP_TOKEN         # paste the sha256~... token at the prompt
```

```json
"allow": ["secret(OCP_TOKEN)"]
```

```
log in to prod with OCP_TOKEN
```

The agent calls `k8s_login` with `cluster: "prod"` and
`token_secret: "OCP_TOKEN"`. The approval prompt, and the record's
`action.requested` (its `reason` and `target`), say where the token goes:
cluster `prod` at its server, and how its certificate is checked. Abhed reads
the token from the store, checks it against the cluster, and holds it
**for that session only**. It is never written to your kubeconfig.

**The token goes only to a declared cluster, over verified TLS.** The model
names a cluster; it never supplies a URL. A name not in `k8s.clusters`, or a
URL, is refused before the secret is read or any request is made, and with no
clusters declared `k8s_login` reaches nothing. A server the model chose could
be anyone's, and a person approving what reads as a login would not notice.

The server's certificate is verified against the system roots plus the
cluster's `ca_file`, or `k8s.ca_file` when it names none. The server must be
`https://`. For a lab cluster with no usable certificate,
`"insecure_skip_tls_verify": true` on that cluster turns verification off:
whoever answers at that address, or in the path to it, gets the token. It is
config only, never an argument, and Abhed names such a cluster on stderr at
start, in `abhed doctor`, in the `abhed serve` banner, and in each approval
prompt (`TLS NOT VERIFIED`). Clusters from a workspace's `.abhed/config.json`
apply only once that file is trusted.

The token is not an argument, and should not be pasted into the chat. An
argument is judged, shown for approval, recorded, and sent back to the model
on every turn; the record is append-only, so a token that reached it could
not be taken out. A `token` argument sent anyway is dropped, and a value in
`token_secret` that is not a secret's name is recorded as
`[withheld: not a secret name]`.

A login belongs to the conversation that made it, and to that
conversation's subagents, in the console as in the terminal. Another
session on the same server, another user's included, keeps using the
operator's kubeconfig. Log in again in a new session, after a server
restart, and in the terminal after `/clear`, `/resume` or a fork: each
starts a conversation with no logins and no connected hosts, and the last
one's are closed. An embedded agent is one conversation: its logins and
hosts are closed by the SDK's `Close` and `Fork`, by a new `start` on `abhed
rpc`, and when an `abhed acp` connection ends. A resumed or forked
conversation's `k8s_get` and `k8s_apply` that name no `cluster` fall back
to the operator's kubeconfig until it logs in again, even where its
history shows a login. The secret itself is the operator's:
on `abhed serve`, users ask the operator to store one. A `secret(NAME)` rule
decides whether sessions on this deployment may use it, not which user may:
any session there can name a secret the rules allow. What stays per session
is the login made with it.

After logging in, name the cluster on each call: `k8s_get` and `k8s_apply`
take `cluster`, a declared cluster this session logged in to. With a single
login and no `cluster` or `context`, that login is used, and the call is
recorded as naming it; with several, the call must name one. A kubeconfig `context` always uses the kubeconfig's own
credential: a login token is never put on a kubeconfig client, whose TLS
settings and exec credential are not the ones the login was approved with.
A session's logins close their connections when it is deleted.

The approval for a `k8s_apply` write names the cluster and server it changes
and whose credential it uses: this session's login, with how TLS is checked,
or a kubeconfig context and the kubeconfig's own credential.

### Rules on a cluster

Permission rules and "always allow" read these tools by where the call goes.
`k8s_login` reads as the cluster's name; `k8s_get` as
`cluster/namespace/resource` and `k8s_apply` as `cluster/namespace/action`.
A kubeconfig context reads as `context:NAME`, and the current context as
`context:`.

Before a rule reads a `k8s_get` or `k8s_apply` call, Abhed puts it in the
form it runs in, and records it that way:

- a call that names no cluster while the session has one login names that
  cluster;
- the resource is lower-cased and its singular or short form made the
  plural it addresses: `Secret`, `secret` and `secrets` all read `secrets`;
- a call that names no namespace gets the one it would use: the login's or
  the kubeconfig context's default, or for an `apply` the namespace its
  manifest names. An `apply` takes one object; a list is refused.

A cluster-scoped object has no namespace: deleting a namespace or a node,
reading nodes, or applying a ClusterRole, ClusterRoleBinding, CRD, webhook
configuration or another cluster-scoped kind reads as `cluster/-/...`,
whatever namespace the call names, and any it names is dropped. So
`k8s_apply(lab/dev/*)` never covers them; allow them on their own with
`k8s_apply(lab/-/restart)` or the like, or leave them to ask. An `apply` of a
kind whose scope Abhed does not know, a custom resource's included, is
refused; apply it with `kubectl` through `bash`.

A namespace that is not a namespace name or `*`, and a name, kind or
apiVersion that could not stand as one segment of the request path, is
refused at step `args` before any rule reads the call.
`action.requested` lists the arguments Abhed set this way in `resolved`. A
call on every namespace, `namespace: "*"`, is matched by a deny or ask rule
that would match any namespace, so `k8s_get(*/kube-system/secrets)` also
stops `k8s_get` of secrets in `*`.

```json
"deny":  ["k8s_login(prod)", "k8s_apply(prod/*)", "k8s_get(*/kube-system/secrets)"],
"allow": ["k8s_login(lab)", "k8s_apply(lab/*/restart)"]
```

"Always allow" on a login is offered as `k8s_login(lab)`, and on a write as
`k8s_apply(lab/demo/delete)`, so a choice made for one cluster never
approves a call to another. Allow rules must name the cluster first. Deny and
ask rules written in the form each tool had before still apply: on the
namespace for `k8s_login`, on the resource for `k8s_get`, such as
`k8s_get(secrets*)`, and on the action for `k8s_apply`. A cluster's name
cannot hold `/`, `:`, `*` or `?`.

A rule on a declared cluster's name does not cover a kubeconfig context that
reaches the same server: that call reads as `context:NAME` and uses the
kubeconfig's own credential. To keep the agent off a server both ways, deny
both, or deny every context:

```json
"deny": ["k8s_get(prod/*)", "k8s_apply(prod/*)", "k8s_get(context:*)", "k8s_apply(context:*)"]
```

**Do not expect `oc login` through bash to work.** Three separate things stop
it, and the combination produced a confusing failure in practice:

1. The sandbox denies reads of `~/.kube`, so `oc` cannot read or write the
   kubeconfig at all.
2. Each bash call is a fresh sandboxed process, so a login inside one would not
   survive to the next.
3. Approving the command approves *running* it — the sandbox denial is a
   separate layer that approval does not lift.

`bash` now says so when a command fails on a denied credential path, and names
the tool to use instead, rather than leaving the agent to conclude the file is
simply unreadable.

For a non-interactive deployment, `ABHED_K8S_TOKEN` overrides the kubeconfig
credential at startup.

**Credentials come from your kubeconfig, never from the model.** The agent picks
a cluster only by naming a context you already have, so the worst it can reach
is what your own `kubectl` can. Token, tokenFile, client certificates and `exec`
credential helpers (the cloud CLIs) all work; exec tokens are refreshed before
they expire, because an expired token returns a 401 that reads like a
permissions problem. An `exec` helper runs only when Abhed sends a request:
never while a `k8s_apply` waits for approval, and never for a call that is
denied or refused in plan mode.

Abhed talks to the API directly rather than importing `client-go`, which would
add roughly a hundred transitive dependencies to a bundle where each one is
something an operator has to accept.

## SSH

```json
{
  "ssh": {
    "enabled": true,
    "hosts": [
      { "name": "build1", "addr": "10.0.0.5", "user": "ci",
        "identity_file": "~/.ssh/id_ed25519" },
      { "name": "db1", "addr": "db.internal:2222", "user": "ops",
        "password_env": "DB1_PASSWORD" }
    ]
  }
}
```

### Connecting to a machine during a conversation

Hosts do not have to be in config. When the user gives an address and a key:

```
connect to 52.116.120.159, key is at ~/Downloads/id_rsa
```

the agent calls `ssh_connect`, which asks for approval once, verifies the
connection works, and registers the host **for that conversation only**.
Nothing is written to `~/.ssh/config`. Another session on the same server
cannot run on it or see its name, and in the terminal `/clear`, `/resume` and
a fork close it; connect again there. A name an `ssh.hosts` entry uses, in any case, cannot
be taken, and names are plain ASCII. That does not stop every look-alike:
`pr0d` or `buiId1` still pass beside `prod` and `build1`. The approval for
each `ssh` command names the account and address it runs on, as
`runs as user@addr`, and says whether the host was declared by the operator
or added in this session; that address, not the name, is what to check.

For a host with a password rather than a key, store the password with
`abhed secret set VM_PASSWORD`, allow `secret(VM_PASSWORD)`, and name it:
the agent passes `password_secret: "VM_PASSWORD"`, never the password. A
password goes only to a host whose key is already in `~/.ssh/known_hosts`:
with `accept_host_key` the call is refused, since whoever answered at the
address would receive it. A key file needs no pinned host, because key
authentication signs and reveals nothing.
`password_env` is for `ssh.hosts` only: from the model, it could name any
variable in Abhed's environment, provider keys included, and send it to a
host the model chose.

`ssh.enabled` is all that is required — the `hosts` list is optional. Requiring
a pre-declared host to reach the tool that declares hosts was a real bug: a user
with a VM and a key had no way in, and the agent fell back to `ssh` through
bash, where the sandbox denies the key read.

The key path is resolved from what the user typed. `~Dowloads/key (1).prv` —
missing slash, misspelled directory, space in the name — resolves correctly,
because a path pasted into a chat is approximate and sending the agent hunting
with `glob` through denied directories is worse than trying the obvious places.

A host whose key is not in `known_hosts` is refused, and the refusal says to
retry with `accept_host_key: true` **only if the user has said the host is new
or ephemeral**. That is a deliberate line: the agent should not decide on its
own to stop verifying the identity of a machine it is about to run commands on.

**Every remote command requires approval — there is no read-only classification.**
A local `bash` call can be judged by its text because it runs inside a sandbox
with a workspace boundary and a checkpoint behind it. None of that holds over
SSH: the command runs with the remote account's full authority, outside any
scoping, with no undo. Calling `cat` safe there would be judging the string
rather than the consequence.

The agent can name a host from this list, or one the user gave `ssh_connect`
in the same session with approval. It cannot introduce one on its own.

Credentials, in order of preference: the **SSH agent** (the key never leaves
it), then `identity_file`, then `password_env` — which names an environment
variable, never the password itself. A passphrase-protected key says so and
tells you to `ssh-add` it, rather than failing with a parse error.

Host keys are verified against `known_hosts`. `insecure_skip_host_key_check`
exists because ephemeral lab VMs have no stable key and refusing would push
people to run `ssh` through bash where Abhed sees nothing — but it is off by
default, and `abhed doctor` marks any host using it.

## External retrieval (RAG)

```json
{
  "rag": { "corpora": [
    { "name": "runbooks", "enabled": true,
      "description": "Operational runbooks and incident history.",
      "url": "https://rag.internal/v1/search",
      "headers_env": { "Authorization": "RAG_TOKEN" },
      "results_path": "results" }
  ]}
}
```

Each corpus becomes a `rag_<name>` tool: namespaced so none can shadow a native
tool, read-only so it never prompts.

There are no per-vendor clients. Every retrieval API is the same shape
underneath and differs only in field names, so dotted paths map any of them:

```json
{ "results_path": "hits.hits", "text_field": "_source.body",
  "title_field": "_source.title", "score_field": "_score" }
```

That is Elasticsearch. With **no** mapping at all, common field names
(`text`, `content`, `passage`, `source`, `title`, `score`) are inferred, so a
simple endpoint needs no configuration.

For a GET-style endpoint use `query_param` instead of `query_field`. For fixed
parameters — an index name, a collection — use `body`.

**Retrieved passages are untrusted.** An internal wiki page is not more
trustworthy than the internet just because it sits behind a firewall: whoever
wrote the indexed document chose its words. Passages are tagged untrusted like
any other tool output and must never be followed as instructions.

## Remote MCP servers

```json
{
  "mcp": { "servers": [
    { "name": "corpus", "enabled": true,
      "url": "https://retrieval.internal/mcp",
      "headers_env": { "Authorization": "CORPUS_TOKEN" } }
  ]}
}
```

`url` reaches a server that already runs; `command` spawns one locally. Exactly
one of the two — a server is either spawned or reached, and leaving which one
wins to chance is a configuration trap.

Both wire shapes are supported, because a deployment does not get to choose
which one its vendor implemented: **Streamable HTTP** (one endpoint, POST, reply
as JSON or SSE by content type) and the older **HTTP+SSE** (a long-lived GET
carrying replies, with a separate POST endpoint announced by an `endpoint`
event). Abhed probes for the legacy shape and falls back, rather than making you
declare it.

## Verifying

`abhed doctor` reports every one of these, resolves the kubeconfig context, and
names any SSH host with host key checking disabled:

```
kubernetes  reads + writes (every write needs approval)
            context prod-eu
            namespace payments · server https://api.prod.example:6443
ssh host    build1 → ci@10.0.0.5
rag corpus  runbooks → https://rag.internal/v1/search
```
