# Configuration and Logging

## Configuration Sources

Long-running commands support TOML configuration:

```bash
multigent --config ./config.toml start
multigent --config ./config.toml api serve
multigent --config ./config.toml daemon install
```

Priority order:

```text
CLI flags > environment variables > config.toml > defaults
```

The global config path can also be set with:

```bash
MULTIGENT_CONFIG=/etc/multigent/config.toml
```

See [config.example.toml](../config.example.toml).

> **Strict Configuration Parsing**: Starting from this version, unknown sections or unknown keys in configuration files are strictly rejected with an error on startup to prevent accidental configuration typos from silently failing.

Currently covered by config:

- workspace directory (`[workspace]`)
- server listen address (`[server]`)
- API key (`[auth]`)
- SMTP invitation delivery (`[smtp]`)
- service logging (`[logging]`)
- runtime image & region (`[runtime]`)
- sandbox direct host execution and self-hosted E2B API URL (`[sandbox]`, `[sandbox.e2b]`)
- remote playbook registries (`[playbooks]`)
- package and module registries (`[registries]`): `npm`, `pip`, `go`, `go_sumdb`, `go_private`
  - *Note*: `go_sumdb` is a mandatory requirement whenever `go` (`GOPROXY`) is configured, preventing unintended queries to public checksum databases while ensuring module integrity.
- proxy configuration (`[network]`): `https_proxy`, `http_proxy`, `no_proxy`

## Registries and Runtime Probe

For enterprise or intranet deployments, private package registries and network proxies can be declared in `[registries]` and `[network]` sections.
Transport variables are forwarded into sandbox containers using Docker `-e KEY` inheritance, ensuring credentials never appear in Docker arguments or logs.

To verify that the runtime container can successfully reach and download artifacts from configured registries through the configured network links, use the probe subcommand:

```bash
multigent runtime probe
# Or format as JSON:
multigent runtime probe --format=json
# Or specify a custom config file:
multigent --config /path/to/multigent.conf runtime probe
```

The probe command runs in an ephemeral container with the same unprivileged non-root UID/GID as the agent sandbox, with zero workspace mounts and zero credentials.

## Logging Policy

Multigent has two log types:

- Service logs: API/web process lifecycle and platform events.
- Agent run logs: per-agent stdout/stderr and execution transcript.

Service logs:

- Default path: `~/.multigent/logs/multigent.log`
- Default format: JSON
- Default level: `info`
- Default rotation: one backup file at `<log>.1`
- Default max size: `10 MB`

Recommended production config:

```toml
[logging]
file = "/var/log/multigent/multigent.log"
level = "info"
format = "json"
max_size_mb = 100
stderr = false
```

Agent run logs remain scoped to each agent workspace:

```text
projects/<project>/agents/<agent>/.multigent/runs/
```

Those logs preserve raw agent output for replay/debugging. Service logs should be
structured and easy to ship to a central collector; run logs are artifacts tied
to an execution record.

## Environment Variables

Logging:

- `MULTIGENT_LOG_FILE`
- `MULTIGENT_LOG_LEVEL`
- `MULTIGENT_LOG_FORMAT`
- `MULTIGENT_LOG_MAX_SIZE_MB`
- `MULTIGENT_LOG_STDERR`

Compatibility:

- `MULTIGENT_LOG_MAX_SIZE` is still accepted as bytes for daemon installs.

Server:

- `MULTIGENT_SERVER_ADDR`
- `MULTIGENT_API_ADDR`
- `MULTIGENT_WEB_API_KEY`

SMTP:

- `MULTIGENT_SMTP_HOST`
- `MULTIGENT_SMTP_PORT`
- `MULTIGENT_SMTP_USERNAME`
- `MULTIGENT_SMTP_PASSWORD`
- `MULTIGENT_SMTP_FROM`
- `MULTIGENT_SMTP_FROM_NAME`
- `MULTIGENT_SMTP_TLS`

Sandbox:

- `MULTIGENT_E2B_API_URL`

Playbooks:

- `MULTIGENT_PLAYBOOK_REGISTRY_URLS`

Registries:

- `NPM_CONFIG_REGISTRY`
- `PIP_INDEX_URL`
- `GOPROXY`
- `GOSUMDB`
- `GOPRIVATE`

Network / Proxies:

- `HTTPS_PROXY` / `https_proxy`
- `HTTP_PROXY` / `http_proxy`
- `NO_PROXY` / `no_proxy`
