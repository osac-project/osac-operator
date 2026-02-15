/*
Copyright 2025.

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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/innabox/cloudkit-operator/internal/webhook"
)

// InflightRequest represents a request that is currently being processed
type InflightRequest struct {
	createTime time.Time
}

// WebhookClient provides a generic webhook client for any Kubernetes resource
type WebhookClient struct {
	inflightRequests       sync.Map // map[string]InflightRequest
	clientTimeout          time.Duration
	minimumRequestInterval time.Duration
}

// NewWebhookClient creates a new webhook client with the specified timeout and minimum request interval
func NewWebhookClient(timeout, minimumRequestInterval time.Duration) *WebhookClient {
	log := ctrllog.Log.WithName("NewWebhookClient")
	log.Info("creating webhook client", "minimumRequestInterval", minimumRequestInterval)
	return &WebhookClient{
		clientTimeout:          timeout,
		minimumRequestInterval: minimumRequestInterval,
	}
}

// checkForExistingRequest checks if there's already an inflight request for the given resource
func (wc *WebhookClient) checkForExistingRequest(ctx context.Context, url, resourceName string) time.Duration {
	var delta time.Duration

	log := ctrllog.FromContext(ctx)
	cacheKey := fmt.Sprintf("%s:%s", url, resourceName)
	if value, ok := wc.inflightRequests.Load(cacheKey); ok {
		request := value.(InflightRequest)
		delta = time.Since(request.createTime)
		if delta >= wc.minimumRequestInterval {
			delta = 0
		}
		log.Info("skip webhook (resource found in cache)", "url", url, "resource", resourceName, "delta", delta, "minimumRequestInterval", wc.minimumRequestInterval)
	}
	wc.purgeExpiredRequests(ctx)
	return delta
}

// addInflightRequest adds a new inflight request to the cache
func (wc *WebhookClient) addInflightRequest(ctx context.Context, url, resourceName string) {
	log := ctrllog.FromContext(ctx)
	cacheKey := fmt.Sprintf("%s:%s", url, resourceName)
	wc.inflightRequests.Store(cacheKey, InflightRequest{
		createTime: time.Now(),
	})
	log.Info("add webhook to cache", "url", url, "resource", resourceName)
	wc.purgeExpiredRequests(ctx)
}

// purgeExpiredRequests removes expired requests from the cache
func (wc *WebhookClient) purgeExpiredRequests(ctx context.Context) {
	log := ctrllog.FromContext(ctx)
	wc.inflightRequests.Range(func(key, value any) bool {
		cacheKey := key.(string)
		request := value.(InflightRequest)
		if delta := time.Since(request.createTime); delta > wc.minimumRequestInterval {
			log.Info("expire cache entry for webhook", "cacheKey", cacheKey, "minimumRequestInterval", wc.minimumRequestInterval)
			wc.inflightRequests.Delete(cacheKey)
		}
		return true
	})
}

// TriggerWebhook sends a webhook request for the given resource
func (wc *WebhookClient) TriggerWebhook(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
	log := ctrllog.FromContext(ctx)

	if delta := wc.checkForExistingRequest(ctx, url, resource.GetName()); delta != 0 {
		return delta, nil
	}

	log.Info("trigger webhook", "url", url, "resource", resource.GetName())

	jsonData, err := json.Marshal(resource)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: wc.clientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("received non-success status code: %d", resp.StatusCode)
	}

	wc.addInflightRequest(ctx, url, resource.GetName())
	return 0, nil
}

// ResetCache clears all inflight requests (useful for testing)
func (wc *WebhookClient) ResetCache() {
	wc.inflightRequests.Range(func(key, value any) bool {
		wc.inflightRequests.Delete(key)
		return true
	})
}
