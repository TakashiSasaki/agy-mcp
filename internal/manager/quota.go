package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Run priorities. PriorityOptional lets StartJob refuse a run whose model's
// quota group is low or worse, or whose quota group mapping cannot be
// established when a trusted snapshot is present (see checkPriority); anything
// else, including an empty value, runs.
const (
	PriorityNormal   = "normal"
	PriorityOptional = "optional"
)

const (
	// usageCommandName is the command.name the /usage envelope must carry before
	// decodeUsageEnvelope trusts it. It is the literal agy prints (MEASURED
	// against agy 1.2.16), mirrored in internal/testutil/fakeagy.go.
	usageCommandName = "usage"

	usageProbeTimeout   = 30 * time.Second
	usageProbeKillGrace = time.Second
	// usageDefaultMaxAge is the max age Usage applies when the caller names none.
	usageDefaultMaxAge = 60 * time.Second
	// usageMinMaxAge floors the max_age an agy_usage caller may ask for.
	usageMinMaxAge = 10 * time.Second
	// usageErrorBackoff is how long a failed probe is remembered, so a broken agy
	// is not re-spawned on every call.
	usageErrorBackoff = 30 * time.Second
	// usageSnapshotFloor is the shortest max age of a snapshot the guard and the
	// piggyback will use; see quotaMaxAge.
	usageSnapshotFloor = 10 * time.Minute
)

// errUsageModelTurn marks a /usage reply that was a model turn rather than a
// command answer: it spent quota. See decodeUsageEnvelope.
var errUsageModelTurn = errors.New("agy answered /usage with a model turn")

// QuotaLevel is how little quota a group has left.
type QuotaLevel string

// Quota levels, from most to least headroom.
const (
	QuotaOK        QuotaLevel = "ok"
	QuotaLow       QuotaLevel = "low"
	QuotaCritical  QuotaLevel = "critical"
	QuotaExhausted QuotaLevel = "exhausted"
)

// QuotaBucket is one quota window of a group (for example the 5h or the weekly
// limit).
type QuotaBucket struct {
	ID                string
	Name              string
	Window            string
	RemainingFraction float64
	ResetTime         time.Time // zero when agy did not report one
}

// QuotaGroup is a set of buckets that models share (for example Gemini Models,
// Gemini 4 Argon, or Claude and GPT models).
type QuotaGroup struct {
	Name        string
	Description string
	Buckets     []QuotaBucket
}

// QuotaSnapshot is one reading of /usage. Levels are not stored: they depend on
// the clock and the thresholds, so they are computed at read time.
type QuotaSnapshot struct {
	CheckedAt time.Time
	Groups    []QuotaGroup
}

// BucketQuota is a bucket evaluated at a point in time.
type BucketQuota struct {
	QuotaBucket
	Refilled bool // its reset time had passed, so it counted as full
}

// GroupQuota is a group evaluated at a point in time.
type GroupQuota struct {
	Group     QuotaGroup
	Buckets   []BucketQuota // Group.Buckets in order, evaluated at the same time as Level
	Level     QuotaLevel
	Remaining float64     // effective remaining fraction of the binding bucket
	Binding   QuotaBucket // the bucket that sets Level
}

// Percent is the remaining quota as a whole percent, rounded down. The level, not
// this number, is the authority: 0.4% reads 0 with level critical.
func (g GroupQuota) Percent() int {
	return int(math.Floor(g.Remaining*100 + 1e-9))
}

type quotaThresholds struct{ Low, Critical float64 }

// QuotaRefusalError is returned by StartJob when an optional run is refused
// because its model's quota group is low or worse, or because its model's quota
// group mapping cannot be established against a trusted snapshot (unpinned,
// unknown, or ambiguous). No job exists when it is returned.
type QuotaRefusalError struct {
	Group       string
	Level       QuotaLevel
	Percent     int
	Bucket      string // name of the binding bucket
	ResetTime   time.Time
	Model       string
	MatchStatus QuotaMatchStatus
}

