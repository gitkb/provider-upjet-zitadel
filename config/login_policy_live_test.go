/*
Copyright 2026 The Crossplane Authors.
*/
package config

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	ujterraform "github.com/crossplane/upjet/v2/pkg/terraform"
)

// TestLiveLoginPolicyOrganization is opt-in and reads credentials from stdin.
// It performs only current-organization discovery, without creating policies,
// logging credentials or claiming a complete fresh-install lifecycle test.
func TestLiveLoginPolicyOrganization(t *testing.T) {
	if os.Getenv("ZITADEL_POLICY_LIVE_TEST") != "1" {
		t.Skip("read-only live discovery is opt-in")
	}
	var input struct {
		Configuration map[string]any `json:"configuration"`
		Expected      string         `json:"expected"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal("invalid test input")
	}
	params := map[string]any{}
	id, err := loginPolicyExternalName.GetIDFn(context.Background(), "", params, (ujterraform.Setup{Configuration: ujterraform.ProviderConfiguration(input.Configuration)}).Map())
	if err != nil {
		t.Fatal(err)
	}
	if id != input.Expected || params["org_id"] != input.Expected {
		t.Fatal("discovered organization does not match retained policy identity")
	}
}
