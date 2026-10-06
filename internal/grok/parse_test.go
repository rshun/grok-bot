package grok

import (
	"strings"
	"testing"
	"time"
)

func TestParseModels(t *testing.T) {
	out := `
You are logged in with grok.com.

Default model: grok-4.7

Available models:
  * grok-4.7 (default)
  - grok-4.7-build-fast
  - grok-4.6
  - grok-4.5
`
	list := ParseModels(out)
	if list.Default != "grok-4.7" {
		t.Fatalf("default %q", list.Default)
	}
	if len(list.Names) != 4 || list.Names[1] != "grok-4.7-build-fast" {
		t.Fatalf("names %#v", list.Names)
	}
}

func TestParseAllowance(t *testing.T) {
	billing := []byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-05T11:47:32Z","end":"2026-10-12T11:47:32Z"},"creditUsagePercent":2,"billingPeriodEnd":"2026-10-12T11:47:32Z","productUsage":[{"product":"GrokBuild","usagePercent":1},{"product":"GrokChat","usagePercent":1}]}}`)
	user := []byte(`{"subscriptionTier":"GrokPro"}`)
	got, err := ParseAllowance(billing, user)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasUsedPercent || got.UsedPercent != 2 || got.Tier != "GrokPro" {
		t.Fatalf("%#v", got)
	}
	text := FormatAllowance(got)
	if !strings.Contains(text, "本期剩余：98%") || !strings.Contains(text, "套餐：GrokPro") || !strings.Contains(text, "每周") {
		t.Fatalf("text:\n%s", text)
	}
	if got.PeriodEnd.UTC().Format(time.RFC3339) != "2026-10-12T11:47:32Z" {
		t.Fatalf("end %s", got.PeriodEnd)
	}
}

func TestParseAllowanceMissingPercent(t *testing.T) {
	got, err := ParseAllowance([]byte(`{"config":{}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasUsedPercent {
		t.Fatal("percent should be absent")
	}
	if !strings.Contains(FormatAllowance(got), "没有剩余额度") {
		t.Fatal(FormatAllowance(got))
	}
}

func TestParseHeadless(t *testing.T) {
	got, err := ParseHeadless("log line\n{\"text\":\"你好\",\"stopReason\":\"end_turn\",\"sessionId\":\"abc\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "你好" || got.SessionID != "abc" {
		t.Fatalf("%#v", got)
	}
	_, err = ParseHeadless(`{"type":"error","message":"You have hit the credit limit for your plan"}`)
	if err == nil || UserFacing(err) != "订阅额度已用完。可以用 /usage 查看剩余情况。" {
		t.Fatalf("err %v facing %q", err, UserFacing(err))
	}
}

func TestValidModelName(t *testing.T) {
	if !ValidModelName("grok-4.7") || ValidModelName("grok 4") || ValidModelName("") {
		t.Fatal("model name check")
	}
}
