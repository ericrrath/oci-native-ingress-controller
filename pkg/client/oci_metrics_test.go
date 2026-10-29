/*
 *
 * * OCI Native Ingress Controller
 * *
 * * Copyright (c) 2026 Oracle America, Inc. and its affiliates.
 * * Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 *
 */
package client

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-native-ingress-controller/pkg/metric"
	"github.com/prometheus/client_golang/prometheus"
)

type metricsConfigGetter struct {
	common.ConfigurationProvider
	key string
}

func (g metricsConfigGetter) GetConfigurationProvider() (common.ConfigurationProvider, error) {
	return g.ConfigurationProvider, nil
}

func (g metricsConfigGetter) GetKey() string { return g.key }

func TestClientProviderOCIMetrics(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	getter := metricsConfigGetter{
		ConfigurationProvider: common.NewRawConfigurationProvider("tenancy", "user", "us-phoenix-1", "fingerprint", string(keyPEM), nil),
		key:                   "default",
	}
	reg := prometheus.NewRegistry()
	provider := New(nil, getter, metric.NewOCIMetrics(reg))
	first, err := provider.GetClient(getter)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := provider.GetClient(getter)
	if err != nil || cached != first {
		t.Fatalf("expected cached client, got %p, err=%v", cached, err)
	}
	// Use a canceled context to exercise real SDK dispatch without making network calls.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call := func(client *WrapperClient) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://oci.example.invalid/cluster", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetContainerEngineClient().BaseClient.Call(ctx, req)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled HTTP request, got %v", err)
		}
	}
	call(first)
	// A second authentication identity and an expired/recreated cache entry must
	// reuse the registered counters rather than register new collectors.
	otherGetter := getter
	otherGetter.key = "other-identity"
	other, err := provider.GetClient(otherGetter)
	if err != nil {
		t.Fatal(err)
	}
	call(other)
	if err := provider.Cache.Delete(first); err != nil {
		t.Fatal(err)
	}
	recreated, err := provider.GetClient(getter)
	if err != nil {
		t.Fatal(err)
	}
	call(recreated)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("expected two metric families, got %d", len(families))
	}
	for _, family := range families {
		values := map[string]float64{}
		for _, sample := range family.Metric {
			values[sample.Label[0].GetValue()] += sample.Counter.GetValue()
			if family.GetName() == "oci_api_errors_total" {
				if len(sample.Label) != 2 || sample.Label[1].GetName() != "status" {
					t.Fatalf("expected status label, got %v", sample.Label)
				}
				if sample.Counter.GetValue() != 0 && (sample.Label[0].GetValue() != "container_engine" || sample.Label[1].GetValue() != "transport_error") {
					t.Errorf("unexpected nonzero error series: %v", sample)
				}
			}
		}
		if family.GetName() == "oci_api_errors_total" && len(family.Metric) != 6*12 {
			t.Errorf("expected 12 initialized error statuses for each of six services, got %d series", len(family.Metric))
		}
		for _, service := range []string{"lb", "certificates", "certificates_management", "virtual_network", "waf", "container_engine"} {
			got, exists := values[service]
			want := float64(0)
			if service == "container_engine" {
				want = 3
			}
			if !exists || got != want {
				t.Errorf("%s{%s}: got %v (exists=%v), want %v", family.GetName(), service, got, exists, want)
			}
		}
	}
}
