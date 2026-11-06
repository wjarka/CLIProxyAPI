package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// UsagePolicy bounds five-hour consumption during scheduled local working hours.
type UsagePolicy struct {
	Timezone            string          `yaml:"timezone" json:"timezone"`
	FiveHourUsedPercent *float64        `yaml:"five-hour-used-percent" json:"five-hour-used-percent"`
	Schedule            []UsageSchedule `yaml:"schedule" json:"schedule"`
}

// UsageSchedule is a same-day interval; start is inclusive and end exclusive.
type UsageSchedule struct {
	Days  []string `yaml:"days" json:"days"`
	Start string   `yaml:"start" json:"start"`
	End   string   `yaml:"end" json:"end"`
}

func (r RoutingConfig) validateUsagePolicies() error {
	for name, p := range r.UsagePolicies {
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("routing.usage-policies: invalid policy name %q", name)
		}
		if p.Timezone == "" {
			return fmt.Errorf("usage policy %q requires a timezone", name)
		}
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("usage policy %q timezone: %w", name, err)
		}
		limit := p.FiveHourUsedPercent
		if limit == nil || math.IsNaN(*limit) || math.IsInf(*limit, 0) || *limit < 0 || *limit > 100 {
			return fmt.Errorf("usage policy %q five-hour-used-percent must be between 0 and 100", name)
		}
		if len(p.Schedule) == 0 {
			return fmt.Errorf("usage policy %q requires a schedule", name)
		}
		for _, entry := range p.Schedule {
			if len(entry.Days) == 0 {
				return fmt.Errorf("usage policy %q requires schedule days", name)
			}
			seen := make(map[string]bool)
			for _, day := range entry.Days {
				if _, ok := reserveWeekdays[day]; !ok || seen[day] {
					return fmt.Errorf("usage policy %q has invalid or duplicate day %q", name, day)
				}
				seen[day] = true
			}
			start, errStart := time.Parse("15:04", entry.Start)
			end, errEnd := time.Parse("15:04", entry.End)
			if errStart != nil || errEnd != nil || len(entry.Start) != 5 || len(entry.End) != 5 || !start.Before(end) {
				return fmt.Errorf("usage policy %q requires HH:MM start before end on the same day", name)
			}
		}
	}
	return nil
}

// ActiveUntil returns zero outside the schedule. Local calendar times preserve
// the working hours when UTC offsets change. Overlapping intervals form a union.
func (p UsagePolicy) ActiveUntil(now time.Time) time.Time {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}
	}
	local := now.In(loc)
	var until time.Time
	for _, entry := range p.Schedule {
		for _, day := range entry.Days {
			weekday, ok := reserveWeekdays[day]
			if !ok || weekday != local.Weekday() {
				continue
			}
			start, errStart := time.Parse("15:04", entry.Start)
			end, errEnd := time.Parse("15:04", entry.End)
			if errStart != nil || errEnd != nil {
				continue
			}
			from := time.Date(local.Year(), local.Month(), local.Day(), start.Hour(), start.Minute(), 0, 0, loc)
			to := time.Date(local.Year(), local.Month(), local.Day(), end.Hour(), end.Minute(), 0, 0, loc)
			if !now.Before(from) && now.Before(to) && to.After(until) {
				until = to
			}
		}
	}
	return until
}
