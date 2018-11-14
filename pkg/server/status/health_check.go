// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package status

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/status/statuspb"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

type threshold struct {
	gauge bool
	min   int64
}

var (
	counterZero = threshold{}
	gaugeZero   = threshold{gauge: true}
)

// TODO(tschottdorf): I think we should just export the metric metadata from
// their respective packages and reference them here, instead of the
// duplication. It also seems useful to specify the metric type in the metadata
// so that we don't have to "guess" whether it's a gauge or counter. However
// there's some massaging for latency histograms that happens in NodeStatus,
// so the logic likely has to be moved up a bit. A thread not worth pulling on
// at the moment, I suppose.
//
// TODO(tschottdorf): there are some metrics that could be used in alerts but
// need special treatment. For example, we want to alert when compactions are
// queued but not processed over long periods of time, or when queues have a
// large backlog but show no sign of processing times.
var trackedMetrics = map[string]threshold{
	// Gauges.
	"ranges.unavailable":          gaugeZero,
	"ranges.underreplicated":      gaugeZero,
	"requests.backpressure.split": gaugeZero,
	"requests.slow.commandqueue":  gaugeZero,
	"requests.slow.lease":         gaugeZero,
	"requests.slow.raft":          gaugeZero,
	"sys.goroutines":              {gauge: true, min: 5000},

	// Latencies (which are really histograms, but we get to see a fixed number
	// of percentiles as gauges)
	"raft.process.logcommit.latency-90": {gauge: true, min: int64(500 * time.Millisecond)},
	"round-trip-latency-p90":            {gauge: true, min: int64(time.Second)},

	// Counters.

	"liveness.heartbeatfailures": counterZero,
	"timeseries.write.errors":    counterZero,

	// Queue processing errors. This might be too aggressive. For example, if the
	// replicate queue is waiting for a split, does that generate an error? If so,
	// is that worth alerting about? We might need severities here at some point
	// or some other way to guard against "blips".
	"compactor.compactions.failure":       counterZero,
	"queue.replicagc.process.failure":     counterZero,
	"queue.raftlog.process.failure":       counterZero,
	"queue.gc.process.failure":            counterZero,
	"queue.split.process.failure":         counterZero,
	"queue.replicate.process.failure":     counterZero,
	"queue.raftsnapshot.process.failure":  counterZero,
	"queue.tsmaintenance.process.failure": counterZero,
	"queue.consistency.process.failure":   counterZero,
}

type metricsMap map[roachpb.StoreID]map[string]float64

// update takes a populated metrics map and extracts the tracked metrics. Gauges
// are returned verbatim, while for counters the diff between the last seen
// value is returned. Only nonzero values are reported and the seen (non-relative)
// values are persisted for the next call.
func (d metricsMap) update(tracked map[string]threshold, m metricsMap) metricsMap {
	out := metricsMap{}
	for storeID := range m {
		for name, threshold := range tracked {
			val, ok := m[storeID][name]
			if !ok {
				continue
			}

			if !threshold.gauge {
				prevVal, havePrev := d[storeID][name]
				if d[storeID] == nil {
					d[storeID] = map[string]float64{}
				}
				d[storeID][name] = val
				if havePrev {
					val -= prevVal
				} else {
					// Can't report the first time around if we don't know the previous
					// value of the counter.
					val = 0
				}
			}

			if val > float64(threshold.min) {
				if out[storeID] == nil {
					out[storeID] = map[string]float64{}
				}
				out[storeID][name] = val
			}
		}
	}
	return out
}

// A HealthChecker inspects the node metrics and optionally a NodeStatus for
// anomalous conditions that the operator should be alerted to.
type HealthChecker struct {
	mu struct {
		syncutil.Mutex
		metricsMap // - the last recorded values of all counters
	}
	tracked map[string]threshold
}

// NewHealthChecker creates a new health checker that emits alerts whenever the
// given metrics are nonzero. Setting the boolean map value indicates a gauge
// (in which case it is reported whenever it's nonzero); otherwise the metric is
// treated as a counter and reports whenever it is incremented between
// consecutive calls of `CheckHealth`.
func NewHealthChecker(trackedMetrics map[string]threshold) *HealthChecker {
	h := &HealthChecker{tracked: trackedMetrics}
	h.mu.metricsMap = metricsMap{}
	return h
}

// CheckHealth performs a (cheap) health check.
func (h *HealthChecker) CheckHealth(
	ctx context.Context, nodeStatus statuspb.NodeStatus,
) statuspb.HealthCheckResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Gauges that trigger alerts when nonzero.
	var alerts []statuspb.HealthAlert

	m := map[roachpb.StoreID]map[string]float64{
		0: nodeStatus.Metrics,
	}
	for _, storeStatus := range nodeStatus.StoreStatuses {
		m[storeStatus.Desc.StoreID] = storeStatus.Metrics
	}

	diffs := h.mu.update(h.tracked, m)

	for storeID, storeDiff := range diffs {
		for name, value := range storeDiff {
			alerts = append(alerts, statuspb.HealthAlert{
				StoreID:     storeID,
				Category:    statuspb.HealthAlert_METRICS,
				Description: name,
				Value:       value,
			})
		}
	}

	return statuspb.HealthCheckResult{Alerts: alerts}
}
