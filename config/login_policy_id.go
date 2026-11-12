/*
Copyright 2026 The Crossplane Authors.
*/

package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ujconfig "github.com/crossplane/upjet/v2/pkg/config"
	ujterraform "github.com/crossplane/upjet/v2/pkg/terraform"
)

// loginPolicyExternalName resolves the organization's singleton ID before the
// first Observe. A missing org_id selects the authenticated principal's org in
// Terraform; PAT-backed configurations can discover that same org read-only.
// Existing external names remain authoritative, including imported resources.
var loginPolicyExternalName = ujconfig.NewExternalNameFrom(ujconfig.IdentifierFromProvider,
	ujconfig.WithGetIDFn(func(parent ujconfig.GetIDFn, ctx context.Context, externalName string, parameters, setup map[string]any) (string, error) {
		if externalName != "" {
			return parent(ctx, externalName, parameters, setup)
		}
		if org, ok := parameters["org_id"].(string); ok && org != "" {
			return org, nil
		}
		// Setup.Map preserves the named ProviderConfiguration map. Decoded test
		// input may also be an ordinary map, so support both representations.
		var configuration map[string]any
		switch value := setup["configuration"].(type) {
		case ujterraform.ProviderConfiguration:
			configuration = map[string]any(value)
		case map[string]any:
			configuration = value
		}
		token, ok := configuration["access_token"].(string)
		if !ok || token == "" {
			return parent(ctx, externalName, parameters, setup)
		}
		org, err := principalOrganization(ctx, configuration, token)
		if err == nil && parameters != nil {
			// Terraform create uses org_id as its returned ID. Supplying the
			// discovered ID also prevents a successful create returning "".
			parameters["org_id"] = org
		}
		return org, err
	}))

func principalOrganization(ctx context.Context, setup map[string]any, token string) (string, error) {
	domain, _ := setup["domain"].(string)
	if domain == "" || strings.ContainsAny(domain, "/?#@") {
		return "", fmt.Errorf("cannot discover login policy organization: invalid provider domain")
	}
	insecure, _ := setup["insecure"].(bool)
	scheme, port := "https", "443"
	if insecure {
		scheme, port = "http", "80"
	}
	switch p := setup["port"].(type) {
	case string:
		if p != "" {
			port = p
		}
	case float64:
		port = strconv.Itoa(int(p))
	case int:
		port = strconv.Itoa(p)
	}
	endpoint := url.URL{Scheme: scheme, Host: net.JoinHostPort(domain, port), Path: "/management/v1/orgs/me"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("cannot build login policy organization request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if headers, ok := setup["transport_headers"].(map[string]any); ok {
		for name, value := range headers {
			if text, ok := value.(string); ok {
				req.Header.Set(name, text)
			}
		}
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot discover login policy organization: request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot discover login policy organization: HTTP %d", response.StatusCode)
	}
	var result struct {
		Org struct {
			ID string `json:"id"`
		} `json:"org"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil || result.Org.ID == "" {
		return "", fmt.Errorf("cannot discover login policy organization: invalid response")
	}
	return result.Org.ID, nil
}
