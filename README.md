# CloudStack Kubernetes Provider

[![](https://img.shields.io/github/v/release/leaseweb/cloudstack-kubernetes-provider?filter=v*&style=flat-square "Release")](https://github.com/apache/cloudstack-kubernetes-provider/releases)
[![](https://img.shields.io/badge/license-Apache%202.0-blue.svg?color=%23282661&logo=apache&style=flat-square "Apache 2.0 license")](/LICENSE-2.0)
[![](https://img.shields.io/badge/language-Go-%235adaff.svg?logo=go&style=flat-square "Go language")](https://golang.org)

A Kubernetes Cloud Controller Manager (CCM) for Apache CloudStack. It provides node metadata, lifecycle management, and load balancer integration for Kubernetes clusters running on CloudStack.

## Quick Start

```bash
helm install cloud-controller-manager charts/cloud-controller-manager/ \
  --namespace kube-system \
  --set cloudConfig.global.api-url="https://cloudstack.example.com/client/api" \
  --set cloudConfig.global.api-key="YOUR_API_KEY" \
  --set cloudConfig.global.secret-key="YOUR_SECRET_KEY"
```

## Documentation

| Guide | Description |
|-------|-------------|
| [Getting Started](docs/getting-started.md) | Installation via Helm or Kubernetes manifests, node setup |
| [Configuration](docs/configuration.md) | Cloud config reference, Helm chart values |
| [Load Balancer](docs/load-balancer.md) | Protocols, annotations, IP management |
| [Development](docs/development.md) | Building, testing, local development |

## Development

```bash
make              # Build
make docker       # Build container image
make lint         # Lint
make test         # Test
```

See [docs/development.md](docs/development.md) for details.

## Copyright

Copyright 2019 The Apache Software Foundation

This product includes software developed at
The Apache Software Foundation (http://www.apache.org/).
