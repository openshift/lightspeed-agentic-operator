//go:build mc_product_e2e

package mcproducte2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runLabel = "agentic.openshift.io/run"

type stepEvidence struct {
	saUID, podUID, secretUID types.UID
	podName                  string
	readers                  map[string]types.UID
	roleUID, bindingUID      types.UID
	podOK, tokenOK, accessOK bool
}

func (s *stepEvidence) statusSummary() string {
	if s == nil {
		return "no run-matched watch evidence"
	}
	return fmt.Sprintf("sa=%t readers=%d pod=%t secret=%t mount=%t token=%t role=%t binding=%t access=%t",
		s.saUID != "", len(s.readers), s.podUID != "", s.secretUID != "", s.podOK, s.tokenOK,
		s.roleUID != "", s.bindingUID != "", s.accessOK)
}

type observedResource struct {
	c   client.Client
	obj client.Object
}

type bufferedEvent struct {
	obj    interface{}
	handle func(interface{})
}

type observer struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	f        *fixture
	spokeAPI *kubernetes.Clientset
	hubAPI   *kubernetes.Clientset
	steps    map[string]*stepEvidence
	problems []error
	watches  []watch.Interface
	pending  []bufferedEvent
	active   bool
}

func newObserver(t *testing.T, c clusters) *observer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o := &observer{ctx: ctx, cancel: cancel, steps: map[string]*stepEvidence{}}
	// stop watches and cancel the context even if a later step fails here or in bind
	t.Cleanup(o.stop)
	for _, item := range []struct {
		path string
		set  **kubernetes.Clientset
	}{
		{requiredEnv(t, "MC_HUB_KUBECONFIG"), &o.hubAPI},
		{requiredEnv(t, "MC_SPOKE_KUBECONFIG"), &o.spokeAPI},
	} {
		cfg, err := clientcmd.BuildConfigFromFlags("", item.path)
		if err != nil {
			t.Fatalf("load observer kubeconfig without fallback: %v", err)
		}
		*item.set, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatalf("create observer client: %v", err)
		}
	}
	// establish spoke-side watches before creating the AgenticRun
	sa, err := o.spokeAPI.CoreV1().ServiceAccounts(managedNamespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("start spoke SA watch: %v", err)
	}
	o.consume(sa, "spoke SA", o.onSA)
	crb, err := o.spokeAPI.RbacV1().ClusterRoleBindings().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		o.stop()
		t.Fatalf("start spoke CRB watch: %v", err)
	}
	o.consume(crb, "spoke reader CRB", o.onCRB)
	return o
}

func (o *observer) bind(t *testing.T, f *fixture) {
	t.Helper()
	o.mu.Lock()
	o.f = f
	f.observer = o
	o.mu.Unlock()
	for _, item := range []struct {
		name  string
		start func() (watch.Interface, error)
		on    func(interface{})
	}{
		{"hub pod", func() (watch.Interface, error) {
			return o.hubAPI.CoreV1().Pods(hubNamespace).Watch(o.ctx, metav1.ListOptions{})
		}, o.onPod},
		{"hub kubeconfig Secret", func() (watch.Interface, error) {
			return o.hubAPI.CoreV1().Secrets(hubNamespace).Watch(o.ctx, metav1.ListOptions{})
		}, o.onSecret},
		{"spoke execution Role", func() (watch.Interface, error) {
			return o.spokeAPI.RbacV1().Roles(f.namespace.Name).Watch(o.ctx, metav1.ListOptions{})
		}, o.onRole},
		{"spoke execution RoleBinding", func() (watch.Interface, error) {
			return o.spokeAPI.RbacV1().RoleBindings(f.namespace.Name).Watch(o.ctx, metav1.ListOptions{})
		}, o.onBinding},
	} {
		w, err := item.start()
		if err != nil {
			t.Fatalf("start %s watch before run creation: %v", item.name, err)
		}
		o.consume(w, item.name, item.on)
	}
}

func (o *observer) stop() {
	o.cancel()
	for _, w := range o.watches {
		w.Stop()
	}
}

func (o *observer) consume(w watch.Interface, name string, handle func(interface{})) {
	o.watches = append(o.watches, w)
	go func() {
		for {
			select {
			case <-o.ctx.Done():
				return
			case event, ok := <-w.ResultChan():
				if !ok {
					o.problem(fmt.Errorf("%s watch ended before verification", name))
					return
				}
				if event.Type == watch.Error {
					o.problem(fmt.Errorf("%s watch returned an error", name))
					return
				}
				if event.Type == watch.Added || event.Type == watch.Modified {
					o.handleEvent(event.Object, handle)
				}
			}
		}
	}()
}

