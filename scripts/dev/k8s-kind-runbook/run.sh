#!/usr/bin/env bash
# Run an Abhed runbook flow against a throwaway kind cluster: read-only
# k8s_get of pods, a deployment and a pod's logs, then the one change the
# runbook approves (scale web to 2), with a change it does not list refused.
# The model is scripted (fake-model.mjs), so nothing leaves the machine.
#
#   scripts/dev/k8s-kind-runbook/run.sh [WORKDIR]
#
# Needs kind, kubectl, node, Go, and docker or podman (KIND_EXPERIMENTAL_PROVIDER=podman
# is set when only podman is found). The cluster gets a unique name and its
# own kubeconfig in WORKDIR: no existing kube context is read or changed, and
# the cluster is deleted on exit, pass or fail. KEEP_CLUSTER=1 keeps it.
# KIND_NODE_IMAGE picks the node image (kind's default otherwise);
# WORKLOAD_IMAGE (alpine:3.20) is loaded into the cluster from the local
# engine, so the pod needs no registry.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
WORK="${1:-$(mktemp -d)}"
mkdir -p "$WORK"
WORK="$(cd "$WORK" && pwd)"
NAME="abhed-runbook-$(date +%s)-$RANDOM"
CTX="kind-$NAME"
KCFG="$WORK/kubeconfig"
WORKLOAD_IMAGE="${WORKLOAD_IMAGE:-docker.io/library/alpine:3.20}"
# Every kubectl and kind call below names this file; the variable is set too,
# so nothing falls back to ~/.kube/config.
export KUBECONFIG="$KCFG"

for bin in kind kubectl node go; do
  command -v "$bin" >/dev/null || { echo "missing: $bin" >&2; exit 2; }
done
ENGINE=docker
if ! command -v docker >/dev/null || ! docker info >/dev/null 2>&1; then
  command -v podman >/dev/null || { echo "missing: docker or podman" >&2; exit 2; }
  ENGINE=podman
  export KIND_EXPERIMENTAL_PROVIDER=podman
fi

model_pid=""
created=""
# shellcheck disable=SC2317 # run by the trap
cleanup() {
  if [ -n "$model_pid" ]; then kill "$model_pid" 2>/dev/null || true; wait "$model_pid" 2>/dev/null || true; fi
  if [ -n "$created" ] && [ -z "${KEEP_CLUSTER:-}" ]; then
    echo "deleting cluster $NAME"
    kind delete cluster --name "$NAME" --kubeconfig "$KCFG" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

k() { kubectl --kubeconfig "$KCFG" --context "$CTX" "$@"; }

echo "creating kind cluster $NAME ($ENGINE)"
created=1
kind create cluster --name "$NAME" --kubeconfig "$KCFG" --wait 180s \
  ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >"$WORK/kind.log" 2>&1 ||
  { cat "$WORK/kind.log" >&2; exit 1; }

echo "loading $WORKLOAD_IMAGE"
"$ENGINE" image inspect "$WORKLOAD_IMAGE" >/dev/null 2>&1 || "$ENGINE" pull -q "$WORKLOAD_IMAGE" >/dev/null
"$ENGINE" save -o "$WORK/workload.tar" "$WORKLOAD_IMAGE" >/dev/null
kind load image-archive "$WORK/workload.tar" --name "$NAME" >/dev/null

k create namespace demo >/dev/null
k -n demo create deployment web --image "$WORKLOAD_IMAGE" --replicas 1 \
  -- sh -c 'echo "web started"; while true; do echo "tick $(date +%s)"; sleep 5; done' >/dev/null
k -n demo patch deployment web --type json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null
k -n demo rollout status deployment/web --timeout 120s >/dev/null

echo "building abhed"
(cd "$ROOT" && go build -o "$WORK/abhed" ./cmd/abhed)

node "$HERE/fake-model.mjs" 0 "$CTX" >"$WORK/model.log" 2>&1 &
model_pid=$!
for _ in $(seq 50); do grep -q '^PORT ' "$WORK/model.log" 2>/dev/null && break; sleep 0.1; done
MODEL_PORT="$(sed -n 's/^PORT //p' "$WORK/model.log")"
[ -n "$MODEL_PORT" ] || { echo "the scripted model did not start" >&2; exit 1; }

HOME_DIR="$WORK/home"; WS="$WORK/ws"
mkdir -p "$HOME_DIR/.abhed" "$WS"
cat >"$HOME_DIR/.abhed/config.json" <<EOF
{
  "sandbox": {"min_tier": "none"},
  "model": {"default": "stub", "providers": {"stub": {"type": "openai-compatible",
    "base_url": "http://127.0.0.1:$MODEL_PORT", "model": "kind-runbook", "context_window": 32768}}},
  "k8s": {"enabled": true, "kubeconfig": "$KCFG", "context": "$CTX", "namespace": "demo", "allow_writes": true}
}
EOF
cat >"$WORK/runbook.json" <<EOF
{"plan": "Scale web in demo to 2 replicas after reading its state.",
 "runbook": [{"tool": "k8s_apply", "note": "scale web to 2",
   "args": {"action": "scale", "resource": "deployments", "name": "web", "replicas": 2,
            "namespace": "demo", "context": "$CTX"}}]}
EOF

echo "running the runbook flow"
set +e
HOME="$HOME_DIR" XDG_CONFIG_HOME="$WORK/xdg" ABHED_SECRETS_FILE="" ABHED_TRUST_WORKSPACE="" \
  "$WORK/abhed" -C "$WS" -p -runbook "$WORK/runbook.json" -output-format json \
  "web in demo is slow: read its pods, deployment and logs, then scale it to 2" \
  </dev/null >"$WORK/result.json" 2>"$WORK/abhed.err"
code=$?
set -e

fail=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fail=1; fi; }
result="$(tail -n 1 "$WORK/result.json")"
replicas="$(k -n demo get deployment web -o jsonpath='{.spec.replicas}')"
check "abhed exited 0 (was $code)" '[ "$code" = 0 ]'
res() { sed -n "s/^RESULT $1 //p" "$WORK/model.log"; }
check "k8s_get listed the web pod" '[[ "$(res 1)" =~ web-[a-z0-9]+-[a-z0-9]+ ]]'
check "k8s_get read the deployment" '[[ "$(res 2)" == *web* && "$(res 2)" == *eplica* ]]'
check "k8s_get read the pod's logs" '[[ "$(res 3)" == *"web started"* ]]'
check "the wait saw the rollout finish" '[[ "$(res 6)" == *"rolled out"* ]]'
check "the runbook step ran: replicas is 2 (was $replicas)" '[ "$replicas" = 2 ]'
check "the unlisted change was refused as not in the plan, with no flag offered" \
  'printf "%s" "$result" | node -e "
    const r = JSON.parse(require(\"fs\").readFileSync(0, \"utf8\"));
    const d = (r.permission_denials || []).filter(x => x.tool === \"k8s_apply\");
    process.exit(d.length === 1 && /not in the approved plan/.test(d[0].reason) && !d[0].flag ? 0 : 1);" 2>/dev/null'
echo "work folder: $WORK (result.json, abhed.err, model.log)"
exit "$fail"
