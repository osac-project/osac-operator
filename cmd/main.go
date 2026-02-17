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

// Main entrypoint for the operator
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	insecurecredentials "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/credentials/oauth"
	experimentalcredentials "google.golang.org/grpc/experimental/credentials"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	cluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	"sigs.k8s.io/multicluster-runtime/providers/single"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/aap"
	"github.com/osac-project/osac-operator/internal/controller"
	"github.com/osac-project/osac-operator/internal/helpers"
	"github.com/osac-project/osac-operator/internal/provisioning"
	// +kubebuilder:scaffold:imports
)

var (
	setupLog = ctrl.Log.WithName("setup")
)

const (
	// Namespace environment variables
	envComputeInstanceNamespace          = "OSAC_COMPUTE_INSTANCE_NAMESPACE"
	envNetworkingNamespace               = "OSAC_NETWORKING_NAMESPACE"
	envComputeInstanceProvisionWebhook   = "OSAC_COMPUTE_INSTANCE_PROVISION_WEBHOOK"
	envComputeInstanceDeprovisionWebhook = "OSAC_COMPUTE_INSTANCE_DEPROVISION_WEBHOOK"

	// ClusterOrder EDA webhook environment variables
	envClusterOrderProvisionWebhook   = "OSAC_CLUSTER_ORDER_PROVISION_WEBHOOK"
	envClusterOrderDeprovisionWebhook = "OSAC_CLUSTER_ORDER_DEPROVISION_WEBHOOK"

	// HostPool EDA webhook environment variables
	envHostPoolProvisionWebhook   = "OSAC_HOSTPOOL_PROVISION_WEBHOOK"
	envHostPoolDeprovisionWebhook = "OSAC_HOSTPOOL_DEPROVISION_WEBHOOK"

	// Provider selection
	envProvisioningProvider = "OSAC_PROVISIONING_PROVIDER"

	// AAP configuration
	envAAPURL                 = "OSAC_AAP_URL"
	envAAPToken               = "OSAC_AAP_TOKEN"
	envAAPProvisionTemplate   = "OSAC_AAP_PROVISION_TEMPLATE"
	envAAPDeprovisionTemplate = "OSAC_AAP_DEPROVISION_TEMPLATE"
	envAAPStatusPollInterval  = "OSAC_AAP_STATUS_POLL_INTERVAL"
	envAAPInsecureSkipVerify  = "OSAC_AAP_INSECURE_SKIP_VERIFY"

	// ClusterOrder AAP template overrides
	envClusterOrderAAPProvisionTemplate   = "OSAC_CLUSTER_ORDER_AAP_PROVISION_TEMPLATE"
	envClusterOrderAAPDeprovisionTemplate = "OSAC_CLUSTER_ORDER_AAP_DEPROVISION_TEMPLATE"

	// HostPool AAP template overrides
	envHostPoolAAPProvisionTemplate   = "OSAC_HOSTPOOL_AAP_PROVISION_TEMPLATE"
	envHostPoolAAPDeprovisionTemplate = "OSAC_HOSTPOOL_AAP_DEPROVISION_TEMPLATE"

	// Job history configuration
	envMaxJobHistory = "OSAC_MAX_JOB_HISTORY"

	// Tenant configuration
	envTenantNamespace = "OSAC_TENANT_NAMESPACE"

	// Remote cluster (tenant and compute-instance controllers)
	envRemoteClusterKubeconfig = "OSAC_REMOTE_CLUSTER_KUBECONFIG"

	// Controller enable flags (defaults when flag is not set)
	envEnableTenantController          = "OSAC_ENABLE_TENANT_CONTROLLER"
	envEnableHostPoolController        = "OSAC_ENABLE_HOST_POOL_CONTROLLER"
	envEnableComputeInstanceController = "OSAC_ENABLE_COMPUTE_INSTANCE_CONTROLLER"
	envEnableClusterController         = "OSAC_ENABLE_CLUSTER_CONTROLLER"
	envEnableNetworkingController      = "OSAC_ENABLE_NETWORKING_CONTROLLER"

	remoteClusterName = "remote"
)

// parsePollInterval parses a poll interval from environment variable with fallback to default.
func parsePollInterval(envVar string, defaultInterval time.Duration) time.Duration {
	pollIntervalStr := os.Getenv(envVar)
	if pollIntervalStr == "" {
		return defaultInterval
	}

	interval, err := time.ParseDuration(pollIntervalStr)
	if err != nil {
		setupLog.Error(err, "invalid poll interval, using default",
			"envVar", envVar, "value", pollIntervalStr, "default", defaultInterval)
		return defaultInterval
	}

	return interval
}

