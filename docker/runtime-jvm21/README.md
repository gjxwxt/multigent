# runtime-jvm21 (profile: jvm21)

Managed JVM runtime image: Ubuntu 24.04 + Node 22 + Python 3 + pinned Temurin
JDK 21 (`JAVA_HOME=/opt/multigent/jdk`). Extends `runtime-base`; everything the
base image provides (mga, toolchain bootstrap, gh CLI) is included.

## Build

```bash
# plain (public Mozilla roots — same trust behavior as runtime-base)
docker build -t multigent/runtime-jvm21:2026.9.1 \
             -f docker/runtime-jvm21/Dockerfile .

# with Chinese mirrors
docker build --build-arg CN_MIRROR=1 \
             -t multigent/runtime-jvm21:2026.9.1 \
             -f docker/runtime-jvm21/Dockerfile .

# with corporate root CAs (PEM files in ./trust)
docker build --build-context trust=$(pwd)/trust \
             -t multigent/runtime-jvm21:2026.9.1 \
             -f docker/runtime-jvm21/Dockerfile .
```

The `trust` build context is **mandatory** to pass (any directory works, empty
means "no corporate CAs"). Its `*.pem` / `*.crt` files are merged into the
system bundle via `update-ca-certificates`, and the merged bundle is imported
into the JDK cacerts with keytool.

## Trust model (why not runtime CA installation)

Sandbox containers run as a non-root host user (Linux default), so runtime
`update-ca-certificates` is impossible by design. CA material is therefore a
**build-time** concern: rotate CAs by rebuilding and re-pushing a
version-tagged image (bump `RuntimeJVM21Version` in internal/sandbox/profile.go
and rebuild, immutable tags — floating `latest` is forbidden in production per
the intranet runtime plan; `multigent sandbox prepare` records the pulled
digest). `NODE_EXTRA_CA_CERTS`
and `PIP_CERT` are pre-wired to the merged bundle because Node (nodesource
build) and pip (certifi) do not read the OS truststore.

## Selecting the profile

Projects/workflows declare it explicitly — never by sniffing build files:

```yaml
# project runtime config
sandbox:
  docker:
    profile: jvm21
```

or warm it up front:

```bash
multigent sandbox prepare --profile jvm21
```
