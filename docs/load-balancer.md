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
