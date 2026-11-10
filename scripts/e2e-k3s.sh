#!/usr/bin/env bash
# End-to-end test of the k3s worker (PLAN M7a): the Helm chart in
# deploy/helm/sandboxd-worker runs `sandboxd worker` as a DaemonSet on a real
# k3s cluster and the pod joins the control plane's fleet.
#
#   - the chart installs; the worker pod runs non-root, read-only, with no
#     service account token and no capabilities
#   - the k3s node registers under its Kubernetes node name, with the
#     capacity given in the chart values
#   - draining the node reaches the pod through the heartbeat reply
#   - a pod restart keeps the same node id (no duplicate node)
#   - uninstalling the chart takes the node offline after the lease
#
# Inputs (env):
#   SANDBOXD_BIN     built sandboxd binary (the control plane runs on the host)
#   SANDBOXD_IMAGE   sandbox image tag for the control plane's local node
#   WORKER_IMAGE     sandboxd image already imported into k3s (repo:tag)
#   KUBECONFIG       kubeconfig of the cluster (default /etc/rancher/k3s/k3s.yaml)
#
#   sudo SANDBOXD_BIN=/tmp/sandboxd SANDBOXD_IMAGE=sandboxd-e2e:local \
#     WORKER_IMAGE=sandboxd:ci bash scripts/e2e-k3s.sh
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "❌ scripts/e2e-k3s.sh must run as root (sudo), like sandboxd in production" >&2
  exit 2
fi

BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
WIMG="${WORKER_IMAGE:?set WORKER_IMAGE}"
export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$HERE/deploy/helm/sandboxd-worker"
DD="$(mktemp -d)"
PORT="${SANDBOXD_E2E_K3S_PORT:-9197}"
API="http://127.0.0.1:$PORT"
TOKEN="sk_k3s_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
WTOKEN="wk_k3s_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NS=sandboxd-e2e
REL=e2e
LEASE=8
BEAT=2
CP_PID=""

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  echo "---- control plane log (tail) ----"; tail -30 "$DD/cp.log" 2>/dev/null || true
  echo "---- pods ----"; kubectl -n "$NS" get pods -o wide 2>/dev/null || true
  kubectl -n "$NS" describe pods 2>/dev/null | tail -30 || true
  echo "---- worker logs ----"; kubectl -n "$NS" logs -l "app.kubernetes.io/instance=$REL" --tail=30 2>/dev/null || true
  exit 1
}
cleanup() {
  helm -n "$NS" uninstall "$REL" >/dev/null 2>&1 || true
  kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
  [ -n "$CP_PID" ] && kill "$CP_PID" >/dev/null 2>&1 || true
  rm -rf "$DD" 2>/dev/null || true
}
trap cleanup EXIT

nodes_json() { curl -s -m10 -H "Authorization: Bearer $TOKEN" "$API/v1/nodes"; }
node_field() { # node_id dotted.field
  nodes_json | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("nodes",[]):
    if n["node_id"]==sys.argv[1]:
        v=n
        for k in sys.argv[2].split("."):
            v=v.get(k) if isinstance(v,dict) else None
        print("" if v is None else v)
        break' "$1" "$2"
}
node_count() { nodes_json | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("nodes",[])))'; }
wait_for() { # tries cmd...
  local n="$1"; shift
  for _ in $(seq 1 "$n"); do "$@" && return 0; sleep 1; done
  return 1
}
is_status() { [ "$(node_field "$1" status)" = "$2" ]; }
healthy() { curl -s -m2 "$API/healthz" | grep -q ok; }
pod_logged() { kubectl -n "$NS" logs "$POD" --tail=200 2>/dev/null | grep -q "$1"; }
active_twice() { [ "$(kubectl -n "$NS" logs "$POD" --tail=500 2>/dev/null | grep -c 'worker is active')" -ge 2 ]; }
worker_logs() { kubectl -n "$NS" logs -l "app.kubernetes.io/instance=$REL" --tail=200 2>/dev/null; }

# --- cluster -----------------------------------------------------------------
kubectl wait --for=condition=Ready node --all --timeout=120s >/dev/null || fail "k3s node not Ready"
K8S_NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
HOST_IP="$(kubectl get node "$K8S_NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
[ -n "$K8S_NODE" ] && [ -n "$HOST_IP" ] || fail "cannot read the k3s node name / IP"
log "k3s node $K8S_NODE ($HOST_IP)"

# --- control plane on the host (auth on) ---------------------------------------
mkdir -p "$DD/log"
SANDBOXD_ADDR="0.0.0.0:$PORT" SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
  SANDBOXD_LOG_DIR="$DD/log" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
  SANDBOXD_DB="$DD/sandboxd.db" SANDBOXD_API_AUTH_DISABLED=false \
  SANDBOXD_API_TOKENS="default=$TOKEN" SANDBOXD_WORKER_TOKEN="$WTOKEN" \
  SANDBOXD_NODE_LEASE_SECONDS="$LEASE" \
  "$BIN" >"$DD/cp.log" 2>&1 &
