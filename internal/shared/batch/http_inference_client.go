/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// HTTPInferenceClient implements InferenceClient interface for HTTP-based inference gateways
// Supports both llm-d (OpenAI-compatible) and GAIE endpoints
type HTTPInferenceClient struct {
	client  *http.Client
	baseURL string
	apiKey  string // optional API key for authentication
}

// HTTPInferenceClientConfig holds configuration for the HTTP client
type HTTPInferenceClientConfig struct {
	BaseURL        string        // Base URL of the inference gateway (e.g., "http://localhost:8000")
	Timeout        time.Duration // Request timeout (default: 5 minutes)
	MaxIdleConns   int           // Maximum idle connections (default: 100)
	IdleConnTimeout time.Duration // Idle connection timeout (default: 90 seconds)
	APIKey         string        // Optional API key for authentication
}

// NewHTTPInferenceClient creates a new HTTP-based inference client
func NewHTTPInferenceClient(config HTTPInferenceClientConfig) *HTTPInferenceClient {
	// Set defaults
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Minute
	}
	if config.MaxIdleConns == 0 {
		config.MaxIdleConns = 100
	}
	if config.IdleConnTimeout == 0 {
		config.IdleConnTimeout = 90 * time.Second
	}

	// Create HTTP client with custom transport for connection pooling
	transport := &http.Transport{
		MaxIdleConns:        config.MaxIdleConns,
		MaxIdleConnsPerHost: config.MaxIdleConns,
		IdleConnTimeout:     config.IdleConnTimeout,
	}

	return &HTTPInferenceClient{
		client: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
		},
		baseURL: config.BaseURL,
		apiKey:  config.APIKey,
	}
}

// Generate makes an inference request to the HTTP gateway
func (c *HTTPInferenceClient) Generate(ctx context.Context, req *InferenceRequest) (*InferenceResponse, *InferenceError) {
	if req == nil {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  "request cannot be nil",
		}
	}

	// Determine endpoint based on request parameters
	endpoint := c.determineEndpoint(req.Params)

	// Marshal request parameters to JSON
	requestBody, err := json.Marshal(req.Params)
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  fmt.Sprintf("failed to marshal request: %v", err),
			RawError: err,
		}
	}

	// Create HTTP request
	url := c.baseURL + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryUnknown,
			Message:  fmt.Sprintf("failed to create HTTP request: %v", err),
			RawError: err,
		}
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}
	if req.RequestID != "" {
		httpReq.Header.Set("X-Request-ID", req.RequestID)
	}

	// Log the request
	klog.V(4).Infof("Sending inference request to %s with request_id=%s, model=%s", url, req.RequestID, req.Model)

	// Execute request
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		// Check if context was cancelled or timed out
		if ctx.Err() == context.Canceled {
			return nil, &InferenceError{
				Category: ErrCategoryUnknown,
				Message:  "request cancelled",
				RawError: err,
			}
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, &InferenceError{
				Category: ErrCategoryServer,
				Message:  "request timeout",
				RawError: err,
			}
		}
		return nil, &InferenceError{
			Category: ErrCategoryServer,
			Message:  fmt.Sprintf("failed to execute request: %v", err),
			RawError: err,
		}
	}
	defer httpResp.Body.Close()

	// Read response body
	responseBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryServer,
			Message:  fmt.Sprintf("failed to read response body: %v", err),
			RawError: err,
		}
	}

	// Check status code
	if httpResp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(httpResp.StatusCode, responseBody)
	}

	// Parse response to extract RawData
	var rawData interface{}
	if err := json.Unmarshal(responseBody, &rawData); err != nil {
		klog.Warningf("Failed to unmarshal response as JSON: %v", err)
		// Continue anyway, just set rawData to nil
		rawData = nil
	}

	klog.V(4).Infof("Received successful response for request_id=%s, status=%d, body_size=%d", req.RequestID, httpResp.StatusCode, len(responseBody))

	return &InferenceResponse{
		RequestID: req.RequestID,
		Response:  responseBody,
		RawData:   rawData,
	}, nil
}

// determineEndpoint determines which endpoint to use based on request parameters
func (c *HTTPInferenceClient) determineEndpoint(params map[string]interface{}) string {
	// Check if messages field exists (indicates chat completion)
	if _, hasMessages := params["messages"]; hasMessages {
		return "/v1/chat/completions"
	}

	// Check if prompt field exists (indicates text completion)
	if _, hasPrompt := params["prompt"]; hasPrompt {
		return "/v1/completions"
	}

	// Default to chat completions
	return "/v1/chat/completions"
}

// handleErrorResponse parses error response and maps to InferenceError
func (c *HTTPInferenceClient) handleErrorResponse(statusCode int, body []byte) *InferenceError {
	// Try to parse OpenAI-style error response
	var errorResp struct {
		Error struct {
			Code    int    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}

	message := string(body)
	if err := json.Unmarshal(body, &errorResp); err == nil && errorResp.Error.Message != "" {
		message = errorResp.Error.Message
	}

	// Map HTTP status codes to error categories
	category := c.mapStatusCodeToCategory(statusCode)

	klog.V(3).Infof("Inference request failed with status=%d, category=%s, message=%s", statusCode, category, message)

	return &InferenceError{
		Category: category,
		Message:  fmt.Sprintf("HTTP %d: %s", statusCode, message),
		RawError: fmt.Errorf("status code: %d, body: %s", statusCode, string(body)),
	}
}

// mapStatusCodeToCategory maps HTTP status codes to error categories
func (c *HTTPInferenceClient) mapStatusCodeToCategory(statusCode int) ErrorCategory {
	switch statusCode {
	case http.StatusBadRequest: // 400
		return ErrCategoryInvalidReq
	case http.StatusUnauthorized, http.StatusForbidden: // 401, 403
		return ErrCategoryAuth
	case http.StatusTooManyRequests: // 429
		return ErrCategoryRateLimit
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 500, 502, 503, 504
		return ErrCategoryServer
	default:
		if statusCode >= 500 {
			return ErrCategoryServer
		}
		return ErrCategoryUnknown
	}
}
