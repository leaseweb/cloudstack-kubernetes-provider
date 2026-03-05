# Development

## Requirements

- Go 1.24+
- Docker (for building container images)

## Building

A Makefile is provided that sets build flags with automatically derived version information.

```bash
make              # Build the cloudstack-ccm binary
make docker       # Build and tag the container image
make lint         # Run linting (fmt, vet, golangci-lint)
make test         # Run tests, vet, and format checks
```

## Local Testing

You can run the CCM locally against a Kubernetes cluster and CloudStack API:

```bash
./cloudstack-ccm --cloud-provider cloudstack --cloud-config /path/to/cloud-config --master <k8s-apiserver>
```

Replace `<k8s-apiserver>` with the hostname or address of your Kubernetes API server.

If you don't have a CloudStack installation available, you can use the CloudStack [simulator image](https://hub.docker.com/r/cloudstack/simulator) for dry-run testing.
