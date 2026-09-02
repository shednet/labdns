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
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestColorizerModeAndEnvironment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		mode    colorMode
		output  string
		noColor string
		term    string
		tty     bool
		want    bool
	}{
		{name: "auto terminal", mode: colorAuto, output: "table", term: "xterm", tty: true, want: true},
		{name: "auto redirected", mode: colorAuto, output: "table", term: "xterm", tty: false, want: false},
		{name: "auto NO_COLOR", mode: colorAuto, output: "table", noColor: "1", term: "xterm", tty: true, want: false},
		{name: "auto empty NO_COLOR", mode: colorAuto, output: "table", noColor: "", term: "xterm", tty: true, want: true},
		{name: "auto dumb terminal", mode: colorAuto, output: "table", term: "dumb", tty: true, want: false},
		{name: "always redirected", mode: colorAlways, output: "table", term: "dumb", tty: false, want: true},
		{name: "always NO_COLOR", mode: colorAlways, output: "table", noColor: "1", term: "dumb", tty: false, want: true},
		{name: "never terminal", mode: colorNever, output: "table", term: "xterm", tty: true, want: false},
		{name: "json always plain", mode: colorAlways, output: "json", term: "xterm", tty: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := &commandOptions{
				color:  test.mode,
				output: test.output,
				getenv: func(name string) (string, bool) {
					switch name {
					case "NO_COLOR":
						return test.noColor, test.noColor != "" || test.name == "auto empty NO_COLOR"
					case "TERM":
						return test.term, test.term != ""
					default:
						return "", false
					}
				},
				isTerminal: func(io.Writer) bool { return test.tty },
			}
			command := &cobra.Command{Use: "test"}
			command.SetOut(&bytes.Buffer{})
			if got := options.colorizer(command).enabled; got != test.want {
				t.Fatalf("color enabled = %t, want %t", got, test.want)
			}
		})
	}
}

func TestInvalidColorIsRejectedBeforeCommandRun(t *testing.T) {
	t.Parallel()
	command := NewCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
	command.SetArgs([]string{"list", "--color=invalid"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "invalid color mode") {
		t.Fatalf("Execute error = %v", err)
	}
}

func TestCommandMetadataOutputIsPlain(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "help", args: []string{"--help"}},
		{name: "version", args: []string{"--version"}},
		{name: "unknown command", args: []string{"unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := NewCommand("test", &stdout, &stderr)
			command.SetArgs(test.args)
			err := command.Execute()
			if err != nil && test.name != "unknown command" {
				t.Fatalf("Execute error = %v", err)
			}
			if strings.Contains(stdout.String()+stderr.String(), "\x1b") {
				t.Fatalf("metadata output contains ANSI: %q", stdout.String()+stderr.String())
			}
			if err != nil && strings.Contains(err.Error(), "\x1b") {
				t.Fatalf("command error contains ANSI: %q", err)
			}
		})
	}
}

