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
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

func WriteJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func WriteRecords(writer io.Writer, list RecordList) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAMESPACE\tDNS NAME\tTYPE\tTARGETS\tTTL\tPROVIDER\tSOURCE\tEXTERNALDNS\tRETIRING"); err != nil {
		return err
	}
	for _, item := range list.Items {
		source := item.Source.Kind + "/" + item.Source.Namespace + "/" + item.Source.Name
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\n", item.DNSEndpoint.Namespace,
			item.DNSName, item.RecordType, strings.Join(item.Targets, ","), item.TTL, item.Provider, source,
			item.ExternalDNSState, len(item.Retiring)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func WriteDetails(writer io.Writer, details RecordDetails) error {
	for index, detail := range details.Items {
		if index != 0 {
			if _, err := fmt.Fprintln(writer); err != nil {
				return err
			}
		}
		item := detail.Record
		table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
		rows := [][2]string{
			{"DNS name", item.DNSName}, {"Type", item.RecordType}, {"Provider", item.Provider},
			{"Source", item.Source.Kind + "/" + item.Source.Namespace + "/" + item.Source.Name},
			{"Source state", foundState(detail.Source.Found, detail.Source.UIDMatches)},
			{"DNSEndpoint", item.DNSEndpoint.Namespace + "/" + item.DNSEndpoint.Name},
			{"Targets", strings.Join(item.Targets, ", ")}, {"Active targets", strings.Join(item.ActiveTargets, ", ")},
			{"TTL", strconv.FormatInt(item.TTL, 10)},
			{"ExternalDNS", fmt.Sprintf("%s (generation %d, observed %d)", item.ExternalDNSState, item.Generation, item.Observed)},
			{"Provider state", presentState(detail.Provider.Found)},
			{"Provider zones", strings.Join(detail.Provider.Zones, ", ")},
			{"Provider IPv4 label", detail.Provider.IPv4Label}, {"Provider IPv6 label", detail.Provider.IPv6Label},
			{"Provider properties", formatProperties(item.Properties)},
		}
		for _, row := range rows {
			if _, err := fmt.Fprintf(table, "%s:\t%s\n", row[0], row[1]); err != nil {
				return err
			}
		}
		for _, retiring := range item.Retiring {
			if _, err := fmt.Fprintf(table, "Retiring target:\t%s at %s\n", retiring.Target, retiring.Deadline.Format("2006-01-02T15:04:05Z07:00")); err != nil {
				return err
			}
		}
		if item.LifecycleError != "" {
			if _, err := fmt.Fprintf(table, "Lifecycle error:\t%s\n", item.LifecycleError); err != nil {
				return err
			}
		}
		if detail.DNS != nil {
			if _, err := fmt.Fprintf(table, "DNS lookup:\t%s via %s (%s)\n", strings.Join(detail.DNS.Answers, ", "), detail.DNS.Server, detail.DNS.State); err != nil {
				return err
			}
			if detail.DNS.Error != "" {
				if _, err := fmt.Fprintf(table, "DNS error:\t%s\n", detail.DNS.Error); err != nil {
					return err
				}
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func WriteStatus(writer io.Writer, status Status) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(table, "OVERALL\t%s\n\nPREREQUISITE\tAVAILABLE\tERROR\n", strings.ToUpper(status.Overall)); err != nil {
		return err
	}
	for _, item := range status.Prerequisites {
		if _, err := fmt.Fprintf(table, "%s\t%t\t%s\n", item.Name, item.Available, item.Error); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(table, "\nCONTROLLER\tAVAILABLE\tREADY PODS\tGATEWAY API"); err != nil {
		return err
	}
	for _, item := range status.Controllers {
		if _, err := fmt.Fprintf(table, "%s/%s\t%d/%d\t%d\t%t\n", item.Namespace, item.Name, item.Available, item.Desired, item.ReadyPods, item.GatewayAPI); err != nil {
			return err
		}
	}
	summary := status.Summary
	if _, err := fmt.Fprintf(table, "\nPROVIDERS\tSOURCES\tDNSENDPOINTS\tRECORDS\tPENDING\tOBSERVED\tSTALE\tINVALID\n%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
		summary.Providers, summary.PublishingSources, summary.DNSEndpoints, summary.Records, summary.PendingTargets,
		summary.Observed, summary.Stale, summary.Invalid); err != nil {
		return err
	}
	for _, warning := range status.Warnings {
		if _, err := fmt.Fprintf(table, "WARNING\t%s\n", warning); err != nil {
			return err
		}
	}
	return table.Flush()
}

func foundState(found, uidMatches bool) string {
	if !found {
		return "missing"
	}
	if !uidMatches {
		return "UID mismatch"
	}
	return "current"
}

func presentState(found bool) string {
	if found {
		return "current"
	}
	return "missing"
}

func formatProperties(properties map[string][]string) string {
	values := []string{}
	for name, propertyValues := range properties {
		for _, value := range propertyValues {
			values = append(values, name+"="+value)
		}
	}
	slices.Sort(values)
	return strings.Join(values, ", ")
}
