/*
 *
 * * OCI Native Ingress Controller
 * *
 * * Copyright (c) 2026 Oracle America, Inc. and its affiliates.
 * * Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 *
 */
package metric

import (
	"net/http"
	"strconv"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/prometheus/client_golang/prometheus"
)

// OCIMetrics counts HTTP attempts across all OCI service clients, including retries.
type OCIMetrics struct {
	calls  *prometheus.CounterVec
	errors *prometheus.CounterVec
}

func NewOCIMetrics(reg prometheus.Registerer) *OCIMetrics {
	m := &OCIMetrics{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oci_api_calls_total",
			Help: "Number of HTTP requests to OCI service APIs, including retries.",
		}, []string{"oci_service"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oci_api_errors_total",
			Help: "Number of OCI service HTTP requests with transport errors or status codes >= 400, including retries. Status is the HTTP response code, or transport_error when no response was received.",
		}, []string{"oci_service", "status"}),
	}
	reg.MustRegister(m.calls, m.errors)
	return m
}

// Wrap delegates to the existing dispatcher to preserve its transport configuration.
// A nil receiver leaves the dispatcher unchanged for clients without metrics.
func (m *OCIMetrics) Wrap(dispatcher common.HTTPRequestDispatcher, service string) common.HTTPRequestDispatcher {
	if m == nil {
		return dispatcher
	}
	errors := m.errors.MustCurryWith(prometheus.Labels{"oci_service": service})
	// Publish a zero baseline before any requests fail. Other statuses are
	// created on demand; wrapping a replacement client preserves existing totals.
	for _, status := range []string{"400", "401", "403", "404", "409", "412", "429", "500", "502", "503", "504", "transport_error"} {
		errors.WithLabelValues(status)
	}
	return &ociMetricsClient{
		dispatcher: dispatcher,
		calls:      m.calls.WithLabelValues(service),
		errors:     errors,
	}
}

type ociMetricsClient struct {
	dispatcher common.HTTPRequestDispatcher
	calls      prometheus.Counter
	errors     *prometheus.CounterVec
}

func (c *ociMetricsClient) Do(req *http.Request) (*http.Response, error) {
	c.calls.Inc()
	resp, err := c.dispatcher.Do(req)
	if err != nil || (resp != nil && resp.StatusCode >= http.StatusBadRequest) {
		status := "transport_error"
		if resp != nil {
			status = strconv.Itoa(resp.StatusCode)
		}
		c.errors.WithLabelValues(status).Inc()
	}
	return resp, err
}
