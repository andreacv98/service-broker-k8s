#!/bin/sh
set -o errexit

# create liqooff namespace
kubectl create namespace liqooff --kubeconfig="$PWD/service-provider"

# offload liqooff namespace to the second cluster
liqoctl offload namespace liqooff --namespace-mapping-strategy EnforceSameName --pod-offloading-strategy Remote --kubeconfig="$PWD/service-provider"

# move to parent directory
cd ..

# build the service-broker
make

# build docker image
docker build --build-arg namespace=service-broker -t service-broker:latest .

# tag the iamge to use the local registry
docker tag service-broker:latest localhost:5001/service-broker:latest

# push the image to the local registry
docker push localhost:5001/service-broker:latest

# delete the deployment if it exists
kubectl delete --kubeconfig="$PWD/examples/service-provider" --ignore-not-found=true -f examples/broker_local.yaml

# apply the deployment
kubectl apply --kubeconfig="$PWD/examples/service-provider" -f examples/broker_local.yaml

# apply the crds
kubectl apply --kubeconfig="$PWD/examples/service-provider" -f crds/servicebroker.couchbase.com_servicebrokerconfigs.yaml

# wait for the deployment to be ready
kubectl wait --kubeconfig="$PWD/examples/service-provider" --for=condition=available deployment/service-broker --timeout=60s

# apply the cluster configuration
kubectl apply -f examples/configurations/couchbase-server/broker.yaml --kubeconfig="$PWD/examples/service-provider"

# port forward the service broker
kubectl port-forward --kubeconfig="$PWD/examples/service-provider" deployment/service-broker 8090:8443