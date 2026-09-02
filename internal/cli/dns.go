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
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"

	"github.com/miekg/dns"
)

const dnsStateError = "error"

func LookupDNS(ctx context.Context, server, name, recordType string, expected []string) DNSLookup {
	result := DNSLookup{Server: server, Answers: []string{}}
	address, err := dnsServerAddress(server)
	if err != nil {
		result.State, result.Error = dnsStateError, err.Error()
		return result
	}
	queryType, found := map[string]uint16{"A": dns.TypeA, "AAAA": dns.TypeAAAA}[strings.ToUpper(recordType)]
	if !found {
		result.State, result.Error = stateUnsupported, fmt.Sprintf("live lookup does not support record type %s", recordType)
		return result
	}
	message := new(dns.Msg)
	message.SetQuestion(dns.Fqdn(name), queryType)
	response, _, err := (&dns.Client{}).ExchangeContext(ctx, message, address)
	if err != nil {
		result.State, result.Error = dnsStateError, err.Error()
		return result
	}
	if response.Truncated {
		response, _, err = (&dns.Client{Net: "tcp"}).ExchangeContext(ctx, message, address)
		if err != nil {
			result.State, result.Error = dnsStateError, err.Error()
			return result
		}
	}
	if response.Rcode == dns.RcodeNameError {
		result.State = stateNXDomain
		return result
	}
	if response.Rcode != dns.RcodeSuccess {
		result.State, result.Error = dnsStateError, dns.RcodeToString[response.Rcode]
		return result
	}
	for _, answer := range response.Answer {
		switch value := answer.(type) {
		case *dns.A:
			if queryType == dns.TypeA {
				result.Answers = append(result.Answers, value.A.String())
			}
		case *dns.AAAA:
			if queryType == dns.TypeAAAA {
				result.Answers = append(result.Answers, value.AAAA.String())
			}
		}
	}
	sort.Strings(result.Answers)
	result.Answers = slices.Compact(result.Answers)
	want := slices.Clone(expected)
	sort.Strings(want)
	want = slices.Compact(want)
	if slices.Equal(result.Answers, want) {
		result.State = stateMatch
	} else {
		result.State = stateMismatch
	}
	return result
}

func dnsServerAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("DNS server must not be empty")
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf("invalid DNS server %q", value)
		}
		return net.JoinHostPort(host, port), nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	if net.ParseIP(value) != nil || !strings.Contains(value, ":") {
		return net.JoinHostPort(value, "53"), nil
	}
	return "", fmt.Errorf("invalid DNS server %q", value)
}