// Buffer only run-labeled events until Create supplies the UID. Never log the
// buffered objects: a kubeconfig Secret can pass through here briefly.
func (o *observer) handleEvent(obj interface{}, handle func(interface{})) {
	o.mu.Lock()
	if !o.active {
		if labeled, ok := obj.(metav1.Object); ok && labeled.GetLabels()[runLabel] != "" {
			o.pending = append(o.pending, bufferedEvent{obj: obj, handle: handle})
		}
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	handle(obj)
}

func (o *observer) activate() {
	o.mu.Lock()
	o.active = true
	pending := o.pending
	o.pending = nil
	o.mu.Unlock()
	for _, event := range pending {
		event.handle(event.obj)
	}
}

func (o *observer) problem(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ctx.Err() == nil {
		o.problems = append(o.problems, err)
	}
}

func (o *observer) step(label string) (*stepEvidence, bool) {
	if o.f == nil || label == "" {
		return nil, false
	}
	s, ok := o.steps[label]
	if !ok {
		s = &stepEvidence{readers: map[string]types.UID{}}
		o.steps[label] = s
	}
	return s, true
}

func (o *observer) matches(labels map[string]string) bool {
	return o.f != nil && labels[runLabel] == string(o.f.run.UID)
}

func stepOf(name string) string {
	for _, step := range []string{"anl", "exe", "ver"} {
		if strings.HasPrefix(name, "ls-"+step+"-") || strings.HasPrefix(name, "ls-reader-"+step+"-") || strings.HasPrefix(name, "ls-sandbox-kubeconfig-"+step+"-") {
			return step
		}
	}
	return ""
}

func (o *observer) onSA(obj interface{}) {
	sa, ok := obj.(*corev1.ServiceAccount)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.matches(sa.Labels) || sa.Namespace != managedNamespace {
		return
	}
	if step := stepOf(sa.Name); step != "" && sa.Name == "ls-"+step+"-"+string(o.f.run.UID) && sa.Labels["agentic.openshift.io/component"] == map[string]string{"anl": "analysis-sa", "exe": "execution-sa", "ver": "verification-sa"}[step] {
		s, _ := o.step(step)
		s.saUID = sa.UID
	}
}

func (o *observer) onCRB(obj interface{}) {
	crb, ok := obj.(*rbacv1.ClusterRoleBinding)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.matches(crb.Labels) || crb.Labels["agentic.openshift.io/component"] != "reader-rbac" {
		return
	}
	if step := stepOf(crb.Name); step != "" && strings.HasPrefix(crb.Name, "ls-reader-"+step+"-"+string(o.f.run.UID)+"-") {
		s, _ := o.step(step)
		if len(crb.Subjects) != 1 || crb.Subjects[0].Kind != rbacv1.ServiceAccountKind || crb.Subjects[0].Namespace != managedNamespace || crb.Subjects[0].Name != "ls-"+stepOf(crb.Name)+"-"+string(o.f.run.UID) || crb.RoleRef.Kind != "ClusterRole" {
			o.problems = append(o.problems, errors.New("spoke reader binding has an unexpected subject or role kind"))
			return
		}
		s.readers[crb.Name] = crb.UID
		if len(s.readers) == 1 && stepOf(crb.Name) != "exe" {
			go o.checkAccess(stepOf(crb.Name))
		}
	}
}

func (o *observer) onRole(obj interface{}) {
	role, ok := obj.(*rbacv1.Role)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.matches(role.Labels) || role.Namespace != o.f.namespace.Name || role.Name != "ls-exec-"+string(o.f.run.UID) {
		return
	}
	if !scopedExecutionRole(role.Rules) {
		o.problems = append(o.problems, errors.New("execution Role includes unexpected resource or API group"))
		return
	}
	s, _ := o.step("exe")
	s.roleUID = role.UID
}

func scopedExecutionRole(rules []rbacv1.PolicyRule) bool {
	if len(rules) == 0 {
		return false
	}
	for _, rule := range rules {
		if !equal(rule.APIGroups, []string{""}) || !equal(rule.Resources, []string{"configmaps"}) {
			return false
		}
	}
	return true
}

func (o *observer) onBinding(obj interface{}) {
	binding, ok := obj.(*rbacv1.RoleBinding)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.matches(binding.Labels) || binding.Namespace != o.f.namespace.Name || binding.Name != "ls-exec-"+string(o.f.run.UID) {
		return
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != binding.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.ServiceAccountKind || binding.Subjects[0].Namespace != managedNamespace || binding.Subjects[0].Name != "ls-exe-"+string(o.f.run.UID) {
		o.problems = append(o.problems, errors.New("execution RoleBinding is not scoped to the run SA"))
		return
	}
	s, _ := o.step("exe")
	s.bindingUID = binding.UID
	go o.checkAccess("exe")
}

func (o *observer) checkAccess(step string) {
	o.mu.Lock()
	f := o.f
	o.mu.Unlock()
	if f == nil {
		return
	}
	user := "system:serviceaccount:" + managedNamespace + ":ls-" + step + "-" + string(f.run.UID)
	for _, tc := range []struct {
		namespace string
		verb      string
		resource  string
		allowed   bool
	}{
		{f.namespace.Name, "get", "configmaps", true},
		{f.namespace.Name, "create", "configmaps", step == "exe"},
		{managedNamespace, "create", "configmaps", false},
	} {
		ctx, cancel := context.WithTimeout(o.ctx, 20*time.Second)
		err := wait.PollUntilContextTimeout(ctx, pollInterval, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: user,
				// evaluate the identity the real SA token carries, including its groups
				Groups: []string{
					"system:serviceaccounts",
					"system:serviceaccounts:" + managedNamespace,
					"system:authenticated",
				},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: tc.namespace, Verb: tc.verb, Resource: tc.resource},
			}}
			result, err := o.spokeAPI.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				// retry only transient errors (network, 5xx, throttling); surface auth
				// and other permanent errors immediately with their detail
				if transientAPIError(err) {
					return false, nil
				}
				return false, err
			}
			return result.Status.EvaluationError == "" && result.Status.Allowed == tc.allowed, nil
		})
		cancel()
		if err != nil {
			o.problem(fmt.Errorf("spoke %s SA %s %s permission in %s does not match expected scope: %w", step, tc.verb, tc.resource, tc.namespace, err))
			return
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	s, _ := o.step(step)
	s.accessOK = true
}

