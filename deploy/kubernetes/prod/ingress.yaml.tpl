# Applied separately after DNS and the qcloud certificate reference are ready.
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: oac
  labels:
    app.kubernetes.io/part-of: openagentcore
  annotations:
    kubernetes.io/ingress.class: qcloud
    kubernetes.io/ingress.existLbId: "${OAC_CLB_ID}"
    ingress.cloud.tencent.com/enable-group: "true"
    ingress.cloud.tencent.com/auto-rewrite: "true"
    ingress.cloud.tencent.com/rewrite-support: "true"
    kubernetes.io/ingress.rule-mix: "true"
    kubernetes.io/ingress.extensiveParameters: '{"ConnectTimeout":300000,"SendTimeout":900000,"ReadTimeout":900000}'
    kubernetes.io/ingress.http-rules: "null"
    kubernetes.io/ingress.https-rules: |
      [{"host":"${OAC_PUBLIC_HOST}","path":"/v1","backend":{"serviceName":"oac-core","servicePort":"8091"}},
       {"host":"${OAC_PUBLIC_HOST}","path":"/api/v1","backend":{"serviceName":"oac-core","servicePort":"8091"}},
       {"host":"${OAC_PUBLIC_HOST}","path":"/","backend":{"serviceName":"oac-web","servicePort":"8080"}}]
spec:
  ingressClassName: qcloud
  tls:
    - hosts: ["${OAC_PUBLIC_HOST}"]
      secretName: "${OAC_TLS_SECRET}"
  rules:
    - host: "${OAC_PUBLIC_HOST}"
      http:
        paths:
          - path: /v1
            pathType: Prefix
            backend:
              service:
                name: oac-core
                port: {number: 8091}
          - path: /api/v1
            pathType: Prefix
            backend:
              service:
                name: oac-core
                port: {number: 8091}
          - path: /
            pathType: Prefix
            backend:
              service:
                name: oac-web
                port: {number: 8080}
