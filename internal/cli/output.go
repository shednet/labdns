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
	"unicode/utf8"
)

const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

type ansiColor uint8

const (
	ansiNone ansiColor = iota
	ansiGreenColor
	ansiYellowColor
	ansiRedColor
)

type outputColorizer struct {
	enabled bool
}

// A table cell retains its unstyled text so column widths can be calculated
// without counting ANSI escape sequences.
type tableSegment struct {
	text  string
	color ansiColor
}

type tableCell []tableSegment
type tableRow []tableCell

func plainCell(text string) tableCell {
	return tableCell{{text: text}}
}

func (c outputColorizer) cell(text string, color ansiColor) tableCell {
	if text == "" || color == ansiNone {
		return plainCell(text)
	}
	return tableCell{{text: text, color: color}}
}

func (c outputColorizer) state(text string) tableCell {
	return c.cell(text, colorForState(text))
}

func (c outputColorizer) composite(parts ...tableSegment) tableCell {
	return tableCell(parts)
}

func (segment tableSegment) rendered(enabled bool) string {
	if !enabled || segment.color == ansiNone || segment.text == "" {
		return segment.text
	}
	return colorCode(segment.color) + segment.text + ansiReset
}

func (cell tableCell) text() string {
	var builder strings.Builder
	for _, segment := range cell {
		builder.WriteString(segment.text)
	}
	return builder.String()
}

func (cell tableCell) width() int {
	return utf8.RuneCountInString(cell.text())
}

func (cell tableCell) rendered(enabled bool) string {
	var builder strings.Builder
	for _, segment := range cell {
		builder.WriteString(segment.rendered(enabled))
	}
	return builder.String()
}

func colorCode(color ansiColor) string {
	switch color {
	case ansiGreenColor:
		return ansiGreen
	case ansiYellowColor:
		return ansiYellow
	case ansiRedColor:
		return ansiRed
	default:
		return ""
	}
}

func writeString(writer io.Writer, value string) error {
	written, err := io.WriteString(writer, value)
	if err != nil {
		return err
	}
	if written != len(value) {
		return io.ErrShortWrite
	}
	return nil
}