// transientAPIError reports whether a Kubernetes API error is likely to succeed
// on retry (server timeouts, throttling, temporary unavailability, network
// errors). Permanent errors, including auth failures, are not transient.
func transientAPIError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) || apierrors.IsConflict(err) {
		return true
	}
	// a connection dropped mid-response (EOF, reset, GOAWAY) is transient; IsProbableEOF
	// unwraps url.Error once, errors.Is covers a deeper wrap chain
	if utilnet.IsProbableEOF(err) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func (o *observer) onSecret(obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.matches(secret.Labels) || secret.Labels["agentic.openshift.io/component"] != "sandbox-kubeconfig" {
		return
	}
	if s, ok := o.step(stepOf(secret.Name)); ok && ownedBy(secret, o.f.run) {
		s.secretUID = secret.UID
	}
	// do not retain Secret data or print the watch event
}

func (o *observer) onPod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	o.mu.Lock()
	if !o.matches(pod.Labels) || !ownedBy(pod, o.f.run) || pod.Namespace != hubNamespace {
		o.mu.Unlock()
		return
	}
	step := map[string]string{"analysis": "anl", "execution": "exe", "verification": "ver"}[pod.Labels["agentic.openshift.io/step"]]
	s, ok := o.step(step)
	if !ok {
		o.mu.Unlock()
		return
	}
	s.podUID = pod.UID
	s.podName = pod.Name
	f := o.f
	o.mu.Unlock()
	if pod.Status.Phase != corev1.PodRunning {
		return
	}
	// the Secret must be read from the API while the sandbox Pod is live
	secretName := "ls-sandbox-kubeconfig-" + step + "-" + string(f.run.UID)
	ctx, cancel := context.WithTimeout(o.ctx, 10*time.Second)
	defer cancel()
	secret, err := o.hubAPI.CoreV1().Secrets(hubNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return // Pod may not have a mounted Secret yet; next event retries
	}
	if err != nil || !ownedBy(secret, f.run) || secret.Labels[runLabel] != string(f.run.UID) {
		o.problem(errors.New("sandbox Pod has no run-owned kubeconfig Secret"))
		return
	}
	if err := checkPodMount(pod, secret.Name); err != nil {
		o.problem(err)
		return
	}
	if err := checkToken(ctx, secret); err != nil {
		o.problem(err)
		return
	}
	o.mu.Lock()
	s.podOK = true
	s.tokenOK = true
	s.secretUID = secret.UID
	o.mu.Unlock()
}

