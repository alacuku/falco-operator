# Centralized OCI Artifact Fetching via Artifact Server

## Problem

Each per-node artifact-operator sidecar previously fetched OCI artifacts (plugins, rulesfiles)
directly from the external registry. In a large cluster this meant:

- Every node required outbound internet access to the OCI registry.
- The same artifact was pulled N × M times (N artifacts × M nodes) on every version update.
- Plugin binaries are platform-specific and can be 5–50 MB each.

## Solution

The **falco-operator** (a cluster-singleton Deployment) now acts as a centralized artifact
cache and HTTP file server. Per-node artifact-operator sidecars download OCI artifacts from
the operator over in-cluster HTTP instead of hitting the registry directly.

```
[OCI Registry]
     ↑ pull once per (artifact, digest, os/arch)
[falco-operator Deployment]
├── Controller Manager (existing)
└── Artifact HTTP Server  :8082
      Cache: /var/cache/falco-operator/artifacts  (emptyDir)
      GET /v1/artifacts/plugins/{namespace}/{name}?os=linux&arch=amd64
      GET /v1/artifacts/rulesfiles/{namespace}/{name}
         ↓ HTTP (in-cluster Service: falco-operator:8082)
[artifact-operator sidecar] × N nodes
```

Scope:

- **Centralized**: OCI-sourced plugins and rulesfiles.
- **Unchanged**: Inline rules, ConfigMap rules, generated plugin config YAML — these are already
  Kubernetes-native or in-memory and need no registry access.

## Architecture

### falco-operator: HTTP artifact server (`internal/pkg/artifactserver`)

A lightweight HTTP server embedded in the falco-operator process.

**Endpoints**

| Method | Path | Query params | Description |
|--------|------|-------------|-------------|
| GET | `/v1/artifacts/plugins/{namespace}/{name}` | `os`, `arch` | Returns the raw plugin tar.gz layer |
| GET | `/v1/artifacts/rulesfiles/{namespace}/{name}` | — | Returns the raw rulesfile tar.gz layer |

**Per-request flow**

1. Parse namespace + name from path; `os`/`arch` from query (default: `runtime.GOOS/GOARCH`).
2. Look up the `Plugin` or `Rulesfile` CRD to get `spec.ociArtifact`.
3. Resolve the current OCI digest via a cheap `HEAD`-equivalent call (`puller.ResolveDigest`).
4. Check disk cache: `{cacheDir}/{type}/{namespace}/{name}/{digest}-{os}-{arch}.tar.gz`.
5. **Cache miss**: pull from the OCI registry, write to the cache file.
6. Stream the cache file to the HTTP response (`Content-Type: application/octet-stream`).

**Concurrent cold-cache handling**

All requests for the same artifact key (`{namespace}/{name}/{digest}/{os}/{arch}`) are
deduplicated via `golang.org/x/sync/singleflight`:

- Exactly one goroutine executes the OCI pull.
- All other concurrent callers block inside `singleflight.Do` and receive the result when the
  single pull completes.
- A pull error is propagated to all waiters; they requeue and retry independently.

**Auth**

The server reads the `OCIArtifact.registry.auth.secretRef` from the CRD and fetches the
referenced Secret using its Kubernetes client (same pattern as `artifact.Manager.fetchOCIAuthSecret`).

**Cache lifetime**

The cache directory is backed by an `emptyDir` volume on the falco-operator pod. Files survive
in-place upgrades (rollout restarts preserve the emptyDir within a pod lifetime) but are cleared
on pod eviction/restart. On a cold cache, the first request triggers an OCI pull; subsequent
requests are served from disk.

### artifact-operator: Manager HTTP mode (`internal/pkg/artifact`)

The `artifact.Manager` gains a new `WithArtifactServer(url string)` option. When set,
`StoreFromOCI` fetches the tar.gz layer from the operator's HTTP server instead of pulling from
the registry directly:

```go
// existing path (no change when option is unset)
payload, err = am.pullOCIFile(ctx, ref, artifactType, artifact, creds)

// new path (when WithArtifactServer is configured)
payload, err = am.fetchFromArtifactServer(ctx, name, artifactType, artifact)
```

`fetchFromArtifactServer` builds the URL from the artifact type, namespace, and name, makes a
plain HTTP GET, reads the body, and unpacks it with the existing
`common.ExtractSingleFileFromTarGz` helper. The rest of `StoreFromOCI` (source-signature change
detection, `installOCIFile`, priority rename) is unchanged.

### Configuration

**falco-operator** gains two new CLI flags (set by the Helm chart):

| Flag | Default | Description |
|------|---------|-------------|
| `--artifact-serve-addr` | `:8082` | Address the artifact HTTP server binds to |
| `--artifact-cache-dir` | `/var/cache/falco-operator/artifacts` | Disk cache directory |

**artifact-operator** reads the artifact server URL from the `ARTIFACT_SERVER_URL` environment
variable (injected by the falco-operator when it builds the sidecar container spec). When the
variable is empty the operator falls back to direct OCI pulling — fully backward compatible.

### Helm chart

- **Deployment**: adds `OPERATOR_NAMESPACE` (DownwardAPI), the new flags, and an `emptyDir`
  volume + mount for the artifact cache.
- **Service** (new): exposes port `8082` as `artifact-server` on the `falco-operator` Service so
  artifact-operator sidecars can reach it at
  `http://falco-operator.{operatorNamespace}.svc.cluster.local:8082`.

## Deployment Diagram

```
falco-operator pod
┌──────────────────────────────────────────┐
│  manager binary                          │
│  ├── controller-runtime manager          │
│  │     ├── Plugin aggregator             │
│  │     ├── Rulesfile aggregator          │
│  │     └── ...                           │
│  └── artifact HTTP server :8082          │
│        └── emptyDir cache               │
│              /var/cache/falco-operator/  │
└──────────────────────────────────────────┘
         ↑ OCI pull (once per digest)
[OCI Registry]

Falco DaemonSet pod (per node)
┌──────────────────────────────────────────┐
│  falco container                         │
│  artifact-operator sidecar               │
│    env ARTIFACT_SERVER_URL=http://...    │
│    ├── plugin reconciler                 │
│    │     └── Manager.StoreFromOCI        │
│    │           → HTTP GET :8082/plugins  │
│    └── rulesfile reconciler              │
│          └── Manager.StoreFromOCI        │
│                → HTTP GET :8082/rules    │
└──────────────────────────────────────────┘
```

## File Index

| File | Change |
|------|--------|
| `internal/pkg/artifactserver/server.go` | **NEW** — HTTP server, cache, singleflight |
| `internal/pkg/artifact/manager.go` | Add `WithArtifactServer` option + `fetchFromArtifactServer` + branch in `StoreFromOCI` |
| `cmd/instance/main.go` | Start artifact server, read `OPERATOR_NAMESPACE`, call `resources.SetArtifactServerURL` |
| `cmd/artifact/main.go` | Read `ARTIFACT_SERVER_URL` env var, pass to reconcilers |
| `internal/pkg/resources/falco.go` | `SetArtifactServerURL` injects `ARTIFACT_SERVER_URL` into sidecar env |
| `controllers/artifact/plugin/controller.go` | Pass artifact server URL to `NewManagerWithOptions` |
| `controllers/artifact/rulesfile/controller.go` | Same |
| `chart/falco-operator/templates/deployment.yaml` | Add env, volume, mount |
| `chart/falco-operator/templates/service.yaml` | **NEW** — expose port 8082 |
