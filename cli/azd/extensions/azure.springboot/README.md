# Spring Boot prototype extension

This locally built prototype supports a **prepared** Spring Boot/PostgreSQL
template using `language: java` and Azure Container Apps. It does not discover
arbitrary projects or replace azd's Java, Docker, or hosting providers.

Requires Go 1.26.4 or later, azd 1.35.1 or later, Java 21, PowerShell 7, a
Linux Docker engine, compatible `pack` (verified with 0.40.9), and Azure CLI
for live registry/container operations. SDK dependency: azd v1.35.1.
Extension version: 0.0.1.

## Commands

| Command | Contribution |
|---|---|
| `azd springboot build` | Builds a pinned Paketo Linux/AMD64 image, verifies its architecture, pushes it to the provisioned ACR, and saves `SPRINGBOOT_IMAGE` |
| `azd springboot build --local-only --image notes:test` | Runs the same buildpack containerization without Azure or persistent azd environment changes |
| `azd springboot configure` | Validates database outputs and saves separate developer and managed-identity Spring configuration, preserving explicit overrides or rejecting conflicts |
| `azd springboot run` | Supplies the selected environment's validated configuration directly to the local Maven application; excludes deployed credential settings |
| `azd springboot apply` | Applies the generated managed-identity configuration to the provisioned Container App |

Every application command accepts `--project-dir` pointing to the prepared
template root containing `.springboot-template`, `azure.yaml`, and `api/pom.xml`.
Commands using an azd environment require that directory to match the host's
project; select it with global `--cwd` or run inside it. Only `build --local-only`
can build a different explicit project without touching any azd environment.
`build` also accepts `--pack-path`; otherwise it reads `AZD_PACK_TOOL_PATH`, then
uses `pack` on PATH. Output stays on the injected command writer. The prototype
supports text output only; explicit unsupported output formats fail.

Build with `go build ./...` and test with `go test ./...`. The companion template's
`scripts/build-extension.ps1` creates a checksummed local registry and installs
the binary into an explicitly supplied `AZD_CONFIG_DIR`. No publication is needed.

## Configuration contract

Provisioning supplies `POSTGRES_HOST`, `POSTGRES_DATABASE`, optional
`POSTGRES_PORT` (default `5432`), `POSTGRES_USERNAME` (developer principal), and
`POSTGRES_MANAGED_IDENTITY_USERNAME` (application principal).

`configure` generates `SPRING_DATASOURCE_URL`, `SPRING_DATASOURCE_USERNAME`, and
`SPRING_PROFILES_ACTIVE=azure` for the developer path. Deployed equivalents use
the `SPRINGBOOT_DEPLOYED_` prefix. `SPRINGBOOT_PREVIOUS_` values distinguish
extension-managed settings from explicit overrides and permit safe refresh.
The JDBC URL uses TLS server verification and Azure's PostgreSQL authentication
plugin. Tokens and production database passwords are not saved.

`configure` preserves a supplied `POSTGRES_DEVELOPER_SSL_ROOT_CERT` PEM path,
or exports the public certificate bundle from the pinned Ubuntu run image into
the prepared template's ignored `.local` directory. It saves that path for local
developer execution. `run` supplies the database outputs, schema `notes`, and
`POSTGRES_MANAGED_IDENTITY_ENABLED=false`; `apply` supplies the application's
managed-identity username, identity flag, schema, and in-image trust path.
This keeps Windows file paths and application identity settings out of each
other's runtime configuration.

The build/publish path additionally reads `AZURE_CONTAINER_REGISTRY_ENDPOINT` and
`AZURE_SUBSCRIPTION_ID`. Runtime application additionally reads
`AZURE_CONTAINER_APP_NAME`, `AZURE_RESOURCE_GROUP`, and `AZURE_CLIENT_ID`.
`apply` requires previously generated deployed settings; it fails on stale values.

## Temporary integration and limitations

Explicit template lifecycle hooks invoke the commands. The extension builds and
pushes the image; `docker.imagePassthrough` lets built-in azd publish/deploy consume
it without another container build. This is prototype integration, not finalized
provider selection. `apply` explicitly sends generated configuration to the
Container App instead of assuming a saved azd environment updates a running app.

Azure CLI and azd authentication are separate prerequisites. All Azure operations
use the selected template subscription rather than changing global defaults.
Only public Azure registry hostnames (`*.azurecr.io`) are supported by this prototype.
`apply` prepares `az containerapp update --help` before sending runtime settings.
Older Azure CLI releases may restart through the Windows command shell when
automatically installing the Container Apps CLI extension; preparing it without
JDBC values prevents that restart from splitting literal `&` query parameters.
Do not claim live passwordless/database/deployment acceptance from unit tests or
local compilation alone.

Follow the [extension style guide](../../docs/extensions/extensions-style-guide.md).
