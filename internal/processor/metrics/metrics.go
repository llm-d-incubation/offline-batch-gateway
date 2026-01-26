/*
Copyright 2026 The llm-d Authors

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

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// labels definition
const (
	// result labels
	ResultSuccess = "success"
	ResultFailed  = "failed"

	// reason lables
	ReasonUnknown     = "unknown"
	ReasonUserError   = "user_error"   // method, request validation failed.. etc.,
	ReasonSystemError = "system_error" // SLO failed, system error.. etc.,

	// size bucket labels
	Bucket100   = "100"   // less than 100 lines
	Bucket1000  = "1000"  // less than 1000 lines
	Bucket10000 = "10000" // less than 10000 lines
	Bucket30000 = "30000" // less than 30000 lines
	BucketLarge = "large" // more than 30000 lines
)

func GetSizeBucket(totalLines int) string {
	switch {
	case totalLines < 100:
		return Bucket100
	case totalLines < 1000:
		return Bucket1000
	case totalLines < 10000:
		return Bucket10000
	case totalLines < 30000:
		return Bucket30000
	default:
		return BucketLarge
	}
}

var (
	// number of jobs processed so far
	jobsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_processed_total",
			Help: "Total number of jobs processed",
		}, []string{"result", "reason"},
	)

	// duration of job processing
	jobProcessingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "job_processing_duration_seconds",
			Help: "Duration of job processing in seconds",
			// Buckets -
			// Bucket 1: ~ 0.1s
			// Bucket 2: ~ 0.2s
			// Bucket 3: ~ 0.4s
			// Bucket 4: ~ 0.8s
			// ...
			// Bucket 13: ~ 409.6s (approx. 6.8m)
			// Bucket 14: ~ 819.2s (approx. 13.6m)
			// Bucket 15: ~ 1838.4  (approx. 27.3m)
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 15),
		}, []string{"tenantID", "size_bucket"},
	)

	// current number of active workers
	activeWorkers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "active_workers",
			Help: "Current number of active workers processing jobs",
		},
	)

	// errors by model
	jobErrorsModelTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "job_errors_by_model_total",
			Help: "Total number of job processing errors by model",
		},
		[]string{"model"},
	)
)

func init() {
	prometheus.MustRegister(jobsProcessed)
	prometheus.MustRegister(jobProcessingDuration)
	prometheus.MustRegister(activeWorkers)
	prometheus.MustRegister(jobErrorsModelTotal)
}

// Recorder funcs

// RecordJobProcessed increments the total processed jobs count.
func RecordJobProcessed(result string, reason string) {
	jobsProcessed.WithLabelValues(result, reason).Inc()
}

// RecordJobDuration observes the time taken to process a job.
func RecordJobDuration(duration time.Duration, tenantID string, sizeBucket string) {
	jobProcessingDuration.WithLabelValues(tenantID, sizeBucket).Observe(duration.Seconds())
}

// IncActiveWorkers increments the gauge for active workers.
func IncActiveWorkers() {
	activeWorkers.Inc()
}

// DecActiveWorkers decrements the gauge for active workers.
func DecActiveWorkers() {
	activeWorkers.Dec()
}

// RecordJobError increments the error count for a specific model.
func RecordJobError(model string) {
	jobErrorsModelTotal.WithLabelValues(model).Inc()
}