// parseIntEnv parses an integer from environment variable with fallback to default.
func parseIntEnv(envVar string, defaultValue int) int {
	valueStr := os.Getenv(envVar)
	if valueStr == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		setupLog.Error(err, "invalid integer value, using default",
			"envVar", envVar, "value", valueStr, "default", defaultValue)
		return defaultValue
	}

	if value < 1 {
		setupLog.Info("integer value must be at least 1, using default",
			"envVar", envVar, "value", value, "default", defaultValue)
		return defaultValue
	}

	return value
}

// controllerFlags holds the enable flags for all controllers.
type controllerFlags struct {
	Tenant          bool
	HostPool        bool
	ComputeInstance bool
	Cluster         bool
	Networking      bool
}

// registerControllerFlags registers controller enable flags with the flag package
// and returns a function that should be called after flag.Parse() to get the final values.
func registerControllerFlags() *controllerFlags {
	flags := &controllerFlags{}
	flag.BoolVar(&flags.Tenant, "enable-tenant-controller",
		helpers.GetEnvWithDefault(envEnableTenantController, false),
		"Enable the tenant controller.")
	flag.BoolVar(&flags.HostPool, "enable-host-pool-controller",
		helpers.GetEnvWithDefault(envEnableHostPoolController, false),
		"Enable the host-pool controller.")
	flag.BoolVar(&flags.ComputeInstance, "enable-compute-instance-controller",
		helpers.GetEnvWithDefault(envEnableComputeInstanceController, false),
		"Enable the compute-instance controller.")
	flag.BoolVar(&flags.Cluster, "enable-cluster-controller",
		helpers.GetEnvWithDefault(envEnableClusterController, false),
		"Enable the cluster controller.")
	flag.BoolVar(&flags.Networking, "enable-networking-controller",
		helpers.GetEnvWithDefault(envEnableNetworkingController, false),
		"Enable the networking controllers (VirtualNetwork, Subnet, SecurityGroup).")
	return flags
}

// enableAllIfNoneSet enables all controllers if none are explicitly enabled.
func (f *controllerFlags) enableAllIfNoneSet() {
	if !f.Tenant && !f.HostPool && !f.ComputeInstance && !f.Cluster && !f.Networking {
		f.Tenant = true
		f.HostPool = true
		f.ComputeInstance = true
		f.Cluster = true
		f.Networking = true
		setupLog.Info("no controller flags set, enabling all controllers")
	}
}

// addSchemesForLocalControllers registers only the API schemes required by the enabled controllers.
// Must be called before creating the manager.
func addSchemesForLocalControllers(
	localScheme *runtime.Scheme,
	enableCluster, enableHostPool, enableComputeInstance, enableTenant, enableNetworking bool,
) {
	utilruntime.Must(clientgoscheme.AddToScheme(localScheme))
	utilruntime.Must(v1alpha1.AddToScheme(localScheme))
	if enableCluster {
		utilruntime.Must(hypershiftv1beta1.AddToScheme(localScheme))
	}
	if enableComputeInstance {
		utilruntime.Must(kubevirtv1.AddToScheme(localScheme))
	}
	if enableTenant {
		utilruntime.Must(ovnv1.AddToScheme(localScheme))
	}
	// +kubebuilder:scaffold:scheme
}

// addSchemesForRemoteControllers registers only the API schemes required by the enabled controllers.
// Must be called before creating the manager.
func addSchemesForRemoteControllers(
	localScheme *runtime.Scheme,
	remoteScheme *runtime.Scheme,
	enableComputeInstance, enableTenant bool,
) {
	utilruntime.Must(clientgoscheme.AddToScheme(localScheme))
	utilruntime.Must(v1alpha1.AddToScheme(localScheme))

	utilruntime.Must(clientgoscheme.AddToScheme(remoteScheme))
	if enableComputeInstance {
		utilruntime.Must(kubevirtv1.AddToScheme(remoteScheme))
	}
	if enableTenant {
		utilruntime.Must(ovnv1.AddToScheme(remoteScheme))
	}
	// +kubebuilder:scaffold:scheme
}

