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
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	v1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/controller"
	"github.com/innabox/cloudkit-operator/internal/helpers"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(hypershiftv1beta1.AddToScheme(scheme))
	utilruntime.Must(kubevirtv1.AddToScheme(scheme))
	utilruntime.Must(ovnv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var grpcPlaintext bool
	var grpcInsecure bool
	var grpcTokenFile string
	var fulfillmentServerAddress string
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
		os.Getenv("CLOUDKIT_FULFILLMENT_TOKEN_FILE"),
		"Path of the file containing the token for gRPC authentication to the fulfillment service.",
	)
	flag.StringVar(
		&fulfillmentServerAddress,
		"fulfillment-server-address",
		os.Getenv("CLOUDKIT_FULFILLMENT_SERVER_ADDRESS"),
		"Address of the fulfillment server.",
	)
	flag.DurationVar(
		&minimumRequestInterval,
		"minimum-request-interval",
		helpers.GetEnvWithDefault("CLOUDKIT_MINIMUM_REQUEST_INTERVAL", time.Duration(0)),
		"Minimum amount of time between calls to the same webook url",
	)
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
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

	computeInstanceNamespace := os.Getenv("CLOUDKIT_COMPUTE_INSTANCE_NAMESPACE")

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
		if err = (controller.NewFeedbackReconciler(
			ctrl.Log.WithName("feedback"),
			mgr.GetClient(),
			grpcConn,
			os.Getenv("CLOUDKIT_CLUSTER_ORDER_NAMESPACE"),
		)).SetupWithManager(mgr); err != nil {
			setupLog.Error(
				err,
				"unable to create feedback controller",
				"controller", "Feedback",
			)
			os.Exit(1)
		}

		// Create the HostPool feedback reconciler:
		if err = (controller.NewHostPoolFeedbackReconciler(
			ctrl.Log.WithName("hostpool-feedback"),
			mgr.GetClient(),
			grpcConn,
			os.Getenv("CLOUDKIT_HOSTPOOL_ORDER_NAMESPACE"),
		)).SetupWithManager(mgr); err != nil {
			setupLog.Error(
				err,
				"unable to create hostpool feedback controller",
				"controller", "HostPoolFeedback",
			)
			os.Exit(1)
		}

		// Create the Host feedback reconciler:
		if err = (controller.NewHostFeedbackReconciler(
			mgr.GetClient(),
			grpcConn,
			os.Getenv("CLOUDKIT_HOST_ORDER_NAMESPACE"),
		)).SetupWithManager(mgr); err != nil {
			setupLog.Error(
				err,
				"unable to create host feedback controller",
				"controller", "HostFeedback",
			)
			os.Exit(1)
		}

		// Create the ComputeInstance feedback reconciler:
		if err = (controller.NewComputeInstanceFeedbackReconciler(
			mgr.GetClient(),
			grpcConn,
			computeInstanceNamespace,
		)).SetupWithManager(mgr); err != nil {
			setupLog.Error(
				err,
				"unable to create computeinstance feedback controller",
				"controller", "ComputeInstanceFeedback",
			)
			os.Exit(1)
		}
	} else {
		setupLog.Info("gRPC connection to fulfillment service is disabled")
	}

	if err = (controller.NewClusterOrderReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		os.Getenv("CLOUDKIT_CLUSTER_CREATE_WEBHOOK"),
		os.Getenv("CLOUDKIT_CLUSTER_DELETE_WEBHOOK"),
		os.Getenv("CLOUDKIT_CLUSTER_ORDER_NAMESPACE"),
		minimumRequestInterval,
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterOrder")
		os.Exit(1)
	}

	if err = (controller.NewHostPoolReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		os.Getenv("CLOUDKIT_HOSTPOOL_CREATE_WEBHOOK"),
		os.Getenv("CLOUDKIT_HOSTPOOL_DELETE_WEBHOOK"),
		os.Getenv("CLOUDKIT_HOSTPOOL_ORDER_NAMESPACE"),
		minimumRequestInterval,
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HostPool")
		os.Exit(1)
	}

	if err = (controller.NewHostReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		os.Getenv("CLOUDKIT_HOST_CREATE_WEBHOOK"),
		os.Getenv("CLOUDKIT_HOST_DELETE_WEBHOOK"),
		os.Getenv("CLOUDKIT_HOST_ORDER_NAMESPACE"),
		minimumRequestInterval,
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Host")
		os.Exit(1)
	}

	// Create the ComputeInstance reconciler
	if err = (controller.NewComputeInstanceReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		os.Getenv("CLOUDKIT_COMPUTE_INSTANCE_CREATE_WEBHOOK"),
		os.Getenv("CLOUDKIT_COMPUTE_INSTANCE_DELETE_WEBHOOK"),
		computeInstanceNamespace,
		minimumRequestInterval,
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ComputeInstance")
		os.Exit(1)
	}

	// Tenant reconciler in ComputeInstance namespace
	if err := (controller.NewTenantReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		computeInstanceNamespace,
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant", "namespace", computeInstanceNamespace)
		os.Exit(1)
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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
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
