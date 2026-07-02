#!/usr/bin/env bash
#// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
#// SPDX-License-Identifier: Apache-2.0

set -o errexit
set -o nounset
set -o pipefail


REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
KUBECTL=$REPO_ROOT/bin/kubectl

# desired kind cluster name; default is "metal"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-metal}"

if [[ "$(kind get clusters)" =~ .*"${KIND_CLUSTER_NAME}".* ]]; then
  echo "cluster already exists, moving on"
  exit 0
fi

reg_name='kind-registry'
reg_port="${KIND_REGISTRY_PORT:-5000}"

# Optional Kind node extraMounts via KIND_EXTRA_MOUNTS:
#   semicolon-separated containerPath=hostPath pairs, e.g.
#   /redfish-clients=/abs/path/on/host;/other=/second/path
KIND_EXTRA_MOUNTS="${KIND_EXTRA_MOUNTS:-}"
kind_extra_mounts=""
if [[ -n "${KIND_EXTRA_MOUNTS}" ]]; then
  extra_mount_entries=()
  IFS=';' read -r -a mount_pairs <<< "${KIND_EXTRA_MOUNTS}"
  for pair in "${mount_pairs[@]}"; do
    if [[ -z "${pair}" ]]; then
      continue
    fi
    if [[ "${pair}" != *"="* ]]; then
      echo "invalid KIND_EXTRA_MOUNTS entry (expected containerPath=hostPath): ${pair}" >&2
      exit 1
    fi
    container_path="${pair%%=*}"
    host_path="${pair#*=}"
    if [[ -z "${container_path}" || -z "${host_path}" ]]; then
      echo "invalid KIND_EXTRA_MOUNTS entry (empty path): ${pair}" >&2
      exit 1
    fi
    if [[ "${container_path}" != /* ]]; then
      echo "KIND_EXTRA_MOUNTS container path must be absolute: ${container_path}" >&2
      exit 1
    fi
    resolved_host_path="$(realpath "${host_path}")"
    if [[ ! -d "${resolved_host_path}" ]]; then
      echo "KIND_EXTRA_MOUNTS host path does not exist: ${resolved_host_path}" >&2
      exit 1
    fi
    extra_mount_entries+=("  - hostPath: ${resolved_host_path}
    containerPath: ${container_path}
    readOnly: true")
  done
  if [[ "${#extra_mount_entries[@]}" -eq 0 ]]; then
    echo "KIND_EXTRA_MOUNTS is set but contains no valid mount pairs" >&2
    exit 1
  fi

  kind_extra_mounts=$'nodes:\n- role: control-plane\n  extraMounts:\n'"$(printf '%s\n' "${extra_mount_entries[@]}")"
fi

# create registry container unless it already exists
running="$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)"
if [ "${running}" != 'true' ]; then
  docker run -d --restart=always -p "127.0.0.1:${reg_port}:5000" --name "${reg_name}" registry:2
fi

# create a cluster with the local registry enabled in containerd
cat <<EOF | kind create cluster --name "${KIND_CLUSTER_NAME}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
${kind_extra_mounts}
EOF

# add the registry config to the nodes
REGISTRY_DIR="/etc/containerd/certs.d/localhost:${reg_port}"
node="${KIND_CLUSTER_NAME}-control-plane"
docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
[host."http://${reg_name}:5000"]
EOF

# connect the registry to the cluster network if not already connected
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
  docker network connect "kind" "${reg_name}"
fi

# Document the local registry
# https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
cat <<EOF | "${KUBECTL}" apply --namespace=kube-public -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${reg_port}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

"${KUBECTL}" wait node "${KIND_CLUSTER_NAME}-control-plane" --for=condition=ready --timeout=90s
