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
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestLookupDNSUsesTCPFallbackAndComparesExactSet(t *testing.T) {
	t.Parallel()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := packet.LocalAddr().(*net.UDPAddr).Port
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", dnsPort(port)))
	if err != nil {
		if closeErr := packet.Close(); closeErr != nil {
			t.Errorf("close UDP listener: %v", closeErr)
		}
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		if writer.RemoteAddr().Network() == "udp" {
			response.Truncated = true
		} else {
			response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.1")}}
		}
		_ = writer.WriteMsg(response)
	})
	udpServer, tcpServer := &dns.Server{PacketConn: packet, Handler: handler}, &dns.Server{Listener: listener, Handler: handler}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := LookupDNS(ctx, packet.LocalAddr().String(), "app.example.com", "A", []string{"192.0.2.1"})
	if result.State != "match" || len(result.Answers) != 1 {
		t.Fatalf("lookup = %#v", result)
	}
}

func TestLookupDNSRejectsUnsupportedAndInvalidServers(t *testing.T) {
	t.Parallel()
	if result := LookupDNS(context.Background(), "bad:address:value", "app.example.com", "A", nil); result.State != "error" {
		t.Fatalf("invalid server result = %#v", result)
	}
	if result := LookupDNS(context.Background(), "127.0.0.1", "app.example.com", "TXT", nil); result.State != "unsupported" {
		t.Fatalf("unsupported result = %#v", result)
	}
}

func dnsPort(port int) string {
	return fmt.Sprintf("%d", port)
}
