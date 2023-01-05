#!/bin/sh
set -o errexit

# build the service-broker
make

# build docker image
docker build --build-arg namespace=service-broker -t service-broker:latest .

# tag the iamge to use the local registry
docker tag service-broker:latest localhost:5001/service-broker:latest

# push the image to the local registry
docker push localhost:5001/service-broker:latest

# delete the deployment if it exists
kubectl delete --kubeconfig="$PWD/service-provider" --ignore-not-found=true -f deployment.yaml

# apply the deployment
kubectl apply --kubeconfig="$PWD/service-provider" -f deployment.yaml

# apply the crds
kubectl apply --kubeconfig="$PWD/service-provider" -f crds/servicebroker.couchbase.com_servicebrokerconfigs.yaml

# wait for the deployment to be ready
kubectl wait --kubeconfig="$PWD/service-provider" -n service-broker --for=condition=available deployment/service-broker-deployment --timeout=60s

# apply the cluster configuration
kubectl apply -f examples/configurations/couchbase-server/broker.yaml -n service-broker --kubeconfig="$PWD/service-provider"

# port forward the service broker
kubectl port-forward --kubeconfig="$PWD/service-provider" -n service-broker deployment/service-broker-deployment 8090:8443