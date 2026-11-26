package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQuotaHistoryV8RoundTrip(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("config-version: 8\nobservability:\n  quota-history:\n    enabled: false\n    path: archive/weekly.json\n    retention-weeks: 26\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaHistory.Enabled == nil || *cfg.QuotaHistory.Enabled || cfg.QuotaHistory.Path != "archive/weekly.json" || cfg.QuotaHistory.RetentionWeeks != 26 {
		t.Fatalf("%+v", cfg.QuotaHistory)
	}
	b, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err = yaml.Unmarshal(b, &restored); err != nil || restored.QuotaHistory.Path != cfg.QuotaHistory.Path {
		t.Fatal(string(b), err)
	}
}
