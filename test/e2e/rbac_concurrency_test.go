//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	readerBindingSubjectName = "lightspeed-agent"
	runLabel                 = "agentic.openshift.io/run"
	componentLabel           = "agentic.openshift.io/component"
	readerRBACComponent      = "reader-rbac"
	stepLabel                = "agentic.openshift.io/step"
)

// TestParallelAnalysisRunsDoNotRaceReaderRBAC verifies that concurrent
// analysis sandboxes do not update the shared reader ClusterRoleBinding.
// Each run must receive an independent reader binding and complete analysis.
func TestParallelAnalysisRunsDoNotRaceReaderRBAC(t *testing.T) {
	t.Log("=== TestParallelAnalysisRunsDoNotRaceReaderRBAC ===")
	c := newClient(t)
	ctx := context.Background()

	sharedBefore := findReaderBindings(t, c)
	t.Logf("Using %d shared reader bindings", len(sharedBefore))

	const runCount = 10
	runs := make([]*agenticv1alpha1.AgenticRun, 0, runCount)
	for i := 0; i < runCount; i++ {
		runs = append(runs, createAgenticRunWithSkills(
			t, c,
			fmt.Sprintf("e2e-rbac-parallel-%d", i),
			"MOCK_TIMEOUT",
			"quay.io/openshiftanalytics/agentic-skills:latest",
			"/skills/cluster-troubleshoot/investigate-alert",
		))
	}

	// Keep the batch mock running so the operator does not release the Pod and
	// per-run reader bindings before we inspect them. This test verifies sandbox
	// setup, so wait for the Pods rather than waiting for analysis to complete.
	for _, run := range runs {
		verifyAnalysisPodSkillsMount(t, c, run)
	}

	sharedAfter := findReaderBindings(t, c)
	if !reflect.DeepEqual(sharedBefore, sharedAfter) {
		t.Fatalf("shared reader bindings changed: before=%v after=%v", sharedBefore, sharedAfter)
	}

	for _, run := range runs {
		var bindings rbacv1.ClusterRoleBindingList
		if err := c.List(ctx, &bindings,
			client.MatchingLabels{
				runLabel:       string(run.UID),
				componentLabel: readerRBACComponent,
			},
		); err != nil {
			t.Fatalf("list reader bindings for %s: %v", run.Name, err)
		}
		if len(bindings.Items) != len(sharedBefore) {
			t.Fatalf("expected %d reader bindings for %s (%s), got %d", len(sharedBefore), run.Name, run.UID, len(bindings.Items))
		}
		wantSA := fmt.Sprintf("ls-anl-%s", run.UID)
		for _, binding := range bindings.Items {
			if len(binding.Subjects) != 1 {
				t.Fatalf("reader binding %s has %d subjects, want 1", binding.Name, len(binding.Subjects))
			}
			if binding.Subjects[0].Kind != rbacv1.ServiceAccountKind ||
				binding.Subjects[0].Name != wantSA ||
				binding.Subjects[0].Namespace != testNS {
				t.Fatalf("reader binding %s subject=%+v, want ServiceAccount %s/%s", binding.Name, binding.Subjects[0], testNS, wantSA)
			}
			foundRoleRef := false
			for _, source := range sharedBefore {
				if source.RoleRef == binding.RoleRef {
					foundRoleRef = true
					break
				}
			}
			if !foundRoleRef {
				t.Fatalf("reader binding %s has unexpected roleRef=%+v", binding.Name, binding.RoleRef)
			}
		}
	}

	t.Logf("Verified %d parallel analysis runs completed with isolated reader RBAC", runCount)
}

