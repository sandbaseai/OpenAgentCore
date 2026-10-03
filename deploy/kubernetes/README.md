# Kubernetes deployment

This fork keeps its Kubernetes deployment in `.github/workflows/deploy.yml`, `.github/actions/setup-kubectl/` and this directory. Upstream application code, documentation and CI selection remain unchanged.

This directory deploys the control plane — Core and the Web console — to a Kubernetes cluster from a source commit, and the `core-deploy` workflow drives it. It is an alternative to the [installer](../../docs/getting-started/install.md), for an operator who already runs PostgreSQL and a cluster. The installer owns the single-host installation and its `config.json`; nothing here reads or writes those files. Core and Web take their settings from the environment, as [Configuration](../../docs/configuration.md#appendix-core-environment-without-the-installer) defines it, and this deployment is the one place that sets those variables for the cluster.

## What it deploys

| Object | From | Notes |
| --- | --- | --- |
| Deployment and Service `oac-core` | `prod/core.yaml.tpl` | Core on port 8091. One replica: Core takes a PostgreSQL lease that gives one execution service per database, so a second replica exits at startup. An init container prepares the adapter state volume, then `oac-core-migrate` applies the schema |
| ConfigMap `oac-core-env` | `prod/core-env.yaml.tpl` | Core's process environment; no secret |
| PersistentVolumeClaim `oac-core-state` | `prod/core-state.yaml.tpl` | `OAC_PROVIDER_STATE_ROOT`, required for E2B; see [the state volume](#the-state-volume). Back it up with the database and the credential key |
| Deployment and Service `oac-web` | `prod/web.yaml.tpl` | The console on port 8080. One replica: Web holds sign-in sessions in process memory, so a cookie is valid only on the Pod that issued it |
| Secrets `oac-core-secrets`, `oac-web-core-key`, `oac-registry` | GitHub Environment secrets | Created by the workflow from stdin; never written to a file in the repository |

PostgreSQL is yours. So is anything installed on a machine: nodes, self-hosted machines and the Runtime images come from a [release](../../docs/getting-started/nodes.md), not from this workflow.

Both images always come from one commit, so the console never talks to a Core of another release.

## Cluster prerequisites

- A PostgreSQL database and an account of its own, reachable from the cluster. Core needs no extension and migrates the schema itself.
- An existing namespace and a Bound ReadWriteOnce `oac-core-state` claim. Prepare it once using `prod/core-state.yaml.tpl` with `OAC_STORAGE_CLASS` and `OAC_STATE_SIZE`; see [the state volume](#the-state-volume).
- An image registry the cluster can pull from, with a user the workflow can push as.
- Configure DNS, certificates and public ingress separately. Core and Web can start before the domain is configured; E2B execution needs the public address to reach Core.
- The runner needs `docker`, `envsubst` and outbound access to the registry and the cluster's API server. Set `OAC_DEPLOY_RUNNER` to a self-hosted label when the API server is private.

## Deploy

Merge the deployment workflow into `main`. Run **Actions** → **core-deploy** → **Run workflow** from `main` and give it a release tag or a full commit SHA already merged into `main`. The run builds both images, pushes them, applies the secrets and the environment, rolls Core out and then Web, and checks that both Services have a ready endpoint. It does not create an Ingress or configure DNS or certificates. Route `/v1` and `/api/v1` to `oac-core:8091`, and all other paths to `oac-web:8080`, in the `openagentcore` namespace. After configuring HTTPS, check `/healthz`, verify that unauthenticated `/v1/agents` returns `401`, and qualify a fresh E2B Session and Turn.

**A deployment is an outage.** Core's single replica stops before its replacement starts, and the replacement migrates the schema before it listens, so the Agents API, the machine routes and every running Session are unavailable for the rollout. Deploy in a window you can afford to lose. The Pod that takes over also needs the previous one's leased database connection to be gone; until it is, `AcquireLease` fails and Core exits, and the rollout depends on a restart landing inside the 15-minute deadline.

To roll back, run the workflow again with the earlier commit. A rollback across a schema migration is not automatic: the earlier Core runs against the newer schema.

## Settings

Restrict the repository’s `production` GitHub Environment to deployments from the `main` branch. Set these on that environment. Variables are not secret; secrets are. The workflow stops and names anything missing or malformed before it touches the cluster.

### Secrets

| Secret | Value |
| --- | --- |
| `OAC_KUBECONFIG` | Base64 of a kubeconfig for the cluster: `base64 -w0 < kubeconfig` |
| `OAC_REGISTRY_USERNAME`, `OAC_REGISTRY_PASSWORD` | The registry account the workflow pushes as and the cluster pulls with |
| `OAC_DATABASE_PASSWORD` | The database account's password. Keep it out of `OAC_DATABASE_URL`; Core refuses a URL that also carries one |
| `OAC_CREDENTIAL_KEY` | Base64 of exactly 32 random bytes, which encrypts stored credentials: `openssl rand -base64 32`. Losing it loses every stored credential |
| `OAC_CORE_KEY` | The Core key: sign-in to the console and the credential for `/core/v1`. At least 32 characters, no whitespace: `openssl rand -hex 32`. Core stores only its SHA-256 |

### Variables

| Variable | Value |
| --- | --- |
| `OAC_REGISTRY` | Image repository prefix, such as `registry.example.com/openagentcore`. The workflow pushes `oac-core` and `oac-web` beneath it |
| `OAC_REGISTRY_HOST` | Registry host for `docker login` and the pull Secret. Optional; the host of `OAC_REGISTRY` by default |
| `OAC_PUBLIC_URL` | The canonical HTTPS origin, such as `https://core.example`, with no path, query or fragment. Core derives the daemon WebSocket address, the sandbox `core_url` and the self-hosted `remote_url` from it, and Web serves only this origin. Without it Core executes no Session |
| `OAC_DATABASE_URL` | `postgres://USER@HOST:PORT/DATABASE`, with `sslmode` and any `pool_*` parameter in the query and no password |
| `OAC_INSTALLATION_ID` | A canonical lowercase UUID, generated once with `uuidgen \| tr 'A-Z' 'a-z'`. Core refuses an ID other than the one its database recorded, so it belongs to the database: keep the two together and restore them together |
| `OAC_NAMESPACE` | Existing namespace; `openagentcore` by default |
| `OAC_STORAGE_CLASS`, `OAC_STATE_SIZE` | StorageClass and size of the required Core state volume. See [the state volume](#the-state-volume). `10Gi` by default |
| `OAC_CLUSTER_UID` | UID of the target cluster’s `kube-system` namespace. The workflow verifies it before applying resources |
| `OAC_K8S_SERVER` | API server URL that overrides the kubeconfig's. Optional |
| `OAC_K8S_INSECURE` | `true` skips API server certificate verification, for an address the certificate does not name. Leave it unset otherwise |
| `OAC_DEPLOY_RUNNER` | Runner label; `ubuntu-22.04` by default. Set this one at repository level: the job that pins the commit runs before the environment is entered, so an environment-scoped value does not reach it |
| `OAC_CORE_CPU_REQUEST`, `OAC_CORE_CPU_LIMIT`, `OAC_CORE_MEMORY_REQUEST`, `OAC_CORE_MEMORY_LIMIT` | Core's resources; `500m`, `2`, `1Gi` and `4Gi` by default |
| `OAC_HARNESSES`, `OAC_DEFAULT_HARNESS`, `OAC_EXECUTION_CONCURRENCY`, `OAC_WRITE_AUDIT_RETENTION`, `OAC_LOG_LEVEL` | The matching [process settings](../../docs/configuration.md#settings), which define their values and defaults |

A change to any of these takes effect on the next run: a digest of the environment and the secrets enters both Pod templates, so the Pods are replaced even when the images are unchanged.

## The state volume

`OAC_PROVIDER_STATE_ROOT` is the private root each Sandbox Provider adapter keeps its own state under, and today exactly one adapter uses it: E2B stores its receipts in `/state/e2b`. The helper writes a receipt before each remote `Create`, because a helper's exit never proves that the remote call settled, and Core needs the receipt afterwards to clean up, observe and verify ownership of a sandbox living in E2B's cloud. Each receipt is at most 64 KiB, they are never pruned, and losing them orphans sandboxes that E2B keeps billing and Core can no longer destroy. [The E2B helper](../../services/core/tools/e2b-provider/README.md#receipts-and-state-directory) owns these rules.

This deployment requires the persistent claim. Set `OAC_STORAGE_CLASS` before deploying; an unset value or missing claim stops the workflow before it applies resources. Every Core rollout mounts `oac-core-state`, preserving the E2B receipts.

Size it for the claim's lifetime rather than for today, because a StorageClass without `allowVolumeExpansion` cannot grow and a `Delete` reclaim policy destroys the receipts with the claim. The default `10Gi` also clears the minimum that a block-storage provisioner may impose.

Keep the existing claim's class and size. This cluster's `cbs` StorageClass does not allow expansion; changing the class or enlarging this claim fails. To change storage, migrate the receipts to a new claim.

## Differences from an installed installation

- **E2B is the only backend that executes anything.** Web serves the node installer from the release bundle's node payload, which these images do not carry, so `/node-install/*` is unavailable and Add node is hidden. The Core image's native installer catalog is empty too — only the release build fills it — so `POST /api/v1/agent-daemon/installation` answers `503 installation_unavailable` and a self-hosted Session's install command cannot resolve. Until the node payload and the native catalog are built into these images, select [E2B](../../docs/getting-started/install-options.md), which needs neither.
- **No startup settings panel.** `OAC_SETTINGS_FILE` is the installer's snapshot, so `GET /core/v1/installation` reports none and the console shows no process settings. Runtime settings in Web are unaffected; they live in Core's database.
- **No domain setup in Web.** There is no installer socket, so the public origin comes from `OAC_PUBLIC_URL` and your external ingress, not from the console.
- **No `oac` command.** The maintenance commands ship in the Core image: `kubectl exec deploy/oac-core -- oac-core-device …`. [Operations](../../docs/getting-started/operations.md) covers keys and backup.
- **A rollout signs operators out** of the console, because Web holds its sessions in memory.

## Public routing on TKE

The Services use NodePort for the existing qcloud CLB controller. Keep DNS, certificates and ingress separate from the image rollout workflow. `prod/ingress.yaml.tpl` adds one host to an existing CLB and routes `/v1` and `/api/v1` to Core and other paths to Web. It preserves the original request paths.

Prepare an Opaque Secret in the deployment namespace with `qcloud_cert_id` referencing a Tencent Cloud certificate covering the hostname. Set `OAC_CLB_ID`, `OAC_PUBLIC_HOST` and `OAC_TLS_SECRET`, then apply the template:

```sh
envsubst '$OAC_CLB_ID $OAC_PUBLIC_HOST $OAC_TLS_SECRET' < deploy/kubernetes/prod/ingress.yaml.tpl | kubectl --context sandbase-prod -n openagentcore apply -f -
```

For this installation, the host is `agentcore.sandbase.ai`, the shared CLB is `lb-9cr62q4u` and the certificate reference Secret is `oac-tls-cert`. After the controller reports Ready, verify HTTPS `/healthz`, the Web home page, unauthenticated `/v1/agents` returning 401 and a Runtime WebSocket upgrade on `/api/v1/agent-daemon/ws`.
