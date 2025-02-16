package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/scheduling"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/util/logging"
)

// HandleRequestBody handles body of the request to the backend server, such as parsing the "model"
// parameter.
// Envoy sends the request body to ext proc before sending the request to the backend server.
func (s *Server) HandleRequestBody(
	ctx context.Context,
	reqCtx *RequestContext,
	req *extProcPb.ProcessingRequest,
) (*extProcPb.ProcessingResponse, error) {
	logger := log.FromContext(ctx)
	loggerVerbose := logger.V(logutil.VERBOSE)
	loggerVerbose.Info("Handling request body")

	// Unmarshal request body (must be JSON).
	v := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	var rb map[string]interface{}
	if err := json.Unmarshal(v.RequestBody.Body, &rb); err != nil {
		return nil, fmt.Errorf("failed to unmarshal request body: %w", err)
	}
	loggerVerbose.Info("Request body unmarshalled", "body", rb)

	// Resolve target models.
	model, ok := rb["model"].(string)
	if !ok {
		return nil, errors.New("model not found in request")
	}
	loggerVerbose.Info("Model requested", "model", model)
	modelName := model

	// NOTE: The nil checking for the modelObject means that we DO allow passthrough currently.
	// This might be a security risk in the future where adapters not registered in the InferenceModel
	// are able to be requested by using their distinct name.
	modelObj := s.datastore.FetchModelData(model)
	if modelObj == nil {
		return nil, fmt.Errorf("error finding a model object in InferenceModel for input %v", model)
	}
	if len(modelObj.Spec.TargetModels) > 0 {
		modelName = backend.RandomWeightedDraw(logger, modelObj, 0)
		if modelName == "" {
			return nil, fmt.Errorf("error getting target model name for model %v", modelObj.Name)
		}
	}
	llmReq := &scheduling.LLMRequest{
		Model:               model,
		ResolvedTargetModel: modelName,
		Critical:            backend.IsCritical(modelObj),
	}
	loggerVerbose.Info("LLM request assembled", "request", llmReq)

	requestBody := v.RequestBody.Body
	var err error
	// Update target models in the body.
	if llmReq.Model != llmReq.ResolvedTargetModel {
		rb["model"] = llmReq.ResolvedTargetModel
		requestBody, err = json.Marshal(rb)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		loggerVerbose.Info("Updated request body marshalled", "body", string(requestBody))
	}

	targetPod, err := s.scheduler.Schedule(ctx, llmReq)
	if err != nil {
		return nil, fmt.Errorf("failed to find target pod: %w", err)
	}
	logger.V(logutil.DEFAULT).Info("Request handled",
		"model", llmReq.Model, "targetModel", llmReq.ResolvedTargetModel, "endpoint", targetPod)

	reqCtx.Model = llmReq.Model
	reqCtx.ResolvedTargetModel = llmReq.ResolvedTargetModel
	reqCtx.RequestSize = len(v.RequestBody.Body)
	reqCtx.TargetPod = targetPod

	// Insert target endpoint to instruct Envoy to route requests to the specified target pod.
	headers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				Key:      s.targetEndpointKey,
				RawValue: []byte(targetPod.Address),
			},
		},
		// We need to update the content length header if the body is mutated, see Envoy doc:
		// https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ext_proc/v3/processing_mode.proto
		{
			Header: &configPb.HeaderValue{
				Key:      "Content-Length",
				RawValue: []byte(strconv.Itoa(len(requestBody))),
			},
		},
	}
	// Print headers for debugging
	for _, header := range headers {
		logger.V(logutil.DEBUG).Info("Request body header", "key", header.Header.Key, "value", header.Header.RawValue)
	}

	resp := &extProcPb.ProcessingResponse{
		// The Endpoint Picker supports two approaches to communicating the target endpoint, as a request header
		// and as an unstructure ext-proc response metadata key/value pair. This enables different integration
		// options for gateway providers.
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: requestBody,
						},
					},
				},
			},
		},
		DynamicMetadata: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				s.targetEndpointKey: {
					Kind: &structpb.Value_StringValue{
						StringValue: targetPod.Address,
					},
				},
			},
		},
	}
	return resp, nil
}

func HandleRequestHeaders(
	ctx context.Context,
	reqCtx *RequestContext,
	req *extProcPb.ProcessingRequest,
) *extProcPb.ProcessingResponse {
	r := req.Request
	h := r.(*extProcPb.ProcessingRequest_RequestHeaders)
	log.FromContext(ctx).V(logutil.VERBOSE).Info("Handling request headers", "headers", h)

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					// Set `clear_route_cache = true` to force Envoy to recompute the target cluster
					// based on the new "target-pod" header.
					// See https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto#service-ext-proc-v3-commonresponse.
					ClearRouteCache: true,
				},
			},
		},
	}

	return resp
}
