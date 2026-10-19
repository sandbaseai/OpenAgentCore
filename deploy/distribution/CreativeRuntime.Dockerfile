# Build from a qualified immutable Runtime image; use the repository root context.
ARG RUNTIME_IMAGE
FROM ${RUNTIME_IMAGE}
USER root
COPY services/core/deploy/creative /opt/oac-creative
RUN sh /opt/oac-creative/install.sh
ENV OAC_RUNTIME_TOOL_ENV_FILE=/opt/oac-creative/tool-env.json \
    PLAYWRIGHT_BROWSERS_PATH=/opt/oac-creative/browsers
USER 1000:1000
RUN node /opt/oac-creative/check.mjs
