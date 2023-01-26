# Couchbase OpenService Broker

## Customized for Liqo

### Prerequisites
1. Kind software installed
2. Liqoctl installed

### Steps:

#### 1. Automatic setup scripts
Setup the enviroment executing the script `./enviroment.sh` in the root of the project.
```bash
bash ./scripts/enviroment.sh
```
This script will create two kind clusters ("service-provider" and "customer") with a peering Liqo from "service-provider" to "customer". Moreover, the "service-provider" cluster is configured to pull docker images from a local registry deployed as "kind-registry".

Deploy the Couchbase OpenService Broker in the "service-provider" cluster as well as the Liqo offloaded namespace "liqooff".

```bash
bash ./scripts/deploy.sh
```

Now you have two clusters "service-provider" and "customer" peered with Liqo. The "service-provider" cluster has the Couchbase OpenService Broker deployed in the "default" namespace. The "service-provider" cluster has the Liqo offloaded namespace "liqooff".

Full script below:
```bash
bash ./scripts/enviroment.sh && bash ./scripts/deploy.sh
```

The scripts will have finished when you see the following output:
```bash
Forwarding from 127.0.0.1:8090 -> 8443
Forwarding from [::1]:8090 -> 8443
```
This means that the OpenService Broker is running and ready to accept API requests on localhost:8090. Be carefull, the request should be through HTTPS, even if the certificate is not secure.

To interact with the "service-provider" cluster you can export the KUBECONFIG environment variable:
```bash
export KUBECONFIG=$PWD/service-provider
```