func TestColoredTablesKeepPlainLayout(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	list := RecordList{Items: []Record{{
		DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 60,
		Provider: "www", Source: SourceRef{Kind: "Ingress", Namespace: "app", Name: "web"},
		DNSEndpoint: ObjectRef{Namespace: "app", Name: "generated"}, ExternalDNSState: "observed",
		Retiring: []RetiringTarget{{Target: "192.0.2.2", Deadline: deadline}},
	}}}
	details := RecordDetails{Items: []RecordDetail{{
		Record: list.Items[0], Provider: ProviderDetail{Found: true}, Source: SourceDetail{Found: true, UIDMatches: true},
		DNS: &DNSLookup{Answers: []string{"192.0.2.1"}, Server: "10.0.0.53", State: "match"},
	}}}
	status := Status{
		Overall: "healthy", Prerequisites: []Prerequisite{{Name: "DNSProvider CRD", Available: true}},
		Controllers: []ControllerStatus{{Namespace: "labdns-system", Name: "labdns", Desired: 1, Available: 1, ReadyPods: 1}},
		Summary:     StateSummary{Providers: 1, PublishingSources: 1, DNSEndpoints: 1, Records: 1, Observed: 1},
		Warnings:    []string{"publication delayed"},
	}
	for _, test := range []struct {
		name     string
		write    func(io.Writer) error
		writeCol func(io.Writer) error
	}{
		{name: "records", write: func(w io.Writer) error { return WriteRecords(w, list) }, writeCol: func(w io.Writer) error { return writeRecords(w, list, outputColorizer{enabled: true}) }},
		{name: "details", write: func(w io.Writer) error { return WriteDetails(w, details) }, writeCol: func(w io.Writer) error { return writeDetails(w, details, outputColorizer{enabled: true}) }},
		{name: "status", write: func(w io.Writer) error { return WriteStatus(w, status) }, writeCol: func(w io.Writer) error { return writeStatus(w, status, outputColorizer{enabled: true}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var plain, colored bytes.Buffer
			if err := test.write(&plain); err != nil {
				t.Fatal(err)
			}
			if err := test.writeCol(&colored); err != nil {
				t.Fatal(err)
			}
			if got := stripANSI(colored.String()); got != plain.String() {
				t.Fatalf("stripped colored output = %q, plain output = %q", got, plain.String())
			}
			if !strings.Contains(colored.String(), ansiGreen) {
				t.Fatalf("colored output has no green semantic value: %q", colored.String())
			}
			assertImmediateResets(t, colored.String())
		})
	}
}

func TestTableTrailingCellsDoNotWidenAlignedColumns(t *testing.T) {
	t.Parallel()
	rows := []tableRow{
		{plainCell("first"), plainCell("middle"), plainCell("end")},
		{plainCell("x"), plainCell("a long trailing value")},
	}
	var output bytes.Buffer
	if err := writeTable(&output, rows, outputColorizer{}); err != nil {
		t.Fatal(err)
	}
	want := "first  middle  end\n" + "x      a long trailing value\n"
	if output.String() != want {
		t.Fatalf("table = %q, want %q", output.String(), want)
	}
}

func TestTableAlignsNestedContiguousColumnBlocks(t *testing.T) {
	t.Parallel()
	rows := []tableRow{
		{plainCell("a"), plainCell("BBBB"), plainCell("x")},
		{plainCell("long"), plainCell("tail")},
		{plainCell("a"), plainCell("c"), plainCell("z")},
	}
	var output bytes.Buffer
	if err := writeTable(&output, rows, outputColorizer{}); err != nil {
		t.Fatal(err)
	}
	want := "a     BBBB  x\n" + "long  tail\n" + "a     c  z\n"
	if output.String() != want {
		t.Fatalf("table = %q, want %q", output.String(), want)
	}
}

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) {
	return max(0, len(value)-1), nil
}