// newClusterFromKubeconfig creates a controller-runtime cluster from a kubeconfig file path.
// The cluster uses the given scheme for type resolution. The caller is responsible for
// starting the cluster (e.g. in a goroutine with cl.Start(ctx)) so it runs with the manager.
func newClusterFromKubeconfig(kubeconfigPath string, scheme *runtime.Scheme) (cluster.Cluster, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build config from kubeconfig %q: %w", kubeconfigPath, err)
	}
	cl, err := cluster.New(config, func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		return nil, fmt.Errorf("create cluster from kubeconfig: %w", err)
	}
	return cl, nil
}

// createEDAProvider creates and validates EDA webhook provider configuration.
func createEDAProvider(
	provisionWebhook, deprovisionWebhook string,
	minimumRequestInterval time.Duration,
) (provisioning.ProvisioningProvider, time.Duration, error) {
	webhookClient := controller.NewWebhookClient(10*time.Second, minimumRequestInterval)

	provider := provisioning.NewEDAProvider(
		webhookClient,
		provisionWebhook, deprovisionWebhook,
	)

	setupLog.Info("using EDA webhook provider",
		"provisionWebhook", provisionWebhook,
		"deprovisionWebhook", deprovisionWebhook)

	return provider, provisioning.DefaultStatusPollInterval, nil
}

// createAAPProvider creates and validates AAP direct provider configuration.
func createAAPProvider(
	aapURL, aapToken, provisionTemplate, deprovisionTemplate string,
	aapInsecureSkipVerify bool,
) (provisioning.ProvisioningProvider, time.Duration, error) {
	statusPollInterval := parsePollInterval(envAAPStatusPollInterval, provisioning.DefaultStatusPollInterval)

	aapClient := aap.NewClient(aapURL, aapToken, aapInsecureSkipVerify)
	config := provisioning.ProviderConfig{
		ProviderType:        provisioning.ProviderTypeAAP,
		AAPClient:           aapClient,
		ProvisionTemplate:   provisionTemplate,
		DeprovisionTemplate: deprovisionTemplate,
	}

	provider, err := provisioning.NewProvider(config)
	if err != nil {
		return nil, 0, err
	}

	setupLog.Info("using AAP direct provider for ComputeInstance",
		"url", aapURL,
		"provisionTemplate", provisionTemplate,
		"deprovisionTemplate", deprovisionTemplate,
		"statusPollInterval", statusPollInterval,
		"insecureSkipVerify", aapInsecureSkipVerify)

	return provider, statusPollInterval, nil
}

// createProvider creates a provisioning provider based on type.
func createProvider(
	providerType provisioning.ProviderType,
	provisionWebhook, deprovisionWebhook string,
	aapURL, aapToken, provisionTemplate, deprovisionTemplate string,
	aapInsecureSkipVerify bool,
	minimumRequestInterval time.Duration,
) (provisioning.ProvisioningProvider, time.Duration, error) {
	switch providerType {
	case provisioning.ProviderTypeEDA:
		return createEDAProvider(provisionWebhook, deprovisionWebhook, minimumRequestInterval)

	case provisioning.ProviderTypeAAP:
		return createAAPProvider(aapURL, aapToken, provisionTemplate, deprovisionTemplate, aapInsecureSkipVerify)

	default:
		return nil, 0, fmt.Errorf("unknown provider type: %s", providerType)
	}
}

// createProviderFromEnv creates a provisioning provider by reading shared env vars
// and optional per-resource-type template overrides.
func createProviderFromEnv(
	provisionWebhookEnv, deprovisionWebhookEnv string,
	templateOverrideProvisionEnv, templateOverrideDeprovisionEnv string,
	minimumRequestInterval time.Duration,
) (provisioning.ProvisioningProvider, time.Duration, error) {
	providerTypeStr := os.Getenv(envProvisioningProvider)
	providerType := provisioning.ProviderType(providerTypeStr)
	if providerType == "" {
		return nil, 0, nil
	}
	provisionWebhook := os.Getenv(provisionWebhookEnv)
	deprovisionWebhook := os.Getenv(deprovisionWebhookEnv)
	aapURL := os.Getenv(envAAPURL)
	aapToken := os.Getenv(envAAPToken)
	provisionTemplate := helpers.GetEnvWithDefault(templateOverrideProvisionEnv, os.Getenv(envAAPProvisionTemplate))
	deprovisionTemplate := helpers.GetEnvWithDefault(templateOverrideDeprovisionEnv, os.Getenv(envAAPDeprovisionTemplate))
	aapInsecureSkipVerify := helpers.GetEnvWithDefault(envAAPInsecureSkipVerify, false)
	return createProvider(
		providerType,
		provisionWebhook, deprovisionWebhook,
		aapURL, aapToken, provisionTemplate, deprovisionTemplate,
		aapInsecureSkipVerify,
		minimumRequestInterval,
	)
}

