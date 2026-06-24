#!/usr/bin/env bash
# Deploy Jaeger all-in-one for dev OTLP trace collection (OLS-3024)
set -euo pipefail

NAMESPACE="${1:-observability}"

echo "==> Deploying Jaeger all-in-one to namespace '$NAMESPACE'"

oc get project "$NAMESPACE" &>/dev/null || oc new-project "$NAMESPACE" --skip-config-write

oc create deployment jaeger --image=jaegertracing/jaeger:latest --port=16686 -n "$NAMESPACE" 2>/dev/null || echo "deployment/jaeger already exists"

oc set env deployment/jaeger COLLECTOR_OTLP_ENABLED=true -n "$NAMESPACE"

for svc in jaeger-ui:16686 jaeger-otlp-grpc:4317 jaeger-otlp-http:4318; do
  name="${svc%%:*}"
  port="${svc##*:}"
  oc expose deployment jaeger --port="$port" --target-port="$port" --name="$name" -n "$NAMESPACE" 2>/dev/null || echo "svc/$name already exists"
done

oc expose svc jaeger-ui -n "$NAMESPACE" 2>/dev/null || echo "route/jaeger-ui already exists"

echo "==> Waiting for rollout..."
oc rollout status deployment/jaeger -n "$NAMESPACE" --timeout=120s

ROUTE=$(oc get route jaeger-ui -n "$NAMESPACE" -o jsonpath='{.spec.host}')

echo ""
echo "Jaeger UI:   http://$ROUTE"
echo "OTLP gRPC:   jaeger-otlp-grpc.$NAMESPACE.svc.cluster.local:4317"
echo "OTLP HTTP:   jaeger-otlp-http.$NAMESPACE.svc.cluster.local:4318"