// writeTable preserves tabwriter's block behavior. A nil row is a blank line
// and ends the current alignment block. The final cell in a row is not part
// of an aligned column, matching tabwriter's trailing-cell semantics.
func writeTable(writer io.Writer, rows []tableRow, colorizer outputColorizer) error {
	for start := 0; start < len(rows); {
		if rows[start] == nil {
			if err := writeString(writer, "\n"); err != nil {
				return err
			}
			start++
			continue
		}
		end := start
		for end < len(rows) && rows[end] != nil {
			end++
		}
		if err := writeTableBlock(writer, rows[start:end], colorizer); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func writeTableBlock(writer io.Writer, rows []tableRow, colorizer outputColorizer) error {
	rowWidths := make([][]int, len(rows))
	maxColumns := 0
	for index, row := range rows {
		rowWidths[index] = make([]int, max(0, len(row)-1))
		maxColumns = max(maxColumns, len(row)-1)
	}
	// A tab-terminated column is aligned only across the contiguous rows that
	// contain it. A shorter row ends deeper column blocks without ending the
	// shallower ones, matching tabwriter's elastic-tabstop behavior.
	for column := range maxColumns {
		for start := 0; start < len(rows); {
			if len(rows[start]) <= column+1 {
				start++
				continue
			}
			end, width := start, 0
			for end < len(rows) && len(rows[end]) > column+1 {
				width = max(width, rows[end][column].width())
				end++
			}
			for row := start; row < end; row++ {
				rowWidths[row][column] = width
			}
			start = end
		}
	}

	for rowIndex, row := range rows {
		for column, cell := range row {
			if err := writeString(writer, cell.rendered(colorizer.enabled)); err != nil {
				return err
			}
			if column < len(row)-1 {
				if err := writeString(writer, strings.Repeat(" ", rowWidths[rowIndex][column]-cell.width()+2)); err != nil {
					return err
				}
			}
		}
		if err := writeString(writer, "\n"); err != nil {
			return err
		}
	}
	return nil
}

func WriteJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func WriteRecords(writer io.Writer, list RecordList) error {
	return writeRecords(writer, list, outputColorizer{})
}

func writeRecords(writer io.Writer, list RecordList, colorizer outputColorizer) error {
	rows := make([]tableRow, 0, 1+len(list.Items))
	rows = append(rows, tableRow{
		plainCell("NAMESPACE"), plainCell("DNS NAME"), plainCell("TYPE"), plainCell("TARGETS"), plainCell("TTL"),
		plainCell("PROVIDER"), plainCell("SOURCE"), plainCell("EXTERNALDNS"), plainCell("RETIRING"),
	})
	for _, item := range list.Items {
		source := item.Source.Kind + "/" + item.Source.Namespace + "/" + item.Source.Name
		retiring := plainCell(strconv.Itoa(len(item.Retiring)))
		if len(item.Retiring) > 0 {
			retiring = colorizer.cell(strconv.Itoa(len(item.Retiring)), ansiYellowColor)
		}
		rows = append(rows, tableRow{
			plainCell(item.DNSEndpoint.Namespace), plainCell(item.DNSName), plainCell(item.RecordType),
			plainCell(strings.Join(item.Targets, ",")), plainCell(strconv.FormatInt(item.TTL, 10)),
			plainCell(item.Provider), plainCell(source), colorizer.state(item.ExternalDNSState), retiring,
		})
	}
	return writeTable(writer, rows, colorizer)
}

func WriteDetails(writer io.Writer, details RecordDetails) error {
	return writeDetails(writer, details, outputColorizer{})
}

func writeDetails(writer io.Writer, details RecordDetails, colorizer outputColorizer) error {
	for index, detail := range details.Items {
		if index != 0 {
			if err := writeString(writer, "\n"); err != nil {
				return err
			}
		}
		if err := writeTable(writer, detailRows(detail, colorizer), colorizer); err != nil {
			return err
		}
	}
	return nil
}

func detailRows(detail RecordDetail, colorizer outputColorizer) []tableRow {
	item := detail.Record
	rows := make([]tableRow, 0, 15+len(item.Retiring)+2)
	rows = append(rows,
		tableRow{plainCell("DNS name:"), plainCell(item.DNSName)},
		tableRow{plainCell("Type:"), plainCell(item.RecordType)},
		tableRow{plainCell("Provider:"), plainCell(item.Provider)},
		tableRow{plainCell("Source:"), plainCell(item.Source.Kind + "/" + item.Source.Namespace + "/" + item.Source.Name)},
		tableRow{plainCell("Source state:"), colorizer.state(foundState(detail.Source.Found, detail.Source.UIDMatches))},
		tableRow{plainCell("DNSEndpoint:"), plainCell(item.DNSEndpoint.Namespace + "/" + item.DNSEndpoint.Name)},
		tableRow{plainCell("Targets:"), plainCell(strings.Join(item.Targets, ", "))},
		tableRow{plainCell("Active targets:"), plainCell(strings.Join(item.ActiveTargets, ", "))},
		tableRow{plainCell("TTL:"), plainCell(strconv.FormatInt(item.TTL, 10))},
		tableRow{plainCell("ExternalDNS:"), externalDNSCell(colorizer, item.ExternalDNSState, item.Generation, item.Observed)},
		tableRow{plainCell("Provider state:"), colorizer.state(presentState(detail.Provider.Found))},
		tableRow{plainCell("Provider zones:"), plainCell(strings.Join(detail.Provider.Zones, ", "))},
		tableRow{plainCell("Provider IPv4 label:"), plainCell(detail.Provider.IPv4Label)},
		tableRow{plainCell("Provider IPv6 label:"), plainCell(detail.Provider.IPv6Label)},
		tableRow{plainCell("Provider properties:"), plainCell(formatProperties(item.Properties))},
	)
	for _, retiring := range item.Retiring {
		rows = append(rows, tableRow{plainCell("Retiring target:"), plainCell(retiring.Target + " at " + retiring.Deadline.Format("2006-01-02T15:04:05Z07:00"))})
	}
	if item.LifecycleError != "" {
		rows = append(rows, tableRow{plainCell("Lifecycle error:"), colorizer.cell(item.LifecycleError, ansiRedColor)})
	}
	if detail.DNS != nil {
		rows = append(rows, tableRow{plainCell("DNS lookup:"), dnsLookupCell(colorizer, detail.DNS)})
		if detail.DNS.Error != "" {
			rows = append(rows, tableRow{plainCell("DNS error:"), colorizer.cell(detail.DNS.Error, ansiRedColor)})
		}
	}
	return rows
}

func externalDNSCell(colorizer outputColorizer, state string, generation, observed int64) tableCell {
	return colorizer.composite(
		tableSegment{text: state, color: colorForState(state)},
		tableSegment{text: fmt.Sprintf(" (generation %d, observed %d)", generation, observed)},
	)
}

func dnsLookupCell(colorizer outputColorizer, lookup *DNSLookup) tableCell {
	return colorizer.composite(
		tableSegment{text: strings.Join(lookup.Answers, ", ") + " via "},
		tableSegment{text: lookup.Server},
		tableSegment{text: " ("},
		tableSegment{text: lookup.State, color: colorForDNSState(lookup.State)},
		tableSegment{text: ")"},
	)
}

func WriteStatus(writer io.Writer, status Status) error {
	return writeStatus(writer, status, outputColorizer{})
}

func writeStatus(writer io.Writer, status Status, colorizer outputColorizer) error {
	rows := make([]tableRow, 0, 3+len(status.Prerequisites)+2+len(status.Controllers)+3+len(status.Warnings))
	rows = append(rows, tableRow{plainCell("OVERALL"), colorizer.state(strings.ToUpper(status.Overall))}, nil, tableRow{
		plainCell("PREREQUISITE"), plainCell("AVAILABLE"), plainCell("ERROR"),
	})
	for _, item := range status.Prerequisites {
		availableColor := ansiRedColor
		if item.Available {
			availableColor = ansiGreenColor
		}
		errorCell := plainCell(item.Error)
		if item.Error != "" {
			errorCell = colorizer.cell(item.Error, ansiRedColor)
		}
		rows = append(rows, tableRow{
			plainCell(item.Name), colorizer.cell(strconv.FormatBool(item.Available), availableColor), errorCell,
		})
	}
	rows = append(rows, nil, tableRow{plainCell("CONTROLLER"), plainCell("AVAILABLE"), plainCell("READY PODS"), plainCell("GATEWAY API")})
	for _, item := range status.Controllers {
		rows = append(rows, tableRow{
			plainCell(item.Namespace + "/" + item.Name),
			colorizer.cell(fmt.Sprintf("%d/%d", item.Available, item.Desired), controllerAvailabilityColor(item.Available, item.Desired)),
			colorizer.cell(strconv.Itoa(item.ReadyPods), readyPodsColor(item.ReadyPods, item.Desired)),
			plainCell(strconv.FormatBool(item.GatewayAPI)),
		})
	}
	summary := status.Summary
	rows = append(rows, nil, tableRow{
		plainCell("PROVIDERS"), plainCell("SOURCES"), plainCell("DNSENDPOINTS"), plainCell("RECORDS"),
		plainCell("PENDING"), plainCell("OBSERVED"), plainCell("STALE"), plainCell("INVALID"),
	}, tableRow{
		plainCell(strconv.Itoa(summary.Providers)), plainCell(strconv.Itoa(summary.PublishingSources)),
		plainCell(strconv.Itoa(summary.DNSEndpoints)), plainCell(strconv.Itoa(summary.Records)),
		nonZeroCell(colorizer, summary.PendingTargets, ansiYellowColor),
		nonZeroCell(colorizer, summary.Observed, ansiGreenColor),
		nonZeroCell(colorizer, summary.Stale, ansiYellowColor),
		nonZeroCell(colorizer, summary.Invalid, ansiRedColor),
	})
	for _, warning := range status.Warnings {
		rows = append(rows, tableRow{plainCell("WARNING"), colorizer.cell(warning, ansiYellowColor)})
	}
	return writeTable(writer, rows, colorizer)
}

func nonZeroCell(colorizer outputColorizer, value int, color ansiColor) tableCell {
	text := strconv.Itoa(value)
	if value == 0 {
		return plainCell(text)
	}
	return colorizer.cell(text, color)
}

func controllerAvailabilityColor(available, desired int32) ansiColor {
	if desired > 0 && available >= desired {
		return ansiGreenColor
	}
	if desired > 0 && available > 0 {
		return ansiYellowColor
	}
	return ansiRedColor
}

func readyPodsColor(ready int, desired int32) ansiColor {
	if desired > 0 && ready >= int(desired) {
		return ansiGreenColor
	}
	if desired > 0 && ready > 0 {
		return ansiYellowColor
	}
	return ansiRedColor
}

func colorForState(state string) ansiColor {
	switch strings.ToLower(state) {
	case "healthy", stateCurrent, stateObserved, stateMatch:
		return ansiGreenColor
	case "unobserved", "stale", "pending", "degraded", "warning", "retiring":
		return ansiYellowColor
	case "unhealthy", stateInvalid, stateMissing, "uid mismatch", stateMismatch, stateNXDomain, stateUnsupported, "error", "unavailable", "failed":
		return ansiRedColor
	default:
		return ansiNone
	}
}

func colorForDNSState(state string) ansiColor {
	switch strings.ToLower(state) {
	case stateMatch:
		return ansiGreenColor
	case stateMismatch, stateNXDomain, stateUnsupported, "error", "failed":
		return ansiRedColor
	default:
		return ansiNone
	}
}

func foundState(found, uidMatches bool) string {
	if !found {
		return stateMissing
	}
	if !uidMatches {
		return "UID mismatch"
	}
	return stateCurrent
}

func presentState(found bool) string {
	if found {
		return stateCurrent
	}
	return stateMissing
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