// setupClusterControllers registers the ClusterOrder controller and, when grpcConn is set,
// the cluster Feedback controller.
func setupClusterControllers(
	mgr mcmanager.Manager, grpcConn *grpc.ClientConn, minimumRequestInterval time.Duration,
	maxJobHistory int,
) error {
	localMgr := mgr.GetLocalManager()
	if grpcConn != nil {
		if err := (controller.NewFeedbackReconciler(
			ctrl.Log.WithName("feedback"),
			localMgr.GetClient(),
			grpcConn,
			os.Getenv("OSAC_CLUSTER_ORDER_NAMESPACE"),
		)).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("feedback controller: %w", err)
		}
	}
	provider, statusPollInterval, err := createProviderFromEnv(
		envClusterOrderProvisionWebhook, envClusterOrderDeprovisionWebhook,
		envClusterOrderAAPProvisionTemplate, envClusterOrderAAPDeprovisionTemplate,
		minimumRequestInterval,
	)
	if err != nil {
		return fmt.Errorf("create clusterorder provisioning provider: %w", err)
	}
	if err := (controller.NewClusterOrderReconciler(
		localMgr.GetClient(),
		localMgr.GetScheme(),
		os.Getenv("OSAC_CLUSTER_CREATE_WEBHOOK"),
		os.Getenv("OSAC_CLUSTER_DELETE_WEBHOOK"),
		os.Getenv("OSAC_CLUSTER_ORDER_NAMESPACE"),
		minimumRequestInterval,
		provider,
		statusPollInterval,
		maxJobHistory,
	)).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("cluster order controller: %w", err)
	}
	return nil
}

// setupHostPoolControllers registers the HostPool controller and, when grpcConn is set,
// the HostPool Feedback controller.
func setupHostPoolControllers(
	mgr mcmanager.Manager, grpcConn *grpc.ClientConn, minimumRequestInterval time.Duration,
	maxJobHistory int,
) error {
	localMgr := mgr.GetLocalManager()
	if grpcConn != nil {
		if err := (controller.NewHostPoolFeedbackReconciler(
			ctrl.Log.WithName("feedback"),
			localMgr.GetClient(),
			grpcConn,
			os.Getenv("OSAC_HOSTPOOL_ORDER_NAMESPACE"),
		)).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("hostpool feedback controller: %w", err)
		}
	}
	provider, statusPollInterval, err := createProviderFromEnv(
		envHostPoolProvisionWebhook, envHostPoolDeprovisionWebhook,
		envHostPoolAAPProvisionTemplate, envHostPoolAAPDeprovisionTemplate,
		minimumRequestInterval,
	)
	if err != nil {
		return fmt.Errorf("create hostpool provisioning provider: %w", err)
	}
	if err := (controller.NewHostPoolReconciler(
		localMgr.GetClient(),
		localMgr.GetScheme(),
		os.Getenv("OSAC_HOSTPOOL_CREATE_WEBHOOK"),
		os.Getenv("OSAC_HOSTPOOL_DELETE_WEBHOOK"),
		os.Getenv("OSAC_HOSTPOOL_ORDER_NAMESPACE"),
		minimumRequestInterval,
		provider,
		statusPollInterval,
		maxJobHistory,
	)).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("hostpool controller: %w", err)
	}
	return nil
}

