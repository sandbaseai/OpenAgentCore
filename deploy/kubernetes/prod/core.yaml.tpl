# Core: the Agents API, the Core administration API and the machine routes, plus
# the execution worker. Rendered by the deploy workflow with envsubst.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oac-core
  labels:
    app.kubernetes.io/name: oac-core
    app.kubernetes.io/part-of: openagentcore
spec:
  # Core takes a PostgreSQL lease that gives one execution service per database,
  # so a second replica exits at startup and a surging Pod cannot take over from
  # a running one. One replica, replaced only after the previous one stops.
  replicas: 1
  strategy:
    type: Recreate
  progressDeadlineSeconds: 900
  selector:
    matchLabels:
      app: oac-core
  template:
    metadata:
      labels:
        app: oac-core
        app.kubernetes.io/name: oac-core
        app.kubernetes.io/part-of: openagentcore
      annotations:
        # Core reads its environment and its secret files once, so a settings or
        # secret change must replace the Pod even when the image is unchanged.
        io.oac/configuration-digest: "${OAC_CONFIGURATION_DIGEST}"
    spec:
      imagePullSecrets:
        - name: oac-registry
      terminationGracePeriodSeconds: 90
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      volumes:
        # Kubernetes owns the files of a Secret volume as root, and Core's user
        # cannot read an owner-only file it does not own. The preparation step
        # copies each one to Core's user with owner-only access.
        - name: secret-source
          secret:
            secretName: oac-core-secrets
            defaultMode: 0400
        - name: secrets
          emptyDir:
            medium: Memory
        # E2B receipts must outlive this Pod. The deploy workflow always mounts
        # the claim owned by core-state.yaml.tpl.
        - {name: state, ${OAC_STATE_VOLUME}}
        - name: tmp
          emptyDir: {}
      initContainers:
        # The volume arrives owned by root, and the E2B adapter requires its state
        # directory to exist with no group or other access.
        - name: prepare
          image: "${OAC_CORE_IMAGE}"
          imagePullPolicy: IfNotPresent
          command:
            - /bin/sh
            - -eu
            - -c
            - |
              mkdir -p /state/e2b
              chown -R 65532:65532 /state
              chmod 700 /state/e2b
              chown 65532:65532 /run/oac
              chmod 700 /run/oac
              for name in database.password credential.key core-key-digests.json; do
                install -o 65532 -g 65532 -m 0400 "/run/oac-source/$name" "/run/oac/$name"
              done
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
            - name: state
              mountPath: /state
          resources:
            requests:
              cpu: 10m
              memory: 32Mi
            limits:
              cpu: 200m
              memory: 128Mi
        # Core never migrates its own schema. The schema must match the image
        # before Core opens the database.
        - name: migrate
          image: "${OAC_CORE_IMAGE}"
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/oac-core-migrate"]
          envFrom:
            - configMapRef:
                name: oac-core-env
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
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 512Mi
      containers:
        - name: core
          image: "${OAC_CORE_IMAGE}"
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 8091
              protocol: TCP
          envFrom:
            - configMapRef:
                name: oac-core-env
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
            - name: state
              mountPath: /state
            - name: tmp
              mountPath: /tmp
          resources:
            requests:
              cpu: "${OAC_CORE_CPU_REQUEST}"
              memory: "${OAC_CORE_MEMORY_REQUEST}"
            limits:
              cpu: "${OAC_CORE_CPU_LIMIT}"
              memory: "${OAC_CORE_MEMORY_LIMIT}"
          startupProbe:
            httpGet:
              path: /healthz
              port: http
            periodSeconds: 3
            timeoutSeconds: 3
            failureThreshold: 60
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
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
---
apiVersion: v1
kind: Service
metadata:
  name: oac-core
  labels:
    app.kubernetes.io/name: oac-core
    app.kubernetes.io/part-of: openagentcore
spec:
  type: NodePort
  selector:
    app: oac-core
  ports:
    - name: http
      port: 8091
      targetPort: http