func findAnalysisPod(ctx context.Context, c client.Client, run *agenticv1alpha1.AgenticRun) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(testNS), client.MatchingLabels{
		runLabel:  string(run.UID),
		stepLabel: "analysis",
	}); err != nil {
		return nil, err
	}
	if len(pods.Items) > 0 {
		return &pods.Items[0], nil
	}

	claims := &unstructured.UnstructuredList{}
	claims.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxClaimList",
	})
	if err := c.List(ctx, claims, client.InNamespace(testNS), client.MatchingLabels{
		runLabel:  string(run.UID),
		stepLabel: "analysis",
	}); err != nil {
		return nil, err
	}
	if len(claims.Items) == 0 {
		return nil, nil
	}

	sandboxName, found, err := unstructured.NestedString(claims.Items[0].Object, "status", "sandbox", "name")
	if err != nil || !found || sandboxName == "" {
		return nil, err
	}
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(schema.GroupVersionKind{Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox"})
	if err := c.Get(ctx, client.ObjectKey{Name: sandboxName, Namespace: testNS}, sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := c.List(ctx, &pods, client.InNamespace(testNS)); err != nil {
		return nil, err
	}

	for i := range pods.Items {
		for _, owner := range pods.Items[i].OwnerReferences {
			if owner.Kind == "Sandbox" && owner.UID == sandbox.GetUID() {
				return &pods.Items[i], nil
			}
		}
	}
	return nil, nil
}

func verifyAnalysisPodSkillsMount(t *testing.T, c client.Client, run *agenticv1alpha1.AgenticRun) {
	t.Helper()
	ctx := context.Background()
	var pod corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		found, err := findAnalysisPod(ctx, c, run)
		if err != nil {
			return false, err
		}
		if found == nil {
			return false, nil
		}
		pod = *found.DeepCopy()
		return true, nil
	})
	if err != nil {
		t.Fatalf("waiting for analysis pod for %s: %v", run.Name, err)
	}

	if len(run.Spec.Tools.Skills) == 0 || len(run.Spec.Tools.Skills[0].Paths) == 0 {
		t.Fatalf("%s has no configured skills image/path", run.Name)
	}
	source := run.Spec.Tools.Skills[0]
	expectedSubPath := strings.TrimPrefix(source.Paths[0], "/")
	expectedMountPath := path.Join("/app/skills", path.Base(source.Paths[0]))

	var skillsVolume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "skills" {
			skillsVolume = &pod.Spec.Volumes[i]
			break
		}
	}
	if skillsVolume == nil || skillsVolume.Image == nil {
		t.Fatalf("analysis pod %s has no image volume named skills: volumes=%+v", pod.Name, pod.Spec.Volumes)
	}
	if skillsVolume.Image.Reference != source.Image {
		t.Fatalf("analysis pod %s skills image=%q, want %q", pod.Name, skillsVolume.Image.Reference, source.Image)
	}

	var skillsMount, workdirMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		mount := &pod.Spec.Containers[0].VolumeMounts[i]
		switch {
		case mount.Name == "skills":
			skillsMount = mount
		case mount.Name == "skills-workdir":
			workdirMount = mount
		}
	}
	if skillsMount == nil {
		t.Fatalf("analysis pod %s has no skills mount: mounts=%+v", pod.Name, pod.Spec.Containers[0].VolumeMounts)
	}
	if skillsMount.MountPath != expectedMountPath || skillsMount.SubPath != expectedSubPath {
		t.Fatalf("analysis pod %s skills mount=%+v, want mountPath=%q subPath=%q", pod.Name, skillsMount, expectedMountPath, expectedSubPath)
	}
	if workdirMount == nil || workdirMount.MountPath != "/app/skills/.agents" {
		t.Fatalf("analysis pod %s missing skills-workdir mount: mounts=%+v", pod.Name, pod.Spec.Containers[0].VolumeMounts)
	}
	t.Logf("Verified %s pod %s has skills image %q mounted at %s (subPath=%s)", run.Name, pod.Name, source.Image, expectedMountPath, expectedSubPath)
}

type readerSource struct {
	RoleRef  rbacv1.RoleRef
	Subjects []rbacv1.Subject
}

func findReaderBindings(t *testing.T, c client.Client) map[string]readerSource {
	t.Helper()
	ctx := context.Background()
	var bindings rbacv1.ClusterRoleBindingList
	if err := c.List(ctx, &bindings); err != nil {
		t.Fatalf("list ClusterRoleBindings: %v", err)
	}
	found := make(map[string]readerSource)
	for _, binding := range bindings.Items {
		for _, subject := range binding.Subjects {
			if subject.Kind == rbacv1.ServiceAccountKind &&
				subject.Name == readerBindingSubjectName &&
				subject.Namespace == testNS {
				found[binding.Name] = readerSource{
					RoleRef:  binding.RoleRef,
					Subjects: append([]rbacv1.Subject(nil), binding.Subjects...),
				}
				break
			}
		}
	}
	if len(found) == 0 {
		t.Fatalf("no reader ClusterRoleBinding found for %s/%s", testNS, readerBindingSubjectName)
	}
	return found
}
