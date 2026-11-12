/*
Copyright 2026 The Crossplane Authors.
*/
package config

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ujterraform "github.com/crossplane/upjet/v2/pkg/terraform"
)

func TestLoginPolicyID(t *testing.T) {
	for _, test := range []struct {
		name, external    string
		parameters, setup map[string]any
		want              string
	}{
		{name: "retained import wins", external: "original", parameters: map[string]any{"org_id": "other"}, want: "original"},
		{name: "explicit organization adopts singleton", parameters: map[string]any{"org_id": "organization-1"}, want: "organization-1"},
		{name: "other authentication retains previous behavior", parameters: map[string]any{}, setup: map[string]any{"configuration": map[string]any{"jwt_profile_file": "profile.json"}}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := loginPolicyExternalName.GetIDFn(context.Background(), test.external, test.parameters, test.setup)
			if err != nil || got != test.want {
				t.Fatalf("got %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestPrincipalOrganization(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/management/v1/orgs/me" || r.Header.Get("Authorization") != "Bearer test-only" || r.Header.Get("x-zitadel-instance-host") != "auth.example.test" {
			t.Error("request did not preserve provider scope/authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"org":{"id":"system-org"}}`))
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	setup := map[string]any{"access_token": "test-only", "domain": host, "port": port, "insecure": true, "transport_headers": map[string]any{"x-zitadel-instance-host": "auth.example.test"}}
	setups := []map[string]any{
		{"configuration": setup},
		(ujterraform.Setup{Configuration: ujterraform.ProviderConfiguration(setup)}).Map(),
	}
	for _, configured := range setups {
		for i := 0; i < 2; i++ {
			parameters := map[string]any{}
			id, err := loginPolicyExternalName.GetIDFn(context.Background(), "", parameters, configured)
			if err != nil || id != "system-org" || parameters["org_id"] != "system-org" {
				t.Fatalf("got %q, %v", id, err)
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("expected repeatable read-only discovery; calls=%d", calls.Load())
	}
}

func TestPrincipalOrganizationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{{"unauthorized", 401, `sensitive response`}, {"invalid JSON", 200, `sensitive response`}, {"missing org", 200, `{}`}, {"redirect", 302, `sensitive response`}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://untrusted.example")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			_, err := principalOrganization(context.Background(), map[string]any{"domain": host, "port": port, "insecure": true}, "test-only")
			if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "test-only") {
				t.Fatalf("expected sanitized failure, got %v", err)
			}
		})
	}
}
