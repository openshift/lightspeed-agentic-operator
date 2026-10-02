//go:build mc_product_e2e

package mcproducte2e

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	tokenMaxAge = 24 * time.Hour
	tokenSkew   = 2 * time.Minute
)

// checkToken uses the exact bearer credential in the run-owned sandbox Secret
// never return token, JWT payload or kubeconfig bytes in an error
func checkToken(ctx context.Context, secret *corev1.Secret) error {
	data := secret.Data["kubeconfig"]
	if len(data) == 0 || secret.CreationTimestamp.IsZero() {
		return errors.New("run-owned kubeconfig Secret has no data or creation timestamp")
	}
	kc, err := clientcmd.Load(data)
	if err != nil {
		return errors.New("run-owned sandbox kubeconfig cannot be parsed")
	}
	current, ok := kc.Contexts[kc.CurrentContext]
	if !ok {
		return errors.New("sandbox kubeconfig has no current context")
	}
	auth, ok := kc.AuthInfos[current.AuthInfo]
	if !ok || auth.Token == "" || auth.Exec != nil || auth.AuthProvider != nil {
		return errors.New("sandbox kubeconfig must contain a bearer token")
	}
	issued, expires, err := jwtTimes(auth.Token)
	if err != nil {
		return err
	}
	if err := validateTimes(issued, expires, secret.CreationTimestamp.Time, time.Now()); err != nil {
		return err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return errors.New("sandbox kubeconfig cannot build a REST client")
	}
	server, err := url.Parse(cfg.Host)
	if err != nil || server.Scheme != "https" || server.Host == "" || cfg.Insecure {
		return errors.New("sandbox kubeconfig must have a verified HTTPS API server")
	}
	if subtle.ConstantTimeCompare([]byte(cfg.BearerToken), []byte(auth.Token)) != 1 {
		return errors.New("sandbox kubeconfig REST client has unexpected credentials")
	}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.New("sandbox kubeconfig cannot create a spoke client")
	}
	if _, err := api.CoreV1().Namespaces().Get(ctx, managedNamespace, metav1.GetOptions{}); err != nil {
		if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
			return errors.New("sandbox token was rejected or lacks spoke read access")
		}
		return errors.New("sandbox token read of spoke managed namespace failed")
	}
	return nil
}

func validateTimes(issued, expires, created, now time.Time) error {
	// iat and exp share the issuer's clock, skew is only for wall-clock comparisons
	if issued.After(now.Add(tokenSkew)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > tokenMaxAge || expires.After(created.Add(tokenMaxAge+tokenSkew)) {
		return errors.New("sandbox token is expired, future-issued or exceeds the 24h maximum lifetime")
	}
	return nil
}

func jwtTimes(token string) (time.Time, time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, time.Time{}, errors.New("sandbox token is not a three-part JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("sandbox JWT payload cannot be decoded")
	}
	var claims struct {
		Issued json.Number `json:"iat"`
		Expiry json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}, time.Time{}, errors.New("sandbox JWT claims cannot be parsed")
	}
	iat, errI := claims.Issued.Int64()
	exp, errE := claims.Expiry.Int64()
	if errI != nil || errE != nil || iat <= 0 || exp <= 0 {
		return time.Time{}, time.Time{}, errors.New("sandbox JWT requires valid numeric iat and exp claims")
	}
	return time.Unix(iat, 0), time.Unix(exp, 0), nil
}
