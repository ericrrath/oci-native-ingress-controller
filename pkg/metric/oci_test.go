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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

type dispatcherFunc func(*http.Request) (*http.Response, error)

func (f dispatcherFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestOCIMetricsPublishesZeroBeforeRequests(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewOCIMetrics(reg)
	dispatcher := dispatcherFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("initializing counters must not send requests")
		return nil, nil
	})
	for _, service := range []string{"lb", "certificates"} {
		metrics.Wrap(dispatcher, service)
		assertOCIScrape(t, reg, "oci_api_calls_total{oci_service=\""+service+"\"} 0\n")
		for _, status := range []string{"400", "401", "403", "404", "409", "412", "429", "500", "502", "503", "504", "transport_error"} {
			assertOCIScrape(t, reg, "oci_api_errors_total{oci_service=\""+service+"\",status=\""+status+"\"} 0\n")
		}
	}
}

func TestOCIMetrics(t *testing.T) {
	transportErr := errors.New("connection failed")
	for _, tt := range []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{name: "success", status: 200, want: "0"},
		{name: "no content", status: 204, want: "0"},
		{name: "redirect", status: 302, want: "0"},
		{name: "bad request", status: 400, want: "1"},
		{name: "throttled", status: 429, want: "1"},
		{name: "server error", status: 500, want: "1"},
		{name: "transport error", err: transportErr, want: "1"},
		{name: "response and error", status: 502, err: transportErr, want: "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			metrics := NewOCIMetrics(reg)
			req := httptest.NewRequest(http.MethodGet, "https://oci.example.com/", nil)
			var response *http.Response
			if tt.status != 0 {
				response = &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader("response"))}
				defer response.Body.Close()
			}
			delegated := 0
			client := metrics.Wrap(dispatcherFunc(func(got *http.Request) (*http.Response, error) {
				delegated++
				if got != req {
					t.Fatal("request changed before delegation")
				}
				return response, tt.err
			}), "lb")
			got, err := client.Do(req)
			if got != response || err != tt.err || delegated != 1 {
				t.Fatalf("dispatcher result changed: response=%v err=%v calls=%d", got, err, delegated)
			}
			assertOCIScrape(t, reg, "oci_api_calls_total{oci_service=\"lb\"} 1\n")
			if tt.want == "1" {
				status := "transport_error"
				if tt.status != 0 {
					status = strconv.Itoa(tt.status)
				}
				assertOCIScrape(t, reg, "oci_api_errors_total{oci_service=\"lb\",status=\""+status+"\"} 1\n")
			} else {
				families, err := reg.Gather()
				if err != nil {
					t.Fatal(err)
				}
				for _, family := range families {
					if family.GetName() == "oci_api_errors_total" {
						for _, sample := range family.Metric {
							if sample.Counter.GetValue() != 0 {
								t.Error("successful request incremented an error counter")
							}
						}
					}
				}
			}
		})
	}
}

func TestOCIMetricsSharedAcrossClientsAndAttempts(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewOCIMetrics(reg)
	attempt := 0
	dispatcher := dispatcherFunc(func(*http.Request) (*http.Response, error) {
		attempt++
		status := http.StatusOK
		if attempt == 1 {
			status = http.StatusTooManyRequests
		}
		return &http.Response{StatusCode: status}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "https://oci.example.com/", nil)
	// A failed attempt and a successful retry both contribute to the shared counters.
	first := metrics.Wrap(dispatcher, "certificates")
	first.Do(req)
	first.Do(req)
	// Recreating a cached client must keep the existing totals.
	metrics.Wrap(dispatcher, "certificates").Do(req)
	metrics.Wrap(dispatcher, "lb").Do(req)
	assertOCIScrape(t, reg,
		"# TYPE oci_api_calls_total counter\n",
		"# TYPE oci_api_errors_total counter\n",
		"oci_api_calls_total{oci_service=\"certificates\"} 3\n",
		"oci_api_errors_total{oci_service=\"certificates\",status=\"429\"} 1\n",
		"oci_api_calls_total{oci_service=\"lb\"} 1\n",
		"oci_api_errors_total{oci_service=\"lb\",status=\"429\"} 0\n")
}

func TestOCIMetricsCountsUncommonStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewOCIMetrics(reg)
	dispatcher := dispatcherFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTeapot}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "https://oci.example.com/", nil)
	metrics.Wrap(dispatcher, "lb").Do(req)
	// Initializing another wrapper must preserve dynamically created totals too.
	metrics.Wrap(dispatcher, "lb").Do(req)
	assertOCIScrape(t, reg,
		"oci_api_calls_total{oci_service=\"lb\"} 2\n",
		"oci_api_errors_total{oci_service=\"lb\",status=\"418\"} 2\n",
		"oci_api_errors_total{oci_service=\"lb\",status=\"429\"} 0\n")
}

func assertOCIScrape(t *testing.T, reg *prometheus.Registry, expected ...string) {
	t.Helper()
	mux := http.NewServeMux()
	RegisterMetrics(reg, mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape failed: %d %s", response.Code, response.Body.String())
	}
	for _, want := range expected {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("missing %q in scrape:\n%s", want, response.Body.String())
		}
	}
}