// setupComputeInstanceControllers registers the ComputeInstance controller and, when grpcConn is set,
// the ComputeInstance Feedback controller.
func setupComputeInstanceControllers(
	mgr mcmanager.Manager,
	grpcConn *grpc.ClientConn,
	minimumRequestInterval time.Duration,
	maxJobHistory int,
) error {
	localMgr := mgr.GetLocalManager()
	computeInstanceNamespace := os.Getenv(envComputeInstanceNamespace)
	tenantNamespace := os.Getenv(envTenantNamespace)
	targetCluster := mcmanager.LocalCluster
	if mgr.GetProvider() != nil {
		targetCluster = remoteClusterName
	}
	providerTypeStr := os.Getenv(envProvisioningProvider)
	provisionWebhook := os.Getenv(envComputeInstanceProvisionWebhook)
	deprovisionWebhook := os.Getenv(envComputeInstanceDeprovisionWebhook)
	aapURL := os.Getenv(envAAPURL)
	aapToken := os.Getenv(envAAPToken)
	provisionTemplate := os.Getenv(envAAPProvisionTemplate)
	deprovisionTemplate := os.Getenv(envAAPDeprovisionTemplate)
	aapInsecureSkipVerify := helpers.GetEnvWithDefault(envAAPInsecureSkipVerify, false)
	providerType := provisioning.ProviderType(providerTypeStr)
	if providerType == "" {
		providerType = provisioning.ProviderTypeEDA
	}
	computeInstanceProvider, statusPollInterval, err := createProvider(
		providerType,
		provisionWebhook, deprovisionWebhook,
		aapURL, aapToken, provisionTemplate, deprovisionTemplate,
		aapInsecureSkipVerify,
		minimumRequestInterval,
	)
	if err != nil {
		return fmt.Errorf("create provisioning provider: %w", err)
	}
	if grpcConn != nil {
		if err := (controller.NewComputeInstanceFeedbackReconciler(
			localMgr.GetClient(),
			grpcConn,
			computeInstanceNamespace,
		)).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("computeinstance feedback controller: %w", err)
		}
	}
	if err := (controller.NewComputeInstanceReconciler(
		mgr,
		computeInstanceNamespace,
		tenantNamespace,
		computeInstanceProvider,
		statusPollInterval,
		maxJobHistory,
		targetCluster,
	)).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("computeinstance controller: %w", err)
	}
	return nil
}

// setupTenantController registers the Tenant controller.
func setupTenantController(mgr mcmanager.Manager) error {
	targetCluster := mcmanager.LocalCluster
	if mgr.GetProvider() != nil {
		targetCluster = remoteClusterName
	}

	tenantNamespace := os.Getenv(envTenantNamespace)
	if err := (controller.NewTenantReconciler(
		mgr,
		tenantNamespace,
		targetCluster,
	)).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("tenant controller: %w", err)
	}
	return nil
}

// setupNetworkingControllers registers the VirtualNetwork, Subnet, and SecurityGroup controllers
// along with their feedback controllers when grpcConn is set.
func setupNetworkingControllers(
	mgr mcmanager.Manager,
	grpcConn *grpc.ClientConn,
	maxJobHistory int,
) error {
	localMgr := mgr.GetLocalManager()

	// Get namespace from environment (single namespace for all networking resources)
	networkingNamespace := os.Getenv(envNetworkingNamespace)

	// Get provider configuration
	aapURL := os.Getenv(envAAPURL)
	aapToken := os.Getenv(envAAPToken)
	provisionTemplate := os.Getenv(envAAPProvisionTemplate)
	deprovisionTemplate := os.Getenv(envAAPDeprovisionTemplate)
	aapInsecureSkipVerify := helpers.GetEnvWithDefault(envAAPInsecureSkipVerify, false)

	// Create provider (AAP only for networking - no EDA webhook support)
	networkingProvider, statusPollInterval, err := createAAPProvider(
		aapURL, aapToken, provisionTemplate, deprovisionTemplate,
		aapInsecureSkipVerify,
	)
	if err != nil {
		return fmt.Errorf("create networking provisioning provider: %w", err)
	}

	// Setup VirtualNetwork controller and feedback
	if grpcConn != nil {
		if err := controller.NewVirtualNetworkFeedbackReconciler(
			localMgr.GetClient(),
			grpcConn,
			networkingNamespace,
		).SetupWithManager(localMgr); err != nil {
			return fmt.Errorf("virtualnetwork feedback controller: %w", err)
		}
	}

	if err := (&controller.VirtualNetworkReconciler{
		Client:               localMgr.GetClient(),
		Scheme:               localMgr.GetScheme(),
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: networkingProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
	}).SetupWithManager(localMgr); err != nil {
		return fmt.Errorf("virtualnetwork controller: %w", err)
	}

	// Setup Subnet controller and feedback
	if grpcConn != nil {
		if err := controller.NewSubnetFeedbackReconciler(
			localMgr.GetClient(),
			grpcConn,
			networkingNamespace,
		).SetupWithManager(localMgr); err != nil {
			return fmt.Errorf("subnet feedback controller: %w", err)
		}
	}

	if err := (&controller.SubnetReconciler{
		Client:               localMgr.GetClient(),
		Scheme:               localMgr.GetScheme(),
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: networkingProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
	}).SetupWithManager(localMgr); err != nil {
		return fmt.Errorf("subnet controller: %w", err)
	}

	// Setup SecurityGroup controller (no feedback controller yet)
	if err := (&controller.SecurityGroupReconciler{
		Client:               localMgr.GetClient(),
		Scheme:               localMgr.GetScheme(),
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: networkingProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
	}).SetupWithManager(localMgr); err != nil {
		return fmt.Errorf("securitygroup controller: %w", err)
	}

	return nil
}

