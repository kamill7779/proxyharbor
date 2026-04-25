# ProxyHarbor

ProxyHarbor is an independent proxy lease, catalog, policy, and gateway service. It is designed as a standalone product with equal consumers: every caller uses the same public API, authentication, rate limits, and audit path.

## v6 principles

- Core services use generic concepts: Tenant, Principal, Subject, ResourceRef, PolicyRef, Lease, Catalog, Gateway, Provider, and AuditEvent.
- No consumer receives privileged control-plane behavior.
- The module can be tagged, released, and operated without depending on any consuming product.

## Quick start

```bash
go run ./cmd/proxyharbor-controller -addr :8080
proxyctl -url http://localhost:8080 health
```

Set a non-empty shared key with `-auth-key` or `PROXYHARBOR_AUTH_KEY`. Protected API calls must send the same value in the `ProxyHarbor-Key` header.

## Proxy registration

Static providers and proxy nodes can be managed through the public API:

```bash
curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -d '{"id":"static-main","type":"static","name":"Static pool","enabled":true}' \
  http://localhost:8080/v1/providers

curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","provider_id":"static-main","endpoint":"http://127.0.0.1:18080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies
```

The gateway supports normal HTTP proxy requests and `CONNECT` tunnels. Use the lease id as the proxy username and the lease password as the proxy password.

## Packaging

- `Dockerfile` builds a static ProxyHarbor binary. Set `--build-arg TARGET=proxyharbor-gateway` to build a role-specific image.
- `docker-compose.yaml` runs the all-in-one service for local development.
- `charts/proxyharbor` contains a minimal Helm chart for alpha deployments.
- `.github/workflows/ci.yaml` runs `go test`, `go vet`, and binary builds.

The alpha storage default is in-memory. SQL migrations are included under `migrations/` as a later durability option.

