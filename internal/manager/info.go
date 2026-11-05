package manager

import (
	"context"
	"os"
	"runtime"
	"time"
)

type RuntimeInfo struct {
	Hostname         string `json:"hostname,omitempty" jsonschema:"server host name; paths in tool arguments refer to this host, not the caller's machine; absent if unavailable"`
	OS               string `json:"os" jsonschema:"server operating system, for interpreting server-side paths and execution capabilities"`
	Architecture     string `json:"architecture" jsonschema:"server process architecture"`
	DefaultCwd       string `json:"default_cwd" jsonschema:"server working directory used when cwd is omitted"`
	DefaultModel     string `json:"default_model,omitempty" jsonschema:"configured model ID; absence means agy's own default is used"`
	DefaultTimeout   string `json:"default_timeout" jsonschema:"configured job duration limit, distinct from the inline wait limit"`
	JobTTL           string `json:"job_ttl" jsonschema:"minimum age since creation for automatic collection of non-running jobs; zero disables automatic expiry"`
	MaxConcurrency   int    `json:"max_concurrency" jsonschema:"maximum admitted jobs in this server process"`
	RunningJobs      int    `json:"running_jobs" jsonschema:"jobs currently accounted for by this server process; not a host-wide count"`
	ConcurrencyScope string `json:"concurrency_scope" jsonschema:"process; sibling servers have separate global admission counters"`
	AgyVersion       string `json:"agy_version,omitempty" jsonschema:"version verified by the CLI version probe; absent when unavailable or unsupported"`
	AgyStatus        string `json:"agy_status" jsonschema:"ok or unavailable according to the binary and minimum-version check"`
	AuthStatus       string `json:"auth_status" jsonschema:"not_checked, ok or unavailable; ok verifies model listing, not permission to execute every model"`
	AuthCheckedAt    string `json:"auth_checked_at,omitempty" jsonschema:"UTC RFC 3339 timestamp of this optional model-list authentication probe"`
	AuthDetail       string `json:"auth_detail,omitempty" jsonschema:"authentication probe diagnostic; CLI integration warnings do not by themselves imply auth failure"`
	ReadOnlyEnforced bool   `json:"read_only_enforced" jsonschema:"false: this runtime does not provide a verified read-only execution backend"`
}

// RuntimeInfo never starts a model turn. Version checking is read-only; the
// optional auth check lists models and does not run Doctor's file-write probe.
func (m *Manager) RuntimeInfo(ctx context.Context, checkAuth bool) (RuntimeInfo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return RuntimeInfo{}, err
	}
	cwd, err = normalizeCwd(cwd)
	if err != nil {
		return RuntimeInfo{}, err
	}
	m.gate.mu.Lock()
	maximum, running := m.gate.max, m.gate.inFlight
	m.gate.mu.Unlock()
	out := RuntimeInfo{
		OS: runtime.GOOS, Architecture: runtime.GOARCH,
		DefaultCwd: cwd, DefaultModel: modelID(m.cfg.DefaultModel), DefaultTimeout: m.cfg.DefaultTimeout.String(),
		JobTTL: m.cfg.JobTTL.String(), MaxConcurrency: maximum, RunningJobs: running,
		ConcurrencyScope: "process", AgyStatus: "unavailable", AuthStatus: "not_checked",
	}
	out.Hostname, _ = os.Hostname()
	_, version, ok := m.checkAgyVersion(ctx)
	if ok {
		out.AgyVersion, out.AgyStatus = version.String(), "ok"
	}
	if checkAuth {
		check := m.checkAgyReachable(ctx)
		out.AuthStatus = "unavailable"
		if check.Status == CheckPass {
			out.AuthStatus = "ok"
		}
		out.AuthDetail, out.AuthCheckedAt = check.Detail, m.now().UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}
