#!/bin/sh
set -o errexit

echo "Creating kind clusters..."
echo "Creating service-provider cluster..."
# create a cluster with the local registry enabled in containerd
cat <<EOF | kind create cluster --name="service-provider" --kubeconfig="service-provider.kconf" 
EOF

echo "Installing CRDs..."
# install CRDs
cat <<EOF | kubectl apply -f ./crds/. --kubeconfig="service-provider.kconf"
EOF

echo "Creating customer cluster..."
# create the second cluster
cat <<EOF | kind create cluster --name="customer" --kubeconfig="customer.kconf"
EOF

echo "Creating customer2 cluster..."
# create the third cluster
cat <<EOF | kind create cluster --name="customer2" --kubeconfig="customer2.kconf"
EOF

echo "Installing liqo..."
echo "Installing liqo on service-provider cluster..."
# install liqo on first cluster
cat <<EOF | liqoctl install kind --cluster-name service-provider --kubeconfig="service-provider.kconf"
EOF

echo "Installing liqo on customer cluster..."
# install liqo on second cluster
cat <<EOF | liqoctl install kind --cluster-name customer --kubeconfig="customer.kconf"
EOF

echo "Installing liqo on customer2 cluster..."
# install liqo on thirds cluster
cat <<EOF | liqoctl install kind --cluster-name customer2 --kubeconfig="customer2.kconf"
EOF

echo "Liqo peering commands:"
echo "Peering command for CUSTOMER cluster:"
liqoctl generate peer-command --kubeconfig="customer.kconf"

echo "Peering command for CUSTOMER2 cluster:"
liqoctl generate peer-command --kubeconfig="customer2.kconf"