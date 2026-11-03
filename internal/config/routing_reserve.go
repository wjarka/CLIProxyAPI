package config

import (
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // Reserve schedules also work in minimal deployment images.
)

// ReservePolicy protects the sum of budgets with deadlines before the next reset.
type ReservePolicy struct {
	Timezone string            `yaml:"timezone" json:"timezone"`
	Schedule []ReserveSchedule `yaml:"schedule" json:"schedule"`
}

// ReserveSchedule releases each day's whole budget at the specified local time.
type ReserveSchedule struct {
	Days                   []string `yaml:"days" json:"days"`
	ProtectedBudgetPercent float64  `yaml:"protected-budget-percent" json:"protected-budget-percent"`
	ReleaseAt              string   `yaml:"release-at" json:"release-at"`
}

// AccountReservePolicy identifies a credential by its exact stable runtime ID.
type AccountReservePolicy struct {
	Account       string `yaml:"account" json:"account"`
	ReservePolicy string `yaml:"reserve-policy" json:"reserve-policy"`
}

var reserveWeekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ValidateReservePolicies also checks runtime-only execution settings, which
// are supplied by -home-jwt rather than ordinary YAML configuration.
func (cfg *Config) ValidateReservePolicies() error {
	if err := cfg.Routing.ValidateReserves(); err != nil {
		return err
	}
	if cfg.Home.Enabled && len(cfg.Routing.AccountPolicies) > 0 {
		return fmt.Errorf("reserve policies require standalone execution; Home dispatch does not enforce them")
	}
	return nil
}

// ValidateReserves rejects configurations that would silently lose protection.
func (r RoutingConfig) ValidateReserves() error {
	for name, policy := range r.ReservePolicies {
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("routing.reserve-policies: invalid policy name %q", name)
		}
		if policy.Timezone == "" {
			return fmt.Errorf("reserve policy %q requires a timezone", name)
		}
		if _, err := time.LoadLocation(policy.Timezone); err != nil {
			return fmt.Errorf("reserve policy %q timezone: %w", name, err)
		}
		if len(policy.Schedule) == 0 {
			return fmt.Errorf("reserve policy %q requires a schedule", name)
		}
		total := 0.0
		for _, entry := range policy.Schedule {
			if len(entry.Days) == 0 {
				return fmt.Errorf("reserve policy %q requires schedule days", name)
			}
			seen := make(map[string]bool)
			for _, day := range entry.Days {
				if _, ok := reserveWeekdays[day]; !ok || seen[day] {
					return fmt.Errorf("reserve policy %q has invalid or duplicate day %q", name, day)
				}
				seen[day] = true
			}
			if _, err := time.Parse("15:04", entry.ReleaseAt); err != nil || len(entry.ReleaseAt) != 5 {
				return fmt.Errorf("reserve policy %q release-at must be HH:MM", name)
			}
			budget := entry.ProtectedBudgetPercent
			if math.IsNaN(budget) || math.IsInf(budget, 0) || budget < 0 || budget > 100 {
				return fmt.Errorf("reserve policy %q protected-budget-percent must be between 0 and 100", name)
			}
			total += float64(len(entry.Days)) * budget
		}
		if total > 100+1e-9 {
			return fmt.Errorf("reserve policy %q weekly protected budgets exceed 100 percent", name)
		}
	}
	seen := make(map[string]bool)
	for _, binding := range r.AccountPolicies {
		if binding.Account == "" || binding.Account != strings.TrimSpace(binding.Account) || seen[binding.Account] {
			return fmt.Errorf("routing.account-policies requires unique non-empty account IDs")
		}
		seen[binding.Account] = true
		if _, ok := r.ReservePolicies[binding.ReservePolicy]; !ok {
			return fmt.Errorf("account %q refers to unknown reserve policy %q", binding.Account, binding.ReservePolicy)
		}
	}
	return nil
}

// ProtectedReserve returns the remaining budgets and their first release instant.
// Calendar dates, rather than 24-hour durations, preserve local deadlines over DST.
func (p ReservePolicy) ProtectedReserve(now, reset time.Time) (float64, time.Time) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil || !reset.After(now) {
		return 0, time.Time{}
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	total := 0.0
	var next time.Time
	for offset := 0; offset <= 7; offset++ {
		date := day.AddDate(0, 0, offset)
		for _, entry := range p.Schedule {
			if entry.ProtectedBudgetPercent == 0 {
				continue
			}
			clock, err := time.Parse("15:04", entry.ReleaseAt)
			if err != nil {
				continue
			}
			for _, name := range entry.Days {
				weekday, ok := reserveWeekdays[name]
				if !ok || weekday != date.Weekday() {
					continue
				}
				release := time.Date(date.Year(), date.Month(), date.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
				if !release.After(now) || !release.Before(reset) {
					continue
				}
				total += entry.ProtectedBudgetPercent
				if next.IsZero() || release.Before(next) {
					next = release
				}
			}
		}
	}
	return math.Min(total, 100), next
}
