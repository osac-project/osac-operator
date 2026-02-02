package provisioning

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WebhookClient is the interface for triggering webhooks to EDA.
// This matches the existing webhook_common.WebhookClient implementation.
type WebhookClient interface {
	TriggerWebhook(ctx context.Context, url string, resource WebhookResource) (remainingTime interface{}, err error)
}

// WebhookResource represents a resource that can be sent via webhook.
type WebhookResource interface {
	GetName() string
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
func (p *EDAProvider) TriggerProvision(ctx context.Context, resource client.Object) (string, error) {
	if p.createURL == "" {
		return "", fmt.Errorf("create webhook URL not configured")
	}

	webhookResource, ok := resource.(WebhookResource)
	if !ok {
		return "", fmt.Errorf("resource does not implement WebhookResource interface")
	}

	_, err := p.webhookClient.TriggerWebhook(ctx, p.createURL, webhookResource)
	if err != nil {
		return "", fmt.Errorf("failed to trigger create webhook: %w", err)
	}

	return resource.GetName(), nil
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
func (p *EDAProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (string, error) {
	if p.deleteURL == "" {
		return "", fmt.Errorf("delete webhook URL not configured")
	}

	webhookResource, ok := resource.(WebhookResource)
	if !ok {
		return "", fmt.Errorf("resource does not implement WebhookResource interface")
	}

	_, err := p.webhookClient.TriggerWebhook(ctx, p.deleteURL, webhookResource)
	if err != nil {
		return "", fmt.Errorf("failed to trigger delete webhook: %w", err)
	}

	return resource.GetName(), nil
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
