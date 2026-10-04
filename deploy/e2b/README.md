# Combined E2B template build

Run **Actions → e2b-template-build → Run workflow** from `main`. Select `main` or `beta` as `source_branch`; optionally enter a full commit SHA belonging to that branch. An empty `ref` pins the branch tip once at the start of the run.

This fork-owned workflow builds one Linux amd64 Runtime image containing `oac-daemon`, Codex, Claude SDK and MiniMax Code, then packages it as one E2B template using the selected revision's maintained builders and pinned dependencies. The three intermediate images are local build stages, not three templates. The build uses the maintained `deploy/distribution/Runtime.Dockerfile` and does not build Core, Web or a full offline distribution.

## GitHub configuration

Create the `e2b-build` GitHub Environment and restrict its deployment branches to `main`. Configure:

| Kind | Name | Value |
| --- | --- | --- |
| Secret | `E2B_API_KEY` | The key for the E2B-compatible account that owns the template |
| Variable | `E2B_API_URL` | The exact HTTPS API origin, without a path or trailing slash; use `https://api.e2b.app` for official E2B or your compatible service's endpoint |

Both are required. For the SandBase endpoint, set `E2B_API_URL` to `https://sandbox.sandbase.ai` (HTTPS, without the trailing slash). The workflow supplies the endpoint through the pinned SDK's `E2B_API_URL` setting. Template creation/upload/build APIs must be implemented by the endpoint; Sandbox Create compatibility alone does not establish template-build support. Returned upload destinations must be reachable from GitHub's hosted runner. The builder currently requests 2 vCPUs and 2048 MiB, as defined by the upstream [template builder](../../services/core/deploy/e2b/README.md#build-a-template).

The E2B key is available only to the settings check and template-build steps. It is temporarily written to a private file, removed on exit, excluded from Runtime images and build reports, and never sent to a model provider. No model credentials are needed to build the template. A run creates a cloud template build and may incur the provider's build charges; do not trigger a run merely to validate workflow syntax.

## Output and qualification

The run summary and the `e2b-template-*` artifact contain `template.json`: the immutable `templateID:build_UUID`, source commit and branch, image ID, packaged Runtime checksum, base image, endpoint and advertised Harnesses. Template names include the run ID and attempt so another run does not replace this build's name. There is no automatic retry of an uncertain cloud build; inspect the provider before starting another run after a failure.

The image checks verify committed daemon/adapter identity and native package loading/version checks. Template build completion establishes packaging readiness, not Core enrollment, real model execution or pause/resume qualification. The workflow changes no active Core selection, Kubernetes resource or production Session. [Sandbox deployment](../../contracts/agents-api/sandbox-deployment.md) owns later template selection and generation behavior.
