# Load Balancer

## Overview

When you create a Kubernetes `Service` with `type: LoadBalancer`, the CCM provisions CloudStack load balancer rules and associates a public IP address with the service. The load balancer name is derived from the service name, namespace, and protocol.

## Protocols

The CCM supports three protocols for load balancer rules:

| Protocol | Description |
|----------|-------------|
| **TCP** | Standard TCP load balancing |
| **UDP** | UDP load balancing (CloudStack 4.6+) |
| **TCP-Proxy** | TCP with [PROXY protocol](https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt) header injection (CloudStack 4.6+) |

Since kube-proxy does not support PROXY protocol or UDP forwarding, these protocols should target pods directly. Deploy your application as a DaemonSet and use `hostPort` on the container port to bypass kube-proxy.

Example ingress controller configurations are provided in [`deploy/ingress-sample/`](../deploy/ingress-sample/):

- `traefik-ingress-controller.yml` - Traefik with PROXY protocol
- `nginx-ingress-controller-patch.yml` - Patch for the nginx ingress controller to enable PROXY protocol

> **Important:** The service running in the pod must support the chosen protocol. Do not enable TCP-Proxy when the service only supports regular TCP.

## Annotations Reference

All annotations use the prefix `service.beta.kubernetes.io/`.

| Annotation | Type | Description |
|------------|------|-------------|
| `cloudstack-load-balancer-proxy-protocol` | string | Enable PROXY protocol on TCP ports. The value specifies which ports to enable it on |
| `cloudstack-load-balancer-hostname` | string | Hostname for in-cluster access when using PROXY protocol. Workaround for [kubernetes/kubernetes#66607](https://github.com/kubernetes/kubernetes/issues/66607) |
| `cloudstack-load-balancer-address` | string | Request a specific IP address for the load balancer. Replaces the deprecated `spec.loadBalancerIP` field |
| `cloudstack-load-balancer-keep-ip` | bool | When set to `"true"`, prevents the public IP from being released when the service is deleted |
| `cloudstack-load-balancer-id` | string | (Managed) CloudStack public IP UUID. Set automatically by the CCM for efficient ID-based lookups |
| `cloudstack-load-balancer-network-id` | string | (Managed) CloudStack network UUID. Set automatically by the CCM together with `load-balancer-id` |

## IP Management

### Requesting a specific IP

Set the `cloudstack-load-balancer-address` annotation to request a specific public IP:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    service.beta.kubernetes.io/cloudstack-load-balancer-address: "203.0.113.10"
spec:
  type: LoadBalancer
  ports:
    - port: 80
      targetPort: 8080
```

> This replaces the deprecated `spec.loadBalancerIP` field, which is still supported as a fallback.

### Retaining an IP after service deletion

To prevent the public IP from being released when the service is deleted, set:

```yaml
metadata:
  annotations:
    service.beta.kubernetes.io/cloudstack-load-balancer-keep-ip: "true"
```

This is useful when you want to recreate a service with the same IP address.

### Changing the IP of an existing service

Live IP reassignment is not supported. To change the IP address of a load balancer:

1. Delete the existing service
2. Create a new service with the desired IP in the `cloudstack-load-balancer-address` annotation

## Large clusters and node rollouts

### How CloudStack applies load balancer changes

All load balancer rules of a network run on the virtual router (VR) of that network. Each change to a rule, such as assigning a VM or removing a VM, makes CloudStack send the **complete** load balancer configuration of the network to the VR again. The VR runs these commands one at a time.

When a node joins or leaves the cluster, every rule changes: one assign or remove per rule, so about one VR command per rule. CloudStack itself also changes every rule when a VM is destroyed: it removes the VM from each rule, one rule at a time. On a network with many rules, a single node replacement can therefore keep the VR busy for a long time. As a rough estimate, count the number of rules × 15–20 seconds, once for the new node and once for the old node.

If node replacements come faster than the VR can process them, the commands queue up. Async jobs then take minutes or hours, VM destroys stay in `Expunging`, and new nodes are added to the rules late.

### Jobs that are still running

When an assign or remove job takes longer than `async-job-timeout`, CloudStack still runs it. The CCM keeps the job ID and does not send the same job again. On the next reconcile, it checks the job, and it only changes the rule again when the job has finished. Until then it logs:

```
error processing service <namespace>/<name> (retrying in 1m0s): … waiting for CloudStack jobs: assign job <job-id> for load balancer rule <rule>: CloudStack job is still running
```

This is expected while the VR is busy. The other rules of the service are still reconciled. When the node sync of the service controller finds a running job, it also records an `UpdateLoadBalancerFailed` event, and its `Successfully updated N out of M load balancers` line counts that service as not updated. This is also expected while jobs are running.

The CCM keeps the running jobs in memory only. After a restart of the CCM, or when another replica becomes the leader, it does not know the running jobs, and it can send a job again once.

### Use a stable set of load balancer nodes

The most effective way to keep node rollouts away from the VR is to keep the nodes that are rolled often out of the load balancer rules. Label them with `node.kubernetes.io/exclude-from-external-load-balancers`, and keep a small, stable set of nodes as load balancer backends:

```sh
kubectl label node <node> node.kubernetes.io/exclude-from-external-load-balancers=true
```

- Use at least 2–3 backend nodes on different hosts, so one node failure does not take the load balancers down.
- With `externalTrafficPolicy: Cluster` (the default), the backend nodes forward traffic to pods on all nodes, so pods can still run anywhere.
- With `externalTrafficPolicy: Local`, traffic only reaches pods on the backend nodes. Make sure the pods of those services run there.
- Roll the backend nodes separately, one node at a time, and wait until the CCM has updated all load balancers before the next one.
- If nodes are created by a tool such as Cluster API, set the label in the node template, so new nodes get it before they join the load balancers.

Then a rollout of the other nodes changes no load balancer rules at all.

### Pace node rollouts

If all nodes are load balancer backends:

- Replace one node only after the previous VM destroy has finished, and the CCM no longer logs `waiting for CloudStack jobs` and has logged `Successfully updated N out of N load balancers to direct traffic to the updated set of nodes`.
- Replace several nodes per step (a larger `maxSurge` / `maxUnavailable`) instead of one node at a time. One assign per rule can add several new nodes, so this needs fewer VR commands per node.

