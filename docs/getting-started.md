# Getting Started

## Prerequisites

- A Kubernetes cluster running on Apache CloudStack
- CloudStack API credentials (API key and secret key) with permissions to manage VMs and load balancers
- `kubectl` configured to access your cluster
- `helm` (optional, for Helm-based installation)

## Installation

### Helm Chart (recommended)

Install from the bundled chart in [`charts/cloud-controller-manager/`](../charts/cloud-controller-manager/):

```bash
helm install cloud-controller-manager charts/cloud-controller-manager/ \
  --namespace kube-system \
  --set cloudConfig.global.api-url="https://cloudstack.example.com/client/api" \
  --set cloudConfig.global.api-key="YOUR_API_KEY" \
  --set cloudConfig.global.secret-key="YOUR_SECRET_KEY"
```

See [`charts/cloud-controller-manager/values.yaml`](../charts/cloud-controller-manager/values.yaml) for the full list of configurable values, or refer to the [Configuration](configuration.md) guide.

### Kubernetes Manifests

1. Create a `cloud-config` file:

    ```ini
    [Global]
    api-url    = https://cloudstack.example.com/client/api
    api-key    = YOUR_API_KEY
    secret-key = YOUR_SECRET_KEY
    ```

    See [Configuration](configuration.md) for all available fields.

2. Create the secret:

    ```bash
    kubectl -n kube-system create secret generic cloudstack-secret --from-file=cloud-config
    ```

3. Apply RBAC and deployment manifests:

    ```bash
    kubectl apply -f deploy/k8s/rbac.yaml
    kubectl apply -f deploy/k8s/deployment.yaml
    ```

Prebuilt container images are available at [ghcr.io/leaseweb/cloudstack-kubernetes-provider](https://github.com/leaseweb/cloudstack-kubernetes-provider/pkgs/container/cloudstack-kubernetes-provider).

## Node Setup

**The node name must match the CloudStack VM hostname** so the controller can fetch and assign metadata.

It is recommended to launch `kubelet` with the following flag:

```
--register-with-taints=node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule
```

This marks the node as uninitialized, causing the CCM to automatically apply metadata labels from CloudStack:

| Label | Value |
|-------|-------|
| `kubernetes.io/hostname` | Instance name |
| `node.kubernetes.io/instance-type` | Compute offering |
| `topology.kubernetes.io/zone` | CloudStack zone |
| `topology.kubernetes.io/region` | CloudStack zone |

To trigger this process manually on an existing node:

```bash
kubectl taint nodes <node-name> node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule
```
