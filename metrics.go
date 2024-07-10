// Copyright (c) 2020 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/atomic"
)

func init() {
	prometheus.MustRegister(newSidekickCollector())
}

// newSidekickCollector describes the collector
// and returns reference of sidekickCollector
// It creates the Prometheus Description which is used
// to define metric and  help string
func newSidekickCollector() *sidekickCollector {
	return &sidekickCollector{
		desc: prometheus.NewDesc("sidekick_stats", "Statistics exposed by Sidekick loadbalancer", nil, nil),
	}
}

// sidekickCollector is the Custom Collector
type sidekickCollector struct {
	desc *prometheus.Desc
}

// Describe sends the super-set of all possible descriptors of metrics
func (c *sidekickCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect is called by the Prometheus registry when collecting metrics.
func (c *sidekickCollector) Collect(ch chan<- prometheus.Metric) {
	// automatically read the global stats
	// Read/Write Lock is not required here
	globalConnStatsRWMutex.RLock()
	connStats := globalConnStats
	globalConnStatsRWMutex.RUnlock()
	for _, c := range connStats {
		if c == nil {
			continue
		}

		// total calls per node
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("sidekick", "requests", "total"),
				"Total number of calls in current SideKick server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.totalCalls.Load()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("sidekick", "errors", "total"),
				"Total number of failed calls in current SideKick server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.totalFailedCalls.Load()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("sidekick", "rx", "bytes_total"),
				"Total number of bytes received by current SideKick server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.getTotalInputBytes()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("sidekick", "tx", "bytes_total"),
				"Total number of bytes sent by current SideKick server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.getTotalOutputBytes()),
			c.endpoint,
		)
	}
}

func metricsHandler() (http.Handler, error) {
	registry := prometheus.NewRegistry()

	err := registry.Register(newSidekickCollector())
	if err != nil {
		return nil, err
	}

	gatherers := prometheus.Gatherers{
		prometheus.DefaultGatherer,
		registry,
	}
	// Delegate http serving to Prometheus client library, which will call collector.Collect.
	return promhttp.InstrumentMetricHandler(
		registry,
		promhttp.HandlerFor(gatherers,
			promhttp.HandlerOpts{
				ErrorHandling: promhttp.ContinueOnError,
			}),
	), nil
}

// ConnStats - statistics on backend
type ConnStats struct {
	endpoint         string
	totalInputBytes  atomic.Uint64
	totalOutputBytes atomic.Uint64
	totalCalls       atomic.Uint64
	totalFailedCalls atomic.Uint64
	minLatency       atomic.Duration
	maxLatency       atomic.Duration
}

// Store current total input bytes
func (s *ConnStats) setInputBytes(n int64) {
	s.totalInputBytes.Store(uint64(n))
}

// Store current total output bytes
func (s *ConnStats) setOutputBytes(n int64) {
	s.totalOutputBytes.Store(uint64(n))
}

// Return total input bytes
func (s *ConnStats) getTotalInputBytes() uint64 {
	return s.totalInputBytes.Load()
}

// Store current total calls
func (s *ConnStats) setTotalCalls(n int64) {
	s.totalCalls.Store(uint64(n))
}

// Store current total call failures
func (s *ConnStats) setTotalCallFailures(n int64) {
	s.totalFailedCalls.Store(uint64(n))
}

// set min latency
func (s *ConnStats) setMinLatency(mn time.Duration) {
	s.minLatency.Store(mn)
}

// set max latency
func (s *ConnStats) setMaxLatency(mx time.Duration) {
	s.maxLatency.Store(mx)
}

// Return total output bytes
func (s *ConnStats) getTotalOutputBytes() uint64 {
	return s.totalOutputBytes.Load()
}

// Prepare new ConnStats structure
func newConnStats(endpoint string) *ConnStats {
	return &ConnStats{endpoint: endpoint}
}