func TestTablePropagatesShortWrites(t *testing.T) {
	t.Parallel()
	err := writeTable(shortWriter{}, []tableRow{{plainCell("state"), outputColorizer{enabled: true}.state("healthy")}}, outputColorizer{enabled: true})
	if err != io.ErrShortWrite {
		t.Fatalf("writeTable error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestPlainStatusLayoutRegression(t *testing.T) {
	t.Parallel()
	status := Status{
		Overall:       "healthy",
		Prerequisites: []Prerequisite{{Name: "DNSProvider CRD", Available: true}},
		Controllers:   []ControllerStatus{{Namespace: "labdns-system", Name: "labdns", Desired: 1, Available: 1, ReadyPods: 1}},
		Summary:       StateSummary{Providers: 1, PublishingSources: 1, DNSEndpoints: 1, Records: 1, Observed: 1},
	}
	var output bytes.Buffer
	if err := WriteStatus(&output, status); err != nil {
		t.Fatal(err)
	}
	want := "OVERALL  HEALTHY\n" +
		"\n" +
		"PREREQUISITE     AVAILABLE  ERROR\n" +
		"DNSProvider CRD  true       \n" +
		"\n" +
		"CONTROLLER            AVAILABLE  READY PODS  GATEWAY API\n" +
		"labdns-system/labdns  1/1        1           false\n" +
		"\n" +
		"PROVIDERS  SOURCES  DNSENDPOINTS  RECORDS  PENDING  OBSERVED  STALE  INVALID\n" +
		"1          1        1             1        0        1         0      0\n"
	if output.String() != want {
		t.Fatalf("status output = %q, want %q", output.String(), want)
	}
}

func TestSemanticLocationsAreColoredAndIdentifiersRemainPlain(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	details := RecordDetails{Items: []RecordDetail{{
		Record: Record{
			DNSName: "app.example.com", Provider: "www", Source: SourceRef{Kind: "Ingress", Namespace: "app", Name: "web"},
			DNSEndpoint: ObjectRef{Namespace: "app", Name: "generated"}, ExternalDNSState: "invalid",
			Retiring: []RetiringTarget{{Target: "192.0.2.1", Deadline: deadline}}, LifecycleError: "bad lifecycle",
		},
		Provider: ProviderDetail{Found: false}, Source: SourceDetail{Found: false},
		DNS: &DNSLookup{Server: "10.0.0.53", State: "error", Error: "lookup failed"},
	}}}
	status := Status{
		Overall: "unhealthy", Prerequisites: []Prerequisite{{Name: "DNSProvider CRD", Available: false, Error: "not installed"}},
		Controllers: []ControllerStatus{{Namespace: "labdns-system", Name: "labdns", Desired: 2, Available: 1, ReadyPods: 1}},
		Summary:     StateSummary{PendingTargets: 1, Observed: 1, Stale: 1, Invalid: 1},
		Warnings:    []string{"publication delayed"},
	}
	var output bytes.Buffer
	if err := writeDetails(&output, details, outputColorizer{enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := writeStatus(&output, status, outputColorizer{enabled: true}); err != nil {
		t.Fatal(err)
	}
	value := output.String()
	for _, span := range []string{
		ansiRed + "invalid" + ansiReset,
		ansiRed + "bad lifecycle" + ansiReset,
		ansiRed + "lookup failed" + ansiReset,
		ansiRed + "false" + ansiReset,
		ansiRed + "not installed" + ansiReset,
		ansiYellow + "1" + ansiReset,
		ansiGreen + "1" + ansiReset,
		ansiYellow + "publication delayed" + ansiReset,
	} {
		if !strings.Contains(value, span) {
			t.Errorf("colored output does not contain %q: %q", span, value)
		}
	}
	for _, ordinary := range []string{"app.example.com", "www", "Ingress/app/web", "app/generated", "192.0.2.1 at 2026-09-02T00:00:00Z", "10.0.0.53"} {
		if strings.Contains(value, ansiGreen+ordinary) || strings.Contains(value, ansiYellow+ordinary) || strings.Contains(value, ansiRed+ordinary) {
			t.Errorf("ordinary value %q is colored: %q", ordinary, value)
		}
	}
}

func TestColorStateMapping(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		state string
		want  ansiColor
	}{
		{"healthy", ansiGreenColor}, {"current", ansiGreenColor}, {"observed", ansiGreenColor}, {"match", ansiGreenColor},
		{"unobserved", ansiYellowColor}, {"stale", ansiYellowColor}, {"pending", ansiYellowColor}, {"degraded", ansiYellowColor}, {"warning", ansiYellowColor}, {"retiring", ansiYellowColor},
		{"unhealthy", ansiRedColor}, {"invalid", ansiRedColor}, {"missing", ansiRedColor}, {"UID mismatch", ansiRedColor}, {"mismatch", ansiRedColor}, {"nxdomain", ansiRedColor}, {"unsupported", ansiRedColor}, {"error", ansiRedColor}, {"unavailable", ansiRedColor},
		{"future", ansiNone},
	} {
		if got := colorForState(test.state); got != test.want {
			t.Errorf("colorForState(%q) = %d, want %d", test.state, got, test.want)
		}
	}
	for _, test := range []struct {
		state string
		want  ansiColor
	}{
		{"match", ansiGreenColor}, {"mismatch", ansiRedColor}, {"nxdomain", ansiRedColor}, {"unsupported", ansiRedColor}, {"error", ansiRedColor}, {"other", ansiNone},
	} {
		if got := colorForDNSState(test.state); got != test.want {
			t.Errorf("colorForDNSState(%q) = %d, want %d", test.state, got, test.want)
		}
	}
}

func TestStatusAvailabilityColorMapping(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		available, desired int32
		want               ansiColor
	}{
		{1, 1, ansiGreenColor}, {2, 1, ansiGreenColor}, {0, 1, ansiRedColor}, {1, 2, ansiYellowColor}, {0, 0, ansiRedColor},
	} {
		if got := controllerAvailabilityColor(test.available, test.desired); got != test.want {
			t.Errorf("controllerAvailabilityColor(%d, %d) = %d, want %d", test.available, test.desired, got, test.want)
		}
	}
	for _, test := range []struct {
		ready   int
		desired int32
		want    ansiColor
	}{
		{1, 1, ansiGreenColor}, {2, 1, ansiGreenColor}, {1, 2, ansiYellowColor}, {0, 1, ansiRedColor}, {1, 0, ansiRedColor},
	} {
		if got := readyPodsColor(test.ready, test.desired); got != test.want {
			t.Errorf("readyPodsColor(%d, %d) = %d, want %d", test.ready, test.desired, got, test.want)
		}
	}
}

func TestJSONOutputNeverContainsANSI(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	value := RecordList{Items: []Record{{DNSName: "app.example.com", ExternalDNSState: "observed"}}}
	if err := WriteJSON(&output, value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("JSON contains ANSI: %q", output.String())
	}
	var decoded RecordList
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
}

func stripANSI(value string) string {
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed, ansiReset} {
		value = strings.ReplaceAll(value, code, "")
	}
	return value
}

func assertImmediateResets(t *testing.T, value string) {
	t.Helper()
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed} {
		remaining := value
		for {
			index := strings.Index(remaining, code)
			if index < 0 {
				break
			}
			remaining = remaining[index+len(code):]
			reset := strings.Index(remaining, ansiReset)
			if reset < 0 {
				t.Fatalf("color %q has no reset in %q", code, value)
			}
			remaining = remaining[reset+len(ansiReset):]
		}
	}
}
