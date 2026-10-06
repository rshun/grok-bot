package grok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ModelList is the catalog printed by `grok models`.
type ModelList struct {
	Default string
	Names   []string
}

// Allowance is the subscription remaining quota shown by /usage.
type Allowance struct {
	Tier           string
	UsedPercent    float64
	HasUsedPercent bool
	PeriodKind     string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Products       []ProductUsage
}

// ProductUsage is one product row inside the billing payload.
type ProductUsage struct {
	Name         string
	UsagePercent float64
}

// HeadlessResult is the JSON object printed by `grok -p --output-format json`.
type HeadlessResult struct {
	Text       string
	SessionID  string
	StopReason string
}

type optionalFloat struct {
	set   bool
	value float64
}

func (f *optionalFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		f.set = true
		f.value = n
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return err
	}
	f.set = true
	f.value = n
	return nil
}

// ParseModels reads `grok models` stdout.
func ParseModels(output string) ModelList {
	var list ModelList
	seen := map[string]struct{}{}
	for _, line := range strings.Split(output, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "Default model:") {
			name := strings.TrimSpace(strings.TrimPrefix(trim, "Default model:"))
			if name != "" {
				list.Default = name
			}
			continue
		}
		name, ok := modelLine(trim)
		if !ok || name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		list.Names = append(list.Names, name)
		if list.Default == "" && strings.Contains(trim, "(default)") {
			list.Default = name
		}
	}
	return list
}

func modelLine(trim string) (string, bool) {
	if trim == "" {
		return "", false
	}
	switch trim[0] {
	case '*', '-', '+':
	default:
		return "", false
	}
	rest := strings.TrimSpace(trim[1:])
	if rest == "" {
		return "", false
	}
	name := strings.Fields(rest)[0]
	name = strings.Trim(name, "()")
	if !validModelName(name) {
		return "", false
	}
	return name, true
}

func validModelName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_' || r == '+' {
			continue
		}
		return false
	}
	return true
}

// ValidModelName reports whether a user-supplied model id is safe to pass as an argument.
func ValidModelName(name string) bool {
	return validModelName(name)
}

type billingEnvelope struct {
	Config struct {
		CurrentPeriod struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		CreditUsagePercent optionalFloat `json:"creditUsagePercent"`
		BillingPeriodStart string        `json:"billingPeriodStart"`
		BillingPeriodEnd   string        `json:"billingPeriodEnd"`
		ProductUsage       []struct {
			Product      string        `json:"product"`
			UsagePercent optionalFloat `json:"usagePercent"`
		} `json:"productUsage"`
	} `json:"config"`
}

type userEnvelope struct {
	SubscriptionTier string `json:"subscriptionTier"`
}

// ParseAllowance reads the billing response and the optional user response.
func ParseAllowance(billingJSON, userJSON []byte) (Allowance, error) {
	var env billingEnvelope
	if err := json.Unmarshal(billingJSON, &env); err != nil {
		return Allowance{}, fmt.Errorf("parse billing: %w", err)
	}
	cfg := env.Config
	out := Allowance{PeriodKind: cfg.CurrentPeriod.Type}
	if cfg.CreditUsagePercent.set {
		out.HasUsedPercent = true
		out.UsedPercent = cfg.CreditUsagePercent.value
	}
	start := cfg.CurrentPeriod.Start
	if start == "" {
		start = cfg.BillingPeriodStart
	}
	end := cfg.CurrentPeriod.End
	if end == "" {
		end = cfg.BillingPeriodEnd
	}
	if t, ok := parseTime(start); ok {
		out.PeriodStart = t
	}
	if t, ok := parseTime(end); ok {
		out.PeriodEnd = t
	}
	for _, product := range cfg.ProductUsage {
		if product.Product == "" || !product.UsagePercent.set {
			continue
		}
		out.Products = append(out.Products, ProductUsage{
			Name:         product.Product,
			UsagePercent: product.UsagePercent.value,
		})
	}
	if len(userJSON) > 0 {
		var user userEnvelope
		if err := json.Unmarshal(userJSON, &user); err != nil {
			return Allowance{}, fmt.Errorf("parse user: %w", err)
		}
		out.Tier = user.SubscriptionTier
	}
	return out, nil
}

func parseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// FormatAllowance renders the remaining subscription allowance for Telegram.
func FormatAllowance(a Allowance) string {
	var b strings.Builder
	if a.Tier != "" {
		fmt.Fprintf(&b, "套餐：%s\n", a.Tier)
	}
	if !a.HasUsedPercent {
		b.WriteString("账单里没有剩余额度。")
		return strings.TrimSpace(b.String())
	}
	remaining := 100 - a.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	fmt.Fprintf(&b, "本期剩余：%s（已用 %s）\n", formatPercent(remaining), formatPercent(a.UsedPercent))
	if kind := periodKind(a.PeriodKind); kind != "" {
		fmt.Fprintf(&b, "周期：%s\n", kind)
	}
	if !a.PeriodEnd.IsZero() {
		fmt.Fprintf(&b, "重置时间：%s\n", a.PeriodEnd.In(time.Local).Format("2006-01-02 15:04 MST"))
	}
	for _, product := range a.Products {
		fmt.Fprintf(&b, "%s 已用 %s\n", productLabel(product.Name), formatPercent(product.UsagePercent))
	}
	return strings.TrimSpace(b.String())
}

func formatPercent(v float64) string {
	if v < 0 {
		v = 0
	}
	text := strconv.FormatFloat(v, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return text + "%"
}

func periodKind(raw string) string {
	switch raw {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return "每周"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return "每月"
	case "":
		return ""
	default:
		return raw
	}
}

func productLabel(name string) string {
	switch name {
	case "GrokBuild":
		return "Grok Build"
	case "GrokChat":
		return "Grok Chat"
	default:
		return name
	}
}

type headlessEnvelope struct {
	Type       string `json:"type"`
	Message    string `json:"message"`
	Text       string `json:"text"`
	StopReason string `json:"stopReason"`
	SessionID  string `json:"sessionId"`
}

// ParseHeadless reads the JSON result of a headless grok run.
func ParseHeadless(output string) (HeadlessResult, error) {
	raw := extractJSON(output)
	if raw == nil {
		return HeadlessResult{}, fmt.Errorf("grok returned no JSON")
	}
	var env headlessEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return HeadlessResult{}, fmt.Errorf("parse grok output: %w", err)
	}
	if env.Type == "error" || (env.Message != "" && env.Text == "" && env.SessionID == "") {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "grok failed"
		}
		return HeadlessResult{}, &RunError{Message: msg}
	}
	return HeadlessResult{
		Text:       strings.TrimSpace(env.Text),
		SessionID:  env.SessionID,
		StopReason: env.StopReason,
	}, nil
}

// RunError is a failure reported by grok itself.
type RunError struct {
	Message string
}

func (e *RunError) Error() string {
	return e.Message
}

func extractJSON(output string) []byte {
	trimmed := strings.TrimSpace(output)
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && json.Valid([]byte(line)) {
			return []byte(line)
		}
	}
	start := strings.LastIndex(output, "{")
	for start >= 0 {
		chunk := strings.TrimSpace(output[start:])
		if json.Valid([]byte(chunk)) {
			return []byte(chunk)
		}
		if end := strings.LastIndex(chunk, "}"); end > 0 {
			candidate := chunk[:end+1]
			if json.Valid([]byte(candidate)) {
				return []byte(candidate)
			}
		}
		next := strings.LastIndex(output[:start], "{")
		start = next
	}
	return nil
}

// UserFacing converts a grok failure into the message sent back on Telegram.
func UserFacing(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "credit limit"),
		strings.Contains(lower, "usage limit"),
		strings.Contains(lower, "out of credits"),
		strings.Contains(lower, "spending cap"),
		strings.Contains(lower, "spending limit"),
		strings.Contains(lower, "free-usage-exhausted"),
		strings.Contains(lower, "usage_limit_reached"),
		strings.Contains(lower, "usage_pool_exhausted"),
		strings.Contains(lower, "quota"):
		return "订阅额度已用完。可以用 /usage 查看剩余情况。"
	case strings.Contains(lower, "grok auth"),
		strings.Contains(lower, "session token"):
		return "读不到 grok 登录信息。在运行 bot 的这个用户下执行 grok login 后再试。"
	case strings.Contains(lower, "login"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "authentication"),
		strings.Contains(lower, "auth"):
		return "grok 未登录或登录已过期。在运行 bot 的这个用户下执行 grok login 后再试。"
	case strings.Contains(lower, "deadline") || strings.Contains(lower, "timeout") || strings.Contains(lower, "context canceled"):
		return "处理超时。请再发一次。"
	default:
		flat := strings.Join(strings.Fields(msg), " ")
		if len([]rune(flat)) > 500 {
			flat = string([]rune(flat)[:500]) + "…"
		}
		return "处理失败：" + flat
	}
}