func (e *QuotaRefusalError) Error() string {
	switch e.MatchStatus {
	case QuotaMatchUnpinned:
		return "optional run skipped (deferred), no job was started: quota group mapping is unpinned (no model was specified and no default model is configured, so quota headroom cannot be verified). " +
			"Pass an explicit mapped model, or pass priority normal only if this run is required."
	case QuotaMatchUnknown:
		return fmt.Sprintf("optional run skipped (deferred), no job was started: quota group mapping for model %q is unknown (cannot verify quota headroom against reported /usage groups). "+
			"Pass a model with a known quota group, or pass priority normal only if this run is required.", e.Model)
	case QuotaMatchAmbiguous:
		return fmt.Sprintf("optional run skipped (deferred), no job was started: quota group mapping for model %q is ambiguous across reported /usage groups (cannot verify quota headroom). "+
			"Pass a model that maps to a single quota group, or pass priority normal only if this run is required.", e.Model)
	}
	detail := fmt.Sprintf("%s%d%% left", e.bucketPrefix(), e.Percent)
	if !e.ResetTime.IsZero() {
		detail += ", resets " + e.ResetTime.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("optional run skipped, no job was started: the %q quota is %s (%s). "+
		"Report the skip instead of retrying before the reset; pass priority normal only if this run is required.",
		e.Group, e.Level, detail)
}

func (e *QuotaRefusalError) bucketPrefix() string {
	if e.Bucket == "" {
		return ""
	}
	return e.Bucket + " "
}

// usageProbeArgs is the argv of the quota probe. It is exactly this and nothing
// more, because /usage only answers without a model turn when agy treats it as a
// slash command: MEASURED against agy 1.2.16, adding --disable-slash-commands
// makes `-p /usage` a model prompt (exit 0, num_turns 1, a fresh conversation id,
// about 21.8k tokens, no command field), which spends quota. The no-turn
// behaviour is documented in agy's changelog 1.1.11 ("-p \"/usage\" ... without
// starting an agent turn, spending quota, or leaving a conversation behind"),
// and agyver.Required is above that version.
func usageProbeArgs() []string {
	return []string{outputFormatFlag, jsonOutputFormat, promptFlag, "/" + usageCommandName}
}

// usageEnvelope is the subset of `agy --output-format json -p /usage` the decoder
// needs. Groups is a pointer so an absent or null array (a renamed field) differs
// from a present empty one.
type usageEnvelope struct {
	Status         string `json:"status"`
	NumTurns       int    `json:"num_turns"`
	ConversationID string `json:"conversation_id"`
	Command        struct {
		Name string `json:"name"`
		Data struct {
			Groups *[]struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Buckets     []struct {
					ID                string   `json:"id"`
					Name              string   `json:"name"`
					Window            string   `json:"window"`
					Disabled          bool     `json:"disabled"`
					RemainingFraction *float64 `json:"remaining_fraction"`
					ResetTime         string   `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// decodeUsageEnvelope turns agy's /usage envelope into groups. Every shape it
// does not recognize is an error, so a changed agy fails loudly instead of
// reading as full quota. A bucket without remaining_fraction is skipped (agy has
// disabled buckets, changelog 1.0.8; their shape is NOT MEASURED) but at least
// one bucket must carry it when any group exists. An explicit empty groups array
// is a valid reading with no groups.
func decodeUsageEnvelope(raw []byte) ([]QuotaGroup, error) {
	var env usageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("agy usage: parse json envelope: %w", err)
	}
	// A reply that is not the usage command but shows a model turn means /usage
	// was treated as a prompt: report it distinctly so the caller stops probing.
	if env.Command.Name != usageCommandName && (env.NumTurns > 0 || env.ConversationID != "") {
		return nil, fmt.Errorf("agy usage: envelope command %q with %d turn(s): %w", env.Command.Name, env.NumTurns, errUsageModelTurn)
	}
	if err := validateListingFraming(usageCommandName, env.Status, env.Command.Name); err != nil {
		return nil, err
	}
	if env.Command.Data.Groups == nil {
		return nil, errors.New("agy usage: envelope has no command.data.groups array")
	}
	rawGroups := *env.Command.Data.Groups
	groups := make([]QuotaGroup, 0, len(rawGroups))
	for _, rg := range rawGroups {
		g := QuotaGroup{Name: rg.Name, Description: rg.Description}
		for _, rb := range rg.Buckets {
			if rb.Disabled || rb.RemainingFraction == nil {
				continue
			}
			b := QuotaBucket{ID: rb.ID, Name: rb.Name, Window: rb.Window, RemainingFraction: *rb.RemainingFraction}
			if rb.ResetTime != "" {
				t, err := time.Parse(time.RFC3339, rb.ResetTime)
				if err != nil {
					return nil, fmt.Errorf("agy usage: bucket %q reset_time %q: %w", rb.ID, rb.ResetTime, err)
				}
				b.ResetTime = t
			}
			g.Buckets = append(g.Buckets, b)
		}
		if len(g.Buckets) > 0 {
			groups = append(groups, g)
		}
	}
	if len(rawGroups) > 0 && len(groups) == 0 {
		return nil, errors.New("agy usage: no bucket carried remaining_fraction")
	}
	return groups, nil
}

// effectiveRemaining is a bucket's remaining fraction at now: 1 once its reset
// time has passed, else the reported fraction clamped to [0, 1].
func effectiveRemaining(b QuotaBucket, now time.Time) (r float64, refilled bool) {
	if !b.ResetTime.IsZero() && !b.ResetTime.After(now) {
		return 1, true
	}
	return min(max(b.RemainingFraction, 0), 1), false
}

func levelOf(r float64, th quotaThresholds) QuotaLevel {
	switch {
	case r <= 0:
		return QuotaExhausted
	case r < th.Critical:
		return QuotaCritical
	case r < th.Low:
		return QuotaLow
	default:
		return QuotaOK
	}
}

// groupQuota evaluates a group at now. The binding bucket is the one with the
// least effective remaining; a tie goes to the later reset, since the caller has
// to wait for that one anyway.
func groupQuota(g QuotaGroup, now time.Time, th quotaThresholds) GroupQuota {
	gq := GroupQuota{Group: g, Buckets: make([]BucketQuota, 0, len(g.Buckets)), Remaining: 1}
	first := true
	for _, b := range g.Buckets {
		r, refilled := effectiveRemaining(b, now)
		gq.Buckets = append(gq.Buckets, BucketQuota{QuotaBucket: b, Refilled: refilled})
		if first || r < gq.Remaining || (r == gq.Remaining && b.ResetTime.After(gq.Binding.ResetTime)) {
			gq.Remaining, gq.Binding, first = r, b, false
		}
	}
	gq.Level = levelOf(gq.Remaining, th)
	return gq
}

// QuotaMatchStatus classifies how a model string mapped to a snapshot's quota
// groups. Unknown, unpinned, and ambiguous mappings are explicit and cause
// optional runs to refuse/defer clearly rather than guessing a wrong pool or
// silently swapping models.
type QuotaMatchStatus string

const (
	QuotaMatchExact     QuotaMatchStatus = "exact"
	QuotaMatchUnpinned  QuotaMatchStatus = "unpinned"
	QuotaMatchUnknown   QuotaMatchStatus = "unknown"
	QuotaMatchAmbiguous QuotaMatchStatus = "ambiguous"
)

type modelFamily string

const (
	modelFamilyNone        modelFamily = ""
	modelFamilyGeminiArgon modelFamily = "gemini_argon"
	modelFamilyGeminiFlash modelFamily = "gemini_flash"
	modelFamilyGeminiPro   modelFamily = "gemini_pro"
	modelFamilyClaude      modelFamily = "claude"
	modelFamilyGPT         modelFamily = "gpt"
	modelFamilyUnknown     modelFamily = "unknown"
)

// QuotaGroupMatch holds the explicit outcome of matching a model against the
// groups in a quota snapshot.
type QuotaGroupMatch struct {
	Index  int
	Status QuotaMatchStatus
	Family modelFamily
	OK     bool
}

func tokenizeQuotaText(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func isDigitToken(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// hasGeminiModelToken reports whether tokens contain a "gemini ... <target>"
// model family reference (for example "gemini pro", "gemini 3 1 pro",
// "gemini 3 8 flash", "gemini 4 argon", or "gemini flash and pro"). Only
// version-number tokens and model-list conjunctions ("flash", "pro", "argon",
// "and", "or") may appear between "gemini" and target, so subscription prose
// such as "For paid Pro plans", "Pro subscribers", or "larger Gemini quota
// bucket ... on Ultra plans" never matches.
func hasGeminiModelToken(tokens []string, target string) bool {
	for i, tok := range tokens {
		if tok != "gemini" {
			continue
		}
		for j := i + 1; j < len(tokens); j++ {
			t := tokens[j]
			if t == target {
				return true
			}
			if isDigitToken(t) || t == "flash" || t == "pro" || t == "argon" || t == "and" || t == "or" {
				continue
			}
			break
		}
	}
	return false
}

// classifyModelFamily maps a known agy model ID, display label, or
// tab-separated `id\tlabel` row into an explicit modelFamily. Unknown model
// strings (including unknown gemini-*, claude-*, or gpt-* suffixes such as
// "claude-x" or "gemini-ultra") remain modelFamilyUnknown.
func classifyModelFamily(model string) modelFamily {
	id := modelID(model)
	lm := strings.ToLower(strings.TrimSpace(id))
	if lm == "" {
		return modelFamilyNone
	}
	toks := tokenizeQuotaText(lm)
	switch {
	case strings.HasPrefix(lm, "gemini-") || strings.HasPrefix(lm, "gemini "):
		switch {
		case slices.Contains(toks, "argon"):
			return modelFamilyGeminiArgon
		case slices.Contains(toks, "flash"):
			return modelFamilyGeminiFlash
		case slices.Contains(toks, "pro"):
			return modelFamilyGeminiPro
		default:
			return modelFamilyUnknown
		}
	case strings.HasPrefix(lm, "claude-") || strings.HasPrefix(lm, "claude "):
		if slices.Contains(toks, "opus") || slices.Contains(toks, "sonnet") || slices.Contains(toks, "haiku") {
			return modelFamilyClaude
		}
		return modelFamilyUnknown
	case strings.HasPrefix(lm, "gpt-") || strings.HasPrefix(lm, "gpt "):
		if slices.Contains(toks, "oss") || slices.Contains(toks, "4o") || slices.Contains(toks, "4") || slices.Contains(toks, "5") {
			return modelFamilyGPT
		}
		return modelFamilyUnknown
	default:
		return modelFamilyUnknown
	}
}

type groupTraits struct {
	isArgon   bool
	hasFlash  bool
	hasPro    bool
	hasGemini bool
	hasClaude bool
	hasGPT    bool
}

// classifyQuotaGroup extracts family traits from a QuotaGroup using stable
// bucket IDs (such as "gemini-argon-wtu-weekly", "gemini-weekly", "gemini-5h",
// "3p-weekly", "3p-5h") and explicit model-family names in Name and Description
// (via hasGeminiModelToken). Subscription-tier wording ("For paid Pro plans",
// "Pro subscribers", "available on Ultra plans") is never treated as a model
// family match.
func classifyQuotaGroup(g QuotaGroup) groupTraits {
	nameToks := tokenizeQuotaText(g.Name)
	descToks := tokenizeQuotaText(g.Description)

	var (
		bucketHasArgon  bool
		bucketHasFlash  bool
		bucketHasPro    bool
		bucketHasGemini bool
		bucketHas3P     bool
		bucketHasClaude bool
		bucketHasGPT    bool
	)
	for _, b := range g.Buckets {
		id := strings.ToLower(strings.TrimSpace(b.ID))
		switch {
		case id == "gemini-argon-wtu-weekly" || strings.HasPrefix(id, "gemini-argon"):
			bucketHasArgon = true
			bucketHasGemini = true
		case id == "gemini-weekly" || id == "gemini-5h" || strings.HasPrefix(id, "gemini-"):
			bucketHasGemini = true
		case id == "3p-weekly" || id == "3p-5h" || strings.HasPrefix(id, "3p-"):
			bucketHas3P = true
		}
		idToks := tokenizeQuotaText(b.ID)
		btoks := append(idToks, tokenizeQuotaText(b.Name)...)
		if slices.Contains(btoks, "argon") {
			bucketHasArgon = true
			bucketHasGemini = true
		}
		if slices.Contains(btoks, "flash") {
			bucketHasFlash = true
			bucketHasGemini = true
		}
		if slices.Contains(idToks, "pro") || hasGeminiModelToken(btoks, "pro") {
			bucketHasPro = true
			bucketHasGemini = true
		}
		if slices.Contains(btoks, "gemini") {
			bucketHasGemini = true
		}
		if slices.Contains(btoks, "claude") {
			bucketHasClaude = true
		}
		if slices.Contains(btoks, "gpt") {
			bucketHasGPT = true
		}
	}

	isArgon := bucketHasArgon ||
		slices.Contains(nameToks, "argon") ||
		hasGeminiModelToken(descToks, "argon")

	hasFlash := bucketHasFlash ||
		slices.Contains(nameToks, "flash") ||
		hasGeminiModelToken(descToks, "flash")

	hasPro := bucketHasPro ||
		hasGeminiModelToken(nameToks, "pro") ||
		(slices.Contains(nameToks, "gemini") && slices.Contains(nameToks, "pro")) ||
		hasGeminiModelToken(descToks, "pro")

	hasGemini := bucketHasGemini ||
		slices.Contains(nameToks, "gemini") ||
		isArgon || hasFlash || hasPro

	explicitClaude := bucketHasClaude ||
		slices.Contains(nameToks, "claude") ||
		slices.Contains(descToks, "claude")

	explicitGPT := bucketHasGPT ||
		slices.Contains(nameToks, "gpt") ||
		slices.Contains(descToks, "gpt")

	hasClaude := explicitClaude || (bucketHas3P && !explicitGPT)
	hasGPT := explicitGPT || (bucketHas3P && !explicitClaude)

	return groupTraits{
		isArgon:   isArgon,
		hasFlash:  hasFlash,
		hasPro:    hasPro,
		hasGemini: hasGemini,
		hasClaude: hasClaude,
		hasGPT:    hasGPT,
	}
}

func resolveCandidateIndices(family modelFamily, candidates []int) QuotaGroupMatch {
	switch len(candidates) {
	case 1:
		return QuotaGroupMatch{Index: candidates[0], Status: QuotaMatchExact, Family: family, OK: true}
	case 0:
		return QuotaGroupMatch{Status: QuotaMatchUnknown, Family: family}
	default:
		return QuotaGroupMatch{Status: QuotaMatchAmbiguous, Family: family}
	}
}

// matchQuotaGroup maps model to a single QuotaGroup in groups using explicit
// model-family classification and group bucket IDs/Name/Description metadata.
// It never assumes there is only one Gemini group: separate Gemini Flash/Pro
// and Gemini Argon pools (as reported by agy 1.4.10) each resolve to their own
// group, Argon never infers a fallback from descriptive plan text when the
// Argon group is absent, and an unscoped single Gemini group still matches
// Flash/Pro when agy reports a unified Gemini pool.
func matchQuotaGroup(groups []QuotaGroup, model string) QuotaGroupMatch {
	family := classifyModelFamily(model)
	switch family {
	case modelFamilyNone:
		return QuotaGroupMatch{Status: QuotaMatchUnpinned, Family: family}
	case modelFamilyUnknown:
		return QuotaGroupMatch{Status: QuotaMatchUnknown, Family: family}
	}

	traits := make([]groupTraits, len(groups))
	var unscopedGemini []int
	anyScopedNonArgonGemini := false
	for i, g := range groups {
		t := classifyQuotaGroup(g)
		traits[i] = t
		if t.hasGemini && !t.isArgon {
			if t.hasFlash || t.hasPro {
				anyScopedNonArgonGemini = true
			} else {
				unscopedGemini = append(unscopedGemini, i)
			}
		}
	}

	switch family {
	case modelFamilyGeminiArgon:
		var argon []int
		for i, t := range traits {
			if t.isArgon {
				argon = append(argon, i)
			}
		}
		return resolveCandidateIndices(family, argon)

	case modelFamilyGeminiFlash:
		var flash []int
		for i, t := range traits {
			if t.hasGemini && !t.isArgon && t.hasFlash {
				flash = append(flash, i)
			}
		}
		if len(flash) > 0 || anyScopedNonArgonGemini {
			return resolveCandidateIndices(family, flash)
		}
		return resolveCandidateIndices(family, unscopedGemini)

	case modelFamilyGeminiPro:
		var pro []int
		for i, t := range traits {
			if t.hasGemini && !t.isArgon && t.hasPro {
				pro = append(pro, i)
			}
		}
		if len(pro) > 0 || anyScopedNonArgonGemini {
			return resolveCandidateIndices(family, pro)
		}
		return resolveCandidateIndices(family, unscopedGemini)

	case modelFamilyClaude:
		var claude []int
		for i, t := range traits {
			if t.hasClaude {
				claude = append(claude, i)
			}
		}
		return resolveCandidateIndices(family, claude)

	case modelFamilyGPT:
		var gpt []int
		for i, t := range traits {
			if t.hasGPT {
				gpt = append(gpt, i)
			}
		}
		return resolveCandidateIndices(family, gpt)

	default:
		return QuotaGroupMatch{Status: QuotaMatchUnknown, Family: family}
	}
}

// groupForModel returns the index of the quota group model draws from, its
// explicit QuotaMatchStatus, and whether a single group was matched.
func groupForModel(groups []QuotaGroup, model string) (int, QuotaMatchStatus, bool) {
	m := matchQuotaGroup(groups, model)
	return m.Index, m.Status, m.OK
}

// quotaDecision reports whether an optional run on model should be refused,
// the group it was judged against (when Match.OK is true), and the explicit
// QuotaGroupMatch. When the model's quota group cannot be established
// (unpinned, unknown, or ambiguous), optional runs are refused/deferred; the
// caller has already checked that snap is young enough.
func quotaDecision(snap QuotaSnapshot, now time.Time, model string, th quotaThresholds) (bool, GroupQuota, QuotaGroupMatch) {
	m := matchQuotaGroup(snap.Groups, model)
	if !m.OK {
		return true, GroupQuota{}, m
	}
	gq := groupQuota(snap.Groups[m.Index], now, th)
	return gq.Level != QuotaOK, gq, m
}

// quotaFlight is one in-progress probe that concurrent callers share.
type quotaFlight struct {
	done chan struct{}
	snap QuotaSnapshot // valid when err is nil, after done is closed
	err  error
}

// quotaCache is the manager's in-memory quota state. Nothing is persisted, so a
// restarted server starts empty and the guard fails open until the first probe.
type quotaCache struct {
	mu        sync.Mutex
	snap      QuotaSnapshot // zero CheckedAt until the first good probe
	lastErr   error
	lastErrAt time.Time
	inflight  *quotaFlight
	unsafe    error // set once a probe spent a model turn; probing stops for the process
}

func (m *Manager) quotaThresholds() quotaThresholds {
	return quotaThresholds{Low: m.cfg.QuotaLow, Critical: m.cfg.QuotaCritical}
}

// QuotaLevels evaluates every group of snap at the manager's clock with its
// configured thresholds.
func (m *Manager) QuotaLevels(snap QuotaSnapshot) []GroupQuota {
	now, th := m.now(), m.quotaThresholds()
	out := make([]GroupQuota, 0, len(snap.Groups))
	for _, g := range snap.Groups {
		out = append(out, groupQuota(g, now, th))
	}
	return out
}

// quotaMaxAge is how old a snapshot the guard and the piggyback still trust.
func (m *Manager) quotaMaxAge() time.Duration {
	return max(2*m.cfg.UsageInterval, usageSnapshotFloor)
}

// startFetchLocked joins the probe in flight or starts one. c.mu must be held.
func (m *Manager) startFetchLocked() *quotaFlight {
	c := &m.quota
	if c.inflight != nil {
		return c.inflight
	}
	f := &quotaFlight{done: make(chan struct{})}
	c.inflight = f
	go m.fetchQuota(f)
	return f
}

// startRefreshLocked starts or joins a background refresh, or returns nil when a
// probe failed within the last usageErrorBackoff, so a refresher tick honours the
// same backoff as Usage. c.mu must be held.
func (m *Manager) startRefreshLocked() *quotaFlight {
	c := &m.quota
	if c.lastErr != nil && m.now().Sub(c.lastErrAt) < usageErrorBackoff {
		return nil
	}
	return m.startFetchLocked()
}

// fetchQuota runs one probe on its own context, so a caller that gives up does
// not abort the probe the others are waiting on, and records the outcome.
func (m *Manager) fetchQuota(f *quotaFlight) {
	raw, err := m.readUsage(context.Background())
	var groups []QuotaGroup
	if err == nil {
		groups, err = decodeUsageEnvelope(raw)
	} else if len(raw) > 0 {
		// A failure exit can still carry a model-turn reply on stdout; it spent
		// quota just the same, so it must trip the latch, never count as a reading.
		if _, derr := decodeUsageEnvelope(raw); errors.Is(derr, errUsageModelTurn) {
			err = fmt.Errorf("%w; %w", err, derr)
		}
	}
	c := &m.quota
	c.mu.Lock()
	now := m.now()
	if err == nil {
		if c.lastErr != nil {
			log.Printf("agy usage probe recovered")
		}
		c.snap = QuotaSnapshot{CheckedAt: now, Groups: groups}
		c.lastErr = nil
		f.snap = c.snap
	} else {
		if errors.Is(err, errUsageModelTurn) {
			c.unsafe = fmt.Errorf("quota probing is disabled for this server process: %w", err)
			log.Printf("agy usage probe disabled: %v", err)
		} else if c.lastErr == nil {
			log.Printf("agy usage probe failed: %v", err)
		}
		c.lastErr, c.lastErrAt = err, now
		f.err = err
	}
	c.inflight = nil
	c.mu.Unlock()
	close(f.done)
}

func (m *Manager) awaitFlight(ctx context.Context, f *quotaFlight) (QuotaSnapshot, error) {
	select {
	case <-f.done:
		return f.snap, f.err
	case <-ctx.Done():
		return QuotaSnapshot{}, ctx.Err()
	}
}

// Usage returns a quota snapshot no older than maxAge (60s when zero, floored at
// 10s), probing agy when the cache cannot answer. Concurrent callers share one
// probe, and a failed probe is remembered for 30s.
func (m *Manager) Usage(ctx context.Context, maxAge time.Duration) (QuotaSnapshot, error) {
	if maxAge == 0 {
		maxAge = usageDefaultMaxAge
	}
	maxAge = max(maxAge, usageMinMaxAge)
	c := &m.quota
	c.mu.Lock()
	if c.unsafe != nil {
		err := c.unsafe
		c.mu.Unlock()
		return QuotaSnapshot{}, err
	}
	now := m.now()
	if !c.snap.CheckedAt.IsZero() && now.Sub(c.snap.CheckedAt) < maxAge {
		snap := c.snap
		c.mu.Unlock()
		return snap, nil
	}
	if c.lastErr != nil && now.Sub(c.lastErrAt) < usageErrorBackoff {
		err := c.lastErr
		c.mu.Unlock()
		return QuotaSnapshot{}, err
	}
	f := m.startFetchLocked()
	c.mu.Unlock()
	return m.awaitFlight(ctx, f)
}

// CachedQuota returns the cached snapshot when it is young enough to trust (see
// quotaMaxAge). It is a pure read: it never waits on agy or starts a probe, and
// keeping the snapshot fresh is the refresher's job.
func (m *Manager) CachedQuota() (QuotaSnapshot, bool) {
	c := &m.quota
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap.CheckedAt.IsZero() || m.now().Sub(c.snap.CheckedAt) >= m.quotaMaxAge() {
		return QuotaSnapshot{}, false
	}
	return c.snap, true
}

// RunUsageRefresherFromConfig keeps the quota snapshot fresh every
// cfg.UsageInterval until ctx ends. It blocks, so callers run it in a goroutine;
// a zero interval makes it a no-op.
func (m *Manager) RunUsageRefresherFromConfig(ctx context.Context) {
	m.runUsageRefresher(ctx, m.cfg.UsageInterval)
}

// runUsageRefresher probes immediately and then every interval, skipping a tick
// that falls inside the failure backoff (see startRefreshLocked). It stops when
// ctx ends or once a probe has spent a model turn (see errUsageModelTurn). A
// non-positive interval is a no-op.
func (m *Manager) runUsageRefresher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c := &m.quota
		c.mu.Lock()
		if c.unsafe != nil {
			c.mu.Unlock()
			return
		}
		f := m.startRefreshLocked()
		c.mu.Unlock()
		// The outcome is recorded in the cache and logged by fetchQuota.
		if f != nil {
			_, _ = m.awaitFlight(ctx, f)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkPriority refuses an optional run whose model's quota group is low or
// worse, or whose model's quota group mapping cannot be established against a
// trusted snapshot (unpinned, unknown, or ambiguous). Normal priority and
// missing/stale snapshots let the run proceed.
func (m *Manager) checkPriority(req StartRequest) error {
	if req.Priority != PriorityOptional {
		return nil
	}
	snap, trusted := m.CachedQuota()
	if !trusted {
		return nil
	}
	model := req.Model
	if model == "" {
		model = m.cfg.DefaultModel
	}
	model = modelID(model)
	refuse, gq, match := quotaDecision(snap, m.now(), model, m.quotaThresholds())
	if !refuse {
		return nil
	}
	if !match.OK {
		return &QuotaRefusalError{
			Model:       model,
			MatchStatus: match.Status,
		}
	}
	bucket := gq.Binding.Name
	if bucket == "" {
		bucket = gq.Binding.Window
	}
	return &QuotaRefusalError{
		Group:       gq.Group.Name,
		Level:       gq.Level,
		Percent:     gq.Percent(),
		Bucket:      bucket,
		ResetTime:   gq.Binding.ResetTime,
		Model:       model,
		MatchStatus: match.Status,
	}
}
