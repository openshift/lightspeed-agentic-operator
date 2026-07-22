#!/usr/bin/env bash
# Deploy Jaeger v2 all-in-one for dev OTLP trace collection (OLS-3024)
# Configures TLS on OTLP gRPC via OpenShift serving certs so the
# OTel collector can connect with insecure: false.
set -euo pipefail

NAMESPACE="${1:-observability}"
CERT_SECRET="jaeger-serving-cert"
CERT_MOUNT="/etc/jaeger-tls"
CONFIG_CM="jaeger-config"

echo "==> Deploying Jaeger all-in-one to namespace '$NAMESPACE'"

oc get project "$NAMESPACE" &>/dev/null || oc new-project "$NAMESPACE" --skip-config-write

oc create deployment jaeger --image=jaegertracing/jaeger:latest --port=16686 -n "$NAMESPACE" 2>/dev/null || echo "deployment/jaeger already exists"

for svc in jaeger-ui:16686 jaeger-otlp-grpc:4317 jaeger-otlp-http:4318; do
  name="${svc%%:*}"
  port="${svc##*:}"
  oc expose deployment jaeger --port="$port" --target-port="$port" --name="$name" -n "$NAMESPACE" 2>/dev/null || echo "svc/$name already exists"
done

oc annotate svc jaeger-otlp-grpc -n "$NAMESPACE" "service.beta.openshift.io/serving-cert-secret-name=$CERT_SECRET" --overwrite

echo "==> Waiting for serving-cert secret..."
for i in $(seq 1 30); do
  oc get secret "$CERT_SECRET" -n "$NAMESPACE" &>/dev/null && break
  sleep 2
done
oc get secret "$CERT_SECRET" -n "$NAMESPACE" &>/dev/null || { echo "ERROR: serving-cert secret not created"; exit 1; }

# Jaeger v2 is an OTel collector — configure TLS via its config file.
oc create configmap "$CONFIG_CM" -n "$NAMESPACE" --from-literal=config.yaml="$(cat <<YAMLEOF
extensions:
  jaeger_query:
    storage:
      traces: some_storage
  jaeger_storage:
    backends:
      some_storage:
        memory:
          max_traces: 100000

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        tls:
          cert_file: $CERT_MOUNT/tls.crt
          key_file: $CERT_MOUNT/tls.key
      http:
        endpoint: 0.0.0.0:4318

exporters:
  jaeger_storage_exporter:
    trace_storage: some_storage

service:
  extensions: [jaeger_query, jaeger_storage]
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [jaeger_storage_exporter]
YAMLEOF
)" --dry-run=client -o yaml | oc apply -f -

oc set volume deployment/jaeger -n "$NAMESPACE" --add --overwrite --name=config --type=configmap --configmap-name="$CONFIG_CM" --mount-path=/etc/jaeger
oc set volume deployment/jaeger -n "$NAMESPACE" --add --overwrite --name=serving-cert --type=secret --secret-name="$CERT_SECRET" --mount-path="$CERT_MOUNT" --read-only

oc patch deployment jaeger -n "$NAMESPACE" --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--config","/etc/jaeger/config.yaml"]}]'

oc expose svc jaeger-ui -n "$NAMESPACE" 2>/dev/null || echo "route/jaeger-ui already exists"

echo "==> Waiting for rollout..."
oc rollout status deployment/jaeger -n "$NAMESPACE" --timeout=120s

ROUTE=$(oc get route jaeger-ui -n "$NAMESPACE" -o jsonpath='{.spec.host}')

echo ""
echo "Jaeger UI:   http://$ROUTE"
echo "OTLP gRPC:   jaeger-otlp-grpc.$NAMESPACE.svc.cluster.local:4317 (TLS)"
echo "OTLP HTTP:   jaeger-otlp-http.$NAMESPACE.svc.cluster.local:4318"
