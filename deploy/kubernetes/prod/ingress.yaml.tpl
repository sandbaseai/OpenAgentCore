# One public origin for Core and Web, split by path. The annotations below are
# ingress-nginx's; another controller needs its own equivalents for the four
# requirements in docs/getting-started/install-options.md#https-and-the-reverse-proxy:
# preserve Host, pass WebSocket upgrades on /api/v1, never buffer or time out a
# stream, and accept large uploads.
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: oac
  labels:
    app.kubernetes.io/part-of: openagentcore
  annotations:
    # /v1 streams Session events and /api/v1 carries long-lived WebSockets.
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    # Core enforces its own upload limits; source files may reach 512 MiB.
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
spec:
  ingressClassName: "${OAC_INGRESS_CLASS}"
  tls:
    - hosts:
        - "${OAC_PUBLIC_HOST}"
      secretName: "${OAC_TLS_SECRET}"
  rules:
    - host: "${OAC_PUBLIC_HOST}"
      http:
        paths:
          # Applications, with a Project API key.
          - path: /v1
            pathType: Prefix
            backend:
              service:
                name: oac-core
                port:
                  number: 8091
          # Nodes, sandboxes and self-hosted machines. Uses WebSockets.
          - path: /api/v1
            pathType: Prefix
            backend:
              service:
                name: oac-core
                port:
                  number: 8091
          # Browsers.
          - path: /
            pathType: Prefix
            backend:
              service:
                name: oac-web
                port:
                  number: 8080
