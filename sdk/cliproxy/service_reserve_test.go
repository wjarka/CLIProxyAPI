package cliproxy

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuilderRejectsReservePoliciesInHomeMode(t *testing.T) {
	cfg := &config.Config{Home: config.HomeConfig{Enabled: true}, Routing: config.RoutingConfig{
		ReservePolicies: map[string]config.ReservePolicy{"work": {Timezone: "UTC", Schedule: []config.ReserveSchedule{{Days: []string{"mon"}, ProtectedBudgetPercent: 10, ReleaseAt: "17:00"}}}},
		AccountPolicies: []config.AccountReservePolicy{{Account: "work.json", ReservePolicy: "work"}},
	}}
	_, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").Build()
	if err == nil || !strings.Contains(err.Error(), "standalone") {
		t.Fatalf("builder accepted Home reserves: %v", err)
	}
}
