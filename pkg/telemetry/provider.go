package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// providerState holds the reader-visible state, stored atomically for lock-free reads.
type providerState struct {
	logger      otellog.Logger
	config      *CollectorConfig
	adminClient *http.Client
}

// Provider manages the OTel trace and log provider lifecycle and supports
// dynamic reconfiguration when the Collector ConfigMap changes.
type Provider struct {
	mu        sync.Mutex // serializes Configure calls
	state     atomic.Pointer[providerState]
	tp        *sdktrace.TracerProvider
	lp        *sdklog.LoggerProvider
	idGen     sdktrace.IDGenerator
	reader    client.Reader // loads credentials-secret; optional in unit tests
	namespace string
}

// NewProvider creates a Provider with the given ID generator.
// Call SetSecretSource before OnConfigMapChange when credentials-secret may be set.
// Call Configure() to initialize or reconfigure the trace provider.
func NewProvider(idGen sdktrace.IDGenerator) *Provider {
	return &Provider{idGen: idGen}
}

// SetSecretSource configures the Kubernetes reader used to resolve
// credentials-secret into client TLS material.
func (p *Provider) SetSecretSource(reader client.Reader, namespace string) {
	p.reader = reader
	p.namespace = namespace
}

// Configure initializes or reconfigures the trace provider based on the
// given CollectorConfig. Builds the replacement providers before shutting
// down the active ones. Pass nil or a disabled config to disable export.
// Invalid enabled configs return an error and leave the previous providers
// unchanged — callers that want "disable on invalid" must Configure(nil).
func (p *Provider) Configure(ctx context.Context, cfg *CollectorConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	log := logf.FromContext(ctx)
	prev := p.state.Load()

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("lightspeed-agentic-operator"),
		semconv.ServiceVersion("dev"),
	)

	if cfg == nil || !cfg.IsValid() {
		newTP := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithIDGenerator(p.idGen),
		)
		p.swapProviders(newTP, nil, &providerState{}, prev)
		log.Info("OTEL telemetry disabled (no Collector config)")
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	certPool := x509.NewCertPool()
	// Validate() already verified AppendCertsFromPEM succeeds.
	_ = certPool.AppendCertsFromPEM(cfg.CACert)
	tlsCfg := &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}
	if len(cfg.ClientCert) > 0 {
		// Validate() already verified X509KeyPair succeeds.
		cert, err := tls.X509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return fmt.Errorf("%s: %w", ErrParseClientCert, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	tlsCreds := credentials.NewTLS(tlsCfg)

	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.CollectorEndpoint),
		otlptracegrpc.WithTLSCredentials(tlsCreds),
	)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrCreateTraceExporter, err)
	}

	newTP := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(p.idGen),
		sdktrace.WithBatcher(traceExp),
	)

	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.CollectorEndpoint),
		otlploggrpc.WithTLSCredentials(tlsCreds),
	)
	if err != nil {
		shutdownProvider(newTP, nil)
		return fmt.Errorf("%s: %w", ErrCreateLogExporter, err)
	}

	newLP := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)

	newState := &providerState{
		logger:      newLP.Logger("audit"),
		config:      cfg,
		adminClient: buildAdminClient(cfg, tlsCfg),
	}
	p.swapProviders(newTP, newLP, newState, prev)
	log.Info("OTEL telemetry configured (traces + logs)", "endpoint", cfg.CollectorEndpoint)
	return nil
}

func (p *Provider) swapProviders(
	newTP *sdktrace.TracerProvider,
	newLP *sdklog.LoggerProvider,
	newState *providerState,
	prev *providerState,
) {
	oldTP, oldLP := p.tp, p.lp
	p.tp = newTP
	p.lp = newLP
	p.state.Store(newState)
	otel.SetTracerProvider(newTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	closeAdminClient(prev)
	shutdownProvider(oldTP, oldLP)
}

func shutdownProvider(tp *sdktrace.TracerProvider, lp *sdklog.LoggerProvider) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if tp != nil {
		_ = tp.Shutdown(shutdownCtx)
	}
	if lp != nil {
		_ = lp.Shutdown(shutdownCtx)
	}
}

// Config returns the current CollectorConfig (may be nil if unconfigured).
func (p *Provider) Config() *CollectorConfig {
	s := p.state.Load()
	if s == nil {
		return nil
	}
	return s.config
}

