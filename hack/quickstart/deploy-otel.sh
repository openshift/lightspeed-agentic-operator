#!/usr/bin/env bash
#
# Deploy the OTEL collector for quickstart.
#
# By default, accepts OTLP gRPC/HTTP and drops all data (nop exporter).
# With --traces=ENDPOINT, exports traces to an OTLP gRPC endpoint.
# With --postgres, deploys a Postgres instance and routes agentic logs
# to it for the console audit UI.
#
# The Service uses service-ca for TLS. Its generated serving-cert secret
# (lightspeed-otel-collector-cert) is mounted by the collector only. Clients
# must instead trust the service CA via lightspeed-agentic-otel-ca (key:
# otel-ca.crt), which deploy-configmap.sh references as otel-ca-secret.
#
# Usage:
#   bash hack/quickstart/deploy-otel.sh
#   bash hack/quickstart/deploy-otel.sh --postgres
#   bash hack/quickstart/deploy-otel.sh \
#     --traces=jaeger-otlp-grpc.observability.svc.cluster.local:4317
#   bash hack/quickstart/deploy-otel.sh --image=quay.io/my-org/my-collector:tag
#
# Prerequisites:
#   - oc CLI on PATH, logged into an OpenShift cluster
#   - Namespace openshift-lightspeed exists
#
# Flags:
#   --image=IMAGE       OTEL collector image (default: Konflux :main).
#   --postgres          Deploy Postgres and wire the collector to export logs to it.
#   --traces=ENDPOINT   Export traces to this OTLP gRPC endpoint without TLS.

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OTEL_IMAGE="quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-otel-collector:main"
TRACE_ENDPOINT=""
WITH_POSTGRES=0

COLLECTOR_NAME="lightspeed-otel-collector"
CERT_SECRET="${COLLECTOR_NAME}-cert"
PG_DSN_SECRET="lightspeed-otel-collector-postgres-dsn"

while [ $# -gt 0 ]; do
  case "$1" in
    --image=*)   OTEL_IMAGE="${1#*=}"; shift ;;
    --image)     [ $# -lt 2 ] && { echo "Missing value for $1" >&2; exit 1; }; OTEL_IMAGE="$2"; shift 2 ;;
    --postgres)  WITH_POSTGRES=1; shift ;;
    --traces=*)  TRACE_ENDPOINT="${1#*=}"; shift ;;
    --traces)    [ $# -lt 2 ] && { echo "Missing value for $1" >&2; exit 1; }; TRACE_ENDPOINT="$2"; shift 2 ;;
    *) echo "Unknown flag: $1" >&2; exit 1 ;;
  esac
done


info()  { echo "  ✓ $*"; }
step()  { echo "[otel] $*"; }

# --- Postgres (optional) -----------------------------------------------------

