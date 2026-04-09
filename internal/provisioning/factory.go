package provisioning

import (
	"fmt"
	"time"
)

// ProviderConfig contains configuration for creating a provisioning provider.
type ProviderConfig struct {
	// ProviderType specifies which provider to create (ProviderTypeEDA or ProviderTypeAAP)
	ProviderType ProviderType

	// EDA provider configuration
	WebhookClient      WebhookClient
	ProvisionWebhook   string
	DeprovisionWebhook string

	// AAP provider configuration
	AAPClient           AAPClient
	ProvisionTemplate   string
	DeprovisionTemplate string

	// TemplatePrefix enables convention-based template name resolution for AAP.
	// When set, template names are derived from the resource Kind:
	//   {prefix}-create-{kind-kebab} and {prefix}-delete-{kind-kebab}
	// Explicit ProvisionTemplate/DeprovisionTemplate take precedence when set.
	TemplatePrefix string
}

// NewProvider creates a provisioning provider based on the configuration.
func NewProvider(config ProviderConfig) (ProvisioningProvider, error) {
	switch config.ProviderType {
	case ProviderTypeEDA:
		if config.WebhookClient == nil {
			return nil, fmt.Errorf("EDA provider requires WebhookClient")
		}
		if config.ProvisionWebhook == "" || config.DeprovisionWebhook == "" {
			return nil, fmt.Errorf("EDA provider requires both ProvisionWebhook and DeprovisionWebhook")
		}
		return NewEDAProvider(
			config.WebhookClient,
			config.ProvisionWebhook, config.DeprovisionWebhook,
		), nil

	case ProviderTypeAAP:
		if config.AAPClient == nil {
			return nil, fmt.Errorf("AAP provider requires AAPClient")
		}
		return &AAPProvider{
			client:              config.AAPClient,
			provisionTemplate:   config.ProvisionTemplate,
			deprovisionTemplate: config.DeprovisionTemplate,
			templatePrefix:      config.TemplatePrefix,
		}, nil

	default:
		return nil, fmt.Errorf("unknown provider type: %s", config.ProviderType)
	}
}

// DefaultStatusPollInterval is the default interval for polling provider status.
const DefaultStatusPollInterval = 30 * time.Second
