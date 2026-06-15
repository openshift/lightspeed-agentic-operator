package system

import (
	"context"
	"fmt"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const configName = "cluster"

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(agenticv1alpha1.AddToScheme(s))
	return s
}()

func newClient(f *genericclioptions.ConfigFlags) (client.Client, error) {
	cfg, err := f.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}
	return c, nil
}

func getConfig(ctx context.Context, c client.Client) (*agenticv1alpha1.AgenticOLSConfig, error) {
	cfg := &agenticv1alpha1.AgenticOLSConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: configName}, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func isNoMatchError(err error) bool {
	return meta.IsNoMatchError(err)
}