CP_PID=$!
wait_for 60 healthy || fail "control plane never became healthy"
log "control plane healthy"

# 1. Chart rendering guards.
log "chart guards"
helm template x "$CHART" >/dev/null 2>&1 && fail "chart rendered without controlPlane.url"
helm template x "$CHART" --set controlPlane.url=http://x --set workerToken.value=t --set kind=Deployment >/dev/null 2>&1 \
  && fail "Deployment rendered without nodeId"
log "chart guards ✓"

# 2. Install the worker DaemonSet.
log "helm install"
kubectl create namespace "$NS" >/dev/null || fail "create namespace"
helm -n "$NS" install "$REL" "$CHART" \
  --set controlPlane.url="http://$HOST_IP:$PORT" \
  --set workerToken.value="$WTOKEN" \
  --set image.repository="${WIMG%:*}" --set image.tag="${WIMG##*:}" --set image.pullPolicy=Never \
  --set capacity.cpus=3 --set capacity.memoryMB=6144 \
  --set heartbeatIntervalSeconds="$BEAT" \
  --wait --timeout 120s >/dev/null || fail "helm install"
POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" -o jsonpath='{.items[0].metadata.name}')"
[ -n "$POD" ] || fail "no worker pod"
log "helm install ✓ ($POD)"

# 3. Pod hardening as applied by the cluster.
log "pod hardening"
jp() { kubectl -n "$NS" get pod "$POD" -o jsonpath="$1"; }
[ "$(jp '{.spec.automountServiceAccountToken}')" = "false" ] || fail "service account token is mounted"
[ "$(jp '{.spec.securityContext.runAsNonRoot}')" = "true" ] || fail "pod may run as root"
[ "$(jp '{.spec.containers[0].securityContext.readOnlyRootFilesystem}')" = "true" ] || fail "root filesystem is writable"
[ "$(jp '{.spec.containers[0].securityContext.allowPrivilegeEscalation}')" = "false" ] || fail "privilege escalation allowed"
[ "$(jp '{.spec.containers[0].securityContext.capabilities.drop[0]}')" = "ALL" ] || fail "capabilities not dropped"
[ -z "$(jp '{.spec.volumes[*].hostPath.path}')" ] || fail "worker pod mounts a host path"
log "pod hardening ✓"

# 4. The k3s node joins the fleet under its Kubernetes node name.
log "worker joins the fleet"
wait_for 60 is_status "$K8S_NODE" online || fail "node $K8S_NODE never came online: $(nodes_json)"
[ "$(node_field "$K8S_NODE" role)" = "worker" ] || fail "role: $(nodes_json)"
[ "$(node_field "$K8S_NODE" capacity.cpus)" = "3" ] || fail "capacity.cpus: $(nodes_json)"
[ "$(node_field "$K8S_NODE" capacity.memory_mb)" = "6144" ] || fail "capacity.memory_mb: $(nodes_json)"
[ "$(node_count)" = "2" ] || fail "want 2 nodes (local + k3s): $(nodes_json)"
worker_logs | grep -q "worker registered" || fail "no register line in the worker log"
log "worker online ✓"

# 5. Drain reaches the pod; undrain too.
log "drain / undrain"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST -H "Authorization: Bearer $TOKEN" "$API/v1/nodes/$K8S_NODE/drain")"
[ "$code" = "200" ] || fail "drain = $code"
wait_for 20 pod_logged DRAINED || fail "the pod never learned it was drained"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST -H "Authorization: Bearer $TOKEN" "$API/v1/nodes/$K8S_NODE/undrain")"
[ "$code" = "200" ] || fail "undrain = $code"
# The first 'worker is active' line is from registration; wait for a second.
wait_for 20 active_twice || fail "the pod never learned it was active again"
log "drain ✓"

# 6. A pod restart keeps the node id: still one k3s node, online.
log "restart keeps the node id"
kubectl -n "$NS" rollout restart "daemonset/$REL-sandboxd-worker" >/dev/null || fail "rollout restart"
kubectl -n "$NS" rollout status "daemonset/$REL-sandboxd-worker" --timeout=120s >/dev/null || fail "rollout"
sleep $((BEAT * 2))
[ "$(node_count)" = "2" ] || fail "restart created another node: $(nodes_json)"
[ "$(node_field "$K8S_NODE" status)" = "online" ] || fail "node not online after restart"
log "stable id ✓"

# 7. Uninstall: the node goes offline after the lease.
log "uninstall -> offline"
helm -n "$NS" uninstall "$REL" --wait --timeout 60s >/dev/null || fail "helm uninstall"
wait_for $((LEASE + 20)) is_status "$K8S_NODE" offline || fail "node still $(node_field "$K8S_NODE" status) after uninstall"
log "offline ✓"

echo "✅ K3S E2E PASSED"
