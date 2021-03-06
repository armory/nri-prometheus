// Package integration ..
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package integration

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/newrelic/newrelic-telemetry-sdk-go/cumulative"
	"github.com/newrelic/newrelic-telemetry-sdk-go/telemetry"
	"github.com/newrelic/nri-prometheus/internal/histogram"
	"github.com/pkg/errors"
	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
)

const (
	defaultDeltaExpirationAge           = 5 * time.Minute
	defaultDeltaExpirationCheckInterval = 5 * time.Minute
)

// Emitter is an interface representing the ability to emit metrics.
type Emitter interface {
	Name() string
	Emit([]Metric) error
}

// TelemetryEmitter emits metrics using the go-telemetry-sdk.
type TelemetryEmitter struct {
	name            string
	percentiles     []float64
	harvester       *telemetry.Harvester
	deltaCalculator *cumulative.DeltaCalculator
}

// TelemetryEmitterConfig is the configuration required for the
// `TelemetryEmitter`
type TelemetryEmitterConfig struct {
	// Percentile values to calculate for every Prometheus metrics of histogram type.
	Percentiles []float64

	// HarvesterOpts configuration functions for the telemetry Harvester.
	HarvesterOpts []TelemetryHarvesterOpt

	// DeltaExpirationAge sets the cumulative DeltaCalculator expiration age
	// which determines how old an entry must be before it is considered for
	// expiration. Defaults to 30s.
	DeltaExpirationAge time.Duration
	// DeltaExpirationCheckInternval sets the cumulative DeltaCalculator
	// duration between checking for expirations. Defaults to 30s.
	DeltaExpirationCheckInternval time.Duration
}

// TelemetryHarvesterOpt sets configuration options for the
// `TelemetryEmitter`'s `telemetry.Harvester`.
type TelemetryHarvesterOpt = func(*telemetry.Config)

// TelemetryHarvesterWithMetricsURL sets the url to use for the metrics endpoint.
func TelemetryHarvesterWithMetricsURL(url string) TelemetryHarvesterOpt {
	return func(config *telemetry.Config) {
		config.MetricsURLOverride = url
	}
}

// TelemetryHarvesterWithHarvestPeriod sets harvest period.
func TelemetryHarvesterWithHarvestPeriod(t time.Duration) TelemetryHarvesterOpt {
	return func(config *telemetry.Config) {
		config.HarvestPeriod = t
	}
}

// TelemetryHarvesterWithLicenseKeyRoundTripper wraps the emitter
// client Transport to use the `licenseKey` instead of the `apiKey`.
//
// Other options that modify the underlying Client.Transport should be
// set before this one, because this will change the Transport type
// to licenseKeyRoundTripper.
func TelemetryHarvesterWithLicenseKeyRoundTripper(licenseKey string) TelemetryHarvesterOpt {
	return func(cfg *telemetry.Config) {
		cfg.Client.Transport = newLicenseKeyRoundTripper(
			cfg.Client.Transport,
			licenseKey,
		)
	}
}

// TelemetryHarvesterWithTLSConfig sets the TLS configuration to the
// emitter client transport.
func TelemetryHarvesterWithTLSConfig(tlsConfig *tls.Config) TelemetryHarvesterOpt {

	return func(cfg *telemetry.Config) {
		rt := cfg.Client.Transport
		if rt == nil {
			rt = http.DefaultTransport
		}

		t, ok := rt.(*http.Transport)
		if !ok {
			logrus.Warning(
				"telemetry emitter TLS configuration couldn't be set, ",
				"client transport is not an http.Transport.",
			)
			return
		}

		t = t.Clone()
		t.TLSClientConfig = tlsConfig
		cfg.Client.Transport = http.RoundTripper(t)
		return
	}
}

