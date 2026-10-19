#!/bin/sh
set -eu
# Reused after the /opt payload is extracted into the E2B base image.
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl unzip zip xz-utils \
  python3 python3-pip python3-venv git ripgrep ffmpeg fontconfig \
  fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji fonts-liberation
cd /opt/oac-creative
if [ "${1:-}" != "--system-only" ]; then
  npm ci --omit=dev --ignore-scripts --no-audit --no-fund
  PLAYWRIGHT_BROWSERS_PATH=/opt/oac-creative/browsers node node_modules/playwright/cli.js install chromium
fi
node node_modules/playwright/cli.js install-deps chromium
rm -rf /var/lib/apt/lists/*
fc-cache -f
PLAYWRIGHT_BROWSERS_PATH=/opt/oac-creative/browsers node configure.mjs
ln -sf /opt/oac-creative/check.mjs /usr/local/bin/oac-creative-check
chmod -R a+rX /opt/oac-creative
chmod 0755 /opt/oac-creative/check.mjs
chown 1000:1000 tool-env.json
chmod 0600 tool-env.json
