package quotahistory

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func field(v gjson.Result, keys ...string) gjson.Result {
	for _, k := range keys {
		if n := v.Get(k); n.Exists() {
			return n
		}
	}
	return gjson.Result{}
}
func numeric(v gjson.Result) *float64 {
	if v.Type != gjson.Number && v.Type != gjson.String {
		return nil
	}
	n, err := strconv.ParseFloat(v.String(), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func percentage(v gjson.Result, used bool) *float64 {
	n := numeric(v)
	if n == nil || *n < 0 || *n > 100 {
		return nil
	}
	p := *n
	if used {
		p = 100 - p
	}
	return &p
}
func instant(v gjson.Result) time.Time {
	if v.Type == gjson.String {
		if t, err := time.Parse(time.RFC3339Nano, v.String()); err == nil {
			return t.UTC()
		}
	}
	if n := numeric(v); n != nil && *n > 0 {
		if *n > 1e12 {
			return time.UnixMilli(int64(*n)).UTC()
		}
		return time.Unix(int64(*n), 0).UTC()
	}
	return time.Time{}
}

// ParseWeekly ports the quota-only contracts used by the dashboard. It never
// retains a response object: provider responses can contain API keys and PII.
func ParseWeekly(provider, account string, body []byte, observed time.Time) ([]PoolObservation, error) {
	if len(body) > MaxArchiveBytes || !gjson.ValidBytes(body) {
		return nil, errors.New("invalid quota response")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, errors.New("invalid quota response")
	}
	out := []PoolObservation{}
	add := func(id, label string, reset time.Time, p *float64, start time.Time) {
		if len(label) > 256 {
			label = label[:256]
		}
		if len(id) > 256 {
			id = Identity("pool", id, "")
		}
		o := PoolObservation{AccountID: account, Provider: provider, PoolID: id, Label: label, ObservedAt: observed.UTC(), ResetAt: reset, RemainingPercent: p}
		if !start.IsZero() {
			o.StartAt = &start
		}
		if validObservation(o) == nil {
			out = append(out, o)
		}
	}
	switch provider {
	case "claude":
		for _, id := range []string{"seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork", "iguana_necktie"} {
			w := root.Get(id)
			if !w.IsObject() {
				continue
			}
			if id == "iguana_necktie" && (w.Get("limit_dollars").Exists() || w.Get("used_dollars").Exists() || w.Get("remaining_dollars").Exists()) {
				continue
			}
			add(id, strings.ReplaceAll(id, "_", " "), instant(w.Get("resets_at")), percentage(w.Get("utilization"), true), time.Time{})
		}
		for _, w := range root.Get("limits").Array() {
			if w.Get("kind").String() != "weekly_scoped" {
				continue
			}
			model := field(w, "scope.model.id", "scope.model.display_name").String()
			if model == "" {
				continue
			}
			label := field(w, "scope.model.display_name", "scope.model.id").String()
			// The weekly-scoped Fable contract supersedes its older non-dollar pool.
			if strings.Contains(strings.ToLower(label), "fable") {
				filtered := out[:0]
				for _, old := range out {
					if old.PoolID != "iguana_necktie" {
						filtered = append(filtered, old)
					}
				}
				out = filtered
			}
			add("scoped:"+model, label, instant(w.Get("resets_at")), percentage(w.Get("percent"), true), time.Time{})
		}
	case "codex":
		limits := []struct {
			id string
			v  gjson.Result
		}{{"weekly", field(root, "rate_limit", "rateLimit")}, {"code-review", field(root, "code_review_rate_limit", "codeReviewRateLimit")}}
		for _, item := range field(root, "additional_rate_limits", "additionalRateLimits").Array() {
			name := field(item, "metered_feature", "meteredFeature", "limit_name", "limitName").String()
			if name != "" {
				limits = append(limits, struct {
					id string
					v  gjson.Result
				}{"model:" + name, field(item, "rate_limit", "rateLimit")})
			}
		}
		for _, l := range limits {
			for _, key := range []string{"primary_window", "primaryWindow", "secondary_window", "secondaryWindow"} {
				w := l.v.Get(key)
				seconds := numeric(field(w, "limit_window_seconds", "limitWindowSeconds"))
				if seconds == nil || *seconds != 604800 {
					continue
				}
				reset := instant(field(w, "reset_at", "resetAt"))
				if reset.IsZero() {
					if delta := numeric(field(w, "reset_after_seconds", "resetAfterSeconds")); delta != nil && *delta >= 0 {
						reset = observed.Add(time.Duration(*delta) * time.Second)
					}
				}
				add(l.id, l.id, reset, percentage(field(w, "used_percent", "usedPercent"), true), time.Time{})
			}
		}
	case "antigravity":
		for _, g := range root.Get("groups").Array() {
			group := field(g, "displayName", "display_name").String()
			for _, b := range g.Get("buckets").Array() {
				scope := strings.ToLower(b.Get("window").String())
				if scope != "weekly" && scope != "week" {
					continue
				}
				id := field(b, "bucketId", "bucket_id").String()
				if id == "" {
					id = scope
				}
				label := field(b, "displayName", "display_name").String()
				if label == "" {
					label = group + " " + scope
				}
				raw := field(b, "remainingFraction", "remaining_fraction")
				reset := instant(field(b, "resetTime", "reset_time"))
				var p *float64
				if !raw.Exists() && !reset.IsZero() {
					p = new(float64)
				} else if f := numeric(raw); f != nil && *f >= 0 && *f <= 1 {
					v := *f * 100
					p = &v
				}
				add(group+":"+id, label, reset, p, time.Time{})
			}
		}
	case "devin":
		status := root.Get("userStatus.planStatus")
		add("weekly", "Weekly", instant(status.Get("weeklyQuotaResetAtUnix")), percentage(status.Get("weeklyQuotaRemainingPercent"), false), time.Time{})
	case "meta":
		w := root.Get("subs_usage.weekly")
		add("weekly", "Weekly", instant(w.Get("resets_at")), percentage(w.Get("used_percent"), true), time.Time{})
	case "xai":
		cfg := root.Get("config")
		period := field(cfg, "currentPeriod", "current_period")
		if !strings.Contains(strings.ToLower(period.Get("type").String()), "weekly") {
			break
		}
		reset, start := instant(period.Get("end")), instant(period.Get("start"))
		add("weekly", "Weekly", reset, percentage(field(cfg, "creditUsagePercent", "credit_usage_percent"), true), start)
		for _, p := range field(cfg, "productUsage", "product_usage").Array() {
			name := p.Get("product").String()
			if name != "" {
				add("product:"+name, name, reset, percentage(field(p, "usagePercent", "usage_percent"), true), start)
			}
		}
	case "zai":
		if root.Get("code").Int() != 200 {
			break
		}
		for _, w := range root.Get("data.limits").Array() {
			if w.Get("type").String() != "TOKENS_LIMIT" || w.Get("unit").Int() != 6 || w.Get("number").Int() != 1 {
				continue
			}
			p := percentage(w.Get("percentage"), true)
			if p == nil {
				total, rem := numeric(w.Get("usage")), numeric(w.Get("remaining"))
				if total != nil && *total > 0 && rem != nil && *rem >= 0 && *rem <= *total {
					v := *rem / *total * 100
					p = &v
				}
			}
			add("weekly", "Weekly", instant(w.Get("nextResetTime")), p, time.Time{})
		}
	case "kimi":
		rows := root.Get("limits").Array()
		if root.Get("usage").IsObject() {
			rows = append(rows, gjson.Parse(`{"name":"Weekly summary","detail":`+root.Get("usage").Raw+`}`))
		}
		for i, item := range rows {
			d := item.Get("detail")
			if !d.IsObject() {
				d = item
			}
			name := field(item, "name", "title", "scope").String()
			if name == "" {
				name = field(d, "name", "title").String()
			}
			duration := numeric(field(item, "window.duration", "duration"))
			if duration == nil {
				duration = numeric(d.Get("duration"))
			}
			unit := strings.TrimPrefix(strings.ToUpper(field(item, "window.timeUnit", "timeUnit").String()), "TIME_UNIT_")
			if unit == "" {
				unit = strings.TrimPrefix(strings.ToUpper(d.Get("timeUnit").String()), "TIME_UNIT_")
			}
			weekly := strings.Contains(strings.ToLower(name), "week")
			if duration != nil {
				factor := float64(60)
				switch unit {
				case "SECOND", "SECONDS":
					factor = 1
				case "HOUR", "HOURS":
					factor = 3600
				case "DAY", "DAYS":
					factor = 86400
				case "WEEK", "WEEKS":
					factor = 604800
				}
				weekly = *duration*factor == 604800
			}
			if !weekly {
				continue
			}
			total, used, remaining := numeric(d.Get("limit")), numeric(d.Get("used")), numeric(d.Get("remaining"))
			var p *float64
			if total != nil && *total > 0 {
				if remaining == nil && used != nil {
					v := *total - *used
					remaining = &v
				}
				if remaining != nil && *remaining >= 0 && *remaining <= *total {
					v := *remaining / *total * 100
					p = &v
				}
			}
			reset := instant(field(d, "reset_at", "resetAt", "reset_time", "resetTime"))
			if reset.IsZero() {
				if delta := numeric(field(d, "reset_in", "resetIn", "ttl")); delta != nil && *delta >= 0 {
					reset = observed.Add(time.Duration(*delta) * time.Second)
				}
			}
			id := name
			if id == "" {
				id = fmt.Sprintf("weekly-%d", i)
			}
			add(id, name, reset, p, time.Time{})
		}
	}
	// A partial endpoint may repeat a bucket; keep the first valid reading.
	seen := map[string]bool{}
	unique := out[:0]
	for _, o := range out {
		if !seen[o.PoolID] {
			seen[o.PoolID] = true
			unique = append(unique, o)
		}
	}
	return unique, nil
}