if [ "${WITH_POSTGRES}" = "1" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  [ -f "${SCRIPT_DIR}/deploy-postgres.sh" ] || { echo "  ✗ Missing deploy-postgres.sh. Run from a repo checkout." >&2; exit 1; }
  command -v openssl >/dev/null 2>&1 || { echo "  ✗ openssl not found. Required for Postgres password generation." >&2; exit 1; }
  step "Deploying Postgres backend..."
  bash "${SCRIPT_DIR}/deploy-postgres.sh"

  step "Creating collector Postgres DSN secret..."
  PG_PASSWORD="$(oc get secret lightspeed-postgres-secret -n "${NAMESPACE}" -o jsonpath='{.data.password}' | base64 -d)"
  PG_PASSWORD_ENCODED="$(python3 -c "import urllib.parse; print(urllib.parse.quote('${PG_PASSWORD}', safe=''))")"
  PG_HOST="lightspeed-postgres-server.${NAMESPACE}.svc"
  PG_DSN="postgres://postgres:${PG_PASSWORD_ENCODED}@${PG_HOST}:5432/postgres?sslmode=require"

  oc apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${PG_DSN_SECRET}
  namespace: ${NAMESPACE}
  labels:
    app: ${COLLECTOR_NAME}
    app.kubernetes.io/name: ${COLLECTOR_NAME}
    app.kubernetes.io/component: otel-collector
type: Opaque
stringData:
  connection_string: "${PG_DSN}"
EOF
  info "DSN secret created"
fi

# --- Collector config ---------------------------------------------------------

step "Deploying OTEL collector to ${NAMESPACE}"
step "Image: ${OTEL_IMAGE}"

if [ "${WITH_POSTGRES}" = "1" ]; then
COLLECTOR_CONFIG='
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: "0.0.0.0:4317"
            tls:
              cert_file: /etc/otel-certs/tls.crt
              key_file: /etc/otel-certs/tls.key
          http:
            endpoint: "0.0.0.0:4318"
            tls:
              cert_file: /etc/otel-certs/tls.crt
              key_file: /etc/otel-certs/tls.key
    processors:
      batch:
        timeout: 1s
        send_batch_size: 100
    exporters:
      nop: {}
      __TRACE_EXPORTER_CONFIG__
      postgres:
        connection_string: ${env:POSTGRES_CONNECTION_STRING}
        schema: templogs
        logs_table: logs
        retry_on_failure:
          enabled: true
        sending_queue:
          enabled: true
          num_consumers: 4
          queue_size: 1000
          storage: file_storage
    connectors:
      routing/logs:
        default_pipelines: [logs/unmatched]
        table:
        - condition: "resource.attributes[\"service.name\"] == \"lightspeed-agentic-sandbox\""
          pipelines: [logs/postgres]
    extensions:
      health_check:
        endpoint: "0.0.0.0:13133"
      file_storage:
        directory: /var/lib/otelcol/file-storage
        create_directory: true
        compaction:
          on_start: true
          directory: /var/lib/otelcol/file-storage/compaction
      postgres_admin:
        endpoint: "0.0.0.0:8080"
        connection_string: ${env:POSTGRES_CONNECTION_STRING}
        schema: templogs
        logs_table: logs
        tls_cert_file: /etc/otel-certs/tls.crt
        tls_key_file: /etc/otel-certs/tls.key
    service:
      extensions: [health_check, file_storage, postgres_admin]
      pipelines:
        logs:
          receivers: [otlp]
          processors: [batch]
          exporters: [routing/logs]
        logs/postgres:
          receivers: [routing/logs]
          exporters: [postgres]
        logs/unmatched:
          receivers: [routing/logs]
          exporters: [nop]
        traces:
          receivers: [otlp]
          exporters: [__TRACE_EXPORTER_NAME__]
      telemetry:
        logs:
          level: info'
else
COLLECTOR_CONFIG='
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: "0.0.0.0:4317"
            tls:
              cert_file: /etc/otel-certs/tls.crt
              key_file: /etc/otel-certs/tls.key
          http:
            endpoint: "0.0.0.0:4318"
            tls:
              cert_file: /etc/otel-certs/tls.crt
              key_file: /etc/otel-certs/tls.key
    processors:
      batch:
        timeout: 1s
        send_batch_size: 100
    exporters:
      nop: {}
      __TRACE_EXPORTER_CONFIG__
    extensions:
      health_check:
        endpoint: "0.0.0.0:13133"
    service:
      extensions: [health_check]
      pipelines:
        logs:
          receivers: [otlp]
          processors: [batch]
          exporters: [nop]
        traces:
          receivers: [otlp]
          exporters: [__TRACE_EXPORTER_NAME__]
      telemetry:
        logs:
          level: info'
fi

if [ -n "${TRACE_ENDPOINT}" ]; then
  TRACE_EXPORTER_NAME="otlp/traces"
  TRACE_EXPORTER_CONFIG="otlp/traces:
        endpoint: ${TRACE_ENDPOINT}
        tls:
          insecure: true"
  info "Trace export enabled: ${TRACE_ENDPOINT} (insecure OTLP gRPC)"
else
  TRACE_EXPORTER_NAME="nop"
  TRACE_EXPORTER_CONFIG=""
fi
COLLECTOR_CONFIG="${COLLECTOR_CONFIG//__TRACE_EXPORTER_NAME__/${TRACE_EXPORTER_NAME}}"
COLLECTOR_CONFIG="${COLLECTOR_CONFIG//__TRACE_EXPORTER_CONFIG__/${TRACE_EXPORTER_CONFIG}}"

# --- Deployment env/volumes (conditional on Postgres) -------------------------

if [ "${WITH_POSTGRES}" = "1" ]; then
EXTRA_ENV="
        - name: POSTGRES_CONNECTION_STRING
          valueFrom:
            secretKeyRef:
              name: ${PG_DSN_SECRET}
              key: connection_string"
EXTRA_VOLUME_MOUNTS="
        - name: file-storage
          mountPath: /var/lib/otelcol/file-storage"
EXTRA_VOLUMES="
      - name: file-storage
        emptyDir:
          sizeLimit: 500Mi"
else
EXTRA_ENV=""
EXTRA_VOLUME_MOUNTS=""
EXTRA_VOLUMES=""
fi

# --- Apply resources ----------------------------------------------------------

oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${COLLECTOR_NAME}-config
  namespace: ${NAMESPACE}
  labels:
    app: ${COLLECTOR_NAME}
    app.kubernetes.io/name: ${COLLECTOR_NAME}
    app.kubernetes.io/component: otel-collector
data:
  config.yaml: |${COLLECTOR_CONFIG}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${COLLECTOR_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${COLLECTOR_NAME}
    app.kubernetes.io/name: ${COLLECTOR_NAME}
    app.kubernetes.io/component: otel-collector
---
apiVersion: v1
kind: Service
metadata:
  name: ${COLLECTOR_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${COLLECTOR_NAME}
    app.kubernetes.io/name: ${COLLECTOR_NAME}
    app.kubernetes.io/component: otel-collector
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: ${CERT_SECRET}
spec:
  selector:
    app: ${COLLECTOR_NAME}
  ports:
  - name: otlp-grpc
    port: 4317
    targetPort: 4317
    protocol: TCP
  - name: otlp-http
    port: 4318
    targetPort: 4318
    protocol: TCP
  - name: admin
    port: 8080
    targetPort: 8080
    protocol: TCP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${COLLECTOR_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${COLLECTOR_NAME}
    app.kubernetes.io/name: ${COLLECTOR_NAME}
    app.kubernetes.io/component: otel-collector
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${COLLECTOR_NAME}
  template:
    metadata:
      labels:
        app: ${COLLECTOR_NAME}
        app.kubernetes.io/name: ${COLLECTOR_NAME}
        app.kubernetes.io/component: otel-collector
    spec:
      serviceAccountName: ${COLLECTOR_NAME}
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: collector
        image: ${OTEL_IMAGE}
        imagePullPolicy: Always
        args: ["--config=/etc/otelcol/config.yaml"]
        ports:
        - name: otlp-grpc
          containerPort: 4317
          protocol: TCP
        - name: otlp-http
          containerPort: 4318
          protocol: TCP
        - name: admin
          containerPort: 8080
          protocol: TCP
        - name: health
          containerPort: 13133
          protocol: TCP
        env:${EXTRA_ENV}
        - name: OTEL_PLACEHOLDER
          value: "true"
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
        livenessProbe:
          httpGet:
            path: /
            port: 13133
          initialDelaySeconds: 10
          periodSeconds: 15
        readinessProbe:
          httpGet:
            path: /
            port: 13133
          initialDelaySeconds: 5
          periodSeconds: 10
        volumeMounts:
        - name: config
          mountPath: /etc/otelcol
          readOnly: true
        - name: certs
          mountPath: /etc/otel-certs
          readOnly: true${EXTRA_VOLUME_MOUNTS}
      volumes:
      - name: config
        configMap:
          name: ${COLLECTOR_NAME}-config
      - name: certs
        secret:
          secretName: ${CERT_SECRET}${EXTRA_VOLUMES}
EOF

info "OTEL collector resources applied"
info "Cert secret: ${CERT_SECRET} (use as otel-ca-secret in agentic ConfigMap)"

step "Waiting for rollout..."
oc rollout status deployment/${COLLECTOR_NAME} -n "${NAMESPACE}" --timeout=120s

info "OTEL collector deployed successfully"
if [ "${WITH_POSTGRES}" = "1" ]; then
  info "Postgres backend: enabled (logs routed to lightspeed-postgres-server)"
else
  info "Postgres backend: disabled (logs dropped via nop exporter)"
fi
