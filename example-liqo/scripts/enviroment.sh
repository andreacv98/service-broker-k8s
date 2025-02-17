#!/bin/sh
set -o errexit

# create registry container unless it already exists
reg_name='kind-registry'
reg_port='5001'
if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != 'true' ]; then
  docker run \
    -d --restart=always -p "127.0.0.1:${reg_port}:5000" --name "${reg_name}" \
    registry:2
fi

# create a cluster with the local registry enabled in containerd
cat <<EOF | kind create cluster --name="service-provider" --kubeconfig="$PWD/service-provider" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:${reg_port}"]
    endpoint = ["http://${reg_name}:5000"]
EOF

# create the second cluster
cat <<EOF | kind create cluster --name="customer" --kubeconfig="$PWD/customer"
EOF

# connect the registry to the cluster network if not already connected
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
  docker network connect "kind" "${reg_name}"
fi

# Document the local registry
# https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
cat <<EOF | kubectl apply --kubeconfig="$PWD/service-provider" -f -
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

# install liqo on first cluster
cat <<EOF | liqoctl install kind --cluster-name service-provider --kubeconfig="$PWD/service-provider"
EOF

# install liqo on second cluster
cat <<EOF | liqoctl install kind --cluster-name customer --kubeconfig="$PWD/customer"
EOF

# peer second clsuter to first cluster
cat <<EOF | echo "$(liqoctl generate peer-command --only-command --kubeconfig="$PWD/customer")" --kubeconfig="$PWD/service-provider" | bash
EOF