// EmitLog emits an OTLP log record to the Collector. No-op if log provider
// is not configured. The traceID links the log to the AgenticRun's trace
// via span context injected into ctx.
func (p *Provider) EmitLog(ctx context.Context, traceID trace.TraceID, event string, payload interface{}) {
	s := p.state.Load()
	if s == nil || s.logger == nil {
		return
	}
	logger := s.logger

	// Inject trace context so the log record carries the trace_id
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx = trace.ContextWithSpanContext(ctx, sc)

	body, err := json.Marshal(payload)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to marshal audit log payload", "event", event)
		return
	}

	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetBody(otellog.StringValue(string(body)))
	record.AddAttributes(otellog.String("event", event))

	logger.Emit(ctx, record)
}

// DeleteLogs deletes all audit log records for the given trace ID from the
// Collector's Postgres store via the admin API. Returns nil if Collector is
// not configured (telemetry disabled).
func (p *Provider) DeleteLogs(ctx context.Context, traceID string) error {
	s := p.state.Load()
	if s == nil || s.adminClient == nil || s.config == nil || s.config.AdminEndpoint == "" {
		return nil
	}
	client := s.adminClient
	cfg := s.config

	url := cfg.AdminEndpoint + "/api/v1/logs?trace_id=" + traceID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrCreateDeleteRequest, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s for trace %s: %w", ErrDeleteLogs, traceID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s for trace %s: unexpected status %d", ErrDeleteLogs, traceID, resp.StatusCode)
	}
	return nil
}

func buildAdminClient(cfg *CollectorConfig, tlsConfig *tls.Config) *http.Client {
	if cfg == nil || cfg.AdminEndpoint == "" {
		return nil
	}

	transport := &http.Transport{TLSClientConfig: tlsConfig}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

func closeAdminClient(s *providerState) {
	if s == nil || s.adminClient == nil {
		return
	}
	if t, ok := s.adminClient.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// OnConfigMapChange is a configwatch.Handler-compatible callback.
// Parses the ConfigMap into CollectorConfig and reconfigures the provider.
// No-op when the parsed config matches the currently active config.
//
// Empty / disabled ConfigMaps disable export (success).
// Invalid enabled ConfigMaps disable export and return an error so startup
// (WaitFor) can exit fatally; runtime reconcile should log and not retry.
func (p *Provider) OnConfigMapChange(ctx context.Context, cm *corev1.ConfigMap) error {
	log := logf.FromContext(ctx)

	if cm == nil {
		if p.Config() == nil {
			return nil
		}
		return p.Configure(ctx, nil)
	}

	cfg := parseConfigMap(cm)
	if err := p.resolveClientCredentials(ctx, cfg); err != nil {
		log.Error(err, "invalid telemetry credentials, disabling export")
		if disErr := p.Configure(ctx, nil); disErr != nil {
			return fmt.Errorf("%s: disable after credentials error: %w", ErrInvalidConfig, disErr)
		}
		return err
	}
	if cfg.Equal(p.Config()) {
		return nil
	}

	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid telemetry ConfigMap, disabling export")
		if disErr := p.Configure(ctx, nil); disErr != nil {
			return fmt.Errorf("%s: disable after invalid config: %w", ErrInvalidConfig, disErr)
		}
		return err
	}

	return p.Configure(ctx, cfg)
}

// resolveClientCredentials loads tls.crt/tls.key when credentials-secret is set.
func (p *Provider) resolveClientCredentials(ctx context.Context, cfg *CollectorConfig) error {
	if cfg == nil || cfg.CredentialsSecret == "" {
		return nil
	}
	if p.reader == nil {
		return fmt.Errorf("%s: credentials-secret %q set but no Secret reader configured", ErrInvalidConfig, cfg.CredentialsSecret)
	}
	cert, key, err := loadClientCredentials(ctx, p.reader, p.namespace, cfg.CredentialsSecret)
	if err != nil {
		return err
	}
	cfg.ClientCert = cert
	cfg.ClientKey = key
	return nil
}

// Shutdown gracefully shuts down trace and log providers.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	if p.tp != nil {
		firstErr = p.tp.Shutdown(ctx)
	}
	if p.lp != nil {
		if err := p.lp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	closeAdminClient(p.state.Load())
	p.tp = nil
	p.lp = nil
	p.state.Store(&providerState{})
	return firstErr
}
