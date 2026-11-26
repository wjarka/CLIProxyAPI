package quotahistory

import (
	"fmt"
	"testing"
	"time"
)

func TestWeeklyProviderParsers(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	end := at.Add(time.Hour)
	iso := end.Format(time.RFC3339)
	cases := []struct {
		provider, body string
		count          int
		remaining      float64
	}{
		{"claude", fmt.Sprintf(`{"seven_day":{"utilization":77,"resets_at":%q},"seven_day_sonnet":{"utilization":0,"resets_at":%q},"five_hour":{"utilization":12,"resets_at":%q}}`, iso, iso, iso), 2, 23},
		{"codex", fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":77,"limit_window_seconds":604800,"reset_at":%d}},"additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}]}`, end.Unix(), end.Unix()), 2, 23},
		{"antigravity", fmt.Sprintf(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"weekly-gemini","window":"weekly","remainingFraction":0.23,"resetTime":%q}]}]}`, iso), 1, 23},
		{"devin", fmt.Sprintf(`{"userStatus":{"planStatus":{"weeklyQuotaRemainingPercent":23,"weeklyQuotaResetAtUnix":"%d"}}}`, end.Unix()), 1, 23},
		{"kimi", fmt.Sprintf(`{"limits":[{"name":"Weekly","window":{"duration":1,"timeUnit":"TIME_UNIT_WEEK"},"detail":{"used":77,"limit":100,"reset_at":%q}}]}`, iso), 1, 23},
		{"kimi", fmt.Sprintf(`{"limits":[{"detail":{"duration":1,"timeUnit":"TIME_UNIT_WEEK","used":77,"limit":100,"reset_at":%q}}]}`, iso), 1, 23},
		{"meta", fmt.Sprintf(`{"api_key":"MUST-NOT-PERSIST","subs_usage":{"weekly":{"used_percent":77,"resets_at":%d}}}`, end.Unix()), 1, 23},
		{"xai", fmt.Sprintf(`{"config":{"currentPeriod":{"type":"WEEKLY","end":%q},"creditUsagePercent":77}}`, iso), 1, 23},
		{"zai", fmt.Sprintf(`{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":77,"nextResetTime":%d}]}}`, end.UnixMilli()), 1, 23},
	}
	for _, tt := range cases {
		t.Run(tt.provider, func(t *testing.T) {
			got, err := ParseWeekly(tt.provider, "account", []byte(tt.body), at)
			if err != nil || len(got) != tt.count {
				t.Fatalf("%+v %v", got, err)
			}
			if got[0].RemainingPercent == nil || *got[0].RemainingPercent != tt.remaining || !got[0].ResetAt.Equal(end) {
				t.Fatalf("%+v", got[0])
			}
		})
	}
}

func TestWeeklyParserUnknownZeroAndNoInventedWindows(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	iso := at.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		p, b  string
		count int
		zero  bool
	}{
		{"antigravity", fmt.Sprintf(`{"groups":[{"displayName":"Gemini","buckets":[{"window":"weekly","resetTime":%q}]}]}`, iso), 1, true},
		{"claude", fmt.Sprintf(`{"seven_day":{"resets_at":%q}}`, iso), 1, false},
		{"claude", fmt.Sprintf(`{"iguana_necktie":{"limit_dollars":100,"utilization":23,"resets_at":%q}}`, iso), 0, false},
		{"kimi", `{"usages":{"limit_month_total":{"used_ratio":0.4}}}`, 0, false},
		{"codex", fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":2592000,"reset_at":%d}}}`, at.Add(time.Hour).Unix()), 0, false},
		{"xai", fmt.Sprintf(`{"config":{"currentPeriod":{"type":"MONTHLY","end":%q},"creditUsagePercent":23}}`, iso), 0, false},
	} {
		got, err := ParseWeekly(tc.p, "a", []byte(tc.b), at)
		if err != nil || len(got) != tc.count {
			t.Fatalf("%s %+v %v", tc.p, got, err)
		}
		if len(got) > 0 {
			if tc.zero && (got[0].RemainingPercent == nil || *got[0].RemainingPercent != 0) {
				t.Fatal(got)
			}
			if !tc.zero && got[0].RemainingPercent != nil {
				t.Fatal(got)
			}
		}
	}
}
