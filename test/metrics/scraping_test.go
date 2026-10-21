//go:build e2e

package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const metricsService = "agentic-operator-metrics"

// TestMetricsScraping requires an installed operator, valid handoff ConfigMap,
// and OpenShift cluster monitoring. It does not create workflow fixtures.
func TestMetricsScraping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ns := os.Getenv("TEST_NAMESPACE")
	if ns == "" {
		ns = "openshift-lightspeed"
	}
	bundle, err := cs.CoreV1().ConfigMaps("openshift-monitoring").Get(ctx, "serving-certs-ca-bundle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get serving-certs-ca-bundle ConfigMap in openshift-monitoring namespace: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle.Data["service-ca.crt"])) {
		t.Fatal("serving-certs-ca-bundle ConfigMap has no valid service-ca.crt data key")
	}
	secret, err := cs.CoreV1().Secrets("openshift-monitoring").Get(ctx, "metrics-client-certs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get metrics-client-certs Secret in openshift-monitoring namespace: %v", err)
	}
	cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	address, svc := forwardService(t, ctx, cfg, cs, ns, metricsService, 8443)
	secretName := svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"]
	if secretName != "agentic-operator-metrics-certs" {
		t.Fatalf("metrics Service serving-certificate Secret = %q", secretName)
	}
	servingSecret, err := cs.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(servingSecret.Data["tls.crt"], servingSecret.Data["tls.key"]); err != nil {
		t.Fatalf("invalid serving-certificate Secret: %v", err)
	}
	serverName := fmt.Sprintf("%s.%s.svc", metricsService, ns)
	t.Run("HTTPSWithPrometheusClientCertificate", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName, Certificates: []tls.Certificate{cert}}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		var lastErr error
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
			body, err := metricsGET(ctx, client, "https://"+address+"/metrics", "")
			if err == nil && !strings.Contains(string(body), "go_goroutines") {
				err = fmt.Errorf("response lacks go_goroutines metric")
			}
			lastErr = err
			return err == nil, nil
		})
		if err != nil {
			t.Fatalf("GET metrics with trusted client certificate: %v (last error: %v)", err, lastErr)
		}
	})
	t.Run("RejectsMissingClientCertificate", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		_, err := metricsGET(ctx, client, "https://"+address+"/metrics", "")
		if err == nil {
			t.Fatal("metrics endpoint accepted a request without a client certificate")
		}
		if !strings.Contains(err.Error(), "certificate required") && !strings.Contains(err.Error(), "bad certificate") {
			t.Fatalf("expected client-certificate rejection, got: %v", err)
		}
	})
	t.Run("ClusterPrometheusScrapesOperator", func(t *testing.T) {
		token := monitoringToken(t, ctx, cs, ns)
		queryAddress, _ := forwardService(t, ctx, cfg, cs, "openshift-monitoring", "thanos-querier", 9091)
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "thanos-querier.openshift-monitoring.svc"}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		started := time.Now().Unix()
		selector := fmt.Sprintf("{namespace=%q,service=%q}", ns, metricsService)
		// Require samples scraped after this test starts, not historical series.
		for _, metric := range []string{"up", "go_goroutines"} {
			query := fmt.Sprintf("min(%s%s and (timestamp(%s%s) >= %d))", metric, selector, metric, selector, started)
			var lastErr error
			err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				body, err := metricsGET(ctx, client, "https://"+queryAddress+"/api/v1/query?query="+url.QueryEscape(query), token)
				if err == nil {
					err = positiveQueryResult(body)
				}
				lastErr = err
				return err == nil, nil
			})
			if err != nil {
				t.Fatalf("Prometheus query %q failed: %v (last error: %v)", query, err, lastErr)
			}
		}
	})
}

func metricsGET(ctx context.Context, client *http.Client, endpoint, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned HTTP %d", response.StatusCode)
	}
	return body, nil
}

// monitoringToken creates a temporary ServiceAccount with query-only monitoring
// access. Its ClusterRoleBinding and ServiceAccount are removed after the test.
func monitoringToken(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns string) string {
	t.Helper()
	sa, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{GenerateName: "metrics-e2e-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := cs.CoreV1().ServiceAccounts(ns).Delete(cleanupCtx, sa.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete test ServiceAccount: %v", err)
		}
	})
	binding, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{GenerateName: "metrics-e2e-"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-monitoring-view"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: ns}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := cs.RbacV1().ClusterRoleBindings().Delete(cleanupCtx, binding.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete test ClusterRoleBinding: %v", err)
		}
	})
	expiration := int64(600)
	token, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiration}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return token.Status.Token
}

type metricsRoundTripper func(*http.Request) (*http.Response, error)

func (f metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// forwardService connects through the Kubernetes API, preserving the Service DNS
// name for certificate validation. Prometheus scraping tests cover the network path.
func forwardService(t *testing.T, ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, ns, name string, port int32) (string, *corev1.Service) {
	t.Helper()
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s Service in %s namespace: %v", name, ns, err)
	}
	if len(svc.Spec.Selector) == 0 {
		t.Fatalf("%s Service has no pod selector", name)
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		t.Fatal(err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		candidate := &pods.Items[i]
		for _, condition := range candidate.Status.Conditions {
			if candidate.DeletionTimestamp == nil && condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				pod = candidate
				break
			}
		}
		if pod != nil {
			break
		}
	}
	if pod == nil {
		t.Fatalf("no ready Pod selected by %s Service in %s namespace", name, ns)
	}
	remote := int32(0)
	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			if p.TargetPort.Type == intstr.Int {
				remote = p.TargetPort.IntVal
				if remote == 0 {
					remote = p.Port
				}
			} else {
				for _, container := range pod.Spec.Containers {
					for _, cp := range container.Ports {
						if cp.Name == p.TargetPort.StrVal {
							remote = cp.ContainerPort
						}
					}
				}
			}
		}
	}
	if remote == 0 {
		t.Fatalf("cannot resolve port %d for %s Service", port, name)
	}
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod.Name).SubResource("portforward").URL()
	forwardCtx, cancel := context.WithCancel(ctx)
	stop, ready := make(chan struct{}), make(chan struct{})
	dialClient := &http.Client{Transport: metricsRoundTripper(func(req *http.Request) (*http.Response, error) {
		return transport.RoundTrip(req.WithContext(forwardCtx))
	})}
	forwarder, err := portforward.New(spdy.NewDialer(upgrader, dialClient, http.MethodPost, endpoint), []string{fmt.Sprintf("0:%d", remote)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var forwardErr error
	t.Cleanup(func() {
		cancel()
		close(stop)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("metrics port-forward did not stop within 5 seconds")
		}
	})
	go func() {
		defer close(done)
		forwardErr = forwarder.ForwardPorts()
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatalf("port-forward %s Service: %v", name, forwardErr)
	case <-time.After(30 * time.Second):
		t.Fatalf("port-forward %s Service did not become ready within 30 seconds", name)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("get forwarded ports: %v", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local))), svc
}
