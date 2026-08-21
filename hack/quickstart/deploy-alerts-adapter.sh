#!/usr/bin/env bash
#
# Deploy the agentic alerts adapter as a standalone workload.
# Can be called from install.sh or run independently.
#
# The adapter polls Alertmanager for firing alerts and creates
# AgenticRun CRs. It needs RBAC to create/list/get agenticruns
# and read-only access to the Alertmanager API.
#
# Usage:
#   bash hack/quickstart/deploy-alerts-adapter.sh
#
# Prerequisites:
#   - oc CLI on PATH, logged into an OpenShift cluster
#   - Namespace openshift-lightspeed exists
#
# Flags:
#   --image=IMAGE   Alerts adapter image (default: Konflux :main).

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
ALERTS_ADAPTER_IMAGE="quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-alerts-adapter:main"

while [ $# -gt 0 ]; do
  case "$1" in
    --image=*) ALERTS_ADAPTER_IMAGE="${1#*=}"; shift ;;
    --image)   [ $# -lt 2 ] && { echo "Missing value for $1" >&2; exit 1; }; ALERTS_ADAPTER_IMAGE="$2"; shift 2 ;;
    *) echo "Unknown flag: $1" >&2; exit 1 ;;
  esac
done


ADAPTER_NAME="lightspeed-agentic-alerts-adapter"
ALERTMANAGER_URL="https://alertmanager-main.openshift-monitoring.svc:9094"

info()  { echo "  ✓ $*"; }
step()  { echo "[alerts-adapter] $*"; }

step "Deploying alerts adapter to ${NAMESPACE}"
step "Image: ${ALERTS_ADAPTER_IMAGE}"

oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: alerts-adapter-config
  namespace: ${NAMESPACE}
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
data:
  config.yaml: |
    pollInterval: 30s
    postRunDelay: 1h
    filtering:
      # Uncomment and configure at least one receiver to start automatic analysis.
      # allowedReceivers:
      #   - critical
    deduplication:
      ignoredLabels:
        - pod
        - instance
        - endpoint
        - uid
    # agent:
    #   default: "default"
    tools:
      skills:
        - image: quay.io/openshiftanalytics/agentic-skills:latest
          paths:
            - /skills/cluster-troubleshoot/investigate-alert
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${ADAPTER_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${ADAPTER_NAME}-agenticruns
  namespace: ${NAMESPACE}
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
rules:
- apiGroups: ["agentic.openshift.io"]
  resources: ["agenticruns"]
  verbs: ["create", "list", "get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${ADAPTER_NAME}-agenticruns
  namespace: ${NAMESPACE}
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${ADAPTER_NAME}-agenticruns
subjects:
- kind: ServiceAccount
  name: ${ADAPTER_NAME}
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${ADAPTER_NAME}-alertmanager
  namespace: openshift-monitoring
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: monitoring-alertmanager-view
subjects:
- kind: ServiceAccount
  name: ${ADAPTER_NAME}
  namespace: ${NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${ADAPTER_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${ADAPTER_NAME}
    app.kubernetes.io/name: ${ADAPTER_NAME}
    app.kubernetes.io/component: alerts-adapter
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${ADAPTER_NAME}
  template:
    metadata:
      labels:
        app: ${ADAPTER_NAME}
        app.kubernetes.io/name: ${ADAPTER_NAME}
        app.kubernetes.io/component: alerts-adapter
    spec:
      serviceAccountName: ${ADAPTER_NAME}
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: adapter
        image: ${ALERTS_ADAPTER_IMAGE}
        imagePullPolicy: Always
        env:
        - name: ALERTMANAGER_URL
          value: "${ALERTMANAGER_URL}"
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
        resources:
          requests:
            cpu: 10m
            memory: 50Mi
        volumeMounts:
        - name: config
          mountPath: /etc/alerts-adapter
          readOnly: true
        - name: tmp
          mountPath: /tmp
      volumes:
      - name: config
        configMap:
          name: alerts-adapter-config
      - name: tmp
        emptyDir: {}
EOF

info "Alerts adapter resources applied"

step "Waiting for rollout..."
oc rollout status deployment/${ADAPTER_NAME} -n "${NAMESPACE}" --timeout=120s

info "Alerts adapter deployed successfully"
