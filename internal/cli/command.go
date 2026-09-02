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

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/source"
)

type colorMode string

const (
	colorAuto   colorMode = "auto"
	colorAlways colorMode = "always"
	colorNever  colorMode = "never"
)

func (m *colorMode) Set(value string) error {
	switch colorMode(value) {
	case colorAuto, colorAlways, colorNever:
		*m = colorMode(value)
		return nil
	default:
		return fmt.Errorf("invalid color mode %q: use auto, always, or never", value)
	}
}

func (m *colorMode) String() string {
	if m == nil {
		return ""
	}
	return string(*m)
}

func (m *colorMode) Type() string {
	return "string"
}

type commandOptions struct {
	kubeconfig string
	context    string
	namespace  string
	output     string
	timeout    time.Duration
	provider   string
	sourceKind string
	recordType string
	color      colorMode
	getenv     func(string) (string, bool)
	isTerminal func(io.Writer) bool
}

func NewCommand(version string, stdout, stderr io.Writer) *cobra.Command {
	return newCommand(version, stdout, stderr, os.LookupEnv, outputIsTerminal)
}

func newCommand(version string, stdout, stderr io.Writer, getenv func(string) (string, bool), isTerminal func(io.Writer) bool) *cobra.Command {
	options := &commandOptions{color: colorAuto, getenv: getenv, isTerminal: isTerminal}
	root := &cobra.Command{
		Use:           "labdns",
		Short:         "Inspect labdns publication state",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&options.kubeconfig, "kubeconfig", "", "Path to the kubeconfig file")
	root.PersistentFlags().StringVar(&options.context, "context", "", "Kubeconfig context to use")
	root.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", "", "Limit inspection to a namespace")
	root.PersistentFlags().StringVarP(&options.output, "output", "o", "table", "Output format: table or json")
	root.PersistentFlags().DurationVar(&options.timeout, "request-timeout", 10*time.Second, "Timeout for Kubernetes and DNS requests")
	root.PersistentFlags().Var(&options.color, "color", "Color table output: auto, always, or never")

	root.AddCommand(newListCommand(options), newShowCommand(options), newStatusCommand(options))
	return root
}

func newListCommand(options *commandOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "list",
		Short: "List labdns-managed records",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			inspector, ctx, cancel, err := prepare(command.Context(), options)
			if err != nil {
				return err
			}
			defer cancel()
			result, err := inspector.Records(ctx, options.filters())
			if err != nil {
				return err
			}
			return writeOutput(command.OutOrStdout(), options.output, result, func(writer io.Writer) error {
				return writeRecords(writer, result, options.colorizer(command))
			})
		},
	}
	addRecordFilters(command, options)
	return command
}

func newShowCommand(options *commandOptions) *cobra.Command {
	var dnsServer string
	command := &cobra.Command{
		Use:   "show <fqdn>",
		Short: "Show labdns, ExternalDNS, and optional live DNS state for a record",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			name, err := source.NormalizeHostname(args[0])
			if err != nil {
				return fmt.Errorf("invalid DNS name: %w", err)
			}
			inspector, ctx, cancel, err := prepare(command.Context(), options)
			if err != nil {
				return err
			}
			defer cancel()
			result, err := inspector.Details(ctx, name, options.filters(), dnsServer)
			if err != nil {
				return err
			}
			if err := writeOutput(command.OutOrStdout(), options.output, result, func(writer io.Writer) error {
				return writeDetails(writer, result, options.colorizer(command))
			}); err != nil {
				return err
			}
			for _, item := range result.Items {
				if item.DNS != nil && item.DNS.Error != "" {
					return errors.New("one or more DNS lookups failed")
				}
			}
			return nil
		},
	}
	addRecordFilters(command, options)
	command.Flags().StringVar(&dnsServer, "dns-server", "", "DNS resolver host or host:port for live comparison")
	return command
}

func newStatusCommand(options *commandOptions) *cobra.Command {
	var controllerNamespace, controllerName string
	command := &cobra.Command{
		Use:   "status",
		Short: "Summarize labdns health and publication state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			inspector, ctx, cancel, err := prepare(command.Context(), options)
			if err != nil {
				return err
			}
			defer cancel()
			selectedNamespace := controllerNamespace
			if selectedNamespace == "" {
				selectedNamespace = options.namespace
			}
			result, failed := inspector.Status(ctx, selectedNamespace, controllerName)
			if err := writeOutput(command.OutOrStdout(), options.output, result, func(writer io.Writer) error {
				return writeStatus(writer, result, options.colorizer(command))
			}); err != nil {
				return err
			}
			if failed {
				return errors.New("labdns status is unhealthy")
			}
			return nil
		},
	}
	command.Flags().StringVar(&controllerNamespace, "controller-namespace", "", "Limit controller discovery to a namespace")
	command.Flags().StringVar(&controllerName, "controller-name", "", "Select an exact controller Deployment name")
	return command
}

func addRecordFilters(command *cobra.Command, options *commandOptions) {
	command.Flags().StringVar(&options.provider, "provider", "", "Limit records to a logical DNSProvider")
	command.Flags().StringVar(&options.sourceKind, "source-kind", "", "Limit records to a source kind")
	command.Flags().StringVar(&options.recordType, "record-type", "", "Limit records to a DNS record type")
}

func (o *commandOptions) filters() Filters {
	return Filters{Namespace: o.namespace, Provider: o.provider, SourceKind: o.sourceKind, RecordType: strings.ToUpper(o.recordType)}
}

func prepare(parent context.Context, options *commandOptions) (Inspector, context.Context, context.CancelFunc, error) {
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	loading.ExplicitPath = options.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: options.context}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
	if err != nil {
		return Inspector{}, nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.Timeout = options.timeout
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, labdnsv1alpha1.AddToScheme,
		externaldnsv1alpha1.AddToScheme, gatewayv1.Install,
		gatewayv1beta1.Install,
	} {
		if err := add(scheme); err != nil {
			return Inspector{}, nil, nil, fmt.Errorf("build Kubernetes scheme: %w", err)
		}
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return Inspector{}, nil, nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return Inspector{}, nil, nil, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, options.timeout)
	return Inspector{Client: kubeClient, Discovery: discoveryClient}, ctx, cancel, nil
}

func (o *commandOptions) colorizer(command *cobra.Command) outputColorizer {
	// Structured output is intended for machine consumption and must remain
	// free of terminal control sequences regardless of the colour setting.
	if strings.EqualFold(o.output, "json") {
		return outputColorizer{}
	}
	switch o.color {
	case colorNever:
		return outputColorizer{}
	case colorAlways:
		return outputColorizer{enabled: true}
	case colorAuto:
		if o.getenv != nil {
			if value, present := o.getenv("NO_COLOR"); present && value != "" {
				return outputColorizer{}
			}
			if value, present := o.getenv("TERM"); present && strings.EqualFold(value, "dumb") {
				return outputColorizer{}
			}
		}
		if o.isTerminal != nil && o.isTerminal(command.OutOrStdout()) {
			return outputColorizer{enabled: true}
		}
	}
	return outputColorizer{}
}

func outputIsTerminal(writer io.Writer) bool {
	file, ok := writer.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func writeOutput(writer io.Writer, format string, value any, table func(io.Writer) error) error {
	switch strings.ToLower(format) {
	case "table":
		return table(writer)
	case "json":
		return WriteJSON(writer, value)
	default:
		return fmt.Errorf("invalid output format %q: use table or json", format)
	}
}
