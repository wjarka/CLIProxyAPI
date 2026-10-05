package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestResetRoutingConfiguredAndWrapped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Strategy = "earliest-reset"
	if _, ok := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.EarliestResetSelector); !ok {
		t.Fatal("reset strategy not selected")
	}
	cfg.Routing.SessionAffinity = true
	sel := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
	affinity, ok := sel.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatal("affinity wrapper missing")
	}
	affinity.Stop()
}
