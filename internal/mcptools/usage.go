package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

type quotaInput struct {
	MaxAge string   `json:"max_age,omitempty" jsonschema:"how old a cached reading may be (Go duration, e.g. 10m); default 60s; it must be positive, and a value under 10s is raised to 10s. Use a long value (10m) to read the background snapshot instantly; use 10s to accept only a reading under 10 seconds old; a new reading takes a few seconds because it runs agy"`
	Models []string `json:"models,omitempty" jsonschema:"optional model IDs to map against this same quota snapshot, at most 128; e.g. gemini-3.8-flash-medium and gemini-4-argon. No extra model-list call or model turn is made. Omit for no mappings; model_groups returns explicit exact, unknown, ambiguous or unpinned outcomes rather than guessing"`
}

type quotaBucketOutput struct {
	ID                string  `json:"id" jsonschema:"agy's identifier for this quota window, e.g. 5h or weekly"`
	Name              string  `json:"name" jsonschema:"human-readable name of the window"`
	Window            string  `json:"window" jsonschema:"the window length as agy reports it, e.g. 5h or weekly"`
	RemainingFraction float64 `json:"remaining_fraction" jsonschema:"remaining quota from 0 to 1 as agy last reported it; not adjusted for a reset that has passed since, see refilled"`
	ResetTime         string  `json:"reset_time,omitempty" jsonschema:"reset timestamp reported by agy, RFC3339 UTC; absent when agy reported none"`
	Refilled          bool    `json:"refilled" jsonschema:"true when the reported reset has passed and the server assumes a refill for level and remaining_percent"`
}

// quotaGroupSummary is the per-group reading both quota shapes share: the
// compact one on run results is exactly this, and agy_usage adds detail.
type quotaGroupSummary struct {
	Name             string `json:"name" jsonschema:"quota group name, e.g. Gemini Models; models in a group share its limits"`
	Level            string `json:"level" jsonschema:"ok, low, critical or exhausted, set by the tightest window of the group at the time of the call; see agy_usage for the thresholds"`
	RemainingPercent int    `json:"remaining_percent" jsonschema:"remaining quota of the tightest window, as a whole percent rounded down; level is the authority, so 0.4 percent reads 0 with level critical"`
	Window           string `json:"window" jsonschema:"the tightest window, the one that sets level"`
	ResetTime        string `json:"reset_time,omitempty" jsonschema:"binding window's reported reset timestamp, RFC3339 UTC; absent when agy reported none"`
}

type quotaGroupOutput struct {
	quotaGroupSummary
	Description string              `json:"description,omitempty" jsonschema:"group coverage and plan wording supplied by agy, when available"`
	Buckets     []quotaBucketOutput `json:"buckets" jsonschema:"every window of the group"`
}

type quotaOutput struct {
	CheckedAt   string             `json:"checked_at" jsonschema:"when agy was asked for this reading, RFC3339 UTC"`
	Groups      []quotaGroupOutput `json:"groups" jsonschema:"one entry per quota group; empty when agy reported none"`
	ModelGroups []modelQuotaOutput `json:"model_groups" jsonschema:"requested models mapped against groups from this exact snapshot using the server's admission classifier; empty when no models were requested. Inference from model-family and CLI group metadata, not a provider guarantee"`
}

type modelQuotaOutput struct {
	Model  string `json:"model" jsonschema:"model string supplied in models, in request order"`
	Status string `json:"status" jsonschema:"exact when the local classifier inferred one quota group from the model ID and CLI group metadata; unknown for an unrecognized model or missing group; ambiguous for multiple candidates; unpinned for an empty model. Does not establish that the model is available or executable"`
	Group  string `json:"group,omitempty" jsonschema:"name of the matching entry in groups; present only for exact, otherwise no group is guessed"`
}

// quotaSummaryOutput is the compact quota reading attached to run results. It is
// read from the server's in-memory snapshot, never by running agy, so it can be
// absent.
type quotaSummaryOutput struct {
	CheckedAt string              `json:"checked_at" jsonschema:"when agy was asked for this reading, RFC3339 UTC"`
	Groups    []quotaGroupSummary `json:"groups" jsonschema:"one entry per quota group, with the level the server computed now"`
}

func formatQuotaTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func toQuotaGroupSummary(gq *manager.GroupQuota) quotaGroupSummary {
	return quotaGroupSummary{
		Name:             gq.Group.Name,
		Level:            string(gq.Level),
		RemainingPercent: gq.Percent(),
		Window:           gq.Binding.Window,
		ResetTime:        formatQuotaTime(gq.Binding.ResetTime),
	}
}

