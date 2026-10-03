# Core's persistent E2B adapter state volume.
#
# Only the E2B adapter uses this volume today: it holds the receipts Core writes
# before each remote Create and needs afterwards to clean up, observe and verify
# ownership of sandboxes in E2B's cloud. They are small, they are never pruned,
# and losing them orphans billed sandboxes Core can no longer destroy. An
# installation with no E2B deployment writes nothing here and needs no claim.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: oac-core-state
  labels:
    app.kubernetes.io/name: oac-core
    app.kubernetes.io/part-of: openagentcore
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: "${OAC_STORAGE_CLASS}"
  resources:
    requests:
      storage: "${OAC_STATE_SIZE}"
