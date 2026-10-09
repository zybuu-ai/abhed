# Running on Kubernetes

The Helm chart in `deploy/helm/abhed` runs `abhed serve` on Kubernetes 1.30
or later, and on OpenShift. It needs Abhed 1.2.8 or later, the first release
with `GET /v1/ready` and `abhed serve -check`.

```bash
helm install abhed deploy/helm/abhed --namespace abhed --create-namespace \
  -f my-values.yaml
kubectl -n abhed rollout status deploy/abhed
```

Every value is checked against `values.schema.json`, so a misspelt key or a
value the chart cannot honour stops `helm install` before anything is
created. The chart never creates a Secret and never writes a secret into
what it renders: each one is a Secret you create, which the chart names.

## What it creates

| Object | Why |
|---|---|
| Deployment | One replica by default, running `abhed serve -addr 0.0.0.0:8080` from `/workspace`. |
| ConfigMap | Your `config` values as `config.json`, mounted read-only at the managed path `/etc/abhed/config.json`. |
| PersistentVolumeClaim | One volume with two folders: `workspace` at `/workspace`, and `state` at `~/.abhed` (accounts, keys, trust decisions, the SQLite store), which the agent cannot reach. Kept when the release is uninstalled. |
| Service | The server's port. |
| ServiceAccount | With no API token mounted. |
| NetworkPolicy | Ingress to the server's port only; egress denied except DNS, the endpoints you list, Postgres and the other replicas. |
| PodDisruptionBudget | `maxUnavailable: 1` by default, so a single replica does not block a node drain. |
| Ingress, Route | Only when turned on. |
| Namespaces per workspace | Only when turned on; see [below](#a-namespace-per-workspace). |

## Configuration

`config` is Abhed's configuration, written as YAML; the guide to every key is
[Configuration](02-configuration.md). It is mounted as the **managed file**,
so neither the workspace's `.abhed/config.json` nor an environment variable
can loosen it, `bypass` mode is refused, and a `permissions` setting there
drops allow rules from lower files ([the managed file](02-configuration.md#the-managed-file)).

```yaml
config:
  model:
    default: claude
    providers:
      claude:
        type: anthropic
        model: claude-opus-5
        api_key_env: ANTHROPIC_API_KEY
  permissions:
    mode: default
  sandbox:
    allow_network: false
secretEnv:
  - name: ANTHROPIC_API_KEY
    secretName: abhed-model
    key: api-key
networkPolicy:
  modelEndpoints:
    - cidr: 160.79.104.0/23
      ports: [443]
```

**No secrets in `config`.** The chart refuses to render when `config` holds
a key named `api_key`, `dsn`, `migrate_dsn`, `password`, `client_secret`,
`secret`, `token` or `private_key`. Name a variable instead (`api_key_env`,
`client_secret_env`) and fill it from a Secret with `secretEnv`.

**Changing `config`** rolls the pods: the Deployment carries a checksum of it.

**`podConfig`** is for settings that must differ per pod. An init container
writes it to `~/.abhed/config.json` before the server starts, and Kubernetes
expands `$(POD_NAME)`, `$(POD_IP)` and `$(POD_NAMESPACE)` in its strings. The
managed file still wins any key both set. With persistence on, it replaces
whatever `~/.abhed/config.json` the volume held.

## Files that must be private

Abhed refuses to read the secrets store, a command-line token key or a
replica relay key that anyone but its owner can read. A Secret volume
with `fsGroup` set is group-readable, so list such keys under
`secretFiles`: an init container copies each into an in-memory volume at
`/run/abhed/secrets/<path>`, mode `0400`, owned by the server's user, and the
server sees that volume read-only.

```yaml
secretFiles:
  - secretName: abhed-secrets-store
    key: secrets.json
    path: secrets.json
extraEnv:
  - name: ABHED_SECRETS_FILE
    value: /run/abhed/secrets/secrets.json
```

A copied file does not follow a later change to its Secret; restart the
pods after rotating one. The command-line token key (`auth.cli.key_file`)
needs no Secret on one replica: Abhed creates it, mode 600, in `~/.abhed` on
the volume at the first start.

## Checks before and while serving

- **Preflight.** An init container runs `abhed serve -check` with the same
  configuration, Secrets and volumes, so a pod whose model, secrets store,
  sandbox, sign-in, storage or control plane would stop the server never
  starts. `kubectl logs deploy/abhed -c preflight` shows each check. Turn it
  off with `preflight.enabled: false`.
- **Startup and liveness** probe `GET /v1/health`: the process is up.
- **Readiness** probes `GET /v1/ready`: the server is not draining, and the
  secrets store, the event store and the control plane each answer. A pod
  that fails it gets no traffic but is not restarted.

## The pod

Every container is held to the `restricted` Pod Security level: non-root
(uid and gid 10001), every capability dropped, no privilege escalation,
seccomp `RuntimeDefault`, a read-only root filesystem, no service-account
token, and no Service environment variables. The writable places are the
volume, a sized `/tmp`, and an empty home directory. Values that would
weaken this (`privileged`, an added capability, a writable root, root) are
refused by the schema.

**The sandbox inside the pod.** The image's `process` tier is bubblewrap,
which needs an unprivileged user namespace. Some container runtimes' default
seccomp profiles refuse to create one; containerd's is one. Abhed then
does not run commands unsandboxed: the default `sandbox.min_tier` is
`process`, so preflight fails on its `sandbox` check and the pod stays in
`Init`. Your choices:

- a `Localhost` seccomp profile that allows user namespaces, installed on
  the nodes and named in `podSecurityContext.seccompProfile` and
  `securityContext.seccompProfile` (restricted allows `Localhost`);
- `sandbox.min_tier: none` in `config`, which makes the pod's own isolation
  the only boundary around the agent's commands;
- the Kubernetes driver in the Team edition, which runs each sandbox as its
  own restricted pod.

## Persistence

`persistence.enabled` (on by default) makes one `ReadWriteOnce` claim and
the Deployment uses the `Recreate` strategy, since the old and new pod cannot
share the volume. `persistence.existingClaim` uses a claim you made.
With persistence off, both folders are `emptyDir` and lost with the pod.

## Postgres

```yaml
postgres:
  enabled: true
  existingSecret: abhed-db       # key database-url: postgres://abhed_runtime:...@db:5432/abhed
  networkPolicy:
    cidrs: [10.30.0.0/24]
```

The connection string reaches the server as `ABHED_DATABASE_URL`, from the
Secret, and the chart sets `storage.driver: postgres`. Apply the schema as
the owning role before the first start and after each upgrade, from
somewhere other than the serving pod, which never holds the owner's
credentials:

```bash
ABHED_MIGRATE_DATABASE_URL='postgres://abhed_owner:...@db/abhed' \
ABHED_DATABASE_URL='postgres://abhed_runtime:...@db/abhed' abhed migrate
```

Until it has run, preflight fails on `storage`.

## TLS

```yaml
tls:
  enabled: true
  existingSecret: abhed-tls      # a kubernetes.io/tls Secret
  minVersion: "1.3"
  clientCA:
    existingSecret: abhed-client-ca
    clientAuth: optional
```

The Secret is mounted at `/etc/abhed/tls`, and the chart sets
`server.tls.cert_file` and `key_file` (and `client_ca_file` with
`clientCA`). Abhed reads the files again every 10 seconds, so a certificate
renewed in the Secret is served without a restart. The probes switch to
HTTPS. The kubelet cannot present a client certificate, so with
`clientAuth: require` the probes only check that the port accepts
connections; `optional` keeps the HTTP checks and still verifies any
certificate a client sends. Set TLS here rather than under
`config.server.tls`; the chart refuses the latter. Behind an Ingress, tell
the controller the backend speaks HTTPS (for ingress-nginx,
`nginx.ingress.kubernetes.io/backend-protocol: HTTPS`). The TLS settings themselves are in [TLS and command-line sign-in](26-tls-and-cli-login.md).

## Ingress and OpenShift

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: abhed.example.internal
      paths: [{path: /, pathType: Prefix}]
  tls:
    - secretName: abhed-ingress-tls
      hosts: [abhed.example.internal]
```

Long-running streams (SSE, the workbench terminal) need the controller's read
timeout raised, for ingress-nginx `nginx.ingress.kubernetes.io/proxy-read-timeout`.
Set `server.trust_proxy` and `server.allowed_origins` in `config` for the
public origin.

On OpenShift set `openshift.enabled`, which leaves the user, group and
`fsGroup` to the `restricted-v2` SCC, and `route.enabled`. The Route
terminates TLS at the router (`edge`) by default; with `tls.enabled` use
`passthrough` or `reencrypt`. OpenShift's DNS listens on 5353 in
`openshift-dns`; set `networkPolicy.dns` to match (the chart's
`ci/route-values.yaml` shows how).

## Network policy

Egress is denied except to the cluster DNS, `networkPolicy.modelEndpoints`,
the Postgres peers in `postgres.networkPolicy`, the other replicas, and any
`networkPolicy.extraEgress` rules. NetworkPolicy matches addresses, not host
names: list your model endpoint's addresses, or send model traffic through a
proxy you allow. `networkPolicy.ingressFrom` narrows who may reach the port,
for example to the ingress controller's namespace. A cluster whose network
plugin does not enforce NetworkPolicy ignores all of this.

## Image

`image.tag` defaults to `v<appVersion>`. Set `image.digest` (`sha256:…`) to
pull by digest; the tag is then ignored.

## Several replicas

`replicaCount` above 1 needs `postgres.enabled` and a workspace that is not a
`ReadWriteOnce` volume (`persistence.accessModes: [ReadWriteMany]`, or
persistence off); the chart refuses otherwise. `podAntiAffinity: hard` or
`soft` spreads the pods across nodes, and `podDisruptionBudget.minAvailable`
keeps some serving through a drain. Sign-in sessions are held by the replica
that made them, so set `service.sessionAffinity: ClientIP` or session
affinity at the ingress. Sharing sandboxes between replicas, with ownership
and failover, is the Enterprise edition's high availability.

## A namespace per workspace

```yaml
workspaceNamespaces:
  enabled: true
  workspaces:
    - name: payments
      resourceQuota:
        hard: {pods: "40"}
    - name: research
```

Each workspace gets a namespace, `abhed-ws-<name>`, labelled to enforce,
audit and warn at the `restricted` Pod Security level, with a ResourceQuota
and a LimitRange (the defaults are in `values.yaml`; an entry may replace
either). `defaultDenyNetworkPolicy` adds a policy denying all traffic in
each. The namespaces are kept when the release is uninstalled
(`workspaceNamespaces.keep`).

## Checking the chart

`scripts/ci/helm-check.sh` lints and renders the chart for every file in its
`ci/` folder, validates the result against the Kubernetes 1.30 and 1.33
schemas with kubeconform, and checks that the values above are refused. It
downloads pinned helm and kubeconform releases and verifies their checksums.
