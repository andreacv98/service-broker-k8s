# Liqo Kubernetes Generic Service Broker - a.k.a. Catalog Server

## What is it?

The Liqo Kubernetes Generic Service Broker (a.k.a. Catalog Server) is a REST API server that implements the [Open Service Broker API EXTENDED specification](documentation/spec.md). It's a fork of the [Kubernetes Generic Service Broker](https://github.com/couchbase/service-broker) project and it's customized for multi-cluster and multi-tenant scenarios extending and adapting the [Open Service Broker Specification](https://github.com/openservicebrokerapi/servicebroker/blob/v2.13/spec.md).

## Original project

Since this project is a fork of the Kubernetes Service Broker, we report here the original README.md [file](./README%20legacy.md).

## How to use it?

### Prerequisites

Ensure to have the following prerequisites:

1. Kubernetes cluster
2. Liqoctl installed

### Steps:

#### 1. Liqo cluster setup

Setup the Liqo cluster following the official [documentation](https://docs.liqo.io/en/v0.7.0/installation/install.html). With a Kubernetes cluster you can easily setup a Liqo cluster with the following command:

```bash
liqoctl install kubeadm
```

#### 2. Catalog Server requirements

The Catalog Server requires a specific CRD to be installed in the cluster. You can install it with the following command:

```bash
kubectl apply -f crds/servicebroker.couchbase.com_servicebrokerconfigs.yaml
```

This CRD defines the configuration of the Catalog Server. You can find an example of this configuration in the [example configuration file](./example-liqo/configuration/servicebrokerconfiguration.yaml). You can customize it following the [specific section](#catalog-customization) of this README.

Now apply the example configuration in order to get a working Catalog Server:

```bash
kubectl apply -f example-liqo/configuration/servicebrokerconfiguration.yaml
```

#### 3. Catalog Server setup

Catalog server is offered as a docker image. You can easily deploy it with our example [deployment](./example-liqo/service-broker/deployment.yaml) file. You can deploy it with the following command:

```bash
kubectl apply -f example-liqo/service-broker/service-account/service-account.yaml example-liqo/service-broker/service-account/croleNbinding.yaml example-liqo/service-broker/secret.yaml example-liqo/service-broker/deployment.yaml example-liqo/service-broker/service.yaml
```

Doing so, you will have an exposed Catalog Server running in your cluster with a specific service account able to interact with the Catalog Server's CRD and Liqo's CRDs.

### Catalog customization

The Catalog Server exposes a catalog of services that can be purchased by authenticated users.

You can start from the example catalog [file](./example-liqo/configuration/servicebrokerconfiguration.yaml) and you can understand it through the legacy documentation [file](./documentation/modules/ROOT/pages/concepts/catalog.adoc).

Please note, from the original documentation, a new field has been added to the service plan specification: "peeringPolicies".

#### Peering policies

Each service plan exposes a "peeringPolicies" field. It's a list of peering policies that will be applied to the service plan and it's reflected on where the resources specified in the template are created.

In fact, each service MUST be provided into a namespace that owns a [Namespace Offloading](https://docs.liqo.io/en/v0.7.0/usage/namespace-offloading.html) resource with "Pod offloading strategy" included in the service plan's peering policies.

The available peering policies are the same as the ones specified in the [Liqo documentation](https://docs.liqo.io/en/v0.7.0/usage/namespace-offloading.html#pod-offloading-strategy).

#### Automatic binding

In order to provide an automatic way to bind the service to an application, you can follow the example in the [example configuration file](./example-liqo/configuration/servicebrokerconfiguration.yaml).

##### TLDR
If you want to let the customer application automatically access to the service you are creating, you need to annotate the credentials resources (e.g. secrets) with the following annotation:

```yaml
annotations:
    synator/sync: 'yes'          
    synator/include-namespaces: '{{ printf "%s,%s" (registry "namespace") (registry "binding-namespace") }}'
```

Just ensure to require to the custom through the parameters of the service plan the namespace where the application is running and memorize it into the registry. For example:

```yaml
serviceBinding:
      registry:
      - name: binding-namespace
        value: '{{ parameter "/binding-namespace" }}'
```

Of course, to work, the customer's cluster needs to have the [Synator operator](https://github.com/TheYkk/synator) installed.

##### How it works

When you define the service templates, you can define a secret with the credentials to access the service. In our example, the database deployment refers to a secret containing the parameters to create the database, the same parameters used as credentials to access the database. These values are created by the "database-secret-binding" template in the service binding phase. If this secret is made known to another application in another namespace (e.g. through a secret copy), the application can access the database, just referring to the namespace.

The secret copy has been made automatic using the [Synator operator](https://github.com/TheYkk/synator), which just with few flags on the resource, can copy it to another namespace. This is not an operator that you need to have in your cluster, but it's an automatic way to copy the credentials secret to the application namespace.

### Security

The Catalog Server exposes some APIs that are usable only by previous authentication (take a look at the [OpenAPI scheme](./documentation/openapi.yaml) to quickly get which APIs are protected). This authentication requirement comes from the original specification of the OpenService Broker API standard, but it's customized to a detached authentication system.

The Catalog Server recevies JWT token in the Authorization header that was generated by the [OpenID Connect](https://openid.net/connect/) authentication server. The Catalog Server will validate the token and will extract the user's information from it. In order to do all these things, the Catalog Server needs to know the OIDC server information and it needs credentials on it as well (as a client with service account enabled).

Nowadays, the Catalog Server supports only the OIDC authentication system provided by a [Keycloak server](https://www.keycloak.org/).

PLEASE NOTE: the Catalog Server is not aware of the authentication server so you need to let it know by calling the API:

```
POST "/auth/credentials"
 body: {
    "auth_url": "",
    "realm": "",
    "client_id": "",
    "client_secret": ""
 }
```
