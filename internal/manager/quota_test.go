package manager

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
)

var testThresholds = quotaThresholds{Low: 0.25, Critical: 0.05}

// measuredUsageEnvelope is the shape agy 1.2.16 prints for
// `agy --output-format json -p /usage` (MEASURED), trimmed to two groups.
const measuredUsageEnvelope = `{"conversation_id":"","status":"SUCCESS","response":"x","duration_seconds":0,"num_turns":0,
"usage":{"input_tokens":0},"command":{"name":"usage","data":{"description":"d","groups":[
{"name":"Gemini Models","description":"Gemini family","buckets":[
 {"id":"5h","name":"5h limit","window":"5h","remaining_fraction":0.03,"reset_time":"2026-10-04T11:32:45Z"},
 {"id":"weekly","name":"Weekly limit","description":"w","window":"weekly","remaining_fraction":0.68,"reset_time":"2026-10-07T08:00:00Z"}]},
{"name":"Claude and GPT Models","description":"Others","buckets":[
 {"id":"5h","name":"5h limit","window":"5h","remaining_fraction":1,"reset_time":"2026-10-04T12:00:00Z"},
 {"id":"weekly","name":"Weekly limit","window":"weekly","remaining_fraction":1,"reset_time":"2026-10-07T08:00:00Z"}]}]}}}`

func TestDecodeUsageEnvelope(t *testing.T) {
	t.Run("measured envelope", func(t *testing.T) {
		groups, err := decodeUsageEnvelope([]byte(measuredUsageEnvelope))
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 2 || len(groups[0].Buckets) != 2 || len(groups[1].Buckets) != 2 {
			t.Fatalf("groups = %+v, want two groups of two buckets", groups)
		}
		b := groups[0].Buckets[0]
		if b.ID != "5h" || b.RemainingFraction != 0.03 || b.Window != "5h" ||
			!b.ResetTime.Equal(time.Date(2026, 10, 4, 11, 32, 45, 0, time.UTC)) {
			t.Fatalf("first bucket = %+v", b)
		}
	})
	tests := []struct {
		name    string
		raw     string
		wantErr string // substring; empty means success
		groups  int
	}{
		{"status error", `{"status":"ERROR","command":{"name":"usage","data":{"groups":[]}}}`, "agy usage", 0},
		{"other command", `{"status":"SUCCESS","command":{"name":"models","data":{"groups":[]}}}`, "agy usage", 0},
		{"missing groups", `{"status":"SUCCESS","command":{"name":"usage","data":{}}}`, "agy usage", 0},
		{"null groups", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":null}}}`, "agy usage", 0},
		{"not json", `nope`, "agy usage", 0},
		{"empty groups is a valid reading", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`, "", 0},
		{"bucket without remaining is skipped",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a"},{"id":"b","remaining_fraction":0.5}]}]}}}`, "", 1},
		{"no bucket carries remaining",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a"}]}]}}}`, "remaining_fraction", 0},
		{"bad reset time",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a","remaining_fraction":1,"reset_time":"tomorrow"}]}]}}}`, "reset_time", 0},
		{"bucket without reset time is tolerated",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a","remaining_fraction":1}]}]}}}`, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, err := decodeUsageEnvelope([]byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(groups) != tt.groups {
				t.Fatalf("groups = %d, want %d", len(groups), tt.groups)
			}
		})
	}
}

// The measured --disable-slash-commands reply: a model turn, no command field.
func TestDecodeUsageEnvelopeDetectsModelTurn(t *testing.T) {
	raw := `{"conversation_id":"abc-123","status":"SUCCESS","response":"hi","num_turns":1,"usage":{"input_tokens":21214}}`
	_, err := decodeUsageEnvelope([]byte(raw))
	if !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("error = %v, want errUsageModelTurn", err)
	}
	// A SUCCESS envelope with no command and no turn is a shape error, not a latch.
	_, err = decodeUsageEnvelope([]byte(`{"status":"SUCCESS"}`))
	if err == nil || errors.Is(err, errUsageModelTurn) {
		t.Fatalf("error = %v, want a plain framing error", err)
	}
}

func TestUsageProbeArgsSpendNoQuota(t *testing.T) {
	got := usageProbeArgs()
	want := []string{"--output-format", "json", "-p", "/usage"}
	if !slices.Equal(got, want) {
		t.Fatalf("usageProbeArgs() = %q, want %q", got, want)
	}
	if slices.Contains(got, disableSlashCommandsFlag) {
		t.Fatal("the usage probe must never carry --disable-slash-commands")
	}
}

func TestQuotaLevelBoundaries(t *testing.T) {
	th := quotaThresholds{Low: 0.15, Critical: 0.05}
	tests := []struct {
		r    float64
		want QuotaLevel
	}{
		{1, QuotaOK}, {0.15, QuotaOK}, {0.1499, QuotaLow}, {0.05, QuotaLow},
		{0.0499, QuotaCritical}, {0.0001, QuotaCritical}, {0, QuotaExhausted}, {-0.1, QuotaExhausted},
	}
	for _, tt := range tests {
		// Through groupQuota so the clamp of a negative fraction is covered too.
		g := QuotaGroup{Buckets: []QuotaBucket{{RemainingFraction: tt.r}}}
		if got := groupQuota(g, time.Now(), th).Level; got != tt.want {
			t.Errorf("remaining %v: level = %s, want %s", tt.r, got, tt.want)
		}
	}
}