func main() {
	var err error

	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var grpcPlaintext bool
	var grpcInsecure bool
	var grpcTokenFile string
	var fulfillmentServerAddress string
	var remoteClusterKubeconfig string
	var minimumRequestInterval time.Duration
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&grpcPlaintext,
		"grpc-plaintext",
		false,
		"Enable gRPC without TLS.",
	)
	flag.BoolVar(
		&grpcInsecure,
		"grpc-insecure",
		false,
		"Enable insecure gRPC, without checking the server TLS certificates.",
	)
	flag.StringVar(
		&grpcTokenFile,
		"fulfillment-server-token-file",
		os.Getenv("OSAC_FULFILLMENT_TOKEN_FILE"),
		"Path of the file containing the token for gRPC authentication to the fulfillment service.",
	)
	flag.StringVar(
		&fulfillmentServerAddress,
		"fulfillment-server-address",
		os.Getenv("OSAC_FULFILLMENT_SERVER_ADDRESS"),
		"Address of the fulfillment server.",
	)
	flag.DurationVar(
		&minimumRequestInterval,
		"minimum-request-interval",
		helpers.GetEnvWithDefault("OSAC_MINIMUM_REQUEST_INTERVAL", time.Duration(0)),
		"Minimum amount of time between calls to the same webook url",
	)
	flag.StringVar(
		&remoteClusterKubeconfig,
		"remote-cluster-kubeconfig",
		os.Getenv(envRemoteClusterKubeconfig),
		"Path to the kubeconfig for the remote cluster (supported by tenant and compute-instance controllers only).",
	)

	// Controller enable flags. Defaults from env; if none are set (flag or env), all controllers are enabled.
	ctrlFlags := registerControllerFlags()
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrlFlags.enableAllIfNoneSet()

	if remoteClusterKubeconfig != "" && (ctrlFlags.HostPool || ctrlFlags.Cluster) {
		setupLog.Error(nil, "remote cluster kubeconfig option is not supported along with host-pool and cluster controllers")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// TODO(user): TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
		// not provided, self-signed certificates will be generated by default. This option is not recommended for
		// production environments as self-signed certificates do not offer the same level of trust and security
		// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
		// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
		// to provide certificates, ensuring the server communicates using trusted and secure certificates.
		TLSOpts: tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Add the schemes depending if controllers reconcile locally or remotely
	localScheme := runtime.NewScheme()
	var remoteScheme *runtime.Scheme
	var remoteProvider multicluster.Provider
	var remoteCluster cluster.Cluster
	if remoteClusterKubeconfig == "" {
		localScheme = runtime.NewScheme()
		addSchemesForLocalControllers(localScheme,
			ctrlFlags.Cluster,
			ctrlFlags.HostPool,
			ctrlFlags.ComputeInstance,
			ctrlFlags.Tenant,
			ctrlFlags.Networking,
		)
	} else {
		remoteScheme = runtime.NewScheme()
		addSchemesForRemoteControllers(localScheme, remoteScheme,
			ctrlFlags.ComputeInstance,
			ctrlFlags.Tenant,
		)
		remoteCluster, err = newClusterFromKubeconfig(remoteClusterKubeconfig, remoteScheme)
		if err != nil {
			setupLog.Error(err, "unable to create remote cluster from kubeconfig")
			os.Exit(1)
		}
		remoteProvider = single.New(remoteClusterName, remoteCluster)
	}

	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), remoteProvider, manager.Options{
		Scheme:                 localScheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "95f7e044.openshift.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create the gRPC connection:
	var grpcConn *grpc.ClientConn
	if fulfillmentServerAddress != "" {
		setupLog.Info("gRPC connection to fulfillment service is enabled")
		grpcConn, err = createGrpcConn(grpcPlaintext, grpcInsecure, grpcTokenFile, fulfillmentServerAddress)
		if err != nil {
			setupLog.Error(err, "failed to create gRPC connection to fulfillment service")
			os.Exit(1)
		}
		defer grpcConn.Close() //nolint:errcheck
	} else {
		setupLog.Info("gRPC connection to fulfillment service is disabled")
	}

	maxJobHistory := parseIntEnv(envMaxJobHistory, controller.DefaultMaxJobHistory)
	setupLog.Info("job history configuration", "maxJobs", maxJobHistory)

	if ctrlFlags.Cluster {
		if err := setupClusterControllers(mgr, grpcConn, minimumRequestInterval, maxJobHistory); err != nil {
			setupLog.Error(err, "unable to setup cluster controllers")
			os.Exit(1)
		}
	}
	if ctrlFlags.HostPool {
		if err := setupHostPoolControllers(mgr, grpcConn, minimumRequestInterval, maxJobHistory); err != nil {
			setupLog.Error(err, "unable to setup hostpool controllers")
			os.Exit(1)
		}
	}
	if ctrlFlags.ComputeInstance {
		if err := setupComputeInstanceControllers(mgr, grpcConn, minimumRequestInterval, maxJobHistory); err != nil {
			setupLog.Error(err, "unable to setup computeinstance controllers")
			os.Exit(1)
		}
	}
	if ctrlFlags.Tenant {
		if err := setupTenantController(mgr); err != nil {
			setupLog.Error(err, "unable to setup tenant controller")
			os.Exit(1)
		}
	}
	if ctrlFlags.Networking {
		if err := setupNetworkingControllers(mgr, grpcConn, maxJobHistory); err != nil {
			setupLog.Error(err, "unable to setup networking controllers")
			os.Exit(1)
		}
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	if remoteCluster != nil {
		setupLog.Info("starting remote cluster")
		go func() {
			err := remoteCluster.Start(ctx)
			if err != nil {
				setupLog.Error(err, "unable to start remote cluster")
				os.Exit(1)
			}
		}()
	}
	if remoteProvider != nil {
		setupLog.Info("starting remote provider")
		go func() {
			if err := remoteProvider.(multicluster.ProviderRunnable).Start(ctx, mgr); err != nil {
				setupLog.Error(err, "remote provider failed")
			}
		}()
	}
	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

//nolint:nakedret
func createGrpcConn(plaintext, insecure bool, tokenFile, serverAddress string) (result *grpc.ClientConn, err error) {
	// Configure use of TLS:
	var dialOpts []grpc.DialOption
	var transportCreds credentials.TransportCredentials
	if plaintext {
		transportCreds = insecurecredentials.NewCredentials()
	} else {
		tlsConfig := &tls.Config{}
		if insecure {
			tlsConfig.InsecureSkipVerify = true
		}

		// TODO: This should have been the non-experimental package, but we need to use this one because
		// currently the OpenShift router doesn't seem to support ALPN, and the regular credentials package
		// requires it since version 1.67. See here for details:
		//
		// https://github.com/grpc/grpc-go/issues/434
		// https://github.com/grpc/grpc-go/pull/7980
		//
		// Is there a way to configure the OpenShift router to avoid this?
		transportCreds = experimentalcredentials.NewTLSWithALPNDisabled(tlsConfig)
	}
	if transportCreds != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(transportCreds))
	}

	// Confgure use of token:
	if tokenFile != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(oauth.TokenSource{
			TokenSource: &fileTokenSource{
				tokenFile: tokenFile,
			},
		}))
	}

	// Create the connection:
	conn, err := grpc.NewClient(serverAddress, dialOpts...)
	if err != nil {
		return
	}

	result = conn
	return
}

// fileTokenSource is a token source that reads the token from a file whenever it is needed.
type fileTokenSource struct {
	tokenFile string
}

func (s *fileTokenSource) Token() (token *oauth2.Token, err error) {
	var data []byte
	data, err = os.ReadFile(s.tokenFile)
	if err != nil {
		err = fmt.Errorf("failed to read token from file '%s': %w", s.tokenFile, err)
		return
	}
	token = &oauth2.Token{
		AccessToken: strings.TrimSpace(string(data)),
	}
	return
}
