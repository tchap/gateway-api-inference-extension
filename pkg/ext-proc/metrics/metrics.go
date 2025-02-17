package metrics

import (
	"errors"
	"sync"
	"time"

	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	InferenceModelComponent = "inference_model"
	InferencePoolComponent  = "inference_pool"
)

var (
	// Inference Model Metrics
	requestCounter = compbasemetrics.NewCounterVec(
		&compbasemetrics.CounterOpts{
			Subsystem:      InferenceModelComponent,
			Name:           "request_total",
			Help:           "Counter of inference model requests broken out for each model and target model.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	requestLatencies = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: InferenceModelComponent,
			Name:      "request_duration_seconds",
			Help:      "Inference model response latency distribution in seconds for each model and target model.",
			Buckets: []float64{
				0.005, 0.025, 0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1.0, 1.25, 1.5, 2, 3,
				4, 5, 6, 8, 10, 15, 20, 30, 45, 60, 120, 180, 240, 300, 360, 480, 600, 900, 1200, 1800, 2700, 3600,
			},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	requestSizes = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: InferenceModelComponent,
			Name:      "request_sizes",
			Help:      "Inference model requests size distribution in bytes for each model and target model.",
			// Use buckets ranging from 1000 bytes (1KB) to 10^9 bytes (1GB).
			Buckets: []float64{
				64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, // More fine-grained up to 64KB
				131072, 262144, 524288, 1048576, 2097152, 4194304, 8388608, // Exponential up to 8MB
				16777216, 33554432, 67108864, 134217728, 268435456, 536870912, 1073741824, // Exponential up to 1GB
			},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	responseSizes = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: InferenceModelComponent,
			Name:      "response_sizes",
			Help:      "Inference model responses size distribution in bytes for each model and target model.",
			// Most models have a response token < 8192 tokens. Each token, in average, has 4 characters.
			// 8192 * 4 = 32768.
			Buckets:        []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32778, 65536},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	inputTokens = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: InferenceModelComponent,
			Name:      "input_tokens",
			Help:      "Inference model input token count distribution for requests in each model.",
			// Most models have a input context window less than 1 million tokens.
			Buckets:        []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32778, 65536, 131072, 262144, 524288, 1048576},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	outputTokens = compbasemetrics.NewHistogramVec(
		&compbasemetrics.HistogramOpts{
			Subsystem: InferenceModelComponent,
			Name:      "output_tokens",
			Help:      "Inference model output token count distribution for requests in each model.",
			// Most models generates output less than 8192 tokens.
			Buckets:        []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192},
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"model_name", "target_model_name"},
	)

	// Inference Pool Metrics
	inferencePoolAvgKVCache = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      InferencePoolComponent,
			Name:           "average_kv_cache_utilization",
			Help:           "The average kv cache utilization for an inference server pool.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"name"},
	)

	inferencePoolAvgQueueSize = compbasemetrics.NewGaugeVec(
		&compbasemetrics.GaugeOpts{
			Subsystem:      InferencePoolComponent,
			Name:           "average_queue_size",
			Help:           "The average number of requests pending in the model server queue.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		[]string{"name"},
	)
)

var registerMetrics sync.Once

// Register all metrics.
func Register() {
	registerMetrics.Do(func() {
		legacyregistry.MustRegister(requestCounter)
		legacyregistry.MustRegister(requestLatencies)
		legacyregistry.MustRegister(requestSizes)
		legacyregistry.MustRegister(responseSizes)
		legacyregistry.MustRegister(inputTokens)
		legacyregistry.MustRegister(outputTokens)

		legacyregistry.MustRegister(inferencePoolAvgKVCache)
		legacyregistry.MustRegister(inferencePoolAvgQueueSize)
	})
}

// RecordRequstCounter records the number of requests.
func RecordRequestCounter(modelName, targetModelName string) {
	requestCounter.WithLabelValues(modelName, targetModelName).Inc()
}

// RecordRequestSizes records the request sizes.
func RecordRequestSizes(modelName, targetModelName string, reqSize int) {
	requestSizes.WithLabelValues(modelName, targetModelName).Observe(float64(reqSize))
}

// RecordRequestLatencies records duration of request.
func RecordRequestLatencies(modelName, targetModelName string, received time.Time, complete time.Time) error {
	if !complete.After(received) {
		return errors.New("invalid request latency values")
	}
	elapsedSeconds := complete.Sub(received).Seconds()
	requestLatencies.WithLabelValues(modelName, targetModelName).Observe(elapsedSeconds)
	return nil
}

// RecordResponseSizes records the response sizes.
func RecordResponseSizes(modelName, targetModelName string, size int) {
	responseSizes.WithLabelValues(modelName, targetModelName).Observe(float64(size))
}

// RecordInputTokens records input tokens count.
func RecordInputTokens(modelName, targetModelName string, size int) {
	if size > 0 {
		inputTokens.WithLabelValues(modelName, targetModelName).Observe(float64(size))
	}
}

// RecordOutputTokens records output tokens count.
func RecordOutputTokens(modelName, targetModelName string, size int) {
	if size > 0 {
		outputTokens.WithLabelValues(modelName, targetModelName).Observe(float64(size))
	}
}

func RecordInferencePoolAvgKVCache(name string, utilization float64) {
	inferencePoolAvgKVCache.WithLabelValues(name).Set(utilization)
}

func RecordInferencePoolAvgQueueSize(name string, queueSize float64) {
	inferencePoolAvgQueueSize.WithLabelValues(name).Set(queueSize)
}
