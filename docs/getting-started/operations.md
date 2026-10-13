---
title: "Operations"
---

The installation operator owns the Core host, its storage and its availability. Node hosts run their own services; see [Nodes](./nodes.md). Settings are described in the [configuration reference](../configuration.md).

## The oac command

Each installation has its own native management command in its directory: `oac` on Unix, `oac.exe` on Windows. It needs Docker access and no root privileges:

```sh
docker compose -f ~/.oac/core/compose.yaml ps
```

| Command | What it does |
| --- | --- |
| `docker compose ps` | Shows the services. Run it in the installation directory |
| `docker compose start` | Starts the services |
| `docker compose stop` | Stops the services. Data, nodes and sandboxes are kept |
| `oac apply` | Runs `oac-core check-config`, then `docker compose up -d --wait`. A failed check changes no service |
| `oac core-key [--show]` | Identifies the key location in the data volume, or prints the key with `--show` |
| `oac rotate-core-key` | Replaces the Core key and restarts Core and Web |
| `docker compose down` | Removes the containers. Data is kept; to delete it, [uninstall](#uninstall) |

The examples use the default installation directory. On Windows, invoke the management command with `& "$HOME/.oac/core/oac.exe"` followed by the same arguments. For a custom installation directory, replace the path in each command.

After local credentials and enrollment are resolved, Runtime Harness discovery and the authenticated bootstrap HTTP request run concurrently. Both must succeed before the Runtime opens its connection or publishes capabilities. Failure cancels the sibling operation and waits for cleanup. The executor remains owned by the connection lifetime. Runtime startup changes require a rebuilt, qualified Runtime template.


## Runtime startup latency

The daemon logs `executor preparation stage` with `stage=workspace`, the executor and Session IDs, duration in milliseconds and `success`. Codex logs `codex preparation stage` for `session_plan`, `model_catalog`, `process_spawn`, `rpc_initialize` and `verification`, with the owner trace, duration and `success`. These records contain no native error text, credentials, configuration, catalog contents or command output. An omitted conditional stage is unobserved, not zero. `session_plan` contains `model_catalog`; the executor readiness interval contains workspace preparation, the adapter stages and transport overhead. Do not add nested intervals together. A failed `rpc_initialize` includes its required child cleanup.

Within `model_catalog`, `model_catalog_command` includes the original native command, exit and pipe draining; `model_catalog_cleanup` measures the existing best-effort process-group signal only when a process was started. Its `success` reports the signal return value, so an already-exited group can report false; it does not prove descendants were reaped and does not change the command outcome. `model_catalog_validation` covers JSON/model/verbosity and managed-home validation; `model_catalog_snapshot` covers snapshot creation, write, close and attachment to the Session plan. Failed stages retain the original rejection and cleanup behavior.

`rpc_initialize_write` and `rpc_initialize_response` are cumulative milestones from the same request start, not additive intervals or Core network RTT. The response timestamp is captured only for a matching response ID before native result decoding; its `success` means the frame has no native error, not that the handshake is valid. The existing `rpc_initialize` parent reports the complete decoded handshake and any required failure cleanup. Missing replies have no response milestone. The reader captures the timestamp without logging; records are emitted after request resolution, so log timestamps are export time. The 10-second initialize deadline and Session-owned process lifetime are unchanged.

The model catalog remains validated and pinned before app-server initialization. These timings distinguish catalog preparation from native process initialization; they do not establish a latency improvement. Compare fresh and reused Sessions on the same Runtime template, model and provider, and verify persisted replies and usage in addition to first-text latency. A Runtime startup change needs a rebuilt, qualified Runtime template; replacing Core alone does not update existing sandboxes.


## Service health

Use these observations for different questions:

| Observation | What it establishes |
| --- | --- |
| `docker compose ps` | The database accepts its readiness check |
| Core `/healthz` | Core's process is alive |
| An authenticated API read | The caller's key works for that resource |
| Environment connection | The Runtime transport is connected |
| A finished Turn and its results | The task's recorded outcome |

Service health does not show that a harness or a model works. Use Session, Turn, Items and Usage reads for execution, and Web's **Nodes** page for node connection, readiness and placement. For local diagnosis, use the installation's own Compose file:

```sh
docker compose -f "$HOME/.oac/core/compose.yaml" ps --all
docker compose -f "$HOME/.oac/core/compose.yaml" logs --tail 200 core
docker compose -f "$HOME/.oac/core/compose.yaml" logs --timestamps init
```

The `init` service exits after initialization. Its step logs are described in [Deployment](https://github.com/MiniMax-AI/OpenAgentCore/blob/main/deploy/README.md#installation).

Don't paste `docker compose config`, `docker inspect` or raw logs into public issue reports.

## Request diagnosis

Open **Monitor → Request log** to inspect recent requests and copy their request IDs for correlation with Core's service log. [Request log](../../contracts/agents-api/admin-api.md#request-log) defines its scope, privacy and limited in-memory retention. For a request that accepted an asynchronous Turn, inspect **Session log** for the execution result; HTTP acceptance alone is not task success. Use your existing log collector for retention across restarts or Core instances.

### Environment file rejection logs

The Environment file creation handler emits one `api_request_rejected` event when it writes a response with HTTP status 400 or higher. Match its `request_id` to the response's `X-Request-Id`; Core generates this ID, so it need not match a caller's request ID. The existing log context also supplies `trace_id` and `span_id`, matching the response's `traceparent` whether the request carries a valid incoming trace or Core generates a new trace. This event covers `operation=environment_file_create` and the fixed route template `/v1/agents/environments/{environment_id}/files`. Requests rejected before this handler are outside its coverage.

The event contains `operation`, `route`, `status`, an allowlisted `code` (unrecognized codes become `unknown`), and a static `reason`. Reasons are `unknown`, `invalid_payload`, `unknown_field`, `invalid_path`, `invalid_base64`, `inline_too_large`, `hosted_environment_provisioning`, `destination_directory`, and `destination_unsafe`. `unknown` means the rejection has no classified reason; a 400 alone does not establish that an Environment is still provisioning.

The event does not record request or response bodies, query strings, raw URL paths, resource IDs, user file paths, field values, arbitrary error messages or credentials. It does not change the HTTP response or execution behavior. Check that the deployed Core version includes this diagnostic before relying on it; missing events do not establish success, and diagnostics alone do not show that file uploads work.

## Stop and restart

Let active work settle before a planned restart:

```sh
docker compose -f ~/.oac/core/compose.yaml stop
docker compose -f ~/.oac/core/compose.yaml start
```

Stopping Core stops no node and no sandbox. Node services, their microVMs and Docker containers keep running; stopping is not a way to reclaim compute. A Core restart does not continue an interrupted native tool call transparently. After reconnecting, query the same Session; don't create a new Session to replay uncertain work. Session event streams are live only; recover through Session, Turn and Items reads.

A Web restart, including one caused by `oac apply`, signs everyone out of the console. Web sign-ins otherwise last 12 hours.

## Core key

Each installation has one administrator credential, the Core key. The installer generates a key with the `oac_admin_` prefix followed by 64 random lowercase hexadecimal characters in `secrets/web/core.key`. Read it with `oac core-key --show`; the file is owned by the container user. The Core key:

- signs in to Web. The browser gets an HttpOnly session cookie, never the key;
- authorizes Core API (`/core/v1`) requests sent as `Authorization: Bearer <Core key>`;
- never authorizes the Agents API (`/v1`). Applications use Project API keys, which in turn can't call `/core/v1`.

Keep it private. Web reads `secrets/web/core.key`. Core reads only its SHA-256 from `secrets/core/core-key-digests.json`. A Core key has at least 32 characters and no whitespace. Web limits failed sign-ins.

### Script the Core API

Core publishes no host port. On the Core host, this helper runs `curl` in Core's network namespace and passes the key on stdin, keeping it off the command line:

```sh
core() (  # core METHOD PATH [JSON body]
  cd ~/.oac/core
  ./oac core-key --show | sed 's/^/Authorization: Bearer /' |
    docker run -i --rm --network "container:$(docker compose ps -q core)" curlimages/curl \
      -fsS -X "$1" "http://127.0.0.1:8091/core/v1$2" -H @- -H 'Content-Type: application/json' ${3:+-d "$3"}
)
```

| Task | Command |
| --- | --- |
| List Projects | `core GET /projects` |
| Create a Project | `core POST /projects '{"name": "billing-bot"}'` |
| Issue an API key (shown once, as `key`) | `core POST /projects/$PROJECT_ID/keys '{"name": "prod"}'` |
| Revoke a key | `core DELETE /projects/$PROJECT_ID/keys/$KEY_ID` |
| Archive a Project (revokes all keys) | `core POST /projects/$PROJECT_ID/archive` |
| See harnesses and their default models | `core GET /harnesses` |
| Set Codex's default model | `core PUT /harnesses/codex/model-configuration '{"model": "your-model-id", "model_provider": {"protocol": "responses", "base_url": "https://provider.example/v1", "api_key": "sk-..."}}'` |
| Installation facts, including the API base URL | `core GET /installation` |

The [Core administration API](../../contracts/agents-api/admin-api.md) lists every route; errors use the [Core error envelope](../../contracts/agents-api/core-errors.md).

### Rotate the Core key

```sh
~/.oac/core/oac rotate-core-key
```

It runs in the initialization container, updates `secrets/web/core.key` and `secrets/core/core-key-digests.json` in the data volume, and restarts Core and Web. The old key stops working as soon as Core restarts, and every console session ends: sign in again and update your scripts.

## Projects and API keys

Create Projects and issue keys in Web, on **Projects and keys**, or through the [Core API](#script-the-core-api). How Projects and keys behave is in [Projects own assets](../concepts.md#projects-own-assets).

To rotate an application key:

1. Issue a new key in the same Project.
2. Update the application to use it.
3. **Revoke** the old key.

**Archive** disables every key of a Project and keeps its assets.

Core records which key made each public resource write; the retention of that history is [`core.write_audit_retention`](../configuration.md#settings).

## Back up

A recoverable backup contains one consistent, stopped-write set. A database dump or Core volume export alone does not include independent workspace storage or node compute state. Use the same release when restoring; see [installation version policy](#installation-version-policy).

### Backup contents

- The Docker volume `<project>_data`, including `database/`, `secrets/` and `state/`. It holds Projects, key digests, nodes, default models, encrypted credentials and execution history, including large objects. Keep `secrets/core/credential.key` with its matching database or stored credentials cannot be decrypted.
- The installation directory, including `.env`, `compose.yaml`, Compose overrides and the management command.
- Every independent workspace namespace selected by current or retained objects, even when it is outside the installation volume. Back up its entire root, including `.oac-storage-root`, object identities, staging, live data, trash and deletion markers. Preserve numeric ownership, modes, links and extended attributes, including every `user.*` attribute. Copying only `live/data` loses lifecycle evidence and can resurrect deleted identities. [Workspace storage](../workspace-provider.md#kernel-nfs-adapter) owns the layout and ownership requirements.
- For a [quota mode](../workspace-provider.md#quota-mode) namespace, the entire XFS filesystem holding the export as one block-level copy: a storage or LVM snapshot, or an image of the frozen or unmounted device. Only such a copy keeps project IDs, quota limits, inode identities, the filesystem UUID and the `oac-workspace-quota` journal consistent. A file-level copy, `tar`, `rsync` or `xfsdump` loses them; the command then refuses the restored objects instead of serving them unbounded.
- Each node's state directory, `/var/lib/oac-node/.oac/nodes/<installation-id>/`, and its provider storage: Docker volumes or microsandbox's store. Include all compute and snapshot resources still referenced by the database. See [when a node host fails](./nodes.md#when-a-node-host-fails).

- Each private `checkpoint_root` in its entirety, including `.oac-checkpoint-store`, archives, operation journals, lock files and tombstones, together with the matching Core database. It is separate from the native SDK store and independent workspace namespace. Preserve its identity and private ownership; the [checkpoint transfer contract](../sandbox-provider.md#checkpoint-transfer) owns its lifecycle.

### Establish a stopped-write window

1. Block new application input, live file access and administrative mutations at the installation's ingress. Let active execution, initialization, file operations and native cleanup finish. Stop other applications or host processes that can write to the same filesystem.
2. While Core and nodes remain available, confirm that every owned writer has stopped and all Create, Kill, snapshot cleanup and filesystem mutations have settled. An offline node, timeout, missing instance or stopped service is insufficient. A confirmed suspension stops its source compute but retains a checkpoint that must be included in the backup. For the simplest cold recovery baseline, wait until eligible retained Sessions have completed checkpoint expiry cleanup and their allocations are released; retain their filesystem objects and native history under the [cold replacement contract](../sandbox-provider.md#cold-replacement-after-checkpoint-retention).
3. Stop Core and Web, then stop the node control services to prevent new lifecycle work. Recheck the exact native resources and keep all writers stopped until every part of the backup is complete. **Stopping Core or a node service does not stop its sandboxes.** The node service uses `KillMode=process`; microVMs and other provider-managed compute may continue to run independently.
4. Flush the stopped filesystem's pending writes using the storage platform's supported procedure and take its snapshot or export. For quota mode, freeze the XFS filesystem with `xfs_freeze -f` or unmount it while copying its device, then thaw it. Export the Core data volume and installation directory, plus required node state and provider stores, within the same window. Stop PostgreSQL before copying its `database/` files. Record the release, backup time, component inventory and checksums together, and keep the backup outside the installation being backed up.
5. Restore service availability only after all copies finish. Bring up storage before Core and nodes, verify readiness, then reopen ingress. Do not replay uncertain native work to make a backup pass.

There is no one-command, lossless maintenance drain. Archive and deployment reset change Session lifecycle state; they are not substitutes for a backup pause that preserves resumability. If any writer or mutation remains unknown, keep the backup incomplete and resolve that ownership first.

For a logical database copy during the stopped-write window, leave only the database service running and use:

```sh
docker compose -f "$HOME/.oac/core/compose.yaml" exec -T database \
  pg_dump -U agents_api agents_api > oac-backup.sql
```

A logical dump still needs the matching secrets, state, independent filesystems and required node resources. Docker Desktop's **Volumes** export is a way to copy a stopped data volume; it does not coordinate these other components.

### Restore and rehearse

Keep application ingress and Core/node services stopped while restoring. Fence or shut down every old writer before exposing the restored filesystem; an unreachable old host is not a fence. A rehearsal must use an isolated installation and storage copy that cannot connect to the original nodes or share their writable namespace.

Restore matching secrets and database state, the complete independent filesystem namespaces and any required node identities/provider stores before starting Core. Preserve the database's configuration and namespace UUIDs, object identities and deletion markers; do not run first-time namespace initialization over restored storage. Restore a quota mode filesystem by writing its block-level copy to the backing device and mounting it with `prjquota` at the configured mount point; never run `oac-workspace-quota init` over it, and do not mount the copy beside the original, which has the same filesystem UUID. Reestablish the configured mount paths and service UIDs, then validate the mounted filesystem, ownership and xattrs from each participating service context. [Independent workspace storage](../configuration.md#independent-workspace-storage) owns deployment setup.

The cold released baseline resumes only qualified native history after new compute admission. A backup containing retained checkpoints also depends on the matching native runtime, node identity, provider store and external filesystem identities. File-level restore can change inode identity and prevent strict checkpoint restore even when file bytes match. This procedure does not promise portable memory snapshots or restoration onto an arbitrary replacement node; preserve unresolved ownership rather than substituting a new sandbox.

Start the database, Core and node services only after their restored dependencies are complete; keep ingress restricted while checking them. In an isolated rehearsal, verify a retained Session's files and native continuation on fresh compute where qualified, confirm that deleted objects cannot be reopened, and check that no old writer can access the restored namespace. Record the results before relying on the backup for recovery.

## Uninstall

```sh
cd ~/.oac/core
docker compose down --volumes --remove-orphans --rmi all
cd && rm -rf ~/.oac/core
```

`down --volumes` deletes the installation data volume. Remove the installation directory afterward; on Windows use `Remove-Item -Recurse "$HOME/.oac/core"`.

All data goes with it: Projects and API keys, Session history, stored credentials and the Core key. To keep the data, stop the installation with `docker compose stop` instead, or [back it up](#back-up) first.

Uninstall stops no sandbox: node sandboxes keep running on their nodes, and E2B sandboxes keep running, and billing, at E2B. While Core is still up, archive their Sessions or [reset the deployment](./nodes.md#change-the-sandbox-configuration) and let it complete; the command shows how many sandboxes Core has in use.

Nodes on other hosts keep running. To uninstall them the usual way, remove them in Web first, as in [Remove a node](./nodes.md#remove-a-node). After the installation directory is gone, their Core is gone: on each node host, run the node uninstall command with `--force`, using `node-install.pyz` from the release that installed them. The installation ID is `secrets/core/installation.id` in the data volume.

## Installation version policy

An installation runs one release for its whole life. In-place version upgrades and downgrades are not supported. Nothing migrates data between releases.

To move to a new release, install it into a new, empty directory, with its own database, Core key and nodes, and add nodes from its Web. Keep the old installation, its data and its nodes until their work is finished. Nodes run the program of the console that added them and are never upgraded in place; Core accepts only nodes that speak its own node protocol.

An interrupted installation can [resume with its saved configuration](./install.md#install). An unrelated nonempty directory is refused.

Installation and mutating `oac` commands share the [installation lock](../configuration.md#installation-directory). If another command is running, wait for it to finish before retrying.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `Port N is already in use.` | Another program holds that port. Stop it, or choose another `--web-port`. The installer does not move to a different port |
| `Directory is not a complete Core installation` | Preserve the directory and choose another `--install-dir` |
| `configuration check failed; no service was changed` | `.env` has a value Core rejects. The message names the variable and not the value. Fix `.env` and run `oac apply` again |
| `Docker Compose 2.26 or newer is required` | Update the Docker Compose plugin |
| `Installation and data retained at …` | Read the service logs above it, fix the cause, then [resume installation](./install.md#install); keep the data |
| Web answers 403 `Forbidden` | The browser host is not `OAC_PUBLIC_URL`. Open that origin; a reverse proxy must pass the original Host |
| Web shows that Core is unavailable (502) | Core is stopped or failing: `docker compose ps`, then Core's log |
| Session creation returns 400 `model_provider_required` | No model provider: set a [default model](../configuration.md#default-models) for the harness, or pass one; self-hosted Sessions always pass their own |
| Add node shows no command | See [Before you add a node](./nodes.md#before-you-add-a-node) |
| A node is not ready | See [node troubleshooting](./nodes.md#troubleshooting) |

## Exposure and network policy

| Listener | Host installation | Behind a reverse proxy |
| --- | --- | --- |
| Web and the API | Web publishes `OAC_WEB_PORT` (8080) on `OAC_HOST` | Web publishes `OAC_WEB_PORT` on `OAC_HOST`. Your proxy should use `127.0.0.1` |
| Core | No published port. Web forwards `/v1`, `/api/v1` and `/docs` | No published port. Web forwards `/v1`, `/api/v1` and `/docs` |
| PostgreSQL | No published port | No published port |

Web signs administrators in with the Core key, checks the origin of every request, and forwards signed-in `/core/v1` requests to Core with the Core key, which stays on the server. It forwards `/v1` and `/api/v1` to Core unchanged, with the caller's own credential, serves only the non-secret node payload at `/node-install/`, and has no Docker or KVM access. Machine routes under `/api/v1` use their own enrollment and connection credentials. No service receives a Docker socket.

Sandboxes are the isolation boundary ([Runtime and outer isolation](../concepts.md#runtime-and-outer-isolation)). Docker sandboxes share the node's kernel, and a Docker node is [root-equivalent](./nodes.md#what-the-installer-sets-up) on its host; microsandbox gives each sandbox a microVM with an explicit [network policy](./nodes.md#what-the-installer-sets-up). Core itself has no Docker socket or KVM access.

## Execution latency

Use the `environment input reserved` log to connect the submitting HTTP trace to `reservation_id` and `execution_trace_id`. The worker derives its diagnostic execution trace from the durable reservation identity, so observer disconnection and owner restart do not require a process-local trace map. This is an explicit link between the HTTP and execution traces, not continuation of the original HTTP span. Follow `environment input selected`, `execution preparation requested` and `environment input admitted` to connect the reservation to the Session, Environment, device, preparation request, Executor and Turn. `preparation_request_id` identifies the control request and is distinct from an HTTP `request_id`. For inputs included in Session creation, join `session initial input origin` to the worker reservation with the same `session_id` and `is_initial=true`; this covers both regular and streaming creation without an extra store query.

`execution stage` records `stage`, `duration_ms` and `status` (`ok`, `error`, `cancelled` or `timeout`). Stages cover input validation/ownership, reservation and admission wait, execution configuration and admission promotion, lifecycle gate wait, Runtime observation/provisioning, provider create/resume and Environment initialization. Runtime connection logs record confirmed observed connection transitions, not the exact socket connection instant. Use Environment, allocation and node IDs to join background Runtime records to an input; a background record without the input trace is not an unbroken request span. `runtime transport registered` records authenticated WebSocket registration. `runtime capability snapshot observed` records validated declaration changes with device ID and kind counts; transport registration alone does not establish execution readiness. Capability changes with an available Harness send a coalesced scheduling hint, and the Worker rechecks its ordinary ownership, capacity, lifecycle and capability requirements; polling recovers missed hints. Stage durations use a local monotonic clock. `queue_age_ms` instead compares selection time to the database reservation creation time and may reflect clock skew; it is not a measured scheduler-only interval.

`execution scheduling observation` is a worker-local reservation summary with `owner_observation_id`, reservation/Session identity, fixed state observed flags and offsets from the first candidate observation. `eligible_seen` means this owner first observed all existing readiness checks pass, not physical resource readiness. The fixed pairs `eligible_to_selected_ms`, `selected_to_prepare_ms`, `ready_to_promote_ms`, `promote_to_notify_ms` and `notify_to_handler_ms` require ordered anchors within this observer and the 1800000 ms bound; absent, reversed or out-of-bound anchors yield null. An existing handler read can mark `handler_observed` only for the same Worker, Session and reservation; no database read is added. Successful dispatch or an observed cancelled/failed/expired reservation releases its entry. A terminal reservation never read by that handler can retain its entry until owner closure. Terminal fields are frozen and entries released before synchronous logging outside observation locks; a slow log sink can delay its caller but does not hold the shared observation locks. An owner-local monotonic fence prevents a candidate page started before a completed observation from reopening a released entry. Already tracked entries remain usable; previously untracked entries on that old page may be conservatively skipped until a fresh page, without changing business scheduling or adding queries. This is an additional observation gap, not proof of a complete candidate history. At most 256 active entries are retained without eviction; excess observations are skipped while business work continues. `execution scheduling owner observation` reports skipped observations with a saturating uint64 `overflow_count` and `overflow_saturated`. `prefix_observed=false` and `observation_complete=false` preserve the unobserved pre-candidate interval. Outcomes are `completed/cancelled/owner_lost/failed/unknown`; diagnostic completion is not successful execution. Never combine different owner IDs into one monotonic span or use these records to infer unobserved database, capacity or candidate-page waiting.

`execution_prepare_send` measures the Core control send; `execution_prepare_wait` measures the subsequent wait for Runtime executor readiness or a terminal reservation result. Both carry Session, reservation and preparation request IDs. `environment_initialize_commit` measures the completion transaction; a successful commit wakes the scheduler, and the existing hinted scan bypasses the one-second candidate scan delay without bypassing admission checks. `input_admission_wait` includes `notification_wakeups`, `poll_wakeups` and `last_wake_source` (`none`, `notification`, `poll`); these count the branches selected by the waiting HTTP handler, not notifications sent or lost. A poll win does not prove a missing notification. `runtime_reconcile`, emitted for nonempty allocation pages, measures the observation batch and subsequent pending provisioning after inventory lookup and gate acquisition, with `allocation_count` and `observed_count`. It overlaps individual provider/observation stages and excludes earlier gate waiting. These records do not change lifecycle ordering or isolate slow allocation observations.

Each lifecycle reconciliation uses a fresh span on its caller's trace, or a fresh background trace when none exists. The same trace/span identifies `runtime reconciliation started`, `runtime_gate_wait`, `runtime_inventory`, each `runtime_observe` and `runtime_provision`, `runtime_pending_inventory`, `runtime_pending` and `runtime_reconcile_total`, including direct-provider scans whose `node_id` is empty. `runtime_inventory` measures each allocation-page query, with `refill` distinguishing the optional wrapped page; failed reads report a null `allocation_count`, not an empty page. `runtime_pending_inventory` measures the pending-page query; `runtime_pending` includes that query and attempts to provision the returned candidates, with null `candidate_count` on a failed read and `attempted_count` counting started attempts, not successes. `runtime_reconcile_total` starts before gate acquisition and ends on every return, including empty pages and errors. The existing nonempty-page `runtime_reconcile` interval keeps its narrower meaning. A successful scan can still contain failed individual observations; inspect their statuses. Shared round identity can establish whether an old allocation observation preceded pending provisioning; temporal overlap across different rounds alone cannot establish blocking. Tracing does not change scan order, fairness or cancellation.

`e2b helper call` measures a validated Go helper call from request serialization through caller return. Its fixed nullable fields measure actual calls: `request_marshal_ms` for request serialization, `command_setup_ms` for command/environment/stdio setup, `process_start_ms` for Start (including a failed Start), `caller_wait_ms` for waiting on completion or cancellation, `process_wait_ms` for the original Wait call, `fence_release_ms` for the original child-fence release callback when a fence exists, `observation_parse_ms` for bounded stderr filtering and forwarding, and `response_decode_ms` for response decoding, validation and trailing-data checks. Wait includes remaining child execution and output-pipe drain, not just process exit. Child measurements reach the caller only through its existing completion channel; cancellation before receiving completion leaves Wait/fence/parse/decode fields null, even if the owned child later drains. A missing fence or boundary not reached is null, not measured zero. Validation rejection before the call retains its existing behavior. The record includes validated `operation`, `helper_started`, finite status and allocation/environment IDs; paths, inputs and raw errors are excluded.

Only after receiving child completion, and when stderr stays within its bound, Core forwards at most four `e2b helper stage` records: `helper_startup` covers the main Python module's first monotonic timestamp through imports, request reading and Provider construction; `helper_execute` covers `Provider.execute`, including local receipt locking, SDK work and typed outcome handling; `helper_response_serialize` covers response JSON encoding, validation/size checks and any existing fallback encoding; `helper_stdout_write` covers the existing stdout write call returning. Successful serialization or writing does not imply successful provider execution. Write return does not establish flushing or parent receipt; no flush is added. A failed reached boundary records `completed=false` without error prose, while an unexecuted boundary emits nothing. The existing `--check` early-return path does not invent response serialization/write stages. Diagnostic sink failure never replaces the authoritative response or stdout exception.

`e2b create stage` separates `template_get_info` (the existing `files.get_info` call) from `template_read_check` (entering and closing the existing `files.read(format="stream")` context). Each emits at most once per create, only if reached, with duration and `completed`; a failed reached stage is false and a missing stage is unobserved. The read check does not consume the whole file. Both are children of `template_check`, not additional independent costs. These helper changes require a Core build, not a Runtime template change.

The caller-wait and process-wait intervals overlap; Python stages and create substages overlap the process-wait envelope and cannot be summed with it. The child may already execute before Go enters Wait, so Wait is not its complete lifetime. Frozen loader/interpreter work before the first Python timestamp, final interpreter shutdown and inter-stage gaps remain unmeasured separately. Residuals are uncovered overhead, not measured boot/exit time; an independent check sample cannot fill them. Timeout still returns promptly while the original owned child drains and the fence remains held until its original release. Buffers are read only after the original Wait and release complete. These diagnostics reuse the caller's trace/span and trusted operation; stdout bytes, SDK ordering, timeout/error precedence and the helper/Runtime protocols are unchanged.

`runtime first heartbeat` records the Runtime connection's first heartbeat attempt with `capability_payload_build_ms` and `first_heartbeat_send_ms`. Send includes local queue/write acknowledgement, not Core receipt. Core emits `runtime first heartbeat observed` for its first attempt with `heartbeat_decode_ms`, `capability_set_ms`, `heartbeat_store_ms` and `capability_hint_ms`. Store includes the synchronous device-heartbeat call; hint follows only successful, non-deleted persistence. Failure or closure ends observation once; unfinished stages remain null. Device IDs provide context, but the two connection IDs belong to different processes and do not prove the same first frame. Later heartbeats do not repeat these summaries.

`control_ready_ms` starts after scheduling, Runtime readiness and configuration assembly. `start_control_ms` describes the control acknowledgement. `input_to_first_text_ms` starts immediately before execution delivery, includes start-control time and is not model-only TTFT. The admission, control and first-text intervals overlap; do not sum them as independent durations. A successful provider call describes that operation, not completed daemon connection or a completed Turn. Model request start, response headers, first model text and retries remain unavailable unless the selected native adapter provides those observations. Logs contain correlation IDs and finite status categories, not input content, credentials or raw provider errors.

`native turn boundary` emits once per owned native Turn at its existing settlement boundary. It records `run_id`, nullable `preparation_request_id` and `native_turn_id`, `input_to_start_turn_ms`, `thread_resolve_ms` with `thread_mode=start/resume/reuse/unobserved`, `turn_start_write_ms`, `turn_start_ack_wait_ms` and `first_native_text_to_forward_ms`. The observed flags distinguish absent boundaries from measured zero; forwarding also records whether the original send succeeded. ACK before write return sets `ack_before_write_return=true` and leaves ACK wait null; missing ACK stays unknown. First text selects only nonempty root text and can precede ACK. These local adapter boundaries do not measure model computation or Core receipt, and ownership acknowledgement is not completion. No input, RPC body or native error is logged. These records require a rebuilt Runtime and qualified template; Core alone cannot add them to an existing sandbox.

`journal first text received` marks the first nonempty delta or native output-message text accepted into the delivery journal, including final drain. Its execution `journal_sequence` is not the public Session event sequence. `journal first text persistence` records the first unsuccessful append as `unconfirmed` and eventual successful append return as `confirmed`, with attempt count, append duration and receipt-to-confirmation duration (null on failure). This is caller confirmation, not the database's exact commit instant. A private per-attempt collector captures the actual first public text `event_id` for that exact source. `mapping_status=confirmed` requires a successful append return; an inserted but unconfirmed event remains a candidate. Uncertain commit followed by idempotent replay may confirm the journal without observing a public mapping: `mapping_status=unobserved` and an empty event ID are intentional. Never promote an earlier candidate because replay succeeded. A subsequent SSE read of that exact ID proves existence, not its original commit timestamp. Unchanged native message projections may produce no public text event. Each Turn emits at most one receipt, one failed attempt and one confirmation, without text or raw errors.

`journal first text persistence` also carries `batch_wait_ms` until the first flush containing the selected source and `first_flush_trigger=size/timer/function_call/deferred_final`. `append_count`, `append_total_ms`, `append_max_ms`, `append_failed_count` and `append_unknown_count` are cumulative across its attempts. Failure and unknown counters may overlap; a returned error does not establish rollback. The first failure and eventual confirmation snapshots must not be added together. No extra per-retry log or flush is introduced.

`session stream first text read` and `session stream first text output` cover the first nonempty public text delta/done observed for each Turn on a connection, including subsequent Turns on that same connection. A connection retains at most 64 distinct Turn IDs without eviction, plus one shared missing-ID slot; duplicate events, late old-Turn text and repeated lifecycle events never reset a selected identity. Beyond that bound, one `session stream text observation limit` record reports `status=unobserved` and `reason=turn_limit`; forwarding continues unchanged. First text and terminal use independent bits per identity; terminal needs no text. A retained Turn can select its other category even at the 64-Turn limit. `session stream terminal read` and `session stream terminal output` use `scope=terminal_per_turn_connection` for completed, failed or cancelled events, the same actual event ID and Write/Flush boundaries, and an `event_type` field. Together the categories emit at most 260 boundary records plus one limit record per connection. They carry actual `event_id`, `session_event_sequence`, Turn/Session IDs, a fresh `connection_id`, `start_cursor`, `turn_identity_observed`, and `scope=first_text_per_turn_connection`; reconnects do not claim the Turn's globally first text. `read_at` marks the database read return for its batch, before earlier batch events are written. Output separates Write and Flush durations/statuses, using null and `unobserved` for boundaries not reached. Flush success means `ResponseController.Flush` returned nil, not downstream receipt. A short write is reported without changing existing behavior. `read_to_emit_return_ms` includes preceding batch output, projection, authorization, serialization, Write and Flush; do not add it to its children. Join by actual event ID, never neighboring timestamps or different sequence domains. GET remains live-only; missed events have no invented replay observations. Batch thresholds, polling, wire format, transactions and authentication are unchanged.

`session stream first text read` carries `timing_scope=connection_prefix`: `empty_read_count`, `authority_cache_hit_count`, and `poll_wait`, `authority_check` and `list_events` count/total/max fields accumulate from this connection's start. They are not exclusive Turn costs and cannot be summed across Turn snapshots. `first_text_list_ms` describes the actual query containing that text; multiple events in the same page share it. `last_empty_to_first_read_ms` requires an observed empty page. The optional last poll/check/list intervals describe the latest loop boundaries, not a complete additive decomposition; cached authority has no check duration. If no text was observed, one `session stream observation ended` records the outcome and counters with null first-text boundaries. Polling and authority refresh intervals are unchanged.

`executor done received` marks the first main-loop done before journal observation, not model completion. `delivery_final_drain` and `delivery_final_flush` measure final drain and flush. Flush includes `pending_count`, `confirmation`, and `done_to_cleanup_ms` (observed done to cleanup start; null if not observed). Successful empty flush is `no_pending`, not a new commit. Done seen only in final drain has no invented receipt boundary. Preparation or function receipts may still be awaited after done.

Completed Turns with local workspace configuration (`openai_hosted` or `self_hosted`) record `artifact_capture_begin`, `artifact_workspace_prepare`, `artifact_export_total`, `artifact_export_staging`, `artifact_workspace_release`, and `artifact_capture_total`. Prepare includes configuration/subscription/send/readiness wait; release includes send/ACK. Staging includes remote archive reads, PostgreSQL large-object writes and staging transaction work, not database-only latency. Capture total includes failure-path `artifact_failure_cancel_check`. Early prepare failure can log `artifact_workspace_cleanup` with `confirmation=unobserved`: `status=ok` means cleanup returned, not remote release acknowledgement. `artifact capture skipped` uses `turn_not_completed` or `workspace_not_local`. The environment-none dispatcher never invokes capture; absent logs are not measured zero. Ordinary directory browsing does not activate this scope. Parent and child durations overlap; do not sum them.

Artifact capture carries an immutable `capture_id` through its existing stages and bounded summaries; ordinary directory reads and calls without that capture scope emit no Core artifact summary. `artifact export reader` emits once per registered export, including consumer failure and cleanup, with `export_id`. Invalid requests or registration rejection have no reader summary. `enqueue_ms` sums begin/next Send calls: success means queue admission, not a WebSocket write. `begin_count` and `next_count` count attempted sends, including the final request for completion; `receipt_wait_count` counts waits actually entered. `receipt_wait_ms` and `receipt_wait_max_ms` include remote work, transport and scheduling until each receipt or interruption, not pure network time. `chunk_count` and `received_bytes` count only validated data chunks; `delivered_bytes` counts bytes copied from those chunks to the consumer or final drain, without double counting recursive reads or small buffers. `first_data_ms` and `completed_receipt_ms` are offsets from reader setup and are null if not observed. `completed` means a validated completion receipt, not successful staging. `cancel_enqueue_ms` is null unless cleanup attempted cancellation, and `cancel_enqueued` confirms only its queue admission. An empty tar can still need data chunks and a separate completion request; archive EOF is not an exporter completion receipt. The reader interval includes consumer work and cleanup; do not add receipt waits to it.

`artifact staging` emits once when the capture scope matches the Session and Turn. `with_staging_ms` encloses the original WithArtifactStaging call through return, including transaction acquisition and completion. It is not a database commit timestamp, and its remainder after child intervals is not commit-only latency. `load_environment_ms`, `read_export_ms`, `lock_session_ms`, `load_turn_ms` and `stage_artifacts_ms` measure the existing calls and are null for boundaries not reached. `put_ms` sums the `put_count` attempted content writes (both zero when none occurred); Put includes reads from the remote export, so it is nested within read-export and is not pure database time. Receipt waits can occur inside Put or archive-header/trailer reads. The staging `confirmation` is confirmed only on successful outer return; failure, rollback or unknown outcome remains unconfirmed even if staging callbacks completed. This does not claim final Turn publication. These summaries log only identity, finite outcomes, durations and counters, never file paths, content or raw errors; their count is independent of files and chunks. Read order, lock timing, transactions, cancellation, error precedence and the Runtime protocol remain unchanged.

`artifact transport` emits one Core snapshot for the capture, mapping `capture_id`, `session_id` and `turn_id` to the existing `preparation_request_id` and `export_id`. Its `connection_id` identifies a Core-local gateway connection. `scope_status=unobserved` marks observation registration or identity limits, without changing the business result. `outbound_queue_wait` starts after queue admission and ends at dequeue; `outbound_encode` and `outbound_write` cover encoding and the WebSocket write call. `inbound_decode` and `inbound_dispatch` cover the matched incoming frame's decoding and handler dispatch. Each stage has `_count`, `_total_ms`, `_max_ms`, `_failed_count` and `_unknown_count`; totals and maxima are null when no completed measurement exists. `outbound_unfinished_count` counts frames still queued or being written at capture closure. `snapshot_complete=false` preserves partial observation, including active measurements or an unavailable queue-admission timestamp; Core does not wait for diagnostics to complete.

`runtime workspace transport` emits a bounded snapshot for each accepted workspace-only preparation and export owner. This Runtime scope includes workspace operations outside Core artifact capture; it does not observe Harness preparation, and a Runtime record alone does not identify an artifact. `scope=preparation` carries `preparation_request_id`; `scope=export` carries `export_id`. Join these existing request identities to Core's capture mapping. `connection_id` is Runtime-local and is not equal to Core's connection identity. `identity_valid=false` omits an unsafe request identity as an empty value; `connection_mismatch=true` prevents attributing transport work to another connection. Neither case permits a time-based substitute join.

Runtime stage names are `receive_dispatch_wait`, `request_queue_wait`, `export_producer`, `export_read`, `export_done_wait`, `prepare_local`, `release_local`, `sender_call`, `outbound_queue_wait`, `outbound_encode` and `outbound_write`. Receive delay runs from decoded arrival to dispatcher consumption, including receive-channel backpressure; request queue delay starts after request admission. Producer time covers local export production; read time covers pipe reads; done wait is the existing producer-completion wait before sending completion. Local preparation and release cover their local work, while sender and transport stages measure status/result delivery. Every stage uses the Core snapshot suffixes plus `_cancelled_count`; null totals and maxima mean no completed measurement. `read_bytes`, `chunk_count` and `local_eof` describe local pipe consumption, not Core receipt or staging. `transport_observed` indicates that the matching transport accepted observation metadata. `outcome` and `snapshot_complete` describe different facts: a complete observation may contain failed operations, and shutdown, suspended publication or in-flight work remains partial. Workspace-only preparation ignores superseded status revisions; a failed terminal notification never restarts completed cleanup. If shutdown or suspension suppresses publication, the owner closes its snapshot outside the router lock with `outcome=shutdown` or `outcome=suspended` and `snapshot_complete=false`, even when its settled preparation record has already been pruned. This is not a release acknowledgement.

Core and Runtime capture summaries also measure `read_return_to_decode_begin` after a successful WebSocket read and before JSON decoding; Runtime adds `inbound_decode`. Only a decoded frame associated with an existing scope is counted. This includes local predecode work and scheduling, not the blocking read or network-only time. Each stage exposes `_overflow`; bounded counters saturate and excess or unfinished measurements remain unknown rather than wrapping.

`artifact export reader` and Runtime export summaries include `rounds_json`, a JSON string containing at most the first eight request rounds with `round_index`, `phase=begin/next`, `offset`, `observed`, fixed `outcome` and a nullable duration. Core measures `request_send_to_receipt_dispatch_ms` from before the original Send call to matched local receipt dispatch; Runtime measures `request_read_to_response_write_ms` from successful read to the original response write returning, with the endpoint captured before acquiring the subsequent observation lock. Their count/total/max/unknown aggregates cover additional rounds with `round_count`, `rounds_truncated` and `rounds_overflow`; counts cap at 1000000 and duration/total at 1800000 ms. Core has null rounds before any attempt; Runtime uses an empty array. Core round outcomes are `chunk/completed/unknown`; Runtime additionally permits `ok/error/failed/rejected`. An unknown round has null duration. No write-return-to-receipt interval or new completion request is synthesized. These envelopes overlap earlier queue/write/receipt measurements and do not isolate one-way transmission.

Runtime `export_writer` measures the original pipe writer calls with count/total/max/failed/cancelled/unknown/overflow and `export_writer_observed`. It preserves the original bytes, returned count/error and backpressure. The interval includes consumer progress and local scheduling; it is not pure blocking or disk time. `export_producer` contains writer calls. Only a complete serial producer and complete writer measurements permit describing their difference as work outside the writer envelope, never filesystem or tar CPU time. In-flight snapshots and saturation remain partial. These Runtime additions require a rebuilt daemon and qualified template; a Core-only deployment cannot provide them.

These records use process-local monotonic durations and fixed-size summaries, not per-frame logs. Sender return, WebSocket write return, validated Core completion receipt and confirmed staging remain separate boundaries. An asynchronous status publication does not prove a socket write; `local_eof` does not prove receipt. Unknown or unfinished measurements remain unknown; a measured `0.000` millisecond interval only reflects resolution. Parent/child intervals and producer/pipe-consumer intervals overlap and must not be added together. Cross-process timestamps help correlate records but do not establish network RTT. Runtime records require the corresponding daemon build; replacing Core or its E2B Provider helper does not upgrade an existing Runtime template. The E2B Provider helper runs on the Core side and is not embedded in the Runtime.

`turn_completion` measures each existing `CompleteExecution` call through transaction return, with `requested_turn_status`, `returned_turn_status`, `attempt`, `confirmation`, `mapping_status` and actual candidate `event_id`. Returned status is populated only on success; requested status does not prove the outcome. Each attempt has a fresh private collector matching exact Session, Turn and expected terminal type at `AppendChanges`, independently of first-text source projection. An inserted candidate remains `unconfirmed` on failure; successful return confirms only that attempt. `mapping_status=unobserved` with an empty ID does not establish a public terminal event, even on successful return. Unlike journal append, `CompleteExecution` is not idempotent replay: retry after unknown commit may return Turn conflict and cannot confirm an earlier candidate. The existing unapplied-input fallback remains a distinct second attempt. Deployment-provider observation runs after completion returns and cannot directly block availability of an already committed terminal event. Join commit and SSE using actual terminal event ID, never journal ordinal or adjacent timestamps. Transactions, wire, retries and Runtime template requirements are unchanged.

Daemon startup logs record `runtime process starting` with its build version and `runtime startup stage` for each Harness discovery, bootstrap and transport dial attempt. Durations use a local monotonic clock and omit credentials and raw errors. Discovery runs before transport registration. Compare daemon timestamps with Core registration and capability observations to separate startup from periodic observation and scheduling. These daemon records require a Runtime built with this instrumentation; updating Core alone does not update an existing Runtime template. A managed startup receipt confirms process spawn, not connection or capability readiness.

Codex CLI availability emits `runtime version probe` records for `process_spawn` and `process_wait`. `first_stdout` measures from the spawn attempt to the first nonempty stdout chunk observed by the Go copier; `stdout_to_completion` measures from that observation to `Wait` returning, including remaining execution and pipe draining. Both are omitted when stdout is absent. They are emitted after completion and never authorize early success: process exit and output checks still decide availability. `runtime version probe resources` reports the OS process accounting returned by `ProcessState` as `user_cpu_ms` and `system_cpu_ms`; this may include reaped descendants according to the platform, and is omitted when process accounting is unavailable. Fault and storage-byte counters are not collected; their absence is unavailable, not zero or evidence of a storage/network cause. None of these intervals is model execution.
