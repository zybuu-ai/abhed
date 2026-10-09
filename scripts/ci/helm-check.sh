#!/usr/bin/env bash
# Lint and render the Helm chart for every values file in its ci/ folder,
# validate what it renders against the Kubernetes schemas, and check that
# values the chart must refuse are refused.
#
#   scripts/ci/helm-check.sh [chart-dir]
#
# helm and kubeconform are downloaded at pinned versions and checked against
# pinned SHA-256 sums, into TOOLS_DIR (a temporary folder by default). Set
# HELM or KUBECONFORM to use binaries you already trust instead.
set -euo pipefail

CHART=${1:-deploy/helm/abhed}
HELM_VERSION=v3.22.0
KUBECONFORM_VERSION=v0.8.0
# Kubernetes versions to validate against: the oldest the chart supports and a current one.
KUBE_VERSIONS=${KUBE_VERSIONS:-"1.30.0 1.33.0"}
# Schemas fetched at fixed commits, so a validation run is reproducible.
K8S_SCHEMAS="https://raw.githubusercontent.com/yannh/kubernetes-json-schema/39a87cd29966bc0b3434e8cfbc2061782c8cb69a/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json"
CRD_SCHEMAS="https://raw.githubusercontent.com/datreeio/CRDs-catalog/63669a570e231d4f1f8396d229a1de512bcf0a34/openshift/v4.15-strict/{{.ResourceKind}}_{{.Group}}_{{.ResourceAPIVersion}}.json"

sha_helm() {
  case $1 in
    linux-amd64) echo 1e4ab49e429626cf6c6958d914248b78c9730803c2751b87627e171dc800e7bb ;;
    linux-arm64) echo f14e804dfee240f55525b667488fe9adca349e63e00c9af634c0beb1421ac310 ;;
    darwin-arm64) echo 4c9982a6cdeb458b60258df66b55398ca5b19293f6877faffe2909ad6f23dfe0 ;;
    darwin-amd64) echo bd1d09f316558dda23698527859600936fed1371021ce0f2272d66b2fdcfa69c ;;
    *) return 1 ;;
  esac
}

sha_kubeconform() {
  case $1 in
    linux-amd64) echo 9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 ;;
    linux-arm64) echo 1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87 ;;
    darwin-arm64) echo f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67 ;;
    darwin-amd64) echo 71dbc87ac9f24099a62b93570e65aa06312ba6ac8aea63b7f86e9d999edf5a92 ;;
    *) return 1 ;;
  esac
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
platform=$os-$arch

TOOLS_DIR=${TOOLS_DIR:-$(mktemp -d)}
mkdir -p "$TOOLS_DIR"

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetch URL FILE SHA: download unless already there, and refuse a mismatch.
fetch() {
  local url=$1 out=$2 want=$3
  [ -f "$out" ] || curl -fsSL --retry 3 -o "$out" "$url"
  local got
  got=$(sha256 "$out")
  if [ "$got" != "$want" ]; then
    rm -f "$out"
    echo "checksum mismatch for $url: got $got, want $want" >&2
    exit 1
  fi
}

if [ -z "${HELM:-}" ]; then
  want=$(sha_helm "$platform") || { echo "no pinned helm for $platform" >&2; exit 1; }
  fetch "https://get.helm.sh/helm-$HELM_VERSION-$platform.tar.gz" "$TOOLS_DIR/helm-$HELM_VERSION-$platform.tar.gz" "$want"
  tar -xzf "$TOOLS_DIR/helm-$HELM_VERSION-$platform.tar.gz" -C "$TOOLS_DIR" "$platform/helm"
  HELM=$TOOLS_DIR/$platform/helm
fi
if [ -z "${KUBECONFORM:-}" ]; then
  want=$(sha_kubeconform "$platform") || { echo "no pinned kubeconform for $platform" >&2; exit 1; }
  fetch "https://github.com/yannh/kubeconform/releases/download/$KUBECONFORM_VERSION/kubeconform-$platform.tar.gz" \
    "$TOOLS_DIR/kubeconform-$KUBECONFORM_VERSION-$platform.tar.gz" "$want"
  tar -xzf "$TOOLS_DIR/kubeconform-$KUBECONFORM_VERSION-$platform.tar.gz" -C "$TOOLS_DIR" kubeconform
  KUBECONFORM=$TOOLS_DIR/kubeconform
