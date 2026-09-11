# multigent JVM runtime image (profile: jvm21)
#
# Extends the stable runtime base with a pinned Temurin JDK 21 so Spring Boot
# and other JVM projects never rely on agents downloading a JDK at task time.
# Also carries the enterprise-trust plumbing required by intranet installs:
#   - a drop-in directory for corporate CA certificates, merged at build time
#     (mount a directory at /opt/multigent/trust/ro during a supervised build
#     or bake certs into a derived image — runtime containers are non-root and
#     must never run update-ca-certificates themselves);
#   - NODE_EXTRA_CA_CERTS / PIP_CERT pointing at the merged bundle, because
#     Node (nodesource build) and pip (certifi) do not read the OS truststore.
#
# Build:
#   docker build -t multigent/runtime-jvm21:2026.9.1 \
#                -f docker/runtime-jvm21/Dockerfile .
#
# Build with Chinese mirrors:
#   docker build --build-arg CN_MIRROR=1 \
#                -t multigent/runtime-jvm21:2026.9.1 \
#                -f docker/runtime-jvm21/Dockerfile .
#
# Build with corporate CAs (PEM files in ./trust):
#   docker build --build-context trust=./trust \
#                -t multigent/runtime-jvm21:2026.9.1 \
#                -f docker/runtime-jvm21/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.26.4 AS runtime-cli-builder

ARG TARGETOS
ARG TARGETARCH
ARG CN_MIRROR=0
ARG GO_MODULE_PROXY=""

WORKDIR /src
COPY go.mod go.sum ./
RUN if [ -n "$GO_MODULE_PROXY" ]; then \
      go env -w GOPROXY="$GO_MODULE_PROXY"; \
    elif [ "$CN_MIRROR" = "1" ]; then \
      go env -w GOPROXY=https://goproxy.cn,direct GONOSUMDB='*'; \
    fi
RUN go mod download
COPY cmd/mga ./cmd/mga
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/mga ./cmd/mga

FROM ubuntu:24.04

ARG CN_MIRROR=0
ARG GO_VERSION=1.26.4
ARG TARGETARCH
ARG INCLUDE_DEV_TOOLS=0
ARG JDK_VERSION=21.0.5+11

ENV DEBIAN_FRONTEND=noninteractive \
    NODE_VERSION=22 \
    IS_SANDBOX=1 \
    CODEX_UNSAFE_ALLOW_NO_SANDBOX=1 \
    MULTIGENT_TOOLCHAIN_HOME=/opt/multigent/toolchains \
    NPM_CONFIG_PREFIX=/opt/multigent/toolchains/npm

RUN if [ "$CN_MIRROR" = "1" ]; then \
      if [ -f /etc/apt/sources.list.d/ubuntu.sources ]; then \
        sed -i 's|http://archive.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g; \
                s|http://security.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g' \
            /etc/apt/sources.list.d/ubuntu.sources; \
      else \
        sed -i 's|http://archive.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g; \
                s|http://security.ubuntu.com/ubuntu|https://mirrors.aliyun.com/ubuntu|g' \
            /etc/apt/sources.list; \
      fi; \
    fi

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    git \
    jq \
    make \
    unzip \
    openssh-client \
    ripgrep \
    fzf \
    python3 \
    python3-pip \
    python3-venv \
    sqlite3 \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_VERSION}.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN if [ "$CN_MIRROR" = "1" ]; then \
      npm config set registry https://registry.npmmirror.com; \
    fi

# ── Temurin JDK 21 ────────────────────────────────────────────────────────────
# Pinned release, installed under /opt/multigent/jdk and exported as JAVA_HOME.
# apt-openjdk is avoided deliberately: the version drifts between Ubuntu point
# releases and breaks reproducibility; Temurin tarballs give a fixed, auditable
# JDK. ca-certificates-java is therefore NOT auto-wired — see trust handling
# below for how the JVM truststore is produced.
RUN case "$TARGETARCH" in \
      amd64) jdk_arch="x64" ;; \
      arm64) jdk_arch="aarch64" ;; \
      *) echo "unsupported JDK architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    jdk_tag="jdk-${JDK_VERSION}"; \
    if [ "$CN_MIRROR" = "1" ]; then \
      jdk_url="https://mirrors.tuna.tsinghua.edu.cn/Adoptium/21/jdk/${JDK_VERSION}/x64/OpenJDK21U-jdk_${jdk_arch}_linux_hotspot_$(echo "$JDK_VERSION" | tr '+' '_').tar.gz"; \
    else \
      jdk_url="https://github.com/adoptium/temurin21-binaries/releases/download/${jdk_tag}/OpenJDK21U-jdk_${jdk_arch}_linux_hotspot_$(echo "$JDK_VERSION" | tr '+' '_').tar.gz"; \
    fi; \
    mkdir -p /opt/multigent/jdk \
    && curl -fsSL "$jdk_url" | tar -C /opt/multigent/jdk --strip-components=1 -xzf - \
    && /opt/multigent/jdk/bin/java -version

