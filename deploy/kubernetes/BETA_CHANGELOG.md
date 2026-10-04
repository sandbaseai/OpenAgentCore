# Beta integration ledger

`beta` starts from the SandBase fork's `main`. Record each upstream integration here with its exact source commit and deployment requirements. Publish stable releases from `main` after the corresponding changes land upstream and the fork synchronizes them. Check ancestry and remaining beta-only changes before switching back; branch names alone do not establish runtime-state compatibility.

## 2026-10-04 — Unified sandbox suspension

| Item | Value |
| --- | --- |
| Fork main baseline | `f9bffbbad8be805f278096d11237be13813c4067` |
| Integrated upstream PR | [MiniMax-AI/OpenAgentCore #297](https://github.com/MiniMax-AI/OpenAgentCore/pull/297) |
| Upstream branch | `codex/e2b-unified-pause` |
| Integrated source commit | `14199e42bcb84d5daa7dfc2c851751c30cb3939f` |
| Merge | Non-fast-forward merge; no conflicts |
| Schema migrations / DDL | No added or modified files in `services/core/migrations` |
| SQL changes | Allocation creation records the compute protocol version; activity tracking and idle suspension cover initialized zero-Turn Sessions; startup checks incompatible unreleased allocation state |

The integration adds a typed suspension lifecycle, E2B pause/resume with durable operation receipts and uncertain-outcome recovery, retained-allocation expiry cleanup, and configurable active/retained capacity. Capacity defaults are 100 active and 400 retained; [configuration](../../docs/configuration.md) owns their settings and constraints.

### Deployment requirements

- Before upgrading, inspect all unreleased runtime allocations, including disabled and suspended allocations. The new worker rejects states without the required `compute_state.protocol_version` (`1`). Use the previous release's supported archive operations to release incompatible allocations before upgrading; preserve Session history and provider receipts. Do not patch receipt JSON to bypass the check.
- Deploy matching Core and its packaged E2B helper together (helper protocol version `2`). Build and qualify an E2B template with this revision's template builder and matching Runtime artifacts before enabling suspension. The builder supplies the private suspend-control path and managed initialization prepares its directory. The Core/Web Kubernetes workflow does not build or activate E2B templates.
- Existing E2B deployment records may have persisted idle/retention values of zero. The new Runtime policy requires positive values. After upgrading, update the E2B selection to the newly qualified template through the supported administrative deployment change flow, supplying the current generation; verify the stored suspension policy (registration defaults: 300 seconds idle, 86400 seconds retention). An identical save can be a no-op and does not repair the stored values. Qualify a fresh Session and Turn before admitting traffic.
- If node providers are used, upgrade matching node binaries and generation artifacts with Core: the node wire protocol changes from `4` to `5`, and older frames are rejected. This E2B-only Kubernetes deployment does not publish node artifacts.
- Returning to main requires compatibility review of allocation state, helper protocol and template/Runtime behavior, even though this integration adds no schema migration. Do not assume an older main commit can consume beta-created state.
- Beta uses the same production database, PVC, hostname and Services when selected by the deployment workflow. Archive incompatible allocations and verify template readiness before a production rollout.

### Verification

Upstream [CI run 37126282818](https://github.com/MiniMax-AI/OpenAgentCore/actions/runs/37126282818) passed its selected checks at the integrated source commit. Local validation passed the Go sandbox, process configuration and Runtime bootstrap tests, focused deployment/execution/server tests, 188 E2B helper tests, 11 E2B template tests, the generated helper contract check and SQL generation freshness check. Database lifecycle integration and fresh cloud execution remain separate qualification gates. This ledger records source integration; it does not establish production activation or fresh E2B Session/Turn acceptance.
