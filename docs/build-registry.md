# Build Registry

The make tasks assume a local registry is accessible.  When the
quay.holos.localhost or k3d-registry.holos.localhost registries are used the
buildx container cannot resolve them by default.  Fix this with host networking
for buildx.

```bash
docker buildx rm loving_brattain || true
```

```bash
docker buildx create \
  --name holos-builder \
  --driver docker-container \
  --driver-opt network=host \
  --use
```

```bash
docker buildx inspect --bootstrap
```