func TestQuotaLevelMonotonic(t *testing.T) {
	severity := map[QuotaLevel]int{QuotaOK: 0, QuotaLow: 1, QuotaCritical: 2, QuotaExhausted: 3}
	prop := func(a, b, lo, cr uint16) bool {
		r1, r2 := float64(a%1001)/1000, float64(b%1001)/1000
		if r1 > r2 {
			r1, r2 = r2, r1
		}
		low, crit := float64(lo%1001)/1000, float64(cr%1001)/1000
		if crit > low {
			low, crit = crit, low
		}
		th := quotaThresholds{Low: low, Critical: crit}
		return severity[levelOf(r1, th)] >= severity[levelOf(r2, th)]
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGroupQuotaBindingBucket(t *testing.T) {
	t5 := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	tw := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	g := QuotaGroup{Name: "Gemini", Buckets: []QuotaBucket{
		{ID: "5h", RemainingFraction: 0.03, ResetTime: t5},
		{ID: "weekly", RemainingFraction: 0.68, ResetTime: tw},
	}}
	before := t5.Add(-time.Hour)
	gq := groupQuota(g, before, testThresholds)
	if gq.Binding.ID != "5h" || gq.Level != QuotaCritical || gq.Percent() != 3 || gq.Buckets[0].Refilled {
		t.Fatalf("before reset: %+v (percent %d)", gq, gq.Percent())
	}
	// Past the 5h reset that bucket counts as full, so the weekly one binds.
	gq = groupQuota(g, t5.Add(time.Minute), testThresholds)
	if gq.Binding.ID != "weekly" || gq.Level != QuotaOK || !gq.Buckets[0].Refilled || gq.Buckets[1].Refilled || gq.Percent() != 68 {
		t.Fatalf("after reset: %+v", gq)
	}

	// A tie binds the later reset.
	tie := QuotaGroup{Buckets: []QuotaBucket{
		{ID: "early", RemainingFraction: 0.1, ResetTime: t5},
		{ID: "late", RemainingFraction: 0.1, ResetTime: tw},
	}}
	if got := groupQuota(tie, before, testThresholds).Binding.ID; got != "late" {
		t.Fatalf("tie binding = %s, want late", got)
	}
	// Percent rounds down: 0.0356 reads 3, and 0.29 does not read 28 through float error.
	if p := (GroupQuota{Remaining: 0.0356}).Percent(); p != 3 {
		t.Fatalf("Percent(0.0356) = %d, want 3", p)
	}
	if p := (GroupQuota{Remaining: 0.29}).Percent(); p != 29 {
		t.Fatalf("Percent(0.29) = %d, want 29", p)
	}
}

func TestGroupForModel(t *testing.T) {
	twoGroups := []QuotaGroup{{Name: "Gemini Models"}, {Name: "Claude and GPT Models"}}
	threeGroups1410 := []QuotaGroup{
		{
			Name:        "Gemini Models",
			Description: "Models within this group: Gemini Flash, Gemini Pro",
			Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
		{
			Name:        "Claude and GPT models",
			Description: "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
			Buckets:     []QuotaBucket{{ID: "3p-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
		{
			Name:        "Gemini 4 Argon",
			Description: "For paid Pro plans, Argon quota is separate. Higher Argon quota (included as part of the larger Gemini quota bucket) is available on Ultra plans.",
			Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly"}},
		},
	}
	versionedProOnProPlans := []QuotaGroup{
		{
			Name:        "Gemini Models",
			Description: "Models within this group: Gemini 3.8 Flash, Gemini 3.1 Pro on Pro plans",
			Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
		{
			Name:        "Gemini 4 Argon",
			Description: "For paid Pro subscribers and Pro membership users on Pro plans, Argon quota is separate.",
			Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly"}},
		},
	}
	subscriptionOnlyNoProModel := []QuotaGroup{
		{
			Name:        "Gemini Flash",
			Description: "Models within this group: Gemini 3.8 Flash (included with Pro subscription, Pro membership, and Pro plans)",
			Buckets:     []QuotaBucket{{ID: "gemini-flash-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
		{
			Name:        "Gemini 4 Argon",
			Description: "For paid Pro subscribers, Pro users, and Pro plans, Argon quota is separate.",
			Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly"}},
		},
	}
	ultraPlanDescriptiveTextNoArgonGroup := []QuotaGroup{
		{
			Name:        "Gemini Models",
			Description: "Models within this group: Gemini Flash, Gemini Pro. Higher Argon quota (included as part of the larger Gemini quota bucket) is available on Ultra plans.",
			Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
		{
			Name:        "Claude and GPT models",
			Description: "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
			Buckets:     []QuotaBucket{{ID: "3p-weekly", Name: "Weekly Limit Remaining", Window: "weekly"}},
		},
	}
	tests := []struct {
		name       string
		model      string
		groups     []QuotaGroup
		want       int
		ok         bool
		wantStatus QuotaMatchStatus
	}{
		// Single unified Gemini group + Claude/GPT group (agy 1.2.16 shape)
		{"2g flash id", "gemini-3.8-flash-high", twoGroups, 0, true, QuotaMatchExact},
		{"2g flash label", "Gemini 3.8 Flash (High)", twoGroups, 0, true, QuotaMatchExact},
		{"2g pro id", "gemini-3.1-pro-high", twoGroups, 0, true, QuotaMatchExact},
		{"2g argon missing group is unknown", "gemini-4-argon-medium", twoGroups, 0, false, QuotaMatchUnknown},
		{"2g claude opus", "claude-opus-4-6-thinking", twoGroups, 1, true, QuotaMatchExact},
		{"2g claude sonnet", "claude-sonnet-5-5-low", twoGroups, 1, true, QuotaMatchExact},
		{"2g claude label", "Claude Sonnet 5.5 (Low)", twoGroups, 1, true, QuotaMatchExact},
		{"2g gpt oss id", "gpt-oss-120b-medium", twoGroups, 1, true, QuotaMatchExact},
		{"2g gpt oss label", "GPT-OSS 120B (Medium)", twoGroups, 1, true, QuotaMatchExact},
		{"2g unknown claude-x remains unknown", "claude-x", twoGroups, 0, false, QuotaMatchUnknown},
		{"2g unknown gpt-x remains unknown", "gpt-x", twoGroups, 0, false, QuotaMatchUnknown},
		{"2g unknown gemini-x remains unknown", "gemini-x", twoGroups, 0, false, QuotaMatchUnknown},

		// Separate Gemini Flash/Pro vs Gemini 4 Argon vs Claude/GPT (agy 1.4.10 shape)
		{"3g flash high", "gemini-3.8-flash-high", threeGroups1410, 0, true, QuotaMatchExact},
		{"3g flash tab row", "gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)", threeGroups1410, 0, true, QuotaMatchExact},
		{"3g flash label", "Gemini 3.8 Flash (Low)", threeGroups1410, 0, true, QuotaMatchExact},
		{"3g pro high", "gemini-3.1-pro-high", threeGroups1410, 0, true, QuotaMatchExact},
		{"3g pro label", "Gemini 3.1 Pro (High)", threeGroups1410, 0, true, QuotaMatchExact},
		{"3g argon medium", "gemini-4-argon-medium", threeGroups1410, 2, true, QuotaMatchExact},
		{"3g argon tab row", "gemini-4-argon-high\tGemini 4 Argon (High)", threeGroups1410, 2, true, QuotaMatchExact},
		{"3g argon label", "Gemini 4 Argon (Low)", threeGroups1410, 2, true, QuotaMatchExact},
		{"3g claude opus", "claude-opus-5-5-low", threeGroups1410, 1, true, QuotaMatchExact},
		{"3g claude sonnet", "claude-sonnet-5-5-medium", threeGroups1410, 1, true, QuotaMatchExact},
		{"3g gpt oss", "gpt-oss-120b-medium", threeGroups1410, 1, true, QuotaMatchExact},

		// Versioned Pro on Pro plans and subscription-only wording
		{"versioned Pro on Pro plans matches Pro group", "gemini-3.1-pro-high", versionedProOnProPlans, 0, true, QuotaMatchExact},
		{"versioned Pro label on Pro plans matches Pro group", "Gemini 3.1 Pro (High)", versionedProOnProPlans, 0, true, QuotaMatchExact},
		{"subscription-only Pro wording never matches Gemini Pro", "gemini-3.1-pro-high", subscriptionOnlyNoProModel, 0, false, QuotaMatchUnknown},
		{"subscription-only Pro wording still matches Flash", "gemini-3.8-flash-high", subscriptionOnlyNoProModel, 0, true, QuotaMatchExact},
		{"subscription-only Pro wording still matches Argon via bucket/name", "gemini-4-argon-medium", subscriptionOnlyNoProModel, 1, true, QuotaMatchExact},

		// Descriptive Ultra plan text never infers Argon -> Gemini Models
		{"descriptive Ultra plan text does not map Argon when Argon group missing", "gemini-4-argon-medium", ultraPlanDescriptiveTextNoArgonGroup, 0, false, QuotaMatchUnknown},
		{"descriptive Ultra plan text still maps Flash", "gemini-3.8-flash-high", ultraPlanDescriptiveTextNoArgonGroup, 0, true, QuotaMatchExact},
		{"descriptive Ultra plan text still maps Pro", "gemini-3.1-pro-high", ultraPlanDescriptiveTextNoArgonGroup, 0, true, QuotaMatchExact},

		// Unknown, unpinned, missing-group, and ambiguous cases
		{"bare flash is unknown", "flash", threeGroups1410, 0, false, QuotaMatchUnknown},
		{"bare pro is unknown", "pro", threeGroups1410, 0, false, QuotaMatchUnknown},
		{"unknown custom model", "unknown-custom-model", threeGroups1410, 0, false, QuotaMatchUnknown},
		{"unknown claude-x", "claude-x", threeGroups1410, 0, false, QuotaMatchUnknown},
		{"unknown gemini-ultra", "gemini-ultra", threeGroups1410, 0, false, QuotaMatchUnknown},
		{"empty model is unpinned", "", threeGroups1410, 0, false, QuotaMatchUnpinned},
		{"missing Argon group in 3g[:2]", "gemini-4-argon-medium", threeGroups1410[:2], 0, false, QuotaMatchUnknown},
		{"missing Claude/GPT group", "claude-sonnet-5-5-low", []QuotaGroup{threeGroups1410[0], threeGroups1410[2]}, 0, false, QuotaMatchUnknown},
		{"ambiguous duplicate Gemini groups", "gemini-3.8-flash-high", []QuotaGroup{{Name: "Gemini A"}, {Name: "Gemini B"}}, 0, false, QuotaMatchAmbiguous},
		{"ambiguous duplicate Argon groups", "gemini-4-argon-medium", []QuotaGroup{{Name: "Gemini 4 Argon A"}, {Name: "Gemini 4 Argon B"}}, 0, false, QuotaMatchAmbiguous},
		{"no Gemini group", "gemini-3.8-flash-high", []QuotaGroup{{Name: "Claude"}}, 0, false, QuotaMatchUnknown},
		{"nil groups", "gemini-3.8-flash-high", nil, 0, false, QuotaMatchUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := matchQuotaGroup(tt.groups, tt.model)
			got, status, ok := groupForModel(tt.groups, tt.model)
			if ok != tt.ok || m.OK != tt.ok || status != tt.wantStatus || m.Status != tt.wantStatus || (ok && (got != tt.want || m.Index != tt.want)) {
				t.Errorf("groupForModel(%q) = {idx:%d status:%q ok:%v}, want {idx:%d status:%q ok:%v}",
					tt.model, got, status, ok, tt.want, tt.wantStatus, tt.ok)
			}
		})
	}
}

func quotaFixture(checked time.Time, remaining float64, reset time.Time) QuotaSnapshot {
	return QuotaSnapshot{CheckedAt: checked, Groups: []QuotaGroup{
		{Name: "Gemini Models", Buckets: []QuotaBucket{{ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: remaining, ResetTime: reset}}},
		{Name: "Claude and GPT Models", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 1}}},
	}}
}

func TestQuotaDecision(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	const model = "gemini-3.8-flash-high"
	ambiguousSnap := QuotaSnapshot{CheckedAt: now, Groups: []QuotaGroup{
		{Name: "Gemini A", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 1, ResetTime: reset}}},
		{Name: "Gemini B", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 1, ResetTime: reset}}},
	}}
	tests := []struct {
		name       string
		snap       QuotaSnapshot
		model      string
		refuse     bool
		wantStatus QuotaMatchStatus
	}{
		{"ok passes", quotaFixture(now, 0.9, reset), model, false, QuotaMatchExact},
		{"low refuses", quotaFixture(now, 0.2, reset), model, true, QuotaMatchExact},
		{"critical refuses", quotaFixture(now, 0.03, reset), model, true, QuotaMatchExact},
		{"exhausted refuses", quotaFixture(now, 0, reset), model, true, QuotaMatchExact},
		{"claude ok group passes", quotaFixture(now, 0, reset), "claude-sonnet-5-5-low", false, QuotaMatchExact},
		{"unmapped model refuses optional", quotaFixture(now, 0.9, reset), "unknown-model-x", true, QuotaMatchUnknown},
		{"unknown claude-x refuses optional", quotaFixture(now, 0.9, reset), "claude-x", true, QuotaMatchUnknown},
		{"empty model refuses optional", quotaFixture(now, 0.9, reset), "", true, QuotaMatchUnpinned},
		{"ambiguous model refuses optional", ambiguousSnap, model, true, QuotaMatchAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refuse, _, match := quotaDecision(tt.snap, now, tt.model, testThresholds)
			if refuse != tt.refuse || match.Status != tt.wantStatus {
				t.Fatalf("refuse = %v (status %q), want %v (status %q)", refuse, match.Status, tt.refuse, tt.wantStatus)
			}
		})
	}
}

func TestQuotaDecisionTransitions(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	reset := t0.Add(30 * time.Minute)
	const model = "gemini-3.8-flash-high"
	decide := func(s QuotaSnapshot, now time.Time) bool {
		r, _, _ := quotaDecision(s, now, model, testThresholds)
		return r
	}
	low := quotaFixture(t0, 0.03, reset)

	// Past the binding bucket's reset a low snapshot no longer refuses.
	if !decide(low, t0) {
		t.Fatal("low snapshot should refuse before the reset")
	}
	if decide(low, reset.Add(time.Second)) {
		t.Fatal("low snapshot should pass after the binding bucket reset")
	}
	// A new snapshot replaces an ok one.
	ok := quotaFixture(t0, 0.9, reset)
	if decide(ok, t0) {
		t.Fatal("ok snapshot refused")
	}
	if !decide(quotaFixture(t0.Add(time.Minute), 0.03, reset), t0.Add(time.Minute)) {
		t.Fatal("new low snapshot should refuse")
	}
}

// quotaTestManager returns a manager whose probe is stub and whose clock is the
// returned pointer-backed fake.
func quotaTestManager(t *testing.T, interval time.Duration, stub func(context.Context) ([]byte, error)) (*Manager, *atomic.Int64) {
	t.Helper()
	m := New(config.Config{StateDir: t.TempDir(), UsageInterval: interval, QuotaLow: 0.25, QuotaCritical: 0.05})
	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	var offset atomic.Int64 // nanoseconds past base
	m.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	m.readUsage = stub
	return m, &offset
}

func okStub(calls *atomic.Int32) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(measuredUsageEnvelope), nil
	}
}

func TestUsageCacheTTLAndMaxAge(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, 0, okStub(&calls))
	ctx := t.Context()
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	off.Store(int64(30 * time.Second))
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls inside the TTL = %d, want 1", calls.Load())
	}
	off.Store(int64(61 * time.Second))
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls after the TTL = %d, want 2", calls.Load())
	}
	// A 10m max_age accepts a 5m-old snapshot without probing.
	off.Store(int64(61*time.Second + 5*time.Minute))
	if _, err := m.Usage(ctx, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls with max_age 10m = %d, want 2", calls.Load())
	}
	// max_age 0 means the 60s default: a 5m-old snapshot is refreshed, a 30s-old
	// one is reused (the 10s floor alone would refresh it).
	t0 := 61*time.Second + 5*time.Minute
	usageAt := func(at, maxAge time.Duration, want int32, what string) {
		t.Helper()
		off.Store(int64(at))
		if _, err := m.Usage(ctx, maxAge); err != nil {
			t.Fatal(err)
		}
		if got := calls.Load(); got != want {
			t.Fatalf("%s: calls = %d, want %d", what, got, want)
		}
	}
	usageAt(t0, 0, 3, "max_age 0 on a 5m-old snapshot")
	usageAt(t0+30*time.Second, 0, 3, "max_age 0 on a 30s-old snapshot")
	usageAt(t0+61*time.Second, 0, 4, "max_age 0 on a 61s-old snapshot")
	// A positive max_age under 10s is raised to the floor: a 5s-old snapshot is
	// reused for max_age 1s.
	usageAt(t0+66*time.Second, time.Second, 4, "max_age 1s on a 5s-old snapshot")
}

