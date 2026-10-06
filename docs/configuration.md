# Configuration

## Cloud Config

The CCM reads its CloudStack credentials from a `cloud-config` file in INI format. When using Kubernetes manifests, this file is mounted from a secret. When using the Helm chart, it is generated from the `cloudConfig` values.

```ini
[Global]
api-url       = <CloudStack API URL>
api-key       = <CloudStack API Key>
secret-key    = <CloudStack API Secret>
project-id    = <CloudStack Project UUID (optional)>
zone          = <CloudStack Zone Name (optional)>
ssl-no-verify = <Disable SSL certificate validation: true or false (optional)>

api-rate-limit-qps   = <Maximum CloudStack API requests per second (optional, default 10)>
api-rate-limit-burst = <Requests that may exceed the QPS for a short time (optional, default 20)>
vm-cache-ttl         = <Seconds to cache the VM list for load balancer host lookups (optional, default 30)>
```

| Field | Required | Description |
|-------|----------|-------------|
| `api-url` | Yes | Full URL to the CloudStack API endpoint |
| `api-key` | Yes | API key for authentication |
| `secret-key` | Yes | Secret key for authentication |
| `project-id` | No | UUID of the CloudStack project. Required when nodes are in a project |
| `zone` | No | CloudStack zone name to scope operations to |
| `ssl-no-verify` | No | Set to `true` to skip TLS certificate verification |
| `api-rate-limit-qps` | No | Maximum number of CloudStack API requests per second, including async job polls. Default `10`. Set to `0` to disable the rate limit. Must not be negative |
| `api-rate-limit-burst` | No | Number of requests that may exceed the QPS for a short time. Must be at least `1` when the rate limit is enabled. Default `20` |
| `vm-cache-ttl` | No | Number of seconds that the list of VMs is cached for load balancer host lookups, so that many services share one `listVirtualMachines` call. The list is fetched again when it does not match all nodes, or when a node is newer than the list. Default `30`. Set to `0` to disable the cache |

The API credentials need permission to fetch VM information and manage load balancers in the project or domain where the nodes reside.

### API rate limiting

When the CCM starts, it reconciles all `LoadBalancer` services at once. On clusters with many services, this can send many requests to the CloudStack management server. The `api-rate-limit-qps` and `api-rate-limit-burst` settings limit the request rate of the CCM.

If API throttling (`api.throttling.enabled`) is enabled in CloudStack and a request is rejected with HTTP 429, the CCM pauses all its requests for an increasing delay, and retries the request up to 5 times. CloudStack does not execute rejected requests, so a retry is safe. Set `api-rate-limit-qps` below the `api.throttling.max` / `api.throttling.interval` of the CCM account to prevent rejections.

## Helm Chart Values

The chart is located at [`charts/cloud-controller-manager/`](../charts/cloud-controller-manager/). Below are the key values. See [`values.yaml`](../charts/cloud-controller-manager/values.yaml) for the full reference.

| Value | Default | Description |
|-------|---------|-------------|
| `replicaCount` | `1` | Number of CCM replicas. Leader election is automatically enabled when > 1 |
| `image.repository` | `ghcr.io/leaseweb/cloudstack-kubernetes-provider` | Container image repository |
| `image.tag` | Chart `appVersion` | Container image tag |
| `nodeSelector` | `node-role.kubernetes.io/control-plane: ""` | Node selector for scheduling |
| `tolerations` | Uninitialized, CriticalAddonsOnly, control-plane, not-ready | Pod tolerations |
| `enabledControllers` | `[cloud-node, cloud-node-lifecycle, route, service]` | List of controllers to enable |
| `cluster.name` | `kubernetes` | Cluster name passed to the CCM |
| `logVerbosityLevel` | `2` | klog verbosity level |
| `hostNetwork` | `true` | Run pods with host networking |
| `dnsPolicy` | `ClusterFirstWithHostNet` | DNS policy (should match `hostNetwork`) |
| `secret.enabled` | `true` | Mount cloud-config from a Kubernetes secret |
| `secret.create` | `true` | Create the secret from `cloudConfig` values. Set to `false` to use a pre-existing secret |
| `secret.name` | `cloud-config` | Name of the secret |
| `cloudConfig.global.api-url` | `""` | CloudStack API URL |
| `cloudConfig.global.api-key` | `""` | CloudStack API key |
| `cloudConfig.global.secret-key` | `""` | CloudStack secret key |
| `serviceMonitor` | `{}` | Prometheus ServiceMonitor configuration |
| `priorityClassName` | `system-node-critical` | Pod priority class |
