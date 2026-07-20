package telemetry

const (
	ErrReadConfigMap         = "read telemetry ConfigMap"
	ErrInvalidConfig         = "invalid telemetry ConfigMap"
	ErrParseCACert           = "parse CA certificate from ConfigMap"
	ErrReadCredentialsSecret = "read telemetry credentials Secret"
	ErrParseClientCert       = "parse client TLS certificate"
	ErrCreateTraceExporter   = "create OTLP trace exporter"
	ErrCreateLogExporter     = "create OTLP log exporter"
	ErrCreateDeleteRequest   = "create delete request"
	ErrDeleteLogs            = "delete logs"
)