// TelemetryHarvesterWithProxy sets proxy configuration to the emitter
// client transport.
func TelemetryHarvesterWithProxy(proxyURL *url.URL) TelemetryHarvesterOpt {
	return func(cfg *telemetry.Config) {
		rt := cfg.Client.Transport
		if rt == nil {
			rt = http.DefaultTransport
		}

		t, ok := rt.(*http.Transport)
		if !ok {
			logrus.Warning(
				"telemetry emitter couldn't be configured with proxy, ",
				"client transport is not an http.Transport, ",
				"continuing without proxy support",
			)
			return
		}

		t = t.Clone()
		t.Proxy = http.ProxyURL(proxyURL)
		cfg.Client.Transport = http.RoundTripper(t)
		return
	}
}

// NewTelemetryEmitter returns a new TelemetryEmitter.
func NewTelemetryEmitter(cfg TelemetryEmitterConfig) (*TelemetryEmitter, error) {
	dc := cumulative.NewDeltaCalculator()

	deltaExpirationAge := defaultDeltaExpirationAge
	if cfg.DeltaExpirationAge != 0 {
		deltaExpirationAge = cfg.DeltaExpirationAge
	}
	dc.SetExpirationAge(deltaExpirationAge)
	logrus.Debugf(
		"telemetry emitter configured with delta counter expiration age: %s",
		deltaExpirationAge,
	)

	deltaExpirationCheckInterval := defaultDeltaExpirationCheckInterval
	if cfg.DeltaExpirationCheckInternval != 0 {
		deltaExpirationCheckInterval = cfg.DeltaExpirationCheckInternval
	}
	dc.SetExpirationCheckInterval(deltaExpirationCheckInterval)
	logrus.Debugf(
		"telemetry emitter configured with delta counter expiration check interval: %s",
		deltaExpirationCheckInterval,
	)

	harvester, err := telemetry.NewHarvester(cfg.HarvesterOpts...)
	if err != nil {
		return nil, errors.Wrap(err, "could not create new Harvester")
	}

	return &TelemetryEmitter{
		name:            "telemetry",
		harvester:       harvester,
		percentiles:     cfg.Percentiles,
		deltaCalculator: dc,
	}, nil
}

// Name returns the emitter name.
func (te *TelemetryEmitter) Name() string {
	return te.name
}

// Emit makes the mapping between Prometheus and NR metrics and records them
// into the NR telemetry harvester.
func (te *TelemetryEmitter) Emit(metrics []Metric) error {
	var results error

	// Record metrics at a uniform time so processing is not reflected in
	// the measurement that already took place.
	now := time.Now()
	for _, metric := range metrics {
		switch metric.metricType {
		case metricType_GAUGE:
			te.harvester.RecordMetric(telemetry.Gauge{
				Name:       metric.name,
				Attributes: metric.attributes,
				Value:      metric.value.(float64),
				Timestamp:  now,
			})
		case metricType_COUNTER:
			m, ok := te.deltaCalculator.CountMetric(
				metric.name,
				metric.attributes,
				metric.value.(float64),
				now,
			)
			if ok {
				te.harvester.RecordMetric(m)
			}
		case metricType_SUMMARY:
			if err := te.emitSummary(metric, now); err != nil {
				if results == nil {
					results = err
				} else {
					results = fmt.Errorf("%v: %w", err, results)
				}
			}
		case metricType_HISTOGRAM:
			if err := te.emitHistogram(metric, now); err != nil {
				if results == nil {
					results = err
				} else {
					results = fmt.Errorf("%v: %w", err, results)
				}
			}
		default:
			if err := fmt.Errorf("unknown metric type %q", metric.metricType); err != nil {
				if results == nil {
					results = err
				} else {
					results = fmt.Errorf("%v: %w", err, results)
				}
			}
		}
	}
	return results
}

