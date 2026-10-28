# Kubeconfig Wrangler

Kubeconfig Wrangler connects to Rancher, AWS EKS, Oracle Cloud (OKE) and static kubeconfig files and turns them into one standard kubeconfig. The result works with `kubectl`, `helm`, `k9s` and anything else that reads kubeconfig files.

It ships as a command-line tool and as a desktop app for macOS, Windows and Linux.

## Features

- **Rancher**: discovers every downstream cluster on a Rancher server and generates kubeconfigs for them. Authenticates with an API token or a username and password.
- **AWS EKS**: lists the EKS clusters in a region and generates kubeconfigs that authenticate through the AWS CLI. Credentials come from a named AWS profile, explicit keys or the default credential chain.
- **Oracle Cloud (OKE)**: lists the Container Engine for Kubernetes clusters in a tenancy and generates kubeconfigs that authenticate through the OCI CLI. Credentials come from a profile in your OCI config file (API key or session token). Discovery walks every active compartment unless you name one.
- **Static kubeconfigs**: imports existing kubeconfig files as a source.
- **Prefixes and aliases**: add a per-source prefix and rename individual clusters so context names stay unique and readable.
- **Desktop app** (Electron): saved connection profiles with encrypted secrets, merging into `~/.kube/config` and a context manager. The app checks for updates and offers to download and install them (macOS, Windows and the Linux AppImage). Install updates for the `.deb` and `.rpm` packages through your package manager.

## Installation

### Pre-built binaries