func checkPodMount(pod *corev1.Pod, secretName string) error {
	volume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "spoke-kubeconfig" && v.Secret != nil && v.Secret.SecretName == secretName {
			volume = true
		}
	}
	if !volume || len(pod.Spec.Containers) == 0 {
		return errors.New("sandbox Pod does not mount its run-owned kubeconfig Secret")
	}
	for _, container := range pod.Spec.Containers {
		mount, env := false, false
		for _, v := range container.VolumeMounts {
			mount = mount || v.Name == "spoke-kubeconfig" && v.MountPath == "/var/run/secrets/spoke-kubeconfig" && v.ReadOnly
		}
		for _, v := range container.Env {
			env = env || v.Name == "KUBECONFIG" && v.Value == "/var/run/secrets/spoke-kubeconfig/kubeconfig"
		}
		if !mount || !env {
			return errors.New("sandbox container lacks read-only spoke mount or KUBECONFIG")
		}
	}
	return nil
}

func (o *observer) require(t *testing.T, step string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, pollInterval, 20*time.Second, true, func(context.Context) (bool, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if len(o.problems) > 0 {
			return false, o.problems[0]
		}
		s := o.steps[step]
		if s == nil {
			return false, nil
		}
		good := s.saUID != "" && len(s.readers) > 0 && s.podUID != "" && s.secretUID != "" && s.podOK && s.tokenOK && s.accessOK
		if step == "exe" {
			good = good && s.roleUID != "" && s.bindingUID != ""
		}
		return good, nil
	})
	if err != nil {
		o.mu.Lock()
		status := o.steps[step].statusSummary()
		o.mu.Unlock()
		t.Fatalf("in-flight %s evidence incomplete (%s): %v", step, status, err)
	}
}

func (o *observer) waitForCleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	if err := o.artifactsReleased(ctx); err != nil {
		t.Fatalf("run-owned spoke RBAC/SA or hub sandbox artifacts remain after completion: %v", err)
	}
}

func (o *observer) artifactsReleased(ctx context.Context) error {
	o.mu.Lock()
	f := o.f
	o.mu.Unlock()
	if f == nil {
		return errors.New("observer not bound to a fixture")
	}
	selector := client.MatchingLabels{runLabel: string(f.run.UID)}
	return wait.PollUntilContextTimeout(ctx, pollInterval, deleteTimeout, true, func(ctx context.Context) (bool, error) {
		// check the names seen in-flight, not only label selectors which could miss a stripped label
		o.mu.Lock()
		var exact []observedResource
		for step, s := range o.steps {
			exact = append(exact,
				observedResource{f.clusters.spoke, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ls-" + step + "-" + string(f.run.UID), Namespace: managedNamespace}}},
				observedResource{f.clusters.hub, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ls-sandbox-kubeconfig-" + step + "-" + string(f.run.UID), Namespace: hubNamespace}}},
			)
			for name := range s.readers {
				exact = append(exact, observedResource{f.clusters.spoke, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}})
			}
			if s.podName != "" {
				exact = append(exact, observedResource{f.clusters.hub, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: s.podName, Namespace: hubNamespace}}})
			}
		}
		o.mu.Unlock()
		exact = append(exact,
			observedResource{f.clusters.spoke, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "ls-exec-" + string(f.run.UID), Namespace: f.namespace.Name}}},
			observedResource{f.clusters.spoke, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ls-exec-" + string(f.run.UID), Namespace: f.namespace.Name}}},
		)
		for _, item := range exact {
			err := item.c.Get(ctx, client.ObjectKeyFromObject(item.obj), item.obj)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			return false, nil
		}

		for _, item := range []struct {
			c    client.Client
			list client.ObjectList
			opts []client.ListOption
		}{
			{f.clusters.spoke, &corev1.ServiceAccountList{}, []client.ListOption{client.InNamespace(managedNamespace), selector}},
			{f.clusters.spoke, &rbacv1.ClusterRoleBindingList{}, []client.ListOption{selector}},
			{f.clusters.spoke, &rbacv1.RoleList{}, []client.ListOption{client.InNamespace(f.namespace.Name), selector}},
			{f.clusters.spoke, &rbacv1.RoleBindingList{}, []client.ListOption{client.InNamespace(f.namespace.Name), selector}},
			{f.clusters.hub, &corev1.SecretList{}, []client.ListOption{client.InNamespace(hubNamespace), selector}},
			{f.clusters.hub, &corev1.PodList{}, []client.ListOption{client.InNamespace(hubNamespace), selector}},
		} {
			if err := item.c.List(ctx, item.list, item.opts...); err != nil {
				return false, err
			}
			metadata, err := meta.ExtractList(item.list)
			if err != nil {
				return false, err
			}
			if len(metadata) != 0 {
				return false, nil
			}
		}
		return true, nil
	})
}
