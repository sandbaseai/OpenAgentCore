# Core's process environment. Rendered by the deploy workflow with envsubst.
# Every variable here is documented in docs/configuration.md; this file holds no
# secret. The database password, the credential key and the Core key digest live
# in the oac-core-secrets Secret the workflow creates from GitHub Environment
# secrets, and Core reads each of them from a file.
apiVersion: v1
kind: ConfigMap
metadata:
  name: oac-core-env
  labels:
    app.kubernetes.io/name: oac-core
    app.kubernetes.io/part-of: openagentcore
data:
  OAC_ADDR: ":8091"
  OAC_PUBLIC_URL: "${OAC_PUBLIC_URL}"
  OAC_INSTALLATION_ID: "${OAC_INSTALLATION_ID}"

  OAC_DATABASE_URL: "${OAC_DATABASE_URL}"
  OAC_DATABASE_PASSWORD_FILE: "/run/oac/database.password"
  OAC_CREDENTIAL_KEY_FILE: "/run/oac/credential.key"
  OAC_CORE_KEY_DIGESTS_FILE: "/run/oac/core-key-digests.json"

  # Adapter artifacts ship in the image; adapter state lives on the Core volume.
  OAC_PROVIDER_ROOT: "/opt/oac"
  OAC_PROVIDER_STATE_ROOT: "/state"

  OAC_LOG_FORMAT: "json"
  # The deploy workflow appends the process settings the operator set here. A
  # setting nobody set stays absent, so Core applies its own documented default
  # instead of a copy of it.
