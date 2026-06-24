#!/usr/bin/env bash
#
# Creates a non-cluster-admin test user for webhook/approval testing.
# Uses OpenShift impersonation — no identity provider changes needed.
#
# Usage:
#   bash hack/setup-test-user.sh
#   bash hack/setup-test-user.sh my-user
#
# After running, test with:
#   oc patch proposalapproval <name> -n $NAMESPACE --as=$USERNAME --as-uid=test-uid-123 \
#     --type=json -p '[{"op":"add","path":"/spec/stages/-","value":{"type":"Execution","execution":{"option":0}}}]'
#   oc get proposalapproval <name> -n $NAMESPACE -o jsonpath='{.spec.approver}' | jq .

set -euo pipefail

USERNAME="${1:-testuser}"
NAMESPACE="${NAMESPACE:-openshift-lightspeed}"

echo "Setting up test user: ${USERNAME}"

# Bind the approver role (get/list/watch/patch on ProposalApprovals, read on Proposals)
oc adm policy add-cluster-role-to-user agentic-proposal-approver "${USERNAME}"
echo "  ✓ Bound agentic-proposal-approver ClusterRole"

# Grant impersonation rights so cluster-admin can --as= this user
oc apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: impersonate-${USERNAME}
rules:
  - apiGroups: [""]
    resources: ["users"]
    verbs: ["impersonate"]
    resourceNames: ["${USERNAME}"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: impersonate-${USERNAME}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: impersonate-${USERNAME}
subjects:
  - kind: User
    name: $(oc whoami)
    apiGroup: rbac.authorization.k8s.io
EOF
echo "  ✓ Impersonation RBAC created"

cat <<DONE

Done. Test with:

  oc patch proposalapproval <name> -n ${NAMESPACE} \\
    --as=${USERNAME} --as-uid=test-uid-123 \\
    --type=json \\
    -p '[{"op":"add","path":"/spec/stages/-","value":{"type":"Execution","execution":{"option":0}}}]'

  oc get proposalapproval <name> -n ${NAMESPACE} -o jsonpath='{.spec.approver}' | jq .

Expected: approver.uid = "test-uid-123", approver.username = "${USERNAME}"

To clean up:
  oc delete clusterrole impersonate-${USERNAME}
  oc delete clusterrolebinding impersonate-${USERNAME}
  oc adm policy remove-cluster-role-from-user agentic-proposal-approver ${USERNAME}
DONE
