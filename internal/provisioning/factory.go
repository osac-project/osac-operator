package provisioning

import (
	"fmt"
	"time"

	"github.com/osac-project/osac-operator/internal/aap"
)

// ProviderConfig contains configuration for creating a provisioning provider.
type ProviderConfig struct {
	// ProviderType specifies which provider to create (ProviderTypeEDA or ProviderTypeAAP)
	ProviderType ProviderType

	// EDA provider configuration - webhook URLs per resource type
	WebhookClient                     WebhookClient
	ComputeInstanceProvisionWebhook   string
	ComputeInstanceDeprovisionWebhook string
	ClusterOrderProvisionWebhook      string
	ClusterOrderDeprovisionWebhook    string
	HostPoolProvisionWebhook          string
	HostPoolDeprovisionWebhook        string

	// AAP provider configuration
	AAPClient           *aap.Client
	ProvisionTemplate   string
	DeprovisionTemplate string
}

// NewProvider creates a provisioning provider based on the configuration.
func NewProvider(config ProviderConfig) (ProvisioningProvider, error) {
	switch config.ProviderType {
	case ProviderTypeEDA:
		if config.WebhookClient == nil {
			return nil, fmt.Errorf("EDA provider requires WebhookClient")
		}
		return NewEDAProvider(
			config.WebhookClient,
			config.ComputeInstanceProvisionWebhook, config.ComputeInstanceDeprovisionWebhook,
			config.ClusterOrderProvisionWebhook, config.ClusterOrderDeprovisionWebhook,
			config.HostPoolProvisionWebhook, config.HostPoolDeprovisionWebhook,
		), nil

	case ProviderTypeAAP:
		if config.AAPClient == nil {
			return nil, fmt.Errorf("AAP provider requires AAPClient")
		}
		return NewAAPProvider(config.AAPClient, config.ProvisionTemplate, config.DeprovisionTemplate), nil

	default:
		return nil, fmt.Errorf("unknown provider type: %s", config.ProviderType)
	}
}

// DefaultStatusPollInterval is the default interval for polling provider status.
const DefaultStatusPollInterval = 30 * time.Second
