package mcptools

import (
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// Mutation guards: quotaModelGroups must use the admission classifier, retain
// request order and withhold a group for every non-exact outcome.
func TestQuotaModelMappings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		groups   []manager.QuotaGroup
		models   []string
		statuses []string
		names    []string
	}{
		{
			name:     "split Gemini pools",
			groups:   []manager.QuotaGroup{{Name: "Gemini Models"}, {Name: "Gemini 4 Argon"}, {Name: "Claude and GPT models"}},
			models:   []string{"gemini-3.8-flash-medium", "gemini-4-argon", "claude-sonnet", "new-unknown-model", ""},
			statuses: []string{"exact", "exact", "exact", "unknown", "unpinned"},
			names:    []string{"Gemini Models", "Gemini 4 Argon", "Claude and GPT models", "", ""},
		},
		{
			name:     "missing Argon pool is not inferred",
			groups:   []manager.QuotaGroup{{Name: "Gemini Models"}},
			models:   []string{"gemini-4-argon"},
			statuses: []string{"unknown"},
			names:    []string{""},
		},
		{
			name:     "ambiguous pools are not guessed",
			groups:   []manager.QuotaGroup{{Name: "Gemini Flash"}, {Name: "Gemini Flash extra"}},
			models:   []string{"gemini-3.8-flash-medium"},
			statuses: []string{"ambiguous"},
			names:    []string{""},
		},
		{name: "no requested models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := quotaModelGroups(manager.QuotaSnapshot{Groups: tc.groups}, tc.models)
			if got == nil || len(got) != len(tc.models) {
				t.Fatalf("mapping count/nullability: %+v", got)
			}
			for i, item := range got {
				if item.Model != tc.models[i] || item.Status != tc.statuses[i] || item.Group != tc.names[i] {
					t.Errorf("model %d: %+v, want %s/%s/%s", i, item, tc.models[i], tc.statuses[i], tc.names[i])
				}
			}
		})
	}
}
