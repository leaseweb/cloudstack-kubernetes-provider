Apache CloudStack Kubernetes Provider Changelog
====

v1.8.0 (2026-04-08)
---
### Features

- feat: K8s 1.35 Support and K8s Dependencies update to v0.34.3
- feat(lint): Update golangci-lint to 2.7.2

v1.7.1 (2026-03-20)
---

### Bug Fixes

- Filter LB rules by name prefix in getLoadBalancerByID
- Default to TCP protocol when proto empty

v1.7.0 (2026-03-05)
---

### Features

- Add load-balancer-id and network-id annotations with ID-based lookup
- Make load balancer address annotation user-settable with keep-ip support
- Clean up load balancer annotations in EnsureLoadBalancerDeleted

### Bug Fixes

- Prevent public IP orphaning under partial failure conditions
- Validate target IP before tearing down existing load balancer
- Address multiple bugs found during code audit
- Enable gosec and wrapcheck linters and fix issues

### Refactoring

- Remove live IP reassignment support from load balancer

### Dependencies

- Updated K8s deps to v1.33.9
- Updated mock to v0.6.0, testify to v1.11.1

### Maintenance

- Move documentation to docs/ folder and update README
- Rename CHANGES.md to CHANGELOG.md and add missing releases
- Remove unused scripts (get_kubernetes_deps.sh, performrelease.sh)

v1.6.2 (2026-02-20)
---

### Bug Fixes

- Reconcile host membership in EnsureLoadBalancer for existing rules

### Maintenance

- Updated golangci-lint to v1.64.8

v1.6.1 (2026-02-16)
---

### Bug Fixes

- Associate specific LB IP when defined in the service spec
- Fix LB reconciles during node rollouts

v1.6.0 (2025-10-29)
---

### Dependencies

- Updated K8s deps to v1.33.3

v1.5.0 (2025-07-30)
---

### Dependencies

- Updated K8s deps to v1.32.7

v1.4.0 (2025-05-08)
---

### Features

- Implement KEP-1860 - support for LoadBalancerIPMode

### Bug Fixes

- Correct Dockerfile and add new GitLab CI step

### Dependencies

- Updated K8s deps to v1.31.8

### Maintenance

- Add lint make target, golangci-lint installation, and lint fixes
- Add lint step to GitHub Actions workflow

v1.3.0 (2024-10-15)
---

### Dependencies

- Updated K8s deps to v1.30.5

v1.2.1 (2024-10-02)
---

### Bug Fixes

- Default to allow-all if no allowed CIDRs are defined

v1.2.0 (2024-09-24)
---

### Features

- Add support for LoadBalancerSourceRanges annotation and better LB rule names
- Add an event recorder to emit events on Service objects to inform or warn users

### Bug Fixes

- Fix panic when listVirtualMachines returns VMs without NICs
- Improve logging

### Dependencies

- Updated K8s deps to v1.29.9
- Updated to Go 1.22

v1.1.0 (2024-05-10)
---

### Features

- Use structured logging and klog.KObj

### Bug Fixes

- Make sure all the required functions for the cloudprovider interfaces are implemented
- Release loadbalancer IP even if it is Spec.LoadBalancerIP
- Set provider name and disable route controller

### Refactoring

- Move cloud package into subdir cloudstack

### Dependencies

- Updated K8s deps to v1.29.4
- Updated cloudstack-go to v2.16.0, testify to v1.9.0

v1.0.1 (2024-03-22)
---

### Dependencies

- Updated Kubernetes deps to v1.26.15 to resolve a moderate level security issue in protobuf (CVE-2024-24786)

v1.0.0 (2024-03-13)
----

This is the first release of the CloudStack Kubernetes Provider.