fi

"$HELM" version --short
"$KUBECONFORM" -v

mkdir -p "$TOOLS_DIR/schemas"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
failed=0

for values in "$CHART"/ci/*-values.yaml; do
  name=$(basename "$values" -values.yaml)
  echo "== $name"
  "$HELM" lint --strict "$CHART" -f "$values"
  "$HELM" template abhed "$CHART" --namespace abhed -f "$values" >"$out/$name.yaml"
  for v in $KUBE_VERSIONS; do
    "$KUBECONFORM" -strict -summary -cache "$TOOLS_DIR/schemas" -kubernetes-version "$v" \
      -schema-location "$K8S_SCHEMAS" -schema-location "$CRD_SCHEMAS" "$out/$name.yaml"
  done
done

# Every pod the chart renders is held to the restricted profile.
for f in "$out"/*.yaml; do
  if grep -nE 'privileged: true|allowPrivilegeEscalation: true|runAsNonRoot: false|readOnlyRootFilesystem: false|hostNetwork: true|hostPID: true|hostPath:' "$f"; then
    echo "FAIL: $(basename "$f") renders a field the restricted profile forbids" >&2
    failed=1
  fi
done

# refuse WHY EXPECT ARGS...: the chart must refuse to render with these
# values, with an error containing EXPECT.
refuse() {
  local why=$1 expect=$2
  shift 2
  local msg
  if msg=$("$HELM" template abhed "$CHART" "$@" 2>&1 >/dev/null); then
    echo "FAIL: rendered although $why" >&2
    failed=1
  elif [[ $msg != *"$expect"* ]]; then
    echo "FAIL: refused $why for another reason: $msg" >&2
    failed=1
  else
    echo "ok   refused: $why"
  fi
}

refuse "config holds an API key" "config holds a secret" --set config.model.providers.x.api_key=sk-test
refuse "config holds a database DSN" "config holds a secret" --set config.storage.dsn=postgres://u:p@db/abhed
refuse "config holds a client secret" "config holds a secret" --set config.auth.client_secret=s
refuse "podConfig holds a password" "podConfig holds a secret" --set podConfig.auth.password=p
refuse "storage is postgres without a Secret" "set postgres.enabled" --set config.storage.driver=postgres
refuse "postgres has no existing Secret" "existingSecret" --set postgres.enabled=true
refuse "TLS has no Secret" "existingSecret" --set tls.enabled=true
refuse "TLS configured in config instead of tls" "use the tls values" --set config.server.tls.cert_file=/x
refuse "several replicas without Postgres" "needs postgres.enabled" --set replicaCount=2
refuse "several replicas on a ReadWriteOnce volume" "ReadWriteOnce" \
  --set replicaCount=2 --set postgres.enabled=true --set postgres.existingSecret=db
refuse "a Route terminates edge in front of TLS" "passthrough or reencrypt" \
  --set route.enabled=true --set tls.enabled=true --set tls.existingSecret=t
refuse "both minAvailable and maxUnavailable" "not both" --set podDisruptionBudget.minAvailable=1
refuse "a digest that is not sha256" "digest" --set image.digest=latest
refuse "privilege escalation" "allowPrivilegeEscalation" --set securityContext.allowPrivilegeEscalation=true
refuse "a writable root filesystem" "readOnlyRootFilesystem" --set securityContext.readOnlyRootFilesystem=false
refuse "a capability added" "add" --set 'securityContext.capabilities.add[0]=NET_ADMIN'
refuse "root" "runAsUser" --set podSecurityContext.runAsUser=0
refuse "an unknown value" "replicas" --set replicas=2
refuse "a secret file path that leaves the folder" "path" \
  --set 'secretFiles[0].secretName=s' --set 'secretFiles[0].key=k' --set 'secretFiles[0].path=../x'

exit $failed
