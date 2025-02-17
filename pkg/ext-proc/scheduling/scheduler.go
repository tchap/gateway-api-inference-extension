// Package scheduling implements request scheduling algorithms.
package scheduling

import (
	"context"
	"errors"
	"math/rand"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/util/logging"
)

const (
	// TODO(https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/16) Make this configurable.
	kvCacheThreshold = 0.8
	// TODO(https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/16) Make this configurable.
	queueThresholdCritical = 5
	// TODO(https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/16) Make this configurable.
	// the threshold for queued requests to be considered low below which we can prioritize LoRA affinity.
	// The value of 50 is arrived heuristicically based on experiments.
	queueingThresholdLoRA = 50
)

var (
	defaultFilter = &filter{
		name:          "critical request",
		filter:        toFilterFunc(criticalRequestPredicate),
		nextOnSuccess: lowLatencyFilter,
		nextOnFailure: sheddableRequestFilter,
	}

	// queueLoRAAndKVCacheFilter applied least queue -> low cost lora ->  least KV Cache filter
	queueLoRAAndKVCacheFilter = &filter{
		name:   "least queuing",
		filter: leastQueuingFilterFunc,
		nextOnSuccessOrFailure: &filter{
			name:   "low cost LoRA",
			filter: toFilterFunc(lowLoRACostPredicate),
			nextOnSuccessOrFailure: &filter{
				name:   "least KV cache percent",
				filter: leastKVCacheFilterFunc,
			},
		},
	}

	// queueAndKVCacheFilter applies least queue followed by least KV Cache filter
	queueAndKVCacheFilter = &filter{
		name:   "least queuing",
		filter: leastQueuingFilterFunc,
		nextOnSuccessOrFailure: &filter{
			name:   "least KV cache percent",
			filter: leastKVCacheFilterFunc,
		},
	}

	lowLatencyFilter = &filter{
		name:   "low queueing filter",
		filter: toFilterFunc((lowQueueingPodPredicate)),
		nextOnSuccess: &filter{
			name:          "affinity LoRA",
			filter:        toFilterFunc(loRAAffinityPredicate),
			nextOnSuccess: queueAndKVCacheFilter,
			nextOnFailure: &filter{
				name:                   "can accept LoRA Adapter",
				filter:                 toFilterFunc(canAcceptNewLoraPredicate),
				nextOnSuccessOrFailure: queueAndKVCacheFilter,
			},
		},
		nextOnFailure: queueLoRAAndKVCacheFilter,
	}

	sheddableRequestFilter = &filter{
		// When there is at least one model server that's not queuing requests, and still has KV
		// cache below a certain threshold, we consider this model server has capacity to handle
		// a sheddable request without impacting critical requests.
		name:          "has capacity for sheddable requests",
		filter:        toFilterFunc(noQueueAndLessThanKVCacheThresholdPredicate(queueThresholdCritical, kvCacheThreshold)),
		nextOnSuccess: queueLoRAAndKVCacheFilter,
		// If all pods are queuing or running above the KVCache threshold, we drop the sheddable
		// request to make room for critical requests.
		nextOnFailure: &filter{
			name: "drop request",
			filter: func(logger logr.Logger, req *LLMRequest, pods []*backend.PodMetrics) ([]*backend.PodMetrics, error) {
				logger.V(logutil.DEFAULT).Info("Request dropped", "request", req)
				return []*backend.PodMetrics{}, status.Errorf(
					codes.ResourceExhausted, "dropping request due to limited backend resources")
			},
		},
	}
)

func NewScheduler(pmp PodMetricsProvider) *Scheduler {
	return &Scheduler{
		podMetricsProvider: pmp,
		filter:             defaultFilter,
	}
}

type Scheduler struct {
	podMetricsProvider PodMetricsProvider
	filter             Filter
}

// PodMetricsProvider is an interface to provide set of pods in the backend and information such as
// metrics.
type PodMetricsProvider interface {
	AllPodMetrics() []*backend.PodMetrics
}

// Schedule finds the target pod based on metrics and the requested lora adapter.
func (s *Scheduler) Schedule(ctx context.Context, req *LLMRequest) (targetPod backend.Pod, err error) {
	logger := log.FromContext(ctx).WithValues("request", req)
	logger.V(logutil.VERBOSE).Info("Scheduling a request", "metrics", s.podMetricsProvider.AllPodMetrics())
	pods, err := s.filter.Filter(logger, req, s.podMetricsProvider.AllPodMetrics())
	if err != nil {
		return backend.Pod{}, err
	}
	if len(pods) == 0 {
		return backend.Pod{}, errors.New("filter returned zero pods on success")
	}
	logger.V(logutil.VERBOSE).Info("Selecting a random pod from the candidates", "candidatePods", pods)
	i := rand.Intn(len(pods))
	return pods[i].Pod, nil
}
