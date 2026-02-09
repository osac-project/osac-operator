package provisioning

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/innabox/cloudkit-operator/internal/webhook"
)

// RateLimitError indicates a request was rate-limited and should be retried.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit active, retry after %v", e.RetryAfter)
}

// WebhookClient is the interface for triggering webhooks to EDA.
// This matches the existing webhook_common.WebhookClient implementation.
type WebhookClient interface {
	TriggerWebhook(ctx context.Context, url string, resource webhook.Resource) (remainingTime time.Duration, err error)
}

// EDAProvider implements ProvisioningProvider using EDA webhooks.
// It maintains backward compatibility with the existing webhook-based approach.
type EDAProvider struct {
	webhookClient WebhookClient
	createURL     string
	deleteURL     string
}

// NewEDAProvider creates a new EDA provider.
func NewEDAProvider(webhookClient WebhookClient, createURL, deleteURL string) *EDAProvider {
	return &EDAProvider{
		webhookClient: webhookClient,
		createURL:     createURL,
		deleteURL:     deleteURL,
	}
}

// TriggerProvision triggers provisioning via EDA webhook.
// Returns the resource name as job ID since EDA doesn't provide a real job ID.
// Returns RateLimitError if the request is rate-limited.
func (p *EDAProvider) TriggerProvision(ctx context.Context, resource client.Object) (*ProvisionResult, error) {
	if p.createURL == "" {
		return nil, fmt.Errorf("create webhook URL not configured")
	}

	webhookResource, ok := resource.(webhook.Resource)
	if !ok {
		return nil, fmt.Errorf("resource does not implement webhook.Resource interface")
	}

	remainingTime, err := p.webhookClient.TriggerWebhook(ctx, p.createURL, webhookResource)
	if err != nil {
		return nil, fmt.Errorf("failed to trigger create webhook: %w", err)
	}

	// If we're within the rate limit window, return rate limit error
	if remainingTime > 0 {
		return nil, &RateLimitError{RetryAfter: remainingTime}
	}

	resourceName := resource.GetName()
	return &ProvisionResult{
		JobID:        resourceName,
		InitialState: JobStateRunning,
		Message:      "Webhook sent to EDA, provisioning in progress",
	}, nil
}

// GetProvisionStatus checks provisioning status.
// EDA doesn't provide status polling, so this always returns JobStateRunning.
// The reconciler must check the CR annotation for completion.
func (p *EDAProvider) GetProvisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	return ProvisionStatus{
		JobID:   jobID,
		State:   JobStateRunning,
		Message: "EDA provider does not support status polling",
	}, nil
}

// TriggerDeprovision triggers deprovisioning via EDA webhook.
// Returns the resource name as job ID since EDA doesn't provide a real job ID.
// Returns RateLimitError if the request is rate-limited.
func (p *EDAProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (*DeprovisionResult, error) {
	log := ctrllog.FromContext(ctx)

	// EDA only deprovisions if AAP finalizer exists (set by playbook during provision)
	if !controllerutil.ContainsFinalizer(resource, "cloudkit.openshift.io/computeinstance-aap") {
		log.Info("no AAP finalizer, skipping EDA deprovisioning")
		return &DeprovisionResult{
			Action:                 DeprovisionSkipped,
			BlockDeletionOnFailure: false,
		}, nil
	}

	// Trigger webhook
	if p.deleteURL == "" {
		return nil, fmt.Errorf("delete webhook URL not configured")
	}

	webhookResource, ok := resource.(webhook.Resource)
	if !ok {
		return nil, fmt.Errorf("resource does not implement webhook.Resource interface")
	}

	remainingTime, err := p.webhookClient.TriggerWebhook(ctx, p.deleteURL, webhookResource)
	if err != nil {
		return nil, fmt.Errorf("failed to trigger delete webhook: %w", err)
	}

	// If we're within the rate limit window, return rate limit error
	if remainingTime > 0 {
		return nil, &RateLimitError{RetryAfter: remainingTime}
	}

	resourceName := resource.GetName()
	return &DeprovisionResult{
		Action:                 DeprovisionTriggered,
		JobID:                  resourceName,
		BlockDeletionOnFailure: false,
	}, nil
}

// GetDeprovisionStatus checks deprovisioning status.
// EDA doesn't provide status polling, so this always returns JobStateRunning.
// The reconciler must check the CR for completion.
func (p *EDAProvider) GetDeprovisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	return ProvisionStatus{
		JobID:   jobID,
		State:   JobStateRunning,
		Message: "EDA provider does not support status polling",
	}, nil
}

// Name returns the provider name for logging.
func (p *EDAProvider) Name() string {
	return "eda"
}
