# Local Dev Setup

## Prerequisites

- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.28.0+.

## Overview

The `metal-operator` is leveraging [envtest](https://book.kubebuilder.io/reference/envtest.html) to conduct and run
unit test suites. Additionally, it is using the [Redfish Mock Server](https://github.com/DMTF/Redfish-Mockup-Server) to
run a local mock Redfish instance to simulate operations performed by various reconcilers.

```mermaid
graph TD
    A[Kubernetes Controller Runtime Based Reconcilers] -->|Interacts with| B[envtest Kube-apiserver Environment]
    A -->|Interacts with| C[Redfish Mock Server]
    C -->|Runs as a| D[Docker Container]
```

### Run the local test suite

The local test suite can be run via

```shell
make test
```

This `Makefile` directive will start under the hood the Redfish mock server, instantiate the `envtest` environment
and run `go test ./...` on the whole project.

### Start/Stop Redfish Mock Server

The Redfish mock server can be started and stopped with the following command

```shell
make startbmc
make stopbmc
```

### Run the local Tilt development environment

#### Prerequisites

- [Tilt v0.33.17+](https://docs.tilt.dev/install.html)
- [Kind v0.23.0+](https://kind.sigs.k8s.io/docs/user/quick-start/)

The local development environment can be started via

```shell
make tilt-up
```

This `Makefile` directive will:

- create a local Kind cluster with local registry
- install cert-manager
- install [boot-operator](https://github.com/ironcore-dev/boot-operator) to reconcile the `ServerBootConfiguration` CRD
- start the `metal-operator` controller and Redfish mock server as a sidecar container
- an Endpoint resource is created to point to the Redfish mock server
- this will result in `Server` resources being created and reconciled by the `metal-operator`

```shell
‹kind-metal› kubectl get server
NAME                            SYSTEMUUID                             MANUFACTURER   POWERSTATE   STATE       AGE
compute-0-bmc-endpoint-sample   38947555-7742-3448-3784-823347823834   Contoso        On           Available   3m21s
```

The local development environment can be deleted via

```shell
make kind-delete
```

### Optional: DMTF Redfish mockup servers

By default, Tilt uses the in-repo Go Redfish mock sidecar. To additionally run
one or more [DMTF Redfish-Mockup-Server](https://github.com/DMTF/Redfish-Mockup-Server)
instances inside the Kind cluster, opt in with environment variables and the
usual `make tilt-up` target.

#### Prerequisites

- A checkout of the DMTF mockup repository with client folders on disk
- Recreate the Kind cluster when enabling or disabling the client mount

#### Enable DMTF mockups

```shell
export METAL_ENABLE_DMTF_MOCKUPS=true
export REDFISH_MOCKUP_CLIENTS_DIR=/path/to/Redfish-Mockup-Server/clients

make kind-delete
make tilt-up
```

`METAL_ENABLE_DMTF_MOCKUPS=true` switches Tilt to the `config/dev-mockups`
kustomize stack, which extends the normal dev stack with DMTF mockup
Deployments. The Go mock sidecar and `endpoint-sample` remain available.

`REDFISH_MOCKUP_CLIENTS_DIR` is required in this mode. When
`METAL_ENABLE_DMTF_MOCKUPS` is unset or `false`, the clients directory is
ignored and Kind is created without `extraMounts`, even if
`REDFISH_MOCKUP_CLIENTS_DIR` is set.

#### Verify

```shell
docker exec metal-control-plane ls /redfish-clients
kubectl get certificate,pods,endpoints,server -n metal-operator-system
kubectl exec -n metal-operator-system deployment/metal-operator-controller-manager \
  -c manager -- curl -k https://10.96.200.10:8000/redfish/v1
```

You should see both the existing Contoso server from the Go sidecar and an
additional server created from the DMTF R660 mockup endpoint.

#### Add another mockup instance

1. Copy `config/redfish-mockups/instances/r660/` to a new instance directory.
2. Update the instance overlay: unique name prefix, `-D /clients/<folder>`,
   static `clusterIP`, and matching `Endpoint` MAC/IP.
3. Register the new instance in `config/redfish-mockups/kustomization.yaml`.
4. Add a matching `macPrefix` entry to `config/dev-mockups/macdb.yaml`.
5. Recreate the cluster if needed, then run Tilt with both env vars set again.

#### Return to the default dev stack

```shell
unset METAL_ENABLE_DMTF_MOCKUPS
unset REDFISH_MOCKUP_CLIENTS_DIR

make kind-delete
make tilt-up
```

### Connecting a Remote BMC in the Tilt Environment

By default, Tilt runs against a local Redfish mock server. To point the environment at real hardware instead, apply the following changes.

#### Prerequisites: Ensure the BMC is not actively managed

Before connecting a real BMC to your local Tilt environment, make sure it is not actively reconciled by another `metal-operator` instance to avoid conflicts. Some common ways to achieve this:

- **ServerMaintenance**: Create a `ServerMaintenance` resource on the production cluster to claim the server and optionally power it off.
- **Exclude from automation**: Remove the server from the production `metal-operator`'s scope, for example via label selectors or namespace isolation, so it is no longer reconciled.
- **Decommission temporarily**: If the server is not in active use, you can power it off or disconnect it from the production cluster before testing.

> **Note:** Refer to your production cluster's runbooks for the appropriate procedure.

If you use the `ServerMaintenance` approach, apply a manifest like this on the cluster that currently owns the server:

```yaml
apiVersion: metal.ironcore.dev/v1alpha1
kind: ServerMaintenance
metadata:
  name: <maintenance-name>
  namespace: default
  annotations:
    metal.ironcore.dev/maintenance-reason: '<maintenance-name>'
spec:
  policy: Enforced
  serverRef:
    name: <server-name>
  serverPower: 'Off'
```

```shell
# Run against the remote cluster
kubectl apply -f servermaintenance-<node-name>.yaml
```

To release the server back when done:

```shell
# Run against the remote cluster
kubectl delete -f servermaintenance-<node-name>.yaml
```

> **Note:** All `kubectl` commands from this point on target the **local** Kind cluster.

#### 1. Replace the mockup endpoint with a real BMC resource

Edit `config/redfish-mockup/redfish_mockup_endpoint.yaml` to define a `BMC` resource targeting the real hardware:

```yaml
apiVersion: metal.ironcore.dev/v1alpha1
kind: BMC
metadata:
  name: <node-name>
spec:
  bmcSecretRef:
    name: <node-name>
  hostname: <bmc-hostname>
  consoleProtocol:
    name: SSH
    port: 22
  access:
    ip: <bmc-ip>
  protocol:
    name: Redfish
    port: 443
    scheme: https
```

#### 2. Create a BMCSecret with credentials

Create a `BMCSecret` file with credentials for the BMC:

```yaml
apiVersion: metal.ironcore.dev/v1alpha1
kind: BMCSecret
metadata:
  name: <node-name>
data:
  username: <base64-encoded-username>
  password: <base64-encoded-password>
```

> **Note:** The `username` and `password` values must be base64-encoded. You can encode them with `echo -n '<value>' | base64`.

Save this as `bmcsecret-<node-name>.yaml`, but do not apply it yet.

#### 3. Enable HTTPS for the BMC connection

The `--insecure` flag is deprecated. Use `--protocol` and `--skip-cert-validation` instead.

For a real BMC that uses HTTPS on port 443, configure the manager to use secure HTTPS with certificate validation enabled in the `Tiltfile`:

```python
settings = {
    "new_args": {
        "metal": [
            # ...
      "--protocol=https",
      "--skip-cert-validation=false",
        ],
    }
}
```

#### 4. Start Tilt and verify

Start the environment:

```shell
make tilt-up
```

Once the manager is running, apply the `BMCSecret` to the local Kind cluster (it is not part of the kustomize config and must be applied manually):

```shell
# Run against the local Kind cluster
kubectl apply -f bmcsecret-<node-name>.yaml
```

The metal-operator will pick up the `BMC` resource, connect to the remote hardware, and create a matching `Server` resource. Watch the resources come up:

```shell
kubectl get bmc -w
kubectl get server -w
```

You can monitor the manager logs to verify the connection succeeds:

```shell
kubectl logs -n metal-operator-system deployment/metal-operator-controller-manager -c manager -f
```

To tear down the environment:

```shell
make kind-delete
```

#### Optional: Use the debug manager image

To get a shell-accessible manager image with `curl` and `ca-certificates` (useful for diagnosing BMC connectivity), switch the Tilt build target to `manager-debug`:

In `Tiltfile`:

```python
docker_build('controller', '../..', dockerfile='./Dockerfile', only=['ironcore-dev/metal-operator', 'gofish'], target = 'manager-debug')
```

And add the corresponding stage to `Dockerfile`:

```dockerfile
FROM debian:testing-slim AS manager-debug
LABEL source_repository="https://github.com/ironcore-dev/metal-operator"
WORKDIR /
COPY --from=manager-builder /workspace/manager .
COPY config/manager/ignition-template.yaml /etc/metal-operator/ignition-template.yaml
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/manager"]
```
