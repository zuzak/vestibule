package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type metrics struct {
	inbound          *prometheus.CounterVec
	upstream         *prometheus.CounterVec
	minIntervalHits  *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	factory := promauto.With(reg)
	return &metrics{
		inbound: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "vestibule_requests_total",
			Help: "Total inbound requests to vestibule.",
		}, []string{"upstream", "endpoint", "status"}),
		upstream: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "vestibule_upstream_requests_total",
			Help: "Total outbound requests made by vestibule to upstream APIs.",
		}, []string{"upstream", "endpoint", "status"}),
		minIntervalHits: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "vestibule_min_interval_served_total",
			Help: "Inbound requests served from the min_interval floor cache.",
		}, []string{"upstream", "endpoint"}),
		upstreamDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vestibule_upstream_request_duration_seconds",
			Help:    "Duration of outbound upstream requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"upstream", "endpoint"}),
	}
}