func TestUsageNegativeCache(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	m, off := quotaTestManager(t, 0, func(context.Context) ([]byte, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("agy usage: boom")
		}
		return []byte(measuredUsageEnvelope), nil
	})
	ctx := t.Context()
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	off.Store(int64(2 * time.Minute))
	_, err1 := m.Usage(ctx, time.Minute)
	off.Store(int64(2*time.Minute + 10*time.Second))
	_, err2 := m.Usage(ctx, time.Minute)
	if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
		t.Fatalf("errors = %v, %v; want the same error twice", err1, err2)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one good, one failed)", calls.Load())
	}
	off.Store(int64(2*time.Minute + 31*time.Second))
	if _, err := m.Usage(ctx, time.Minute); err == nil {
		t.Fatal("expected the failure to persist")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls after the backoff = %d, want 3", calls.Load())
	}
	// The earlier good snapshot is still trusted until the max age.
	if _, ok := m.CachedQuota(); !ok {
		t.Fatal("good snapshot dropped after a failed probe")
	}
	off.Store(int64(11 * time.Minute))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("snapshot past the max age was still trusted")
	}
}

// TestUsageSingleflight: the caller that starts a probe gives up while the probe
// is blocked, a second caller then finds the same flight in progress instead of
// starting another, and the probe the starter abandoned still completes and
// fills the cache. Every step happens while the stub is blocked, so none of it
// depends on timing: the second caller is observed through the in-flight
// pointer, which a build that does not share the flight would replace.
func TestUsageSingleflight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var calls atomic.Int32
	// The stub honours its context, so a probe that ran on the starter's
	// context would fail when the starter cancels and leave the cache empty.
	m, _ := quotaTestManager(t, 0, func(ctx context.Context) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return []byte(measuredUsageEnvelope), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	inflight := func() *quotaFlight {
		m.quota.mu.Lock()
		defer m.quota.mu.Unlock()
		return m.quota.inflight
	}

	starterCtx, cancelStarter := context.WithCancel(t.Context())
	starterErr := make(chan error, 1)
	go func() { _, err := m.Usage(starterCtx, time.Minute); starterErr <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never started")
	}
	flight := inflight()
	if flight == nil {
		t.Fatal("no flight in progress while the probe is blocked")
	}

	// The starter gives up while the probe is still blocked: it must return its
	// own context error rather than wait for the probe.
	cancelStarter()
	select {
	case err := <-starterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("starter error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the starter stayed blocked on the probe after its context was cancelled")
	}

	// A second caller joins the flight in progress. Its context is already
	// cancelled so the call returns at once, but it has by then either joined the
	// flight or replaced it with one of its own.
	gone, cancelGone := context.WithCancel(t.Context())
	cancelGone()
	if _, err := m.Usage(gone, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("second caller error = %v, want context.Canceled", err)
	}
	if inflight() != flight {
		t.Fatal("the second caller started its own probe instead of joining the flight")
	}

	close(release)
	snap, err := m.Usage(t.Context(), time.Minute)
	if err != nil || len(snap.Groups) != 2 {
		t.Fatalf("Usage after the abandoned probe finished = %v, %+v", err, snap)
	}
	if calls.Load() != 1 {
		t.Fatalf("probes = %d, want 1 shared", calls.Load())
	}
}

