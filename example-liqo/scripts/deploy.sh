#!/bin/sh
set -o errexit

# delete liqooff namespace if already exists
kubectl delete namespace liqooff --kubeconfig="$PWD/service-provider" --ignore-not-found=true

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
kubectl delete --kubeconfig="$PWD/example-liqo/service-provider" --ignore-not-found=true -f example-liqo/service-broker/service-account/service-account.yaml -f example-liqo/service-broker/service-account/croleNbinding.yaml -f example-liqo/service-broker/secret.yaml -f example-liqo/service-broker/deployment.yaml -f example-liqo/service-broker/service.yaml

# apply the resources to deploy the service broker
kubectl apply --kubeconfig="$PWD/example-liqo/service-provider" -f example-liqo/service-broker/service-account/service-account.yaml -f example-liqo/service-broker/service-account/croleNbinding.yaml -f example-liqo/service-broker/secret.yaml -f example-liqo/service-broker/deployment.yaml -f example-liqo/service-broker/service.yaml

# apply the crds
kubectl apply --kubeconfig="$PWD/example-liqo/service-provider" -f crds/servicebroker.couchbase.com_servicebrokerconfigs.yaml

# wait for the deployment to be ready
kubectl wait --kubeconfig="$PWD/example-liqo/service-provider" --for=condition=available deployment/service-broker --timeout=60s

# apply the cluster configuration
kubectl apply -f example-liqo/service-broker/configuration/servicebrokerconfiguration.yaml --kubeconfig="$PWD/example-liqo/service-provider"

# port forward the service broker
kubectl port-forward --kubeconfig="$PWD/example-liqo/service-provider" deployment/service-broker 8090:8443