// emitSummary sends all quantiles included with the summary as percentiles to New Relic.
//
// Related specification:
// https://github.com/newrelic/newrelic-exporter-specs/blob/master/Guidelines.md#percentiles
func (te *TelemetryEmitter) emitSummary(metric Metric, timestamp time.Time) error {
	summary, ok := metric.value.(*dto.Summary)
	if !ok {
		return fmt.Errorf("unknown summary metric type for %q: %T", metric.name, metric.value)
	}

	var results error
	metricName := metric.name + ".percentiles"
	quantiles := summary.GetQuantile()
	for _, q := range quantiles {
		// translate to percentiles
		p := q.GetQuantile() * 100.0
		if p < 0.0 || p > 100.0 {
			err := fmt.Errorf("invalid percentile `%g` for %s: must be in range [0.0, 100.0]", p, metric.name)
			if results == nil {
				results = err
			} else {
				results = fmt.Errorf("%v: %w", err, results)
			}
			continue
		}

		percentileAttrs := copyAttrs(metric.attributes)
		percentileAttrs["percentile"] = p
		te.harvester.RecordMetric(telemetry.Gauge{
			Name:       metricName,
			Attributes: percentileAttrs,
			Value:      q.GetValue(),
			Timestamp:  timestamp,
		})
	}
	return results
}

// emitHistogram sends histogram data and curated percentiles to New Relic.
//
// Related specification:
// https://github.com/newrelic/newrelic-exporter-specs/blob/master/Guidelines.md#histograms
func (te *TelemetryEmitter) emitHistogram(metric Metric, timestamp time.Time) error {
	hist, ok := metric.value.(*dto.Histogram)
	if !ok {
		return fmt.Errorf("unknown histogram metric type for %q: %T", metric.name, metric.value)
	}

	if m, ok := te.deltaCalculator.CountMetric(metric.name+".sum", metric.attributes, hist.GetSampleSum(), timestamp); ok {
		te.harvester.RecordMetric(m)
	}

	metricName := metric.name + ".buckets"
	buckets := make(histogram.Buckets, 0, len(hist.Bucket))
	for _, b := range hist.GetBucket() {
		upperBound := b.GetUpperBound()
		count := float64(b.GetCumulativeCount())
		if !math.IsInf(upperBound, 1) {
			bucketAttrs := copyAttrs(metric.attributes)
			bucketAttrs["histogram.bucket.upperBound"] = upperBound
			if m, ok := te.deltaCalculator.CountMetric(metricName, bucketAttrs, count, timestamp); ok {
				te.harvester.RecordMetric(m)
			}
		}
		buckets = append(
			buckets,
			histogram.Bucket{
				UpperBound: upperBound,
				Count:      count,
			},
		)
	}

	var results error
	metricName = metric.name + ".percentiles"
	for _, p := range te.percentiles {
		v, err := histogram.Percentile(p, buckets)
		if err != nil {
			if results == nil {
				results = err
			} else {
				results = fmt.Errorf("%v: %w", err, results)
			}
			continue
		}

		percentileAttrs := copyAttrs(metric.attributes)
		percentileAttrs["percentile"] = p
		te.harvester.RecordMetric(telemetry.Gauge{
			Name:       metricName,
			Attributes: percentileAttrs,
			Value:      v,
			Timestamp:  timestamp,
		})
	}

	return results
}

// copyAttrs returns a (shallow) copy of the passed attrs.
func copyAttrs(attrs map[string]interface{}) map[string]interface{} {
	duplicate := make(map[string]interface{}, len(attrs))
	for k, v := range attrs {
		duplicate[k] = v
	}
	return duplicate
}

// StdoutEmitter emits metrics to stdout.
type StdoutEmitter struct {
	name string
}

// NewStdoutEmitter returns a NewStdoutEmitter.
func NewStdoutEmitter() *StdoutEmitter {
	return &StdoutEmitter{
		name: "stdout",
	}
}

// Name is the StdoutEmitter name.
func (se *StdoutEmitter) Name() string {
	return se.name
}

// Emit prints the metrics into stdout.
func (se *StdoutEmitter) Emit(metrics []Metric) error {
	b, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