// toQuotaOutput converts a snapshot and its evaluated groups (same order, see
// manager.Manager.QuotaLevels) into the agy_usage wire shape. Arrays are never
// null.
func toQuotaOutput(snap manager.QuotaSnapshot, levels []manager.GroupQuota) quotaOutput {
	out := quotaOutput{CheckedAt: formatQuotaTime(snap.CheckedAt), Groups: make([]quotaGroupOutput, 0, len(levels)), ModelGroups: []modelQuotaOutput{}}
	for i := range levels {
		gq := &levels[i]
		g := quotaGroupOutput{
			quotaGroupSummary: toQuotaGroupSummary(gq),
			Description:       gq.Group.Description,
			Buckets:           make([]quotaBucketOutput, 0, len(gq.Buckets)),
		}
		for _, b := range gq.Buckets {
			g.Buckets = append(g.Buckets, quotaBucketOutput{
				ID: b.ID, Name: b.Name, Window: b.Window,
				RemainingFraction: b.RemainingFraction,
				ResetTime:         formatQuotaTime(b.ResetTime),
				Refilled:          b.Refilled,
			})
		}
		out.Groups = append(out.Groups, g)
	}
	return out
}

func quotaModelGroups(snap manager.QuotaSnapshot, models []string) []modelQuotaOutput {
	out := make([]modelQuotaOutput, 0, len(models))
	for _, model := range models {
		match := manager.MatchQuotaGroup(snap.Groups, model)
		item := modelQuotaOutput{Model: model, Status: string(match.Status)}
		if match.OK {
			item.Group = snap.Groups[match.Index].Name
		}
		out = append(out, item)
	}
	return out
}

// toQuotaSummary is the compact form of toQuotaOutput.
func toQuotaSummary(snap manager.QuotaSnapshot, levels []manager.GroupQuota) *quotaSummaryOutput {
	out := &quotaSummaryOutput{CheckedAt: formatQuotaTime(snap.CheckedAt), Groups: make([]quotaGroupSummary, 0, len(levels))}
	for i := range levels {
		out.Groups = append(out.Groups, toQuotaGroupSummary(&levels[i]))
	}
	return out
}

// quotaFor returns the quota summary to attach to a run result, or nil when the
// server has no trustworthy reading. It never waits on agy.
func quotaFor(mgr *manager.Manager) *quotaSummaryOutput {
	snap, ok := mgr.CachedQuota()
	if !ok {
		return nil
	}
	return toQuotaSummary(snap, mgr.QuotaLevels(snap))
}

// registerUsage adds the agy_usage tool.
func registerUsage(s *toolServer, mgr *manager.Manager) {
	addTool(s, &mcp.Tool{
		Name:        toolAgyUsage,
		Title:       "Check agy quota",
		Annotations: annReadExternal,
		Description: "Report how much agy quota is left, per quota group, with a level per group: ok, low, critical or exhausted. Groups and plan descriptions come from the CLI reading. Gemini Models and Gemini 4 Argon can be separate. " +
			"Optional models is an array of up to 128 model IDs; model_groups maps them against this same snapshot using the admission classifier, with exact, unknown, ambiguous or unpinned status. These are inferences from CLI group metadata, not proof of model availability or provider-guaranteed membership. Omit models for no mappings. " +
			"The level comes from the group's tightest active window (5h or weekly; disabled buckets are skipped), assumes a full window after its reported reset time, and uses the server's thresholds: low below 25 percent remaining, critical below 5, exhausted at 0 (AGY_MCP_USAGE_LOW and AGY_MCP_USAGE_CRITICAL change the first two). " +
			"It requests agy's non-turn /usage command. The server rejects replies that indicate a model turn and disables further probes in that server process. " +
			"With priority optional, agy_run and agy_run_sync use a trusted cached reading to refuse low quota or unresolved mappings. Missing or stale readings permit admission. Background refresh pauses during tracked jobs. Use checked_at to assess the age of this reading. " +
			"max_age bounds how old a cached reading may be (default 60s, minimum 10s); a fresh reading takes a few seconds. " +
			"Shells out to the agy CLI, so it fails if agy is missing from PATH or not authenticated.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in quotaInput) (*mcp.CallToolResult, quotaOutput, error) {
		if len(in.Models) > 128 {
			return nil, quotaOutput{}, fmt.Errorf("models must contain at most 128 entries")
		}
		var maxAge time.Duration // zero: the manager's default
		if in.MaxAge != "" {
			d, err := parsePositiveDuration("max_age", in.MaxAge, "10m")
			if err != nil {
				return nil, quotaOutput{}, err
			}
			maxAge = d
		}
		snap, err := mgr.Usage(ctx, maxAge)
		if err != nil {
			return nil, quotaOutput{}, err
		}
		out := toQuotaOutput(snap, mgr.QuotaLevels(snap))
		out.ModelGroups = quotaModelGroups(snap, in.Models)
		return nil, out, nil
	})
}
