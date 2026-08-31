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

package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics contains operational counters fed by controller watch events. Its
// bookkeeping is never consulted when deciding DNS output.
type Metrics struct {
	mu        sync.Mutex
	sources   map[string]map[string]struct{}
	endpoints map[string]int
	duration  *prometheus.HistogramVec
	errors    *prometheus.CounterVec
	source    *prometheus.GaugeVec
	generated prometheus.Gauge
	pending   prometheus.Gauge
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		sources:   map[string]map[string]struct{}{},
		endpoints: map[string]int{},
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "labdns_reconcile_duration_seconds", Help: "Duration of labdns reconciliations.",
		}, []string{"controller"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "labdns_reconcile_errors_total", Help: "Failed labdns reconciliations.",
		}, []string{"controller"}),
		source: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "labdns_managed_sources", Help: "Enabled sources managed by labdns.",
		}, []string{"source_kind"}),
		generated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "labdns_generated_dnsendpoints", Help: "DNSEndpoints currently managed by labdns.",
		}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "labdns_pending_target_deletions", Help: "DNS targets waiting for their deletion deadline.",
		}),
	}
	registerer.MustRegister(m.duration, m.errors, m.source, m.generated, m.pending)
	return m
}

func (m *Metrics) Observe(controller string, started time.Time, err error) {
	if m == nil {
		return
	}
	m.duration.WithLabelValues(controller).Observe(time.Since(started).Seconds())
	if err != nil {
		m.errors.WithLabelValues(controller).Inc()
	}
}

func (m *Metrics) SetSource(kind, key string, managed bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.sources[kind]
	if set == nil {
		set = map[string]struct{}{}
		m.sources[kind] = set
	}
	if managed {
		set[key] = struct{}{}
	} else {
		delete(set, key)
	}
	m.source.WithLabelValues(kind).Set(float64(len(set)))
}

func (m *Metrics) SetEndpoint(key string, managed bool, pending int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if managed {
		m.endpoints[key] = pending
	} else {
		delete(m.endpoints, key)
	}
	total := 0
	for _, count := range m.endpoints {
		total += count
	}
	m.generated.Set(float64(len(m.endpoints)))
	m.pending.Set(float64(total))
}
