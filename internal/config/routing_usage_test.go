package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const usageConfigExample = `config-version: 8
routing:
  usage-policies:
    work-hours:
      timezone: Europe/Warsaw
      five-hour-used-percent: 70
      schedule:
        - days: [mon, tue, wed, thu, fri]
          start: "09:00"
          end: "17:00"
  account-policies:
    - account: work.json
      usage-policy: work-hours
`

func TestUsagePolicyConfigRoundTripAndValidation(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(usageConfigExample))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err = NormalizeConfigLayout(encoded, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"usage-policies:", "five-hour-used-percent: 70", "usage-policy: work-hours"} {
		if !strings.Contains(string(encoded), required) {
			t.Errorf("roundtrip lost %q", required)
		}
	}
	if err := ValidateV8Config(encoded); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"timezone":           {"Europe/Warsaw", "Invalid/Zone"},
		"unknown reference":  {"usage-policy: work-hours", "usage-policy: missing"},
		"empty binding":      {"usage-policy: work-hours", "usage-policy: ''"},
		"threshold negative": {"percent: 70", "percent: -1"},
		"threshold overflow": {"percent: 70", "percent: 101"},
		"threshold missing":  {"five-hour-used-percent: 70", ""},
		"day":                {"[mon, tue, wed, thu, fri]", "[bad]"},
		"duplicate day":      {"[mon, tue, wed, thu, fri]", "[mon, mon]"},
		"empty days":         {"[mon, tue, wed, thu, fri]", "[]"},
		"bad start":          {`"09:00"`, `"25:00"`},
		"bad end":            {`"17:00"`, `"9:00"`},
		"equal bounds":       {`"17:00"`, `"09:00"`},
		"reverse bounds":     {`"17:00"`, `"08:00"`},
		"empty schedule":     {"- days: [mon, tue, wed, thu, fri]\n          start: \"09:00\"\n          end: \"17:00\"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.ReplaceAll(usageConfigExample, pair[0], pair[1])
			if _, err := ParseConfigBytes([]byte(raw)); err == nil {
				t.Fatal("accepted invalid usage policy")
			}
			if err := ValidateV8Config([]byte(raw)); err == nil {
				t.Fatal("v8 accepted invalid usage policy")
			}
		})
	}
	cfg.Home.Enabled = true
	if cfg.ValidateReservePolicies() == nil {
		t.Fatal("Home accepted unenforceable policy")
	}
}

func TestUsagePolicyScheduleBoundariesAndDST(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(usageConfigExample))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at     string
		active bool
	}{
		{"2026-11-06T07:59:59Z", false}, {"2026-11-06T08:00:00Z", true},
		{"2026-11-06T15:59:59Z", true}, {"2026-11-06T16:00:00Z", false},
		{"2026-11-07T11:00:00Z", false}, {"2026-11-08T11:00:00Z", false},
		{"2026-03-30T07:00:00Z", true}, {"2026-03-30T15:00:00Z", false},
		{"2026-10-26T08:00:00Z", true}, {"2026-10-26T16:00:00Z", false},
	} {
		at, err := time.Parse(time.RFC3339, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		until := cfg.Routing.UsagePolicies["work-hours"].ActiveUntil(at)
		if !until.IsZero() != tc.active {
			t.Fatalf("at %s active=%v want%v", tc.at, !until.IsZero(), tc.active)
		}
		if tc.active && until.In(mustUsageLocation(t)).Format("15:04") != "17:00" {
			t.Fatal("active end differs from local 17:00")
		}
	}
}

func mustUsageLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
