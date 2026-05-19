package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var ctx = context.Background()

type App struct {
	RedisClient         *redis.Client
	SqsSvc              *sqs.SQS
	SqsQueueURL         string
	HttpClient          *http.Client
	FlagServiceURL      string
	TargetingServiceURL string

	Tracer trace.Tracer

	EvaluationCounter  metric.Int64Counter
	EvaluationErrors   metric.Int64Counter
	EvaluationDuration metric.Float64Histogram
	CacheCounter       metric.Int64Counter
	SqsEventsCounter   metric.Int64Counter
}

func initTracerProvider(ctx context.Context, serviceName string, endpoint string) (*sdktrace.TracerProvider, error) {
	traceExporter, err := otlptracehttp.New(
		ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("service.version", getenvDefault("SERVICE_VERSION", "1.0.0")),
			attribute.String("deployment.environment", getenvDefault("ENVIRONMENT", "dev")),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	otel.SetTracerProvider(tp)

	return tp, nil
}

func initMeterProvider(ctx context.Context, serviceName string, endpoint string) (*sdkmetric.MeterProvider, error) {
	metricExporter, err := otlpmetrichttp.New(
		ctx,
		otlpmetrichttp.WithEndpoint(endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("service.version", getenvDefault("SERVICE_VERSION", "1.0.0")),
			attribute.String("deployment.environment", getenvDefault("ENVIRONMENT", "dev")),
		),
	)
	if err != nil {
		return nil, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(
				metricExporter,
				sdkmetric.WithInterval(10*time.Second),
			),
		),
	)

	otel.SetMeterProvider(mp)

	return mp, nil
}

func main() {
	_ = godotenv.Load()

	port := getenvDefault("PORT", "8004")

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		log.Fatal("REDIS_URL deve ser definida (ex: redis://localhost:6379)")
	}

	flagSvcURL := os.Getenv("FLAG_SERVICE_URL")
	if flagSvcURL == "" {
		log.Fatal("FLAG_SERVICE_URL deve ser definida")
	}

	targetingSvcURL := os.Getenv("TARGETING_SERVICE_URL")
	if targetingSvcURL == "" {
		log.Fatal("TARGETING_SERVICE_URL deve ser definida")
	}

	sqsQueueURL := os.Getenv("AWS_SQS_URL")
	awsRegion := os.Getenv("AWS_REGION")

	if sqsQueueURL == "" {
		log.Println("Atenção: AWS_SQS_URL não definida. Eventos não serão enviados.")
	}
	if awsRegion == "" && sqsQueueURL != "" {
		log.Fatal("AWS_REGION deve ser definida para usar SQS")
	}

	serviceName := getenvDefault("OTEL_SERVICE_NAME", "evaluation-service")
	otelEndpoint := getenvDefault(
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"otel-collector.monitoring.svc.cluster.local:4318",
	)

	tracerProvider, err := initTracerProvider(ctx, serviceName, otelEndpoint)
	if err != nil {
		log.Fatalf("Erro ao inicializar tracer provider: %v", err)
	}
	defer func() {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			log.Printf("Erro ao finalizar tracer provider: %v", err)
		}
	}()

	meterProvider, err := initMeterProvider(ctx, serviceName, otelEndpoint)
	if err != nil {
		log.Fatalf("Erro ao inicializar meter provider: %v", err)
	}
	defer func() {
		if err := meterProvider.Shutdown(ctx); err != nil {
			log.Printf("Erro ao finalizar meter provider: %v", err)
		}
	}()

	tracer := otel.Tracer("evaluation-service")
	meter := otel.Meter("evaluation-service")

	evaluationCounter, err := meter.Int64Counter(
		"evaluation_requests_total",
		metric.WithDescription("Total de avaliações de feature flags"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.Fatalf("Erro ao criar métrica evaluation_requests_total: %v", err)
	}

	evaluationErrors, err := meter.Int64Counter(
		"evaluation_errors_total",
		metric.WithDescription("Total de erros durante avaliação de feature flags"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.Fatalf("Erro ao criar métrica evaluation_errors_total: %v", err)
	}

	evaluationDuration, err := meter.Float64Histogram(
		"evaluation_duration_seconds",
		metric.WithDescription("Duração das avaliações de feature flags"),
		metric.WithUnit("s"),
	)
	if err != nil {
		log.Fatalf("Erro ao criar métrica evaluation_duration_seconds: %v", err)
	}

	cacheCounter, err := meter.Int64Counter(
		"evaluation_cache_total",
		metric.WithDescription("Total de operações de cache no evaluation-service"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.Fatalf("Erro ao criar métrica evaluation_cache_total: %v", err)
	}

	sqsEventsCounter, err := meter.Int64Counter(
		"evaluation_sqs_events_total",
		metric.WithDescription("Total de eventos enviados ao SQS pelo evaluation-service"),
		metric.WithUnit("1"),
	)
	if err != nil {
		log.Fatalf("Erro ao criar métrica evaluation_sqs_events_total: %v", err)
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Não foi possível parsear a URL do Redis: %v", err)
	}

	rdb := redis.NewClient(opt)
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		log.Fatalf("Não foi possível conectar ao Redis: %v", err)
	}
	log.Println("Conectado ao Redis com sucesso!")

	var sqsSvc *sqs.SQS
	if sqsQueueURL != "" {
		sess, err := session.NewSession(&aws.Config{Region: aws.String(awsRegion)})
		if err != nil {
			log.Fatalf("Não foi possível criar sessão AWS: %v", err)
		}
		sqsSvc = sqs.New(sess)
		log.Println("Cliente SQS inicializado com sucesso.")
	}

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	app := &App{
		RedisClient:         rdb,
		SqsSvc:              sqsSvc,
		SqsQueueURL:         sqsQueueURL,
		HttpClient:          httpClient,
		FlagServiceURL:      flagSvcURL,
		TargetingServiceURL: targetingSvcURL,

		Tracer:             tracer,
		EvaluationCounter:  evaluationCounter,
		EvaluationErrors:   evaluationErrors,
		EvaluationDuration: evaluationDuration,
		CacheCounter:       cacheCounter,
		SqsEventsCounter:   sqsEventsCounter,
	}

	mux := http.NewServeMux()

	mux.Handle(
		"/health",
		otelhttp.NewHandler(http.HandlerFunc(app.healthHandler), "GET /health"),
	)

	mux.Handle(
		"/evaluate",
		otelhttp.NewHandler(http.HandlerFunc(app.evaluationHandler), "POST /evaluate"),
	)

	log.Printf("Serviço de Avaliação (Go) rodando na porta %s", port)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func getenvDefault(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
