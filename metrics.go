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
// along with this program.  If not, see <http://www.gnu.org/licenses/>.package main

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

	for _, c := range globalConnStats {
		if c == nil {
			continue
		}

		// total calls per node
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("", "requests", "total"),
				"Total number of calls in current MinIO server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.totalCalls.Load()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("", "errors", "total"),
				"Total number of failed calls in current MinIO server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.totalFailedCalls.Load()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("", "rx", "bytes_total"),
				"Total number of bytes received by current MinIO server instance",
				[]string{"endpoint"}, nil),
			prometheus.CounterValue,
			float64(c.getTotalInputBytes()),
			c.endpoint,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName("", "tx", "bytes_total"),
				"Total number of bytes sent by current MinIO server instance",
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
	totalDowntime    atomic.Duration
	minLatency       atomic.Duration
	maxLatency       atomic.Duration
	status           atomic.String
}

// Increase total input bytes
func (s *ConnStats) incInputBytes(n int64) {
	s.totalInputBytes.Add(uint64(n))
}

// Increase total output bytes
func (s *ConnStats) incOutputBytes(n int64) {
	s.totalOutputBytes.Add(uint64(n))
}

// Return total input bytes
func (s *ConnStats) getTotalInputBytes() uint64 {
	return s.totalInputBytes.Load()
}
func (s *ConnStats) incTotalCalls() {
	s.totalCalls.Add(1)
}
func (s *ConnStats) incTotalCallFailures() {
	s.totalFailedCalls.Add(1)
}

// set backend status
func (s *ConnStats) setStatus(st string) {
	s.status.Store(st)
}

// Increase total downtime
func (s *ConnStats) incTotalDowntime(t time.Duration) {
	s.totalDowntime.Add(time.Duration(t))
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
