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
	"net"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestLookupDNSUsesTCPFallbackAndComparesExactSet(t *testing.T) {
	t.Parallel()
	var networks []string
	clients := dnsClientFactory(func(network string) dnsClient {
		networks = append(networks, network)
		return fakeDNSClient{network: network}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := lookupDNS(ctx, "127.0.0.1", "app.example.com", "A", []string{"192.0.2.1"}, clients)
	if result.State != "match" || len(result.Answers) != 1 {
		t.Fatalf("lookup = %#v", result)
	}
	if got, want := networks, []string{"udp", "tcp"}; !slices.Equal(got, want) {
		t.Fatalf("DNS client networks = %v, want %v", got, want)
	}
}

type fakeDNSClient struct {
	network string
}

func (f fakeDNSClient) ExchangeContext(_ context.Context, request *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	response := new(dns.Msg)
	response.SetReply(request)
	if f.network == "udp" {
		response.Truncated = true
	} else {
		response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.1")}}
	}
	return response, 0, nil
}

func TestLookupDNSRejectsUnsupportedAndInvalidServers(t *testing.T) {
	t.Parallel()
	if result := lookupDNSDefault(context.Background(), "bad:address:value", "app.example.com", "A", nil); result.State != "error" {
		t.Fatalf("invalid server result = %#v", result)
	}
	if result := lookupDNSDefault(context.Background(), "127.0.0.1", "app.example.com", "TXT", nil); result.State != "unsupported" {
		t.Fatalf("unsupported result = %#v", result)
	}
}
