#!/usr/bin/env bash
# Copyright 2026 Alibaba Group Holding Ltd.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cluster_name="${KIND_CLUSTER_NAME:-nodeagent-smoke}"
image="opensandbox/nodeagent:kind-smoke"
node="${cluster_name}-control-plane"
kubeconfig="$(mktemp)"
cluster_created=0
export KUBECONFIG="${kubeconfig}"

cleanup() {
	if [[ "${KEEP_KIND_CLUSTER:-}" == "1" && "${cluster_created}" == "1" ]]; then
		echo "keeping Kind cluster ${cluster_name} for inspection" >&2
		return
	fi
	if [[ "${cluster_created}" == "1" ]]; then
		kind delete cluster --name "${cluster_name}" >/dev/null 2>&1 || true
	fi
	rm -f "${kubeconfig}"
}

finish() {
	status=$?
	trap - EXIT
	if [[ ${status} -ne 0 ]]; then
		echo "Kind smoke test failed; collecting diagnostics" >&2
		kubectl get pods -A -o wide >&2 || true
		kubectl logs -n opensandbox-system -l app.kubernetes.io/component=node-agent --all-containers --tail=-1 >&2 || true
		docker exec "${node}" sh -c 'find /var/lib/opensandbox/nodeagent /var/lib/opensandbox/nodeagent-data -maxdepth 6 -ls 2>/dev/null' >&2 || true
	fi
	cleanup
	exit "${status}"
}
trap finish EXIT

kind create cluster --name "${cluster_name}" --kubeconfig "${kubeconfig}" --wait 120s
cluster_created=1
docker build -f "${repo_root}/components/nodeagent/Dockerfile" -t "${image}" "${repo_root}"
kind load docker-image --name "${cluster_name}" "${image}"

helm install nodeagent "${repo_root}/kubernetes/charts/opensandbox-node-agent" \
  --namespace opensandbox-system \
  --create-namespace \
  --set image.repository=opensandbox/nodeagent \
  --set image.tag=kind-smoke \
  --set image.pullPolicy=Never \
  --set config.clusterID=kind-test \
  --set config.reconcileInterval=2s \
  --set config.partialTimeout=1s \
  --set config.endedStateRetention=1m \
  --set sink.type=file \
  --wait --timeout 180s

kubectl create namespace workloads
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: normal-sandbox
  namespace: workloads
  labels:
    opensandbox.io/id: sb-normal
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: opensandbox/nodeagent:kind-smoke
      imagePullPolicy: Never
      command: ["sh", "-c", "echo before-restart; while [ ! -f /tmp/release ]; do sleep 1; done; echo after-restart"]
---
apiVersion: v1
kind: Pod
metadata:
  name: pooled-sandbox
  namespace: workloads
  labels:
    opensandbox.io/id: sb-pool
    sandbox.opensandbox.io/pool-name: test-pool
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: opensandbox/nodeagent:kind-smoke
      imagePullPolicy: Never
      command: ["sh", "-c", "echo must-not-be-collected; sleep 2"]
YAML

for _ in $(seq 1 90); do
  if docker exec "${node}" sh -c 'grep -R -q "before-restart" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
    break
  fi
  sleep 1
done
docker exec "${node}" sh -c 'grep -R -q "before-restart" /var/lib/opensandbox/nodeagent-data'

old_agent_uid="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o jsonpath='{.items[*].metadata.uid}')"
kubectl delete pod -n opensandbox-system -l app.kubernetes.io/component=node-agent --wait=true
kubectl rollout status daemonset/nodeagent-opensandbox-node-agent -n opensandbox-system --timeout=120s
kubectl wait --for=condition=Ready pod -n opensandbox-system -l app.kubernetes.io/component=node-agent --timeout=120s
new_agent_uid="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o jsonpath='{.items[*].metadata.uid}')"
if [[ -z "${new_agent_uid}" || "${new_agent_uid}" == "${old_agent_uid}" ]]; then
	echo "Node Agent Pod was not replaced" >&2
	exit 1
fi

kubectl exec -n workloads normal-sandbox -- touch /tmp/release

for _ in $(seq 1 90); do
  if docker exec "${node}" sh -c 'grep -R -q "after-restart" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
    break
  fi
  sleep 1
done
docker exec "${node}" sh -c 'grep -R -q "after-restart" /var/lib/opensandbox/nodeagent-data'
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/normal-sandbox -n workloads --timeout=120s
for _ in $(seq 1 30); do
  marker="$(docker exec "${node}" sh -c 'find /var/lib/opensandbox/nodeagent-data -name "sandbox.finalized.*.json" -print -quit')"
  if [[ -n "${marker}" ]]; then
    docker exec "${node}" cat "${marker}" | jq -e '.status == "complete" and (.objects | length) >= 1' >/dev/null
		if docker exec "${node}" sh -c 'grep -R -q "must-not-be-collected" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
			echo "Pool Pod unexpectedly produced file-sink output" >&2
			exit 1
		fi
    exit 0
  fi
  sleep 2
done

echo "finalization marker was not created" >&2
exit 1
