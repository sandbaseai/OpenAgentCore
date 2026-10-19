# Creative Runtime tools

Use this image for browser-based visual Skills. It adds Python with venv support, curl, ZIP tools, FFmpeg/ffprobe, Noto Latin/CJK/emoji fonts and a lockfile-pinned Playwright Chromium to a qualified Debian Bookworm Runtime. Skill-specific npm/Python packages remain project dependencies; this image does not choose their versions or install every Skill.

Build from the repository root with an immutable qualified Linux amd64 Runtime image:

```sh
docker build --platform linux/amd64 \
  --build-arg RUNTIME_IMAGE=sha256:QUALIFIED_RUNTIME_IMAGE_DIGEST \
  -f deploy/distribution/CreativeRuntime.Dockerfile -t your-creative-runtime .
docker run --rm --entrypoint oac-creative-check your-creative-runtime
```

`OAC_RUNTIME_TOOL_ENV_FILE` points to the Runtime-owned tool environment containing browser paths for Playwright, HyperFrames and Puppeteer. The image owns this file; Session preparation reads it using the existing explicit tool-environment contract. If an operator needs additional tool variables, create one combined private file and select it instead. Never put credentials into the image.

Run `oac-creative-check /workspace/outputs/creative-check` inside the prepared execution Environment to produce a screenshot and a one-second H.264 video. The command verifies browser startup, CJK font selection, FFmpeg and ffprobe. These are tool checks; qualify each Skill with its real scripts and inspect the resulting film separately. Browser processes run without Chromium's inner sandbox; the outer Runtime container or E2B VM remains the isolation boundary.

The E2B builder detects `/opt/oac-creative/install.sh` in the qualified image and runs its `--system-only` mode after extracting the Runtime payload. This restores Debian libraries and fonts that the `/opt` archive cannot carry, while reusing the same pinned browser and npm files. Use the existing [E2B build command](../e2b/README.md). The output receipt includes `creative_tools`. Build completion does not activate a deployment: select and qualify the immutable template through the existing Sandbox deployment workflow before directing user Sessions to it.

Browser/npm versions are pinned in `package-lock.json`. Debian packages follow the configured Bookworm security repositories; record the image digest and template build for release provenance.
