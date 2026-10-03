# Web: the console. It serves the built frontend from the image and forwards
# signed-in /core/v1 requests to Core with the Core key. Rendered with envsubst.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oac-web
  labels:
    app.kubernetes.io/name: oac-web
    app.kubernetes.io/part-of: openagentcore
spec:
  # Web holds console sign-in sessions in process memory, so a cookie is valid
  # only on the Pod that issued it. One replica, and no surge that would send a
  # signed-in operator to a Pod that does not know the cookie. A rollout signs
  # operators out; they sign in again with the Core key.
  replicas: 1
  strategy:
    type: Recreate
  progressDeadlineSeconds: 600
  selector:
    matchLabels:
      app: oac-web
  template:
    metadata:
      labels:
        app: oac-web
        app.kubernetes.io/name: oac-web
        app.kubernetes.io/part-of: openagentcore
      annotations:
        io.oac/configuration-digest: "${OAC_CONFIGURATION_DIGEST}"
    spec:
      imagePullSecrets:
        - name: oac-registry
      terminationGracePeriodSeconds: 30
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      volumes:
        # Web rejects a Core key file that grants group or other access, and
        # Kubernetes owns the files of a Secret volume as root. The preparation
        # step copies the key to Web's user with owner-only access.
        - name: secret-source
          secret:
            secretName: oac-web-core-key
            defaultMode: 0400
        - name: secrets
          emptyDir:
            medium: Memory
        - name: tmp
          emptyDir: {}
      initContainers:
        # The Web image is distroless and has no shell, so the preparation step
        # runs Core's image from the same release.
        - name: prepare
          image: "${OAC_CORE_IMAGE}"
          imagePullPolicy: IfNotPresent
          command:
            - /bin/sh
            - -eu
            - -c
            - |
              chown 65532:65532 /run/oac
              chmod 700 /run/oac
              install -o 65532 -g 65532 -m 0400 /run/oac-source/core.key /run/oac/core.key
          securityContext:
            runAsUser: 0
            runAsGroup: 0
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
              add: ["CHOWN", "FOWNER", "DAC_OVERRIDE"]
          volumeMounts:
            - name: secret-source
              mountPath: /run/oac-source
              readOnly: true
            - name: secrets
              mountPath: /run/oac
          resources:
            requests:
              cpu: 10m
              memory: 32Mi
            limits:
              cpu: 200m
              memory: 128Mi
      containers:
        - name: web
          image: "${OAC_WEB_IMAGE}"
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          env:
            - name: OAC_WEB_ADDR
              value: ":8080"
            # The exact browser-facing origin. Web serves no other host.
            - name: OAC_WEB_ORIGIN
              value: "${OAC_PUBLIC_URL}"
            - name: OAC_WEB_UPSTREAM
              value: "http://oac-core:8091"
            - name: OAC_WEB_CORE_KEY_FILE
              value: "/run/oac/core.key"
            - name: OAC_LOG_LEVEL
              value: "${OAC_LOG_LEVEL}"
            - name: OAC_LOG_FORMAT
              value: "json"
          securityContext:
            runAsUser: 65532
            runAsGroup: 65532
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: secrets
              mountPath: /run/oac
              readOnly: true
            - name: tmp
              mountPath: /tmp
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
          startupProbe:
            httpGet:
              path: /healthz
              port: http
            periodSeconds: 2
            timeoutSeconds: 2
            failureThreshold: 30
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            periodSeconds: 30
            timeoutSeconds: 5
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
---
apiVersion: v1
kind: Service
metadata:
  name: oac-web
  labels:
    app.kubernetes.io/name: oac-web
    app.kubernetes.io/part-of: openagentcore
spec:
  type: ClusterIP
  selector:
    app: oac-web
  ports:
    - name: http
      port: 8080
      targetPort: http
