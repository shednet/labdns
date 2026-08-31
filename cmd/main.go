/*
Copyright 2026 Konstantinos Kalyvas.

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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// Import all Kubernetes client auth plugins so local runs can use the same
	// kubeconfig authentication mechanisms as kubectl.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	labdnscontroller "github.com/shednet/labdns/internal/controller"
	"github.com/shednet/labdns/internal/dnsendpoint"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(labdnsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(externaldnsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

// +kubebuilder:rbac:groups=labdns.shednet.dev,resources=dnsproviders,verbs=get;list;watch

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var healthAddr string
	var logLevel string
	var leaderElect, secureMetrics, enableHTTP2, enableGatewayAPI bool

	flag.StringVar(&metricsAddr, "metrics-addr", "0", "Metrics listener address; use 0 to disable metrics.")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "Health and readiness listener address.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing the metrics TLS certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "Metrics TLS certificate filename.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "Metrics TLS private-key filename.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "Enable HTTP/2 for the metrics server.")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, or error.")
	flag.BoolVar(&enableGatewayAPI, "enable-gateway-api", false, "Watch Gateway API HTTPRoute sources.")
	flag.Parse()

	level, err := parseLogLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(config *tls.Config) {
			config.NextProtos = []string{"http/1.1"}
		})
	}
	metricsOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if metricsCertPath != "" {
		metricsOptions.CertDir = metricsCertPath
		metricsOptions.CertName = metricsCertName
		metricsOptions.KeyName = metricsCertKey
	}

	config := ctrl.GetConfigOrDie()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes discovery client")
		os.Exit(1)
	}
	if err := checkPrerequisites(discoveryClient, enableGatewayAPI); err != nil {
		setupLog.Error(err, "startup prerequisites are not satisfied")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		HealthProbeBindAddress: healthAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "066b3aee.shednet.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder
	output := dnsendpoint.NewWriter(mgr.GetClient())
	operationalMetrics := labdnscontroller.NewMetrics(ctrlmetrics.Registry)
	if err := labdnscontroller.Setup(context.Background(), mgr, output, enableGatewayAPI, operationalMetrics); err != nil {
		setupLog.Error(err, "unable to set up source controllers")
		os.Exit(1)
	}
	if err := labdnscontroller.SetupLifecycle(mgr, output, enableGatewayAPI, operationalMetrics); err != nil {
		setupLog.Error(err, "unable to set up DNSEndpoint lifecycle controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add health check")
		os.Exit(1)
	}
	readiness := &cacheReadiness{}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("manager cache synchronization failed")
		}
		readiness.markSynced()
		<-ctx.Done()
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add cache synchronization readiness tracker")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", readiness.check); err != nil {
		setupLog.Error(err, "unable to add readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager stopped with an error")
		os.Exit(1)
	}
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid --log-level %q: use debug, info, warn, or error", value)
	}
}