Download the latest release from the [Releases](https://github.com/dropte/kubeconfig-wrangler/releases) page.

| Platform | CLI | Desktop app |
|----------|-----|-------------|
| macOS (Apple Silicon and Intel) | `kubeconfig-wrangler-darwin-arm64` / `-darwin-amd64` | `.dmg` / `.zip` (arm64 and x64) |
| Linux | `kubeconfig-wrangler-linux-amd64` / `-linux-arm64` | AppImage, `.deb`, `.rpm` |
| Windows | `kubeconfig-wrangler-windows-amd64.exe` | `.exe` installer |

The macOS app and CLI releases are signed with a Developer ID and notarized by Apple. The app bundles a universal (Apple Silicon and Intel) backend. If you build an unsigned copy yourself, clear the quarantine flag with `xattr -cr "/Applications/Kubeconfig Wrangler.app"`.

### From source

```bash
git clone https://github.com/dropte/kubeconfig-wrangler.git
cd kubeconfig-wrangler

make build        # bin/kubeconfig-wrangler for the current platform
make install      # or install into $GOPATH/bin
```

## CLI usage

```
kubeconfig-wrangler generate     Generate a kubeconfig from Rancher clusters
kubeconfig-wrangler list         List Rancher clusters
kubeconfig-wrangler eks list     List EKS clusters in a region
kubeconfig-wrangler eks generate Generate a kubeconfig from EKS clusters
kubeconfig-wrangler eks profiles List AWS CLI profiles (~/.aws/credentials and ~/.aws/config)
kubeconfig-wrangler eks regions  List common AWS regions
kubeconfig-wrangler oke list     List OKE clusters
kubeconfig-wrangler oke generate Generate a kubeconfig from OKE clusters
kubeconfig-wrangler oke profiles List profiles in the OCI config file (~/.oci/config)
kubeconfig-wrangler oke compartments List the compartments in the tenancy
kubeconfig-wrangler oke regions  List OCI commercial regions
kubeconfig-wrangler serve        Start the web GUI server
kubeconfig-wrangler version      Print the version
```

Run `kubeconfig-wrangler <command> --help` for the full flag list.

### Rancher: `generate` and `list`

| Flag | Env var | Description |
|------|---------|-------------|
| `-u, --url` | `RANCHER_URL` | Rancher server URL |
| `-t, --token` | `RANCHER_TOKEN` | API token (`access_key:secret_key`) |
| `-a, --access-key` | `RANCHER_ACCESS_KEY` | API access key |
| `-s, --secret-key` | `RANCHER_SECRET_KEY` | API secret key |
| `--username` | `RANCHER_USERNAME` | Username for password auth |
| `--password` | `RANCHER_PASSWORD` | Password for password auth |
| `-k, --insecure-skip-tls-verify` | `RANCHER_INSECURE_SKIP_TLS_VERIFY` | Skip TLS verification (`true`/`false`) |
| `--ca-cert` | `RANCHER_CA_CERT` | Path to a CA certificate file |
| `-p, --prefix` (generate only) | `RANCHER_CLUSTER_PREFIX` | Prefix for cluster names |
| `-o, --output` (generate only) | `RANCHER_KUBECONFIG_OUTPUT` | Output file (default: stdout) |

```bash
# API token, with a prefix, written to a file
kubeconfig-wrangler generate \
  --url https://rancher.example.com \
  --token token-xxxxx:yyyyyyyyyyy \
  --prefix prod- \
  --output ~/.kube/rancher-config

# Username/password
kubeconfig-wrangler list --url https://rancher.example.com --username admin --password '...'

# Environment variables instead of flags
export RANCHER_URL=https://rancher.example.com
export RANCHER_TOKEN=token-xxxxx:yyyyyyyyyyy
kubeconfig-wrangler generate
```

To get a Rancher API token, open your user menu in Rancher, choose **Account & API Keys**, then **Create API Key**. Copy the `access_key:secret_key` token.

### AWS EKS: `eks list` and `eks generate`

| Flag | Env var | Description |
|------|---------|-------------|
| `-r, --region` | `AWS_REGION`, `AWS_DEFAULT_REGION` | AWS region |
| `--profile` | `AWS_PROFILE` | AWS CLI profile name |
| `--access-key` | `AWS_ACCESS_KEY_ID` | AWS access key ID |
| `--secret-key` | `AWS_SECRET_ACCESS_KEY` | AWS secret access key |
| `--session-token` | `AWS_SESSION_TOKEN` | AWS session token |
| `-p, --prefix` (generate only) | | Prefix for cluster names |
| `-o, --output` (generate only) | | Output file (default: stdout) |

```bash
kubeconfig-wrangler eks list --region us-west-2 --profile production
kubeconfig-wrangler eks generate --region us-west-2 --prefix eks- --output ~/.kube/eks-config
```

The generated kubeconfig calls `aws eks get-token`, so the AWS CLI must be installed wherever it is used. If you authenticate with a profile, the kubeconfig refers to that profile by name. If you pass explicit keys, they are written into the kubeconfig's `env` section, **including the secret access key**. Treat that file as a credential and prefer profiles where you can.

### Oracle Cloud OKE: `oke list` and `oke generate`

Prerequisites: install the [OCI CLI](https://docs.oracle.com/en-us/iaas/Content/API/Concepts/cliconcepts.htm) (`oci`) and configure at least one profile in `~/.oci/config` (for example with `oci setup config`). For security-token auth, sign in with `oci session authenticate --profile-name <name>` first. No secrets are stored by Kubeconfig Wrangler; it always reads credentials from a profile in the OCI config file.

| Flag | Env var | Description |
|------|---------|-------------|
| `--profile` | `OCI_CLI_PROFILE` | Profile name in the OCI config file (default: `DEFAULT`) |
| `--config-file` | `OCI_CLI_CONFIG_FILE` | Path to the OCI config file (default: `~/.oci/config`) |
| `-r, --region` | | Region override (default: the profile's region) |
| `--compartment` | | Compartment OCID (default: the whole tenancy) |
| `--auth` | | `api_key` (default) or `security_token` |
| `--endpoint` | | Kubernetes API endpoint: `public` (default), `private` or `vcn_hostname` |
| `-p, --prefix` (generate only) | | Prefix for cluster names |
| `-o, --output` (generate only) | | Output file (default: stdout) |

Explicit flags win over environment variables. `oke profiles`, `oke compartments` and `oke regions` list the available profiles, the compartments in the tenancy and the OCI regions.

```bash
kubeconfig-wrangler oke profiles
kubeconfig-wrangler oke list --profile prod --region us-ashburn-1
kubeconfig-wrangler oke list --profile prod --compartment ocid1.compartment.oc1..xxxxx
kubeconfig-wrangler oke generate --profile prod --prefix oke- --output ~/.kube/oke-config

# Session-token auth
oci session authenticate --profile-name prod
kubeconfig-wrangler oke generate --profile prod --auth security_token
```

Without `--compartment`, discovery walks every active compartment in the tenancy, so the profile needs permission to list compartments and clusters there. The generated kubeconfig authenticates with `oci ce cluster generate-token`, so the OCI CLI must be installed wherever it is used. The command is pinned to the chosen profile with `--profile <name>`, plus `--auth security_token` for session auth and `--config-file <path>` when a custom config file is used. Nothing secret is written to the kubeconfig.

### Web GUI: `serve`

```bash
kubeconfig-wrangler serve                 # 127.0.0.1:8080
kubeconfig-wrangler serve --port 3000
```

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `127.0.0.1` | Address to bind to |
| `--port` | `8080` | Port to listen on |
| `--token` | random | Access token for the UI and API |

On startup the server logs a line like `Open http://127.0.0.1:8080/?token=<token>`. Open that URL; the UI and the API only respond to requests that carry the token. You can set the token with `--token` or the `KUBECONFIG_WRANGLER_TOKEN` environment variable. Without either, a random token is generated on each start.

> **Security:** the GUI can read every saved credential and rewrite your kubeconfig. Keep the default loopback address. Binding `--addr` to anything other than `127.0.0.1`/`localhost` exposes the GUI to your network, protected only by the token, over plain HTTP.

## Desktop application

The desktop app starts the backend on a random local port and opens the GUI in its own window.

- **Profiles**: save connections to Rancher servers (token or username/password, custom CA, TLS options), AWS EKS (profile or keys, region), Oracle Cloud OKE and static kubeconfig files.
- **Oracle Cloud (OKE) connection fields**: OCI profile (from the OCI config file), config file path, region override, compartment OCID (leave empty for the whole tenancy), auth mode (API key or security token) and endpoint kind (public, private or VCN hostname). No secrets are stored.
- **Cluster discovery**: browse clusters from one profile or from all of them at once.
- **Aliases and prefixes**: rename clusters and give each profile a prefix.
- **Merge into kubeconfig**: write the selected contexts into your kubeconfig (`$KUBECONFIG` or `~/.kube/config`) or download them as a file. The app remembers which contexts each profile exported. Before its first change to your kubeconfig in each session, it saves the old file as a `.bak` backup.
- **Context manager**: view, switch, rename and delete contexts in your kubeconfig.

### Configuration and storage

Profiles are stored in `profiles.json` in the configuration directory:

| OS | Default directory |
|----|-------------------|
| macOS | `~/Library/Application Support/kubeconfig-wrangler` |
| Linux | `$XDG_CONFIG_HOME/kubeconfig-wrangler` or `~/.config/kubeconfig-wrangler` |
| Windows | `%APPDATA%\kubeconfig-wrangler` |

Secrets in profiles are encrypted with a key that is kept in the OS keychain.

| Environment variable | Purpose |
|----------------------|---------|
| `KUBECONFIG_WRANGLER_CONFIG_DIR` | Override the configuration directory |
| `KUBECONFIG_WRANGLER_KEYRING` | Set to `file` to keep the encryption key in `<config dir>/encryption.key` (mode 0600) instead of the OS keychain. The app also falls back to this file when no keychain is available, for example on Linux without a Secret Service. |
| `KUBECONFIG_WRANGLER_TOKEN` | Access token for `serve`. The desktop app uses this to hand its token to the backend. |

## Using the generated kubeconfig

```bash
export KUBECONFIG=~/.kube/config:~/.kube/rancher-config
kubectl config get-contexts
kubectl config use-context prod-my-cluster

helm --kubeconfig ~/.kube/rancher-config list
k9s --kubeconfig ~/.kube/rancher-config
```

## Development

### Prerequisites

- Go (the version in `go.mod`, currently 1.26+)
- Node.js 22+ (for the Electron app)
- Make

### Building

```bash
make build                 # CLI for the current platform -> bin/kubeconfig-wrangler
make build-all             # bin/linux, bin/linux-arm64, bin/mac (universal), bin/win
make test                  # go test -race ./...
make lint                  # golangci-lint v2
make electron-dev          # build the host backend into bin/<mac|win|linux> and start Electron
make electron-build-mac    # universal backend + signed dmg/zip (arm64 and x64) when signing env is set
make electron-build-linux
make electron-build-win
make electron-build-all    # Linux and Windows (Windows needs wine off Windows), plus macOS on a macOS host
```

`make build-darwin` builds amd64 and arm64 binaries and merges them with `lipo` into `bin/mac/kubeconfig-wrangler`. `lipo` only exists on macOS; on other hosts the target copies the binary for the host architecture instead. electron-builder bundles `bin/<mac|win|linux>` (`extraResources`) into the app.

### Releases

Push a `vX.Y.Z` tag, or run the **Release** workflow manually with an existing tag. Manual runs are only for a tag that has no release yet, or only a draft release; the workflow refuses a tag whose release is already published, so delete that release or use a new tag. The workflow builds and tests that tag, creates a draft GitHub release, attaches the CLI binaries and publishes the Electron artifacts. The macOS CLI binaries and app are signed and notarized; the workflow fails if the Apple signing secrets are missing.

### Project structure

```
kubeconfig-wrangler/
├── cmd/                  # CLI commands (generate, list, eks, oke, serve, version)
├── pkg/
│   ├── config/           # Rancher CLI configuration
│   ├── context/          # kubeconfig context management and merging
│   ├── kubeconfig/       # kubeconfig generation, prefixes and aliases
│   ├── profile/          # encrypted profile store
│   ├── provider/         # Rancher, EKS, OKE and static kubeconfig providers
│   ├── rancher/          # Rancher API client
│   └── web/              # web server and GUI
├── electron/             # Electron desktop app
├── .github/workflows/    # CI (ci.yml) and releases (release.yml)
├── main.go
└── Makefile
```

## License

MIT (see `electron/package.json`).

## Contributing

Contributions are welcome. Fork the repository, create a feature branch and open a pull request.
