package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const reserveConfigExample = `config-version: 8
routing:
  strategy: earliest-reset
  reserve-policies:
    work:
      timezone: Europe/Warsaw
      schedule:
        - days: [mon, wed, thu, fri]
          protected-budget-percent: 10
          release-at: "17:00"
        - days: [tue]
          protected-budget-percent: 20
          release-at: "17:00"
  account-policies:
    - account: work-claude.json
      reserve-policy: work
`

func TestReservePolicyConfigRoundTrip(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(reserveConfigExample))
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
	for _, required := range []string{"protected-budget-percent: 20", "reserve-policy: work", "Europe/Warsaw"} {
		if !strings.Contains(string(encoded), required) {
			t.Errorf("round trip lost %q", required)
		}
	}
	if err := ValidateV8Config(encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfigBytes(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestReservePolicyConfigRejectsInvalidRules(t *testing.T) {
	for name, replacements := range map[string][2]string{
		"timezone":          {"Europe/Warsaw", "Not/A_Timezone"},
		"day":               {"[tue]", "[tomorrow]"},
		"duplicate day":     {"[tue]", "[tue, tue]"},
		"release":           {`"17:00"`, `"25:00"`},
		"negative":          {"percent: 20", "percent: -1"},
		"overflow":          {"percent: 20", "percent: 101"},
		"weekly overflow":   {"percent: 20", "percent: 70"},
		"unknown policy":    {"reserve-policy: work", "reserve-policy: missing"},
		"empty account":     {"account: work-claude.json", `account: ""`},
		"duplicate account": {"reserve-policy: work", "reserve-policy: work\n    - account: work-claude.json\n      reserve-policy: work"},
		"empty schedule":    {"schedule:\n        - days: [mon, wed, thu, fri]\n          protected-budget-percent: 10\n          release-at: \"17:00\"\n        - days: [tue]\n          protected-budget-percent: 20\n          release-at: \"17:00\"", "schedule: []"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.ReplaceAll(reserveConfigExample, replacements[0], replacements[1])
			if raw == reserveConfigExample {
				t.Fatal("test replacement did not change fixture")
			}
			if _, err := ParseConfigBytes([]byte(raw)); err == nil {
				t.Fatal("accepted invalid reserve policy")
			}
			if err := ValidateV8Config([]byte(raw)); err == nil {
				t.Fatal("v8 accepted invalid reserve policy")
			}
		})
	}
}