# ── Enterprise trust material ────────────────────────────────────────────────
# Optional build context "trust" (default: ./trust next to this Dockerfile,
# shipped empty; pass --build-context trust=/path/to/certs to supply PEMs)
# carries corporate root CA files. They are merged into the system bundle at
# build time (root, safe), and the resulting bundle path is exported for
# Node/pip, which do not consult the OS store. The JVM truststore is merged
# from the same bundle with keytool; apt-era ca-certificates-java hooks do not
# run for a tarball JDK. Without certs the image keeps stock Mozilla roots and
# behaves exactly like runtime-base.
COPY --from=trust / /tmp/multigent-trust/
RUN mkdir -p /usr/local/share/ca-certificates/corp; \
    if ls /tmp/multigent-trust/*.pem /tmp/multigent-trust/*.crt >/dev/null 2>&1; then \
      cp /tmp/multigent-trust/*.pem /tmp/multigent-trust/*.crt \
         /usr/local/share/ca-certificates/corp/; \
    fi; \
    rm -rf /tmp/multigent-trust; \
    update-ca-certificates; \
    if ls /usr/local/share/ca-certificates/corp/* >/dev/null 2>&1; then \
      keytool -importcert -noprompt -storepass changeit \
        -alias multigent-corp-bundle \
        -file /etc/ssl/certs/ca-certificates.crt \
        -keystore /opt/multigent/jdk/lib/security/cacerts; \
    fi

# Node and pip keep their own trust stores; point both at the merged system
# bundle so one update-ca-certificates run covers every client.
ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt \
    PIP_CERT=/etc/ssl/certs/ca-certificates.crt

RUN if [ "$INCLUDE_DEV_TOOLS" = "1" ]; then \
      apt-get update \
      && apt-get install -y --no-install-recommends build-essential \
      && rm -rf /var/lib/apt/lists/*; \
      case "$TARGETARCH" in \
        amd64|arm64) go_arch="$TARGETARCH" ;; \
        *) echo "unsupported Go architecture: $TARGETARCH" >&2; exit 1 ;; \
      esac; \
      if [ "$CN_MIRROR" = "1" ]; then \
        go_url="https://mirrors.aliyun.com/golang/go${GO_VERSION}.linux-${go_arch}.tar.gz"; \
      else \
        go_url="https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz"; \
      fi; \
      curl -fsSL "$go_url" | tar -C /usr/local -xzf -; \
      if [ "$CN_MIRROR" = "1" ]; then \
        /usr/local/go/bin/go env -w GOPROXY=https://goproxy.cn,direct GONOSUMDB='*'; \
      fi; \
    fi

ENV PATH="/opt/multigent/toolchains/npm/bin:/opt/multigent/jdk/bin:/usr/local/go/bin:/root/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    JAVA_HOME="/opt/multigent/jdk" \
    GOPATH="/root/go" \
    GOMODCACHE="/root/go/pkg/mod"

RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) \
         signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
         https://cli.github.com/packages stable main" \
        | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update \
    && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/multigent/toolchains/npm /opt/multigent/toolchains/markers \
        /opt/multigent/mga/bin \
    && printf '#!/bin/sh\necho "multigent: binary not mounted" >&2\nexit 1\n' \
        > /usr/local/bin/multigent \
    && chmod +x /usr/local/bin/multigent

COPY --from=runtime-cli-builder /out/mga /opt/multigent/mga/bin/mga
RUN chmod 0755 /opt/multigent/mga/bin/mga \
    && /opt/multigent/mga/bin/mga version

ENV PATH="/opt/multigent/mga/bin:${PATH}"

WORKDIR /workspace

CMD ["/bin/bash"]