func TestUsageUnsafeLatch(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, time.Minute, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"conversation_id":"c","status":"SUCCESS","num_turns":1}`), nil
	})
	_, err := m.Usage(t.Context(), time.Minute)
	if !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("first Usage error = %v, want the model-turn latch", err)
	}
	// Past the failure backoff, so only the latch can stop a second probe.
	off.Store(int64(usageErrorBackoff + time.Second))
	if _, err := m.Usage(t.Context(), time.Minute); !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("second Usage error = %v", err)
	}
	m.CachedQuota()
	done := make(chan struct{})
	go func() { m.runUsageRefresher(t.Context(), time.Minute); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher kept running after the latch")
	}
	if calls.Load() != 1 {
		t.Fatalf("probes = %d after the latch, want exactly 1", calls.Load())
	}
}

// TestCachedQuotaIsPureRead: CachedQuota never starts a probe, with no snapshot
// or a stale one, even with background probing on; the refresher owns that.
func TestCachedQuotaIsPureRead(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, time.Minute, okStub(&calls))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("trusted a snapshot that was never taken")
	}
	if _, err := m.Usage(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	off.Store(int64(5 * time.Minute))
	if _, ok := m.CachedQuota(); !ok {
		t.Fatal("snapshot inside the max age was not trusted")
	}
	off.Store(int64(time.Hour))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("snapshot past the max age was trusted")
	}
	m.quota.mu.Lock()
	inflight := m.quota.inflight
	m.quota.mu.Unlock()
	if calls.Load() != 1 || inflight != nil {
		t.Fatalf("probes = %d, in flight = %v; CachedQuota must not probe", calls.Load(), inflight != nil)
	}
}

// TestQuotaTrustAgeFollowsInterval pins quotaMaxAge: a snapshot is trusted for
// twice the refresh interval, not for the 10m floor alone, and both the cached
// read and the optional-run guard use that age. With no snapshot at all the
// guard lets the run through.
func TestQuotaTrustAgeFollowsInterval(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, 30*time.Minute, okStub(&calls))
	req := StartRequest{Priority: PriorityOptional, Model: "gemini-3.8-flash-high"}
	if err := m.checkPriority(req); err != nil {
		t.Fatalf("optional run refused with no snapshot: %v", err)
	}
	m.quota.mu.Lock()
	m.quota.snap = quotaFixture(m.now(), 0.03, m.now().Add(24*time.Hour))
	m.quota.mu.Unlock()
	for _, tc := range []struct {
		at      time.Duration
		trusted bool
	}{
		{45 * time.Minute, true}, // between the interval and twice it
		{59 * time.Minute, true},
		{61 * time.Minute, false}, // past twice the interval
	} {
		off.Store(int64(tc.at))
		if _, ok := m.CachedQuota(); ok != tc.trusted {
			t.Errorf("at %s: CachedQuota trusted = %v, want %v", tc.at, ok, tc.trusted)
		}
		if refused := m.checkPriority(req) != nil; refused != tc.trusted {
			t.Errorf("at %s: optional run refused = %v, want %v", tc.at, refused, tc.trusted)
		}
	}
}

func TestRunUsageRefresher(t *testing.T) {
	var calls atomic.Int32
	m, _ := quotaTestManager(t, 0, okStub(&calls))
	m.runUsageRefresher(t.Context(), 0)
	if calls.Load() != 0 {
		t.Fatal("zero interval probed")
	}

	// waitCalls polls until at least want probes ran or 5s pass.
	waitCalls := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := calls.Load(); got < want {
			t.Fatalf("probes = %d, want at least %d", got, want)
		}
	}
	// The first probe runs at once, not after the first tick: an hour-long
	// interval never ticks inside the wait.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.runUsageRefresher(ctx, time.Hour); close(done) }()
	waitCalls(1)
	cancel()
	<-done

	// Then it probes again on every tick.
	ctx, cancel = context.WithCancel(t.Context())
	done = make(chan struct{})
	go func() { m.runUsageRefresher(ctx, 20*time.Millisecond); close(done) }()
	waitCalls(3)
	cancel()
	<-done
	// A probe started just before the cancel may still be running; let it
	// finish so only a probe started after the cancel can change the count.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		m.quota.mu.Lock()
		idle := m.quota.inflight == nil
		m.quota.mu.Unlock()
		if idle {
			break
		}
	}
	n := calls.Load()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("refresher probed after its context ended")
	}
}

func TestCheckPriorityRefusalMessage(t *testing.T) {
	var calls atomic.Int32
	m, _ := quotaTestManager(t, 0, okStub(&calls))
	if _, err := m.Usage(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "gemini-3.8-flash-high"})
	if _, ok := errors.AsType[*QuotaRefusalError](err); !ok {
		t.Fatalf("error = %v, want *QuotaRefusalError", err)
	}
	for _, want := range []string{`"Gemini Models"`, "critical", "3%", "2026-10-04T11:32:45Z", "5h limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityNormal, Model: "gemini-3.8-flash-high"}); err != nil {
		t.Fatalf("normal run refused: %v", err)
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "claude-sonnet-5-5-low"}); err != nil {
		t.Fatalf("optional run on an OK Claude/GPT group refused: %v", err)
	}

	// Unknown and unpinned models must refuse/defer optional runs clearly without
	// claiming quota is exhausted, while still allowing normal runs.
	unkErr := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "unknown-model-x"})
	unkRefusal, ok := errors.AsType[*QuotaRefusalError](unkErr)
	if !ok || unkRefusal.MatchStatus != QuotaMatchUnknown || unkRefusal.Level != "" {
		t.Fatalf("unknown optional error = %v (%+v), want QuotaMatchUnknown with empty Level", unkErr, unkRefusal)
	}
	if !strings.Contains(unkErr.Error(), "unknown") || !strings.Contains(unkErr.Error(), "unknown-model-x") {
		t.Errorf("unknown refusal message = %q, want 'unknown' and model name", unkErr.Error())
	}
	for _, forbidden := range []string{"exhausted", "critical", "low"} {
		if strings.Contains(unkErr.Error(), forbidden) {
			t.Errorf("unknown refusal message %q must not claim quota is %q", unkErr.Error(), forbidden)
		}
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityNormal, Model: "unknown-model-x"}); err != nil {
		t.Fatalf("normal run on unknown model refused: %v", err)
	}

	unpinnedErr := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: ""})
	unpinnedRefusal, ok := errors.AsType[*QuotaRefusalError](unpinnedErr)
	if !ok || unpinnedRefusal.MatchStatus != QuotaMatchUnpinned || unpinnedRefusal.Level != "" {
		t.Fatalf("unpinned optional error = %v (%+v), want QuotaMatchUnpinned with empty Level", unpinnedErr, unpinnedRefusal)
	}
	if !strings.Contains(unpinnedErr.Error(), "unpinned") {
		t.Errorf("unpinned refusal message = %q, want 'unpinned'", unpinnedErr.Error())
	}
	for _, forbidden := range []string{"exhausted", "critical", "low"} {
		if strings.Contains(unpinnedErr.Error(), forbidden) {
			t.Errorf("unpinned refusal message %q must not claim quota is %q", unpinnedErr.Error(), forbidden)
		}
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityNormal, Model: ""}); err != nil {
		t.Fatalf("normal run on unpinned model refused: %v", err)
	}
}

// TestQuotaMultiGroupFlashProVsArgonAndClaudeGPT verifies:
//   - agy 1.4.10's 3-group /usage envelope (including a disabled 5h bucket);
//   - exhausted Flash/Pro with available Argon (and vice versa);
//   - Claude and GPT models mapped to "Claude and GPT models";
//   - unknown, unpinned, ambiguous, and missing-group models refusing/deferring
//     optional runs clearly without claiming quota is exhausted, while normal
//     runs proceed;
//   - all pools exhausted simultaneously;
//   - versioned Pro on Pro plans vs subscription-only Pro wording;
//   - rounded 0% (0.004 -> 0% critical) vs 0 (0% exhausted);
//   - priority normal vs optional, and no hidden model fallback/mutation.
func TestQuotaMultiGroupFlashProVsArgonAndClaudeGPT(t *testing.T) {
	const envelopeFlashExhaustedArgonOK = `{"conversation_id":"","status":"SUCCESS","response":"x","duration_seconds":0,"num_turns":0,
"usage":{"input_tokens":0},"command":{"name":"usage","data":{"description":"d","groups":[
{"name":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[
 {"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0,"reset_time":"2026-10-31T12:11:13Z"},
 {"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","disabled":true,"remaining_fraction":0.9912158250808716}]},
{"name":"Claude and GPT models","description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS","buckets":[
 {"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.95,"reset_time":"2026-11-05T10:23:24Z"},
 {"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.91,"reset_time":"2026-10-29T15:23:24Z"}]},
{"name":"Gemini 4 Argon","description":"For paid Pro plans, Argon quota is separate. Higher Argon quota (included as part of the larger Gemini quota bucket) is available on Ultra plans.","buckets":[
 {"id":"gemini-argon-wtu-weekly","name":"Gemini 4 Argon Weekly Limit","window":"weekly","remaining_fraction":0.722382664680481,"reset_time":"2026-11-05T09:21:10Z"}]}]}}}`

	t.Run("exhausted Flash/Pro blocks optional Flash/Pro, allows optional Argon and Claude/GPT, and defers unknown/unpinned", func(t *testing.T) {
		var calls atomic.Int32
		m, _ := quotaTestManager(t, 0, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte(envelopeFlashExhaustedArgonOK), nil
		})
		snap, err := m.Usage(t.Context(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Groups) != 3 {
			t.Fatalf("len(snap.Groups) = %d, want 3", len(snap.Groups))
		}
		// Disabled 5h bucket on Gemini Models must be skipped.
		if len(snap.Groups[0].Buckets) != 1 || snap.Groups[0].Buckets[0].ID != "gemini-weekly" {
			t.Fatalf("Gemini Models buckets = %+v, want only active gemini-weekly", snap.Groups[0].Buckets)
		}

		for _, blockedModel := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.1-pro-high"} {
			req := StartRequest{Priority: PriorityOptional, Model: blockedModel, Cwd: t.TempDir()}
			err := m.checkPriority(req)
			refusal, ok := errors.AsType[*QuotaRefusalError](err)
			if !ok {
				t.Fatalf("checkPriority(%q, optional) = %v, want *QuotaRefusalError", blockedModel, err)
			}
			if refusal.Group != "Gemini Models" || refusal.Level != QuotaExhausted || refusal.Percent != 0 || refusal.MatchStatus != QuotaMatchExact {
				t.Fatalf("refusal for %q = %+v, want group Gemini Models exhausted 0%% exact", blockedModel, refusal)
			}
			// Normal priority must still be allowed and must never mutate req.Model.
			normReq := StartRequest{Priority: PriorityNormal, Model: blockedModel, Cwd: t.TempDir()}
			if err := m.checkPriority(normReq); err != nil {
				t.Fatalf("checkPriority(%q, normal) = %v, want nil", blockedModel, err)
			}
			if normReq.Model != blockedModel {
				t.Fatalf("normReq.Model mutated to %q, want %q", normReq.Model, blockedModel)
			}
		}

		for _, allowedModel := range []string{
			"gemini-4-argon-medium",
			"gemini-4-argon-high",
			"claude-sonnet-5-5-low",
			"claude-opus-5-5-medium",
			"gpt-oss-120b-medium",
		} {
			req := StartRequest{Priority: PriorityOptional, Model: allowedModel, Cwd: t.TempDir()}
			if err := m.checkPriority(req); err != nil {
				t.Fatalf("checkPriority(%q, optional) = %v, want nil", allowedModel, err)
			}
			if req.Model != allowedModel {
				t.Fatalf("req.Model mutated to %q, want %q", req.Model, allowedModel)
			}
		}

		for _, deferred := range []struct {
			model      string
			wantStatus QuotaMatchStatus
		}{
			{"unknown-model-xyz", QuotaMatchUnknown},
			{"claude-x", QuotaMatchUnknown},
			{"gemini-ultra", QuotaMatchUnknown},
			{"", QuotaMatchUnpinned},
		} {
			req := StartRequest{Priority: PriorityOptional, Model: deferred.model, Cwd: t.TempDir()}
			err := m.checkPriority(req)
			refusal, ok := errors.AsType[*QuotaRefusalError](err)
			if !ok || refusal.MatchStatus != deferred.wantStatus || refusal.Level != "" {
				t.Fatalf("checkPriority(%q, optional) = %v (%+v), want status %q with empty Level", deferred.model, err, refusal, deferred.wantStatus)
			}
			for _, forbidden := range []string{"exhausted", "critical", "low"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("deferral message %q for %q must not claim quota is %q", err.Error(), deferred.model, forbidden)
				}
			}
			normReq := StartRequest{Priority: PriorityNormal, Model: deferred.model, Cwd: t.TempDir()}
			if err := m.checkPriority(normReq); err != nil {
				t.Fatalf("checkPriority(%q, normal) = %v, want nil", deferred.model, err)
			}
			if normReq.Model != deferred.model {
				t.Fatalf("normReq.Model mutated to %q, want %q", normReq.Model, deferred.model)
			}
		}
	})

	t.Run("vice versa: exhausted Argon and low Claude/GPT while Flash/Pro is available", func(t *testing.T) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		reset := now.Add(48 * time.Hour)
		m, _ := quotaTestManager(t, 0, nil)
		m.quota.snap = QuotaSnapshot{
			CheckedAt: m.now(),
			Groups: []QuotaGroup{
				{
					Name:        "Gemini Models",
					Description: "Models within this group: Gemini Flash, Gemini Pro",
					Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly", RemainingFraction: 0.85, ResetTime: reset}},
				},
				{
					Name:        "Claude and GPT models",
					Description: "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
					Buckets:     []QuotaBucket{{ID: "3p-5h", Name: "Five Hour Limit Remaining", Window: "5h", RemainingFraction: 0.12, ResetTime: reset}},
				},
				{
					Name:        "Gemini 4 Argon",
					Description: "For paid Pro plans, Argon quota is separate.",
					Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly", RemainingFraction: 0, ResetTime: reset}},
				},
			},
		}

		// Argon optional is refused naming "Gemini 4 Argon" at exhausted (0%).
		err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "gemini-4-argon-medium"})
		argonRefusal, ok := errors.AsType[*QuotaRefusalError](err)
		if !ok || argonRefusal.Group != "Gemini 4 Argon" || argonRefusal.Level != QuotaExhausted || argonRefusal.Percent != 0 {
			t.Fatalf("argon refusal = %v (%+v), want Gemini 4 Argon exhausted 0%%", err, argonRefusal)
		}

		// Claude and GPT optional are refused naming "Claude and GPT models" at low (12%).
		for _, thirdParty := range []string{"claude-sonnet-5-5-low", "gpt-oss-120b-medium"} {
			err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: thirdParty})
			tpRefusal, ok := errors.AsType[*QuotaRefusalError](err)
			if !ok || tpRefusal.Group != "Claude and GPT models" || tpRefusal.Level != QuotaLow || tpRefusal.Percent != 12 {
				t.Fatalf("%s refusal = %v (%+v), want Claude and GPT models low 12%%", thirdParty, err, tpRefusal)
			}
		}

		// Flash and Pro optional succeed because "Gemini Models" has 85% left.
		for _, okModel := range []string{"gemini-3.8-flash-high", "gemini-3.1-pro-high"} {
			if err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: okModel}); err != nil {
				t.Fatalf("checkPriority(%q, optional) = %v, want nil", okModel, err)
			}
		}
	})

	t.Run("all pools exhausted, ambiguous groups, and missing model groups", func(t *testing.T) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		reset := now.Add(48 * time.Hour)
		m, _ := quotaTestManager(t, 0, nil)
		m.quota.snap = QuotaSnapshot{
			CheckedAt: m.now(),
			Groups: []QuotaGroup{
				{
					Name:        "Gemini Models",
					Description: "Models within this group: Gemini Flash, Gemini Pro",
					Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly", RemainingFraction: 0, ResetTime: reset}},
				},
				{
					Name:        "Claude and GPT models",
					Description: "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
					Buckets:     []QuotaBucket{{ID: "3p-weekly", Name: "Weekly Limit Remaining", Window: "weekly", RemainingFraction: 0, ResetTime: reset}},
				},
				{
					Name:        "Gemini 4 Argon",
					Description: "For paid Pro plans, Argon quota is separate.",
					Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly", RemainingFraction: 0, ResetTime: reset}},
				},
			},
		}

		for _, tc := range []struct {
			model     string
			wantGroup string
		}{
			{"gemini-3.8-flash-high", "Gemini Models"},
			{"gemini-3.1-pro-high", "Gemini Models"},
			{"claude-sonnet-5-5-low", "Claude and GPT models"},
			{"gpt-oss-120b-medium", "Claude and GPT models"},
			{"gemini-4-argon-medium", "Gemini 4 Argon"},
		} {
			err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: tc.model})
			refusal, ok := errors.AsType[*QuotaRefusalError](err)
			if !ok || refusal.Group != tc.wantGroup || refusal.Level != QuotaExhausted || refusal.Percent != 0 || refusal.MatchStatus != QuotaMatchExact {
				t.Fatalf("all-exhausted checkPriority(%q, optional) = %v (%+v), want %q exhausted 0%%", tc.model, err, refusal, tc.wantGroup)
			}
			normReq := StartRequest{Priority: PriorityNormal, Model: tc.model}
			if err := m.checkPriority(normReq); err != nil {
				t.Fatalf("all-exhausted checkPriority(%q, normal) = %v, want nil", tc.model, err)
			}
		}

		// Missing Argon group (even when Gemini Models description contains Ultra plan prose):
		// Argon optional must be deferred as QuotaMatchUnknown, never mapped to Gemini Models.
		m.quota.snap = QuotaSnapshot{
			CheckedAt: m.now(),
			Groups: []QuotaGroup{
				{
					Name:        "Gemini Models",
					Description: "Models within this group: Gemini Flash, Gemini Pro. Higher Argon quota (included as part of the larger Gemini quota bucket) is available on Ultra plans.",
					Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly", RemainingFraction: 0.9, ResetTime: reset}},
				},
			},
		}
		for _, missingModel := range []string{"gemini-4-argon-medium", "claude-sonnet-5-5-low", "gpt-oss-120b-medium"} {
			err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: missingModel})
			refusal, ok := errors.AsType[*QuotaRefusalError](err)
			if !ok || refusal.MatchStatus != QuotaMatchUnknown || refusal.Level != "" {
				t.Fatalf("missing-group checkPriority(%q, optional) = %v (%+v), want QuotaMatchUnknown", missingModel, err, refusal)
			}
		}

		// Ambiguous groups: duplicate Gemini groups cause optional runs to defer with QuotaMatchAmbiguous.
		m.quota.snap = QuotaSnapshot{
			CheckedAt: m.now(),
			Groups: []QuotaGroup{
				{Name: "Gemini A", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 0.9, ResetTime: reset}}},
				{Name: "Gemini B", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 0.9, ResetTime: reset}}},
			},
		}
		ambErr := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "gemini-3.8-flash-high"})
		ambRefusal, ok := errors.AsType[*QuotaRefusalError](ambErr)
		if !ok || ambRefusal.MatchStatus != QuotaMatchAmbiguous || ambRefusal.Level != "" {
			t.Fatalf("ambiguous checkPriority = %v (%+v), want QuotaMatchAmbiguous", ambErr, ambRefusal)
		}
		if !strings.Contains(ambErr.Error(), "ambiguous") {
			t.Errorf("ambiguous refusal message = %q, want 'ambiguous'", ambErr.Error())
		}
		for _, forbidden := range []string{"exhausted", "critical", "low"} {
			if strings.Contains(ambErr.Error(), forbidden) {
				t.Errorf("ambiguous refusal message %q must not claim quota is %q", ambErr.Error(), forbidden)
			}
		}
		if err := m.checkPriority(StartRequest{Priority: PriorityNormal, Model: "gemini-3.8-flash-high"}); err != nil {
			t.Fatalf("normal run on ambiguous group refused: %v", err)
		}
	})

	t.Run("rounded 0 percent critical is distinct from 0 exhausted", func(t *testing.T) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		reset := now.Add(2 * time.Hour)
		snap := QuotaSnapshot{
			CheckedAt: now,
			Groups: []QuotaGroup{
				{
					Name:        "Gemini Models",
					Description: "Models within this group: Gemini Flash, Gemini Pro",
					Buckets:     []QuotaBucket{{ID: "gemini-weekly", Name: "Weekly Limit Remaining", Window: "weekly", RemainingFraction: 0.004, ResetTime: reset}},
				},
				{
					Name:        "Gemini 4 Argon",
					Description: "For paid Pro plans, Argon quota is separate.",
					Buckets:     []QuotaBucket{{ID: "gemini-argon-wtu-weekly", Name: "Gemini 4 Argon Weekly Limit", Window: "weekly", RemainingFraction: 0, ResetTime: reset}},
				},
			},
		}

		refuseFlash, gqFlash, matchFlash := quotaDecision(snap, now, "gemini-3.8-flash-high", testThresholds)
		if !refuseFlash || !matchFlash.OK || gqFlash.Percent() != 0 || gqFlash.Level != QuotaCritical {
			t.Fatalf("0.004 remaining = refuse:%v percent:%d level:%s, want true / 0%% / critical",
				refuseFlash, gqFlash.Percent(), gqFlash.Level)
		}

		refuseArgon, gqArgon, matchArgon := quotaDecision(snap, now, "gemini-4-argon-medium", testThresholds)
		if !refuseArgon || !matchArgon.OK || gqArgon.Percent() != 0 || gqArgon.Level != QuotaExhausted {
			t.Fatalf("0 remaining = refuse:%v percent:%d level:%s, want true / 0%% / exhausted",
				refuseArgon, gqArgon.Percent(), gqArgon.Level)
		}
	})
}

// A model-turn reply that comes with a failure exit spent quota just like one on
// a clean exit, so it must trip the latch rather than count as a retryable
// failure that a later call or refresh would repeat.
func TestUsageModelTurnOnFailureExitLatches(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, time.Minute, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"conversation_id":"c","status":"ERROR","num_turns":1}`), errors.New("agy /usage: exit status 1")
	})
	if _, err := m.Usage(t.Context(), time.Minute); !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("Usage error = %v, want the model-turn latch", err)
	}
	off.Store(int64(usageErrorBackoff + time.Second))
	if _, err := m.Usage(t.Context(), time.Minute); !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("second Usage error = %v, want the model-turn latch", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("probes = %d, want 1: the latch must stop every later probe", calls.Load())
	}
}

// A refresher tick inside the failure backoff starts no probe, the same rule
// Usage applies, and one after the backoff does.
func TestRefreshHonoursFailureBackoff(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, time.Minute, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("agy usage: boom")
	})
	if _, err := m.Usage(t.Context(), time.Minute); err == nil {
		t.Fatal("expected the stub's failure")
	}
	refresh := func() *quotaFlight {
		m.quota.mu.Lock()
		defer m.quota.mu.Unlock()
		return m.startRefreshLocked()
	}
	off.Store(int64(usageErrorBackoff - time.Second))
	if f := refresh(); f != nil {
		t.Fatal("refresh started a probe inside the failure backoff")
	}
	off.Store(int64(usageErrorBackoff + time.Second))
	f := refresh()
	if f == nil {
		t.Fatal("refresh started no probe after the backoff")
	}
	<-f.done
	if calls.Load() != 2 {
		t.Fatalf("probes = %d, want 2", calls.Load())
	}
}
