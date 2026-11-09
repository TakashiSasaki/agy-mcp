<p align="center">
  <img src="assets/agy-mcp.svg" alt="agy-mcp Banner" width="100%">
</p>

<p align="center">
  <a href="https://github.com/tphakala/agy-mcp/actions/workflows/ci.yml"><img src="https://github.com/tphakala/agy-mcp/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/tphakala/agy-mcp/actions/workflows/codeql.yml"><img src="https://github.com/tphakala/agy-mcp/actions/workflows/codeql.yml/badge.svg" alt="CodeQL"></a>
  <a href="https://github.com/tphakala/agy-mcp/tags"><img src="https://img.shields.io/github/v/tag/tphakala/agy-mcp?label=version&sort=semver" alt="Version"></a>
  <a href="https://pkg.go.dev/github.com/tphakala/agy-mcp/v2"><img src="https://pkg.go.dev/badge/github.com/tphakala/agy-mcp/v2.svg" alt="Go Reference"></a>
  <a href="https://codecov.io/gh/tphakala/agy-mcp"><img src="https://codecov.io/gh/tphakala/agy-mcp/branch/main/graph/badge.svg" alt="Coverage"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/tphakala/agy-mcp" alt="License: MIT"></a>
  <a href="https://modelcontextprotocol.io"><img src="https://img.shields.io/badge/MCP-Protocol-00ADD8" alt="MCP Spec"></a>
</p>

An MCP (Model Context Protocol) server that wraps the [Antigravity CLI](https://antigravity.google) (`agy`), so any MCP client (Claude Code, Cursor, Cline, and others) can run `agy` prompts, peer reviews, and follow-up turns as native tools.

> Status: feature complete (stdio and HTTP transports, async and sync job lifecycle, model and session discovery, per-run controls and structured output, cross-platform builds) and verified against a live agy, with 1.1.15 the minimum supported version.

Use the [Linux self-hosting guide](docs/self-hosting.md) to build this fork and choose [ChatGPT through Secure MCP Tunnel](docs/self-hosting.md#connect-through-chatgpt-secure-mcp-tunnel) or a [standalone Streamable HTTP server under systemd](docs/self-hosting.md#run-a-standalone-http-server-with-systemd).

## Why

Driving `agy` from a shell for automation has two recurring problems:

- `agy -p` (print mode) reads stdin even when the prompt is passed with `-p`. If stdin is an open pipe that never closes, it blocks indefinitely. The fix is to always close stdin (`</dev/null`), which is easy to forget.
- A review can run for many minutes. A single blocking call ties up the caller and can exceed a client's tool-call timeout with nothing recoverable.

`agy-mcp` solves both by running `agy` as managed, asynchronous jobs behind a small, typed tool surface, and by capturing output to disk, with a completed run's result and metadata `fsync`'d, so the result survives a client disconnect, a server restart, or a crash.

Every job runs `agy --output-format stream-json`, and the supervisor decodes that event stream as it arrives. That is where the conversation id, the response text, the failure message, and the token accounting all come from, so agy-mcp never has to infer any of them.

## What it provides

- `agy_run` / `agy_status` / `agy_cancel`: start an AGY job with a required cloud `project`, inspect execution and sync independently, or cancel execution.
- `agy_run_sync`: start a job and wait for execution and required cloud sync inline. Use the returned `job_id` with `agy_wait` or `agy_status` if either phase exceeds the wait cap.
- `agy_wait`: block until an already-started job finishes (bounded, with MCP progress notifications); one call replaces an `agy_status` poll loop.
- `agy_list_jobs`: recover retained jobs by working directory, label, state or the original idempotency key.
- `agy_get_result`: retrieve terminal result text in bounded UTF-8 pages.
- `agy_info`: inspect runtime defaults, the actual tool catalog, cached quota and an optional model-list authentication probe without starting an agent turn.
- `agy_list_artifacts`: discover job-associated evidence with scope, file sizes, SHA-256 and job-handle GC eligibility.
- `agy_events` / `agy_logs`: read live progress events and bounded log/artifact pages without another model turn.
- `agy_verification`: parse a selected JUnit XML, native Go test JSON or restricted flat TAP report independently of agent/job completion.
- `agy_read_artifact`: read contents of a specific artifact, receipt, or log file produced by an `agy` job, with line range or tail limits and boundary confinement checks.
- `list_models`: enumerate available `agy` models, as the ids the `model` parameter accepts plus their display labels.
- `list_agents`: enumerate available `agy` agents, as the names the `agent` parameter accepts. Unlike `list_models` there is no id/label split, because `agy` takes an agent name verbatim; an empty list just means no agents are configured. Pass `cwd` (the directory you give `agy_run`) to include that repository's project agents from `.agents/agents/`; agy lists them only for a workspace directory, so without `cwd` they are absent. On agy 1.3.4 and later a name that is not in the list fails the run before any model call, as `failed` with `failure_reason: agy_error` and an error that lists the available agents (MEASURED against agy 1.4.6); an older agy runs its default agent instead (agy changelog 1.3.4), so check the name with `list_agents` first.
- `agy_usage`: report the remaining `agy` quota per quota group (for example `Gemini Models`, `Gemini 4 Argon`, and `Claude and GPT models`) with a level of `ok`, `low`, `critical` or `exhausted`. It spends no model quota. See [Quota](#quota).
- `list_sessions`: list known conversations so review threads can be continued. By default, it excludes defunct or ephemeral workspaces (/tmp, /var/tmp, scratch); supports pagination via `limit` and `offset`.
- `agy_list_projects`: discover and list registered Antigravity projects and repositories under /data with their paths, git remote URLs, default branches, and active linked worktrees.
- `agy_resolve_workspace`: resolve a project name, project ID, repository URL, or directory path and optional branch into an unambiguous concrete filesystem directory for `agy_run`.

Every new `agy_run` or `agy_run_sync` request supplies `prompt` and `project`. Select a registered cloud project ID or unique name from `agy_list_projects(sync_filter: "cloud_only")`. Optional `title` sets the conversation display title. Omission keeps an existing title or derives one from the prompt preview. `cwd` still determines execution. Continuing an already-cloud conversation requires its current cloud project. A continuation waits for any prior job on the same conversation to settle its sync.

Job execution `state` and `cloud_sync_state` are independent. A finished job may still be `pending` or temporarily `unavailable` for sync. Poll or wait on its existing `job_id`. `confirmed` is a receipt that the Remote Control daemon persisted the required project, title and conversation revision. `failed` with `cloud_sync_reason` reports a terminal sync failure, such as AGY exiting without a conversation ID. Existing jobs without sync intent remain readable. Cloud job admission requires a qualified AGY 1.5.3 installation and the Linux systemd [sync worker](docs/self-hosting.md#required-cloud-job-sync).

`agy_run` and `agy_run_sync` also take optional per-run controls: pick the `model`, reasoning `effort` (`low`/`medium`/`high`/`xhigh`/`max`), execution `mode` (`accept-edits`/`auto`; `plan` is refused, see the constraints below), a named `agent`, `sandbox` terminal restrictions, and a `json_schema` to constrain the result to structured output. Those run-shaping values are forwarded to `agy`; `idempotency_key` is handled only by agy-mcp and is never forwarded.

For retry-safe job creation, record an `idempotency_key` before sending a request when a transport failure could leave it ambiguous whether the run started. Reusing the same key with the same normalized request returns the existing job instead of starting another; using the key for a different request is rejected. Keys are scoped to the retained job store, shared across processes using that store, server restarts and both run tools, not per cwd or caller. Different stores have independent bindings. The binding lasts until the job is removed; automatic collection becomes eligible after the configured TTL measured from terminal completion (24 hours by default). Jobs interrupted without a terminal receipt use a durable first-observed-dead timestamp, recorded once by status inspection or garbage collection. Live jobs are never removed. The binding remains until an actual collection sweep removes the job. This is transport-retry protection rather than permanent deduplication. Normalization includes cwd, model, timeout, execution options, schema and artifact directories; explicitly pin cwd/model/timeout when retrying across configuration changes. `wait`, `priority` and `task_label` do not change the match. Optional `task_label` is discovery metadata and is never passed to agy; replay keeps the original label. Labels are trimmed, limited to 128 Unicode characters and cannot contain control characters. Avoid putting secrets in labels.

An exact retry can recover a readable admitted job even if another job record is unreadable. If the server cannot find an admitted match and encounters unreadable metadata, it refuses to start new work because the unreadable record could hold the key. Resolve the storage error and retry the exact request with the original key. A new key could start duplicate work.

For `json_schema` runs, agy-mcp returns AGY's terminal `structured_output` and fails closed when a successful terminal event lacks that field. It deliberately does not run a second local JSON Schema validator; schema conformance remains AGY's responsibility, while agy-mcp's responsibility is preserving and selecting the correct structured-result channel.

A delegated run should not rely on launching a long background shell command (a build, a test suite, a typecheck) and then waiting for it. In headless `-p` mode `agy` has no interactive loop to await one: after its agent goes idle it waits a bounded time for outstanding background tasks, then terminates any still running and exits, so a command that outlives that wait never returns its output to the agent and the run's answer is only progress narration. Before agy 1.2.9 the wait was about 5 seconds. From 1.2.9 it lasts until the run's `--print-timeout` deadline (agy-mcp passes the job `timeout`), capped at 30 minutes per agy's changelog, so a short background command now completes but one that outlives the job's budget is still cut off. The flip side is that a background command that never exits on its own, such as one waiting on an interactive prompt, holds the whole job open for as long as that wait lasts. Have the agent run any such verification in the foreground, or pass the results in the prompt. When `agy` does abort a run this way it still exits 0 cleanly (with a `SUCCESS` payload in the run MEASURED against agy 1.2.7); agy-mcp detects the abort from `agy`'s stderr and reports the run as `failed` with `failure_reason: background_aborted`, carrying the narration as a partial result, so a caller can tell it apart from a real answer rather than consuming a phantom success. If agy-mcp's own hard kill lands before `agy` gets there, the run is reported with `failure_reason: timeout` instead.

A delegated run should likewise avoid spawning an internal subagent to do the real work and then waiting for it. When `agy`'s agent delegates to a subagent, the subagent's output is not streamed on the parent run's stdout, so `agy` can finish and report a `SUCCESS` payload whose response is only the parent's progress narration ("waiting for the reviewer subagent to complete") while the actual result is lost. Unlike the background-shell abort above, this leaves no distinguishing signal in `agy`'s output: the parent's own token accounting counts the subagent's generation as ordinary output tokens, which is indistinguishable from a normal tool-heavy run that ends in a short answer, so agy-mcp cannot reclassify it without also downgrading genuine short answers. Keep the work in the parent run instead: instruct the agent to write its findings directly rather than delegate, or continue the same `conversation_id` with that instruction to recover the answer on the next turn.

Pass `model` as an **id** (`gemini-3.1-pro-high`), the form `list_models` returns in its `models` field, not as the display label `agy` prints beside it (`Gemini 3.1 Pro (High)`). `agy` takes either form on its own, but every display label in the `agy` 1.1.11 catalog was refused once `effort` was set too, at every effort. The same applies to `AGY_MCP_DEFAULT_MODEL`. Not every id accepts every effort either, and some accept none, so omit `effort` unless you need a specific one.

Session continuation rides `agy`'s own durable conversation store (`--conversation <id>`), so threads survive across calls without keeping a process warm.

**A delegated run can modify your files.** `agy` is launched with
`--dangerously-skip-permissions`, so the agent it starts can read *and write* anywhere under
`cwd` and under every directory passed in `dirs`, without prompting. For a review that must
not touch the repo, say so explicitly in the prompt; this is not enforced read-only access. The tools declare this on the wire: `agy_run` and
`agy_run_sync` are annotated `destructiveHint: true` / `openWorldHint: true`, `agy_cancel` is
`destructiveHint: true` / `idempotentHint: true`, and `agy_status`, `agy_wait`, `agy_usage`, `list_models`,
`list_agents`, `list_sessions`, the artifact/project discovery tools, `agy_list_jobs`, `agy_get_result`
and `agy_info` are `readOnlyHint: true`. Annotations are hints, so a client is free to
ignore them; one that does gate confirmation on them may stop prompting for the read-only
tools.

**Project rules load by default.** agy-mcp passes the run's `cwd` as an `--add-dir`, which makes it an agy workspace, so the project's `AGENTS.md` applies to the delegated run, including one at the repository root when `cwd` is a subdirectory ([#188](https://github.com/tphakala/agy-mcp/issues/188)). Whether the flag is needed depends on the agy version. MEASURED against agy 1.2.9 (issue #188): in print mode, running `agy` inside `cwd` did not make it a workspace, so no rules loaded without `--add-dir`, and `agy`'s `/hooks` listing showed the hooks in the workspace's `.agents/hooks.json` as enabled only with it. MEASURED against agy 1.2.16 and 1.4.8: `cwd` is a workspace without `--add-dir`. Its `AGENTS.md` (including the repository root's, from a subdirectory) is applied and its `.agents/hooks.json` hooks run, with or without the flag. Which release changed this is not measured. So `project_rules: false` only omits the implicit `--add-dir`: on agy versions that need the flag that keeps the rules and hooks out, and on 1.2.16 and 1.4.8 it does not, and neither has a flag that turns them off. Do not rely on it to isolate a run from a repository you do not trust. Each directory in `dirs` is passed as given and is a workspace in its own right, so listing `cwd` there makes it a workspace even with `project_rules: false`, and a repository passed only in `dirs` also has its rules applied and its hooks run (MEASURED against agy 1.4.8). The one shape measured to keep a repository's rules and hooks out on agy 1.4.8 is a `cwd` that is an empty directory outside any repository, with the repository left out of `dirs` and its files named by absolute path in the prompt; reading them that way worked, while writing outside the workspace was not measured. `continue_latest` and `list_sessions` then key on that directory rather than the repository, and `list_agents` for it shows none of the repository's project agents.

Two transports run the same core:

- **stdio** (default): zero-config, one line in your MCP client config.
- **Streamable HTTP** (opt-in): a long-lived, loopback-only daemon for multi-client use. See [HTTP mode](#http-mode).

## Requirements

- **agy 1.1.15 or newer**, on `PATH` or configured explicitly via `AGY_MCP_AGY_PATH`. This is a hard floor, not a recommendation. The floor is 1.1.15 because that release fixed streamed text corrupting non-ASCII characters into replacement characters in the `--output-format stream-json` text deltas. Those deltas are the text agy-mcp falls back to for a run cut short before agy's result event, or one agy completes with an empty response: on an older agy that text is lossy for non-ASCII content (another language, emoji, box drawing, unicode in code), and agy-mcp cannot repair bytes it never received intact. Earlier, lower floors it subsumes: 1.1.8 added `--output-format` to print mode and agy-mcp drives every job through the `stream-json` format it introduced; 1.1.10 is the first release where the run-shaping flags agy-mcp forwards in headless `-p` (`--model` and `--effort`) are honored rather than silently ignored; 1.1.12 added the machine-readable `models` envelope (`command.data.models`) that `list_models` decodes into ids and labels instead of tab-splitting text, and fixed `--mode` being ignored in headless `-p`; and 1.1.14 made a failed language server exit non-zero instead of reporting success, so agy-mcp records that failure as failed (all per agy's changelog). Older builds are refused rather than degraded. agy 1.2.9 or newer is worth having if you run jobs in parallel: per its changelog it fixed a conversation-database corruption risk in which checking the database file for write access could drop file locks that concurrent agy processes held on the same file. Whether parallel agy-mcp jobs actually share such a file has not been measured.

  The version is checked once per process, the first time a tool actually needs agy, and the verdict is cached. A binary that is too old is reported as `agy 1.1.15 or newer is required ...; found 1.1.7 at /usr/local/bin/agy`. A failed check is deliberately not cached, so upgrading agy is picked up without restarting the server.

  A missing `agy` does not stop the server from starting. `initialize`, `tools/list`, and `list_sessions` never exec it, so the lookup is deferred: the server starts, logs a warning to stderr, serves discovery normally, and the tools that do need the binary (`agy_run`, `agy_run_sync`, `agy_usage`, `list_models`, `list_agents`) fail per call with `agy not found on PATH; set AGY_MCP_AGY_PATH`. An `agy` installed later is picked up without restarting the server. An explicit `AGY_MCP_AGY_PATH` is treated differently: it is a claim about one specific binary, so a typo or a non-executable target still fails fast at startup.
- Go 1.27+ to build.
- The server builds and runs on Linux, macOS, and Windows. Job supervision (running agy as managed jobs via `agy_run` / `agy_run_sync` / `agy_status` / `agy_cancel`) is implemented on **Linux** and **macOS** (process groups, SIGTERM cancel, an advisory flock) and on **Windows** (Job Objects, `OpenProcess` + process creation time, `LockFileEx`); stdio/HTTP serving, `list_models`, and `list_sessions` work identically everywhere.

  Windows behaves the same as Linux with two documented differences:
  - **Cancel and hard timeout are hard kills.** Linux sends agy `SIGTERM` and waits a 10s grace before `SIGKILL`; Windows has no equivalent signal for a detached process, so `TerminateJobObject` ends the whole process tree immediately with no flush window.
  - **Cancel is delivered via a sentinel file, not a signal.** The manager writes a `cancel` file into the job directory and the supervisor polls for it (a few hundred ms), since Windows cannot signal an arbitrary process. Liveness uses the process creation time (an absolute wall-clock value) to defend against PID recycling, which subsumes the Linux boot-id check across reboots.

> Note: every job spawns a fresh `agy` process, which on startup launches whatever MCP servers are configured in agy's own `mcp_config.json`. Peer-review and automation runs usually do not need those servers, and a slow or hanging one adds latency to every job. agy 1.0.7+ bounds this with a per-server launch `timeout` (set `-1` to disable it). If startup is slow, give the unneeded servers a `timeout` in agy's `mcp_config.json`, or point agy at a trimmed config.
>
> Note (continuation): a follow-up turn runs `agy --conversation <id> -p <prompt>`, and the job result is the `response` field of agy's terminal `result` event. Verified against 1.1.10: a resumed run returns only the new turn and echoes the same conversation id, with `num_turns` incremented.
>
> Note (auth): agy-mcp passes its own process environment through to every spawned `agy`, so agy's normal sign-in is used by default. For headless or daemon deployments (HTTP mode, cron) where no browser is available, set `AGY_ADC_AUTH=1` in the server's environment to have agy sign in with Google Application Default Credentials (agy's changelog names the variable from 1.3.3 on; ADC sign-in itself arrived in 1.1.10). The variable does not create credentials: set them up with Google's tooling, for example `gcloud auth application-default login` on a host with a browser or a service-account key named by `GOOGLE_APPLICATION_CREDENTIALS` on one without (agy 1.1.12 made service-account logins work without extra configuration, per its changelog). agy does not read `USE_ADC`: setting it changes nothing (MEASURED against agy 1.4.8). On a host that had an ADC credentials file (its validity not checked) and normally signs agy in another way, setting the variable made every run fail (MEASURED against agy 1.4.8: `agy -p` exits 1 with `Error: authentication required. Run 'agy' to log in.`, `agy models` with `Please sign in to view available models. Launch the CLI without arguments to sign in.`, and `agy-mcp doctor` fails its auth check). agy's own advice there is to run `agy` interactively to sign in; its first-run setup for ADC shows the Google Cloud Platform Terms of Service (changelog, 1.4.1), and whether one interactive sign-in clears the failure for later headless runs is NOT MEASURED. agy 1.4.7 fixed skills, rules, custom agents, plugins, plugin MCP servers and hooks not loading under ADC sign-in (changelog, 1.4.7); which earlier releases had that bug is not stated. No agy-mcp flag is needed; unset the variable to fall back to the other sign-in methods. agy-mcp recognizes that failure: when agy fails a run with an ERROR result, or exits non-zero without a SUCCESS result, the failure is not a quota wall, and the result's error or stderr has the `authentication required` line above (or, per the changelog for 1.3.3, `Authentication expired. Please log back in.`), a run with no result text gets a `recovery` note saying a person must sign agy in on the host running agy-mcp (or check the ADC credentials when `AGY_ADC_AUTH` is set). Its `failure_reason` stays `agy_error`. `agy-mcp doctor` warns when `USE_ADC` is set without `AGY_ADC_AUTH` on, and when `AGY_ADC_AUTH` is on with an agy below 1.4.7 (doctor counts only `1` and `true` as on, the two spellings MEASURED against agy 1.4.8). It reads its own environment, so run it with the server's environment (the same unit file, container or shell).

## Install

### Homebrew (macOS / Linux)

```bash
brew install tphakala/tap/agy-mcp
```

This installs the prebuilt release binary (macOS and Linux, amd64 and arm64) from [tphakala/homebrew-tap](https://github.com/tphakala/homebrew-tap); upgrade later with `brew upgrade agy-mcp`. The tap formula is regenerated on every release, so upgrades track new versions as they ship.

### Go install

```bash
go install github.com/tphakala/agy-mcp/v2@latest
```

The module path carries a `/v2` suffix from v2.0.0 on, as Go requires for a major version. `go install github.com/tphakala/agy-mcp@latest` still resolves, but to the last v1 release.

### Build from source (forks and local branches)

Requirements: Git, and a Go toolchain new enough for the `go` line in [go.mod](go.mod) (currently Go 1.27). With Go 1.21 or newer, an older local `go` fetches the toolchain named in `go.mod` automatically (`GOTOOLCHAIN=auto`, the default), which needs network access on the first build. Nothing else is required: the build is pure Go and has no code generation step.

```bash
git clone https://github.com/<owner>/agy-mcp.git   # your fork, or the upstream repository
cd agy-mcp
git checkout <branch>                               # the branch you want to build
go build -trimpath -o agy-mcp .                     # writes ./agy-mcp (agy-mcp.exe on Windows)
./agy-mcp --version
```

To install the result somewhere on your `PATH`, build straight to the destination, for example `go build -trimpath -o "$HOME/.local/bin/agy-mcp" .`. Build into a temporary file and move it into place if a running server already uses that path.

**Version string.** `agy-mcp --version` (also `agy-mcp version` and `agy-mcp -version`), the server startup log line and the MCP `serverInfo.version` all report the same string, so you can tell which build a running server is:

| Build | Reported version |
|-------|------------------|
| `go install github.com/tphakala/agy-mcp/v2@vX.Y.Z` (tagged release, no VCS data) | `vX.Y.Z` |
| `go build` inside a Git checkout, clean tree | `v2.10.0+local.<short-sha>` |
| `go build` inside a Git checkout with uncommitted changes | `v2.10.0+local.<short-sha>.dirty` |
| Source tarball or `-buildvcs=false`, so there is no VCS data | `dev` |

The `+local.<short-sha>` suffix is [SemVer 2.0.0 build metadata](https://semver.org/#spec-item-10): it keeps the upstream base version and identifies the exact commit, so a modified build is never mistaken for an unmodified release. The base is the `BaseVersion` variable in [internal/mcptools/tools.go](internal/mcptools/tools.go); update it when you rebase a fork onto a newer upstream release. Both values can be overridden at link time:

```bash
go build -trimpath \
  -ldflags "-X github.com/tphakala/agy-mcp/v2/internal/mcptools.BaseVersion=v2.10.0" -o agy-mcp .
# or set the whole string:
#   -X github.com/tphakala/agy-mcp/v2/internal/mcptools.Version=v2.10.0+local.mybuild
```

To verify a change before using the binary, run `go vet ./...` and `go test ./...` (see [AGENTS.md](AGENTS.md) for the full list including lint).

### Linux job cleanup and systemd tunnel restarts

The dedicated `run-job` process initializes Linux child-subreaper adoption and verifies pidfd signaling before starting AGY. This requires kernel support and permissions for `PR_SET_CHILD_SUBREAPER`, `pidfd_open`, and `pidfd_send_signal` (normally Linux 5.3 or newer), plus readable `/proc` process identities. Initialization failure records `spawn_failed` with the diagnostic in the job stderr artifact and starts no AGY turn. The long-lived manager is not made a subreaper.

While AGY runs, the Linux supervisor reaps exited adopted children by PID and leaves the AGY root exit status to its own waiter. Linux status treats a zombie supervisor as stopped. Linux cancellation verifies the recorded start time after opening a pidfd and sends the signal through that descriptor. Cancellation reports an error if a live supervisor lacks a verifiable identity or pidfd support.

On cancellation or timeout, the Linux supervisor signals currently verified direct children by pidfd after checking their start ticks and parent identity, then repeats as deeper descendants are adopted, including shell commands that create a new process group/session and orphans adopted from intermediate processes. It continues bounded cleanup after AGY itself exits, escalates after the existing grace period, and waits for cleanup before writing the terminal sentinel. A further two-second SIGKILL cleanup window is bounded; inability to complete cleanup is recorded in job stderr. This is job-process lifecycle handling, not confinement against privileged or deliberately hostile process escape. Normal AGY completion keeps the existing background-task behavior and does not provide this cancellation cleanup guarantee. macOS retains process-group semantics; Windows retains Job Object semantics.

A service restart can still kill the supervisor itself. [systemd's kill policy](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html) applies to cgroups: `mixed` sends a final kill to remaining unit processes when the main process exits, and `control-group` also terminates descendants. Process groups and sessions do not move a job out of that cgroup.

For the specific deployment where a tunnel hosts these independently supervised jobs, [deploy/agy-tunnel-job-lifecycle.conf](deploy/agy-tunnel-job-lifecycle.conf) is an opt-in instance drop-in for `tunnel-client@agy.service`. It changes only `KillMode=process`; do not install it in the shared tunnel template or unrelated services. This intentionally lets bounded job supervisors outlive the tunnel service. systemd discourages process mode generally because descendants can outlive the unit, so it is not a generic safer default. After deployment, verify that the old tunnel and MCP server exit, exactly one new tunnel owns the profile, live jobs retain their identities and continue, and cancellation/timeouts still clean up their workload processes. Preserve credentials, tunnel ID and profile. Stop/cancel jobs explicitly when stopping their work is intended; do not infer job cancellation from stopping the tunnel.

### Request diagnostics and cached tool definitions

SIGTERM and SIGINT stop the stdio server successfully, without reporting context cancellation as a server failure.

The server writes JSON request records to stderr for `initialize`, `server/discover`, `tools/list`, and `tools/call`. Each request has `started` and `completed` records correlated by the process `pid` and a process-local `call_id`, with the tool name and handler duration in `elapsed_ms`. `outcome` distinguishes `ok`, `tool_error`, `protocol_error`, `cancelled`, `deadline_exceeded`, and `panic`. An `outcome: "ok"` response may carry `job_state: "failed"` and `failure_reason: "timeout"` when the tool returns the job's failure successfully. These records describe handler completion. Use the tunnel client's response-delivery counters to check delivery. The logger omits arguments, headers, prompts, result bodies, paths and error text.

Completed start errors from `agy_run` and `agy_run_sync` include recognized `start_error_code` values and the job ID when available. With a start error code, `job_started: false` means the request created no work, `true` means it created a job, and omission means the start was uncertain. Successful `agy_get_result` calls log `result_complete` from the response's pagination flag, alongside `job_state`. Successful `agy_verification` calls log `verification_outcome` from the selected evidence as `passed`, `failed`, or `unknown`.

`agy_info.runtime.executable_state` is a passive Linux snapshot comparing `/proc/self/exe` with the path returned by `os.Executable`: `current` means the same file identity, `replaced` means the running process still uses a different executable, `missing` means its installed path no longer exists, and `unknown` means the comparison is unavailable (including other platforms). Replacement or removal includes recovery advice in `executable_advice`; no restart is performed. Reconnect the affected MCP server after verifying the installation to load an atomic replacement. `current` does not prove the newest release is installed, detect edits made in place, or refresh client tool metadata. A newly launched `doctor` process cannot establish which binary an existing client connection uses.

`tools/list` completion records also include `tool_count`, `catalog_sha256` (a name-sorted fingerprint of the returned tool definitions, including schemas, with JSON object keys sorted recursively), and `catalog_complete`. A paginated response fingerprints only its returned page. `agy_info` reports the complete server catalog using the same fingerprint. Compare its `tool_names` with the tools visible to the calling agent to distinguish an older server from a connection that still exposes cached metadata.

To inspect the native contract without a public server URL, call `agy_info` with
`{"tool":"agy_run","include_instructions":true}`. Its optional `contract.tool_json`
is the complete JSON-encoded descriptor from the same catalog as `tools/list`,
including field descriptions, examples, defaults and enums. Parse that string
and compare it with the corresponding native tool object; JSON key order is
not significant. `contract.tool_sha256` hashes the exact returned string bytes.
`contract.instructions` contains the full native initialize instructions, even
when the client's server overview shows only an initial excerpt. Top-level
`instructions_bytes` and `instructions_sha256` always identify the full UTF-8
instructions. Omit `tool` and leave `include_instructions` false for a compact
diagnostic response. An unknown tool name is an error and never selects a file
or executes a tool.

This read-only inspection uses the existing MCP connection and its access
controls. It does not require an unauthenticated diagnostic URL, expose runtime
credentials, or change how a client renders declarations. The runtime
`server_version` identifies the responding process; a matching tool count alone
does not prove that the client's cached descriptions or schemas are current.

After changing the server's tools, schemas, or instructions, refresh the existing ChatGPT connection to pull the updated definitions, then test from a new conversation. Restarting the MCP server alone does not refresh the connection's cached tool definitions. See [OpenAI's metadata refresh procedure](https://developers.openai.com/plugins/deploy/connect-chatgpt#refresh-metadata). Preserve the existing app, tunnel ID, authentication, and permissions.

Some clients render input types without their property descriptions. Both run tools therefore include a compact argument guide and execution boundary in their own descriptions, in addition to property descriptions, examples and finite-value enums in the native schema. Empty-string enum values preserve the existing omission/default behavior. Only static defaults are inserted by schema validation; cwd, model, agent, effort, mode and timeout are resolved by the server or CLI as described in their fields. Compare the actual `initialize` instructions and `tools/list` JSON at the direct and relay layers before attributing missing or truncated prose to the server. A connection's short overview is not evidence that the native instructions were truncated.

## Use with Claude Code (stdio)

```bash
claude mcp add agy -- agy-mcp
```

Or add to your MCP client config:

```json
{
  "mcpServers": {
    "agy": { "command": "agy-mcp" }
  }
}
```

## Tools

- `agy_run(prompt, project, title?, model?, effort?, mode?, agent?, sandbox?, dirs?, project_rules?, conversation_id?, continue_latest?, cwd?, timeout?, json_schema?, idempotency_key?, priority?, task_label?, artifact_dirs?, expected_git_branch?, expected_git_head?)` -> `{ job_id, conversation_id?, state, cloud_project_id, cloud_sync_state, quota? }`
- `agy_run_sync(prompt, project, title?, model?, effort?, mode?, agent?, sandbox?, dirs?, project_rules?, conversation_id?, continue_latest?, cwd?, timeout?, json_schema?, idempotency_key?, priority?, task_label?, artifact_dirs?, expected_git_branch?, expected_git_head?, wait?, include_result?)` -> `{ job_id, state, elapsed, result?, error?, failure_reason?, recovery?, retryable?, error_id?, conversation_id?, cloud_project_id?, cloud_sync_state?, cloud_sync_reason?, execution_cwd?, git_branch?, git_head?, git_dirty?, model?, requested_model?, selected_model?, selected_model_source?, effective_model, effective_model_source?, partial, num_turns?, usage?, job_usage?, usage_scope?, step_type?, quota?, note? }`
- `agy_status(job_id, include_result?)` -> `{ job_id, state, elapsed, result?, error?, failure_reason?, recovery?, retryable?, error_id?, conversation_id?, cloud_project_id?, cloud_sync_state?, cloud_sync_reason?, execution_cwd?, git_branch?, git_head?, git_dirty?, model?, requested_model?, selected_model?, selected_model_source?, effective_model, effective_model_source?, partial, num_turns?, usage?, job_usage?, usage_scope?, step_type?, quota? }`
- `agy_wait(job_id, wait?, include_result?)` -> `{ job_id, state, elapsed, result?, error?, failure_reason?, recovery?, retryable?, error_id?, conversation_id?, cloud_project_id?, cloud_sync_state?, cloud_sync_reason?, execution_cwd?, git_branch?, git_head?, git_dirty?, model?, requested_model?, selected_model?, selected_model_source?, effective_model, effective_model_source?, partial, num_turns?, usage?, job_usage?, usage_scope?, step_type?, quota?, note? }`
- `agy_cancel(job_id)` -> `{ state }`
- `agy_usage(max_age?, models?)` -> `{ checked_at, groups, model_groups }`, each group `{ name, description?, level, remaining_percent, window, reset_time?, buckets }`
- `agy_list_jobs(cwd?, task_label?, state?, idempotency_key?, limit?, offset?)` -> `{ jobs, total_count, next_offset? }`
- `agy_get_result(job_id, cursor?, max_bytes?)` -> `{ job_id, conversation_id?, state, partial, failure_reason?, error?, recovery?, retryable?, result_available, content, total_bytes, result_sha256?, next_cursor?, complete, next_action }`
- `agy_info(check_auth?, tool?, include_instructions?)` -> `{ api_version, server_version, runtime, max_inline_wait, quota?, tool_names, tool_count, catalog_sha256, instructions_bytes, instructions_sha256, contract? }`
- `agy_events(job_id, cursor?, limit?, max_bytes?)` -> timestamped event pages, capture health, CLI process outcome and agent outcome
- `agy_logs(job_id, stream?, path?, cursor?, from_start?, tail_bytes?, max_bytes?)` -> bounded byte pages or tails of job streams/associated files
- `agy_verification(job_id, path)` -> parsed report outcome, counts, bounded failure details and exact report SHA-256 (`format: "junit"` by default, `"go_test_json"` or `"tap"`)
- `agy_list_artifacts(job_id, subpath?, pattern?, max_entries?)` -> `{ job_id, root, entries, scope, roots, expires_at?, limit_reached }`
- `agy_read_artifact(job_id, path, start_line?, end_line?, tail_lines?, max_bytes?)` -> `{ job_id, path, size_bytes, sha256, total_lines, start_line, end_line, truncated, content }`
- `agy_list_projects(query?)` -> `{ projects }`
- `agy_resolve_workspace(project, branch?)` -> `{ resolved, path?, branch?, head?, head_full?, ambiguous, candidates?, error? }`
- `list_models()` -> `{ models, model_details }`
- `list_agents(cwd?)` -> `{ agents }`
- `list_sessions(dir?, exclude_transient?, limit?, offset?)` -> `{ sessions, total_count }`

Status-shaped responses (`agy_status`, `agy_wait`, `agy_run_sync`) also include `result_available`, `result_included`, `next_action`, and optional `task_label`, `progress_updated_at`, `step_index`. An absent progress timestamp means unknown; a quiet timestamp alone does not prove the process is stuck.

Every status-shaped response includes the boolean `partial`, including `false`. It describes result provenance, not factual correctness, independently validated schema conformance or passing tests. On older servers, an absent `partial` serialized the old false value and gives no additional guarantee. `agy_get_result.complete` only means there are no more pages in that snapshot. Neither flag establishes task success; inspect `state`, `result_available` and verification evidence.

All execution and schema-file paths refer to the server host, which can differ from the caller's machine. `agy_info.runtime` reports `hostname` when available, `os`, `architecture`, the effective `default_cwd`, CLI version, configured defaults and `read_only_enforced: false`. An omitted cwd uses that server directory; a relative cwd resolves from it. Symlinks are resolved when possible. cwd selects where the process runs and `dirs` adds read/write workspaces; relative dirs resolve from cwd. Neither is a filesystem boundary. `sandbox: true` only forwards the CLI terminal-restriction flag: agy-mcp does not verify filesystem confinement, network isolation or read-only access. Every run disables permission checks, and a no-edit prompt is an instruction.

`conversation_id` resumes the named context while cwd still selects the execution directory; keep the original cwd unless intentionally changing it. `continue_latest: true` resolves the latest thread for canonical cwd or starts fresh if none is cached. An unreadable or malformed cache refuses new work with `conversation_cache_unavailable`; it never silently starts a fresh conversation. An idempotent replay with a stored request key still returns the existing job without reading that cache; legacy records that only stored CLI arguments require a readable cache for comparison. The two selectors are mutually exclusive. Simultaneous continuations of the same ID are refused even with different cwd values; fresh runs can share cwd. A `json_schema` argument is a JSON-encoded schema string, not an object, or a file path on the server. Prefer an absolute file path; the value is forwarded unchanged to agy running in cwd. For example, pass the string `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`. Omit it for free text.

### Agent delegation and recovery

1. Discover available tools. When available, call `agy_info` to check effective defaults and compare its catalog with the tools exposed by your connection. `auth_status: "not_checked"` means no authentication probe was requested. `check_auth: true` checks model-list access, which does not prove that every model can execute. For a fresh quota reading, use `agy_usage`.
2. Make the prompt self-contained: state the absolute `cwd`, scope, source revision when relevant, acceptance criteria, expected output and permitted edits. The delegated agent cannot see the calling conversation. Ask for shell verification in the foreground. A review prompt and the CLI `sandbox` flag do not establish filesystem read-only access; `agy_info.runtime.read_only_enforced` is currently `false`.
3. Record the idempotency key before sending `agy_run`, together with a short `task_label`. Retain the returned `job_id`, then wait on that same job with `agy_wait`. An inline wait expiring means the job may still be running; do not send the task again merely because the wait ended.
4. If the handle is lost, call `agy_list_jobs` with the original `idempotency_key` or label and cwd, then inspect the match. Listing covers the configured retained job store, not agy's conversation history. It omits prompts, raw arguments, errors, result bodies and idempotency tokens. Labels match case-insensitive substrings; other filters match exactly, with cwd canonicalized. An exact key search can recover a readable match despite an unreadable record. It omits metadata-only records left before supervisor admission. A missing match can also mean admission is still in progress, so replay the exact saved request. If it finds no match after an incomplete scan, it returns an error. Default page size is 20, maximum 100. Results sort by start time descending, then job ID descending. `total_count` is a scan observation; concurrent starts or expiry can shift offset pages. Listing does not collect results.
5. Use `agy_wait` or `agy_status` with `include_result: false` for metadata, then `agy_get_result` for terminal text. Inspect `state`, `partial`, `failure_reason`, `retryable` and recovery advice before adopting the answer or continuing. Retrieval succeeding does not mean the task succeeded. Verify edits and acceptance criteria in the caller's workspace before committing.

### Reviews with inspectable evidence

Choose the review's scope explicitly. A conceptual review assesses the supplied
assumptions; it does not establish new defects in a repository that was never read.
For a repository review, supply exact files and a revision or diff, including relevant
DDL, design contracts and tests. `execution_cwd` and `git_head` record job context,
not proof that the agent inspected those sources. Ask for an inspection inventory,
supporting evidence and gaps, and independently reconcile each actionable finding.

Adapt this prompt to the task; replace the paths and revision before sending it:

```text
Perform a repository review of <absolute repository path> at <commit or diff>.
Read <DDL files>, <design contract files> and <test files>. If a source cannot be
read or the revision differs, report the gap instead of assuming its contents.
Do not edit/create/delete files or run mutating commands. Do the review directly
and return findings yourself, without delegating it to another agent.
Report:
- Scope: conceptual, repository, or mixed; the actual inspected files/revision,
  supplied assumptions, and requested sources you could not inspect.
- Findings: distinguish confirmed defects, conditional risks and proposals.
  Include severity, file/line or supplied premise, reasoning and a counterexample
  or reproduction where possible. Do not invent findings to reach a target count.
- Verification: commands actually run, observed results and checks not run.
  Run authorized checks in the foreground and identify any retained evidence.
- Standards/API proposals: exact specification version/profile, primary source,
  required extensions and whether the target implementation supports them.
  Label untested syntax as a proposal, not an executable fix.
- Remaining gaps: what this review cannot establish, even if no defects were found.
```

These are instructions to the agent, not enforced read-only access or a server
attestation of inspection. A structured `json_schema` response can make the inventory
easier to consume, but neither JSON structure nor `partial: false` verifies its claims.
Check the cited files, commands, artifacts and standards independently. Include both
examples that must be accepted and counterexamples that must be rejected when testing
proposed constraints; otherwise a constraint may pass by inspecting no relevant data
or reject states the design deliberately permits.

Report model provenance separately from review quality. `requested_model` and
`selected_model` do not confirm backend execution. When `effective_model` is null,
state that the actual backend model is unverified; do not ask the model to self-identify
as a substitute for provider metadata. This limitation does not discard useful findings.

### Live progress and verification evidence

For a long verification task, prepare a distinct evidence directory in the task prompt and declare it with `artifact_dirs`, for example `/tmp/pr253_evidence_20261103`. Have the delegated agent write test stdout/stderr there and request a JUnit report (`pytest --junitxml=...`). This field only authorizes evidence retrieval. It is not forwarded as an agy workspace, does not load extra workspace rules, and does not enforce where the agent can write.

When `artifact_dirs` is non-empty, artifact reads use those directories plus the job state directory. The legacy cwd/add-dir roots are excluded. The first declared directory is the default for relative artifact paths and listings; absolute paths may identify another declared root. Without the field, existing calls retain the legacy workspace/add-dir scope, reported as `scope: "legacy_workspace"`. Association means permission to retrieve a file, not proof that the job generated it. An undeclared external directory from an older job is not silently added to its authority. Inner symlinks and rename races cannot redirect an opened artifact outside its selected root.

Use the tools as follows:

1. `agy_status(job_id, include_result: false)` gives a compact lifecycle/progress snapshot. Repeated CLI step updates advance the progress heartbeat, even when `step_type` and step index stay unchanged. A quiet timestamp still does not prove a hang.
2. `agy_events(job_id)` reads CLI events observed so far, including the fields the response decoder does not understand. Retain `next_cursor`, including at EOF, to follow later appends. A record carries its observation time and original payload. The default page scans up to 50 records within a 64 KiB event JSON budget (maximum 200 records and 256 KiB). Payloads over 16 KiB, or those that do not fit the page, are marked `payload_omitted`; their original bytes remain available through the event log.
3. `agy_logs(job_id, stream: "stderr")` returns a tail without another agent turn. `stdout` means the assistant text captured by agy-mcp, and `stderr` means agy's own stderr. They are not independently recorded stdout/stderr of each nested shell command. Use `stream: "artifact", path: "pytest.log"` for a test log in associated roots. `from_start: true` starts at byte zero; otherwise the initial tail window defaults to `max_bytes`. Pages default to 64 KiB, with a 256 KiB limit; the initial tail window can be up to 4 MiB. Cursor offsets and page hashes refer to original bytes. Invalid UTF-8 is consumed without stalling pagination and replaced only in displayed content. While the job is running, an incomplete character at the observed end of the file is withheld with `pending_utf8: true`; `has_more` is false when only these pending bytes remain. Retain `next_cursor` and wait for append or job completion before retrying. At completion an unfinished suffix is consumed as invalid text. A tail skips the remainder of a valid character split by its starting boundary; standalone invalid bytes are retained as replacement characters. A tail starting inside a running incomplete suffix retains its leading-byte offset, up to three bytes before the requested tail window.
4. `agy_list_artifacts` discovers associated reports. `limit_reached` means more matching entries may exist. `expires_at` is terminal completion time (or durable first-observed-dead time when no terminal receipt survives) plus the configured job TTL, matching garbage collection. It is omitted for running jobs, disabled expiry, or an interruption not yet observed by status/GC. Collection happens on a later sweep; this timestamp is not a promise to retain or delete external `/tmp` files. `agy_read_artifact` reads line ranges of stable files with a full-file hash; it scans the file and is intended for completed evidence. Displayed artifact content is valid UTF-8 and its byte limit applies after replacing invalid byte sequences; the full-file hash and size still describe original bytes. Use `agy_logs` for bounded live byte reads. The listing hashes files up to 32 MiB each, within a shared 128 MiB and two-second hashing budget. Entries can omit SHA-256 when the budget is exhausted, when the opened file differs from the listed identity, size or modification time, or when its size or modification time changes during hashing. Use `agy_read_artifact` for an individual full digest.
5. `agy_verification(job_id, path: "junit.xml")` reads up to 4 MiB of explicitly selected JUnit XML. It returns the exact report hash, observed testcase counts and up to 100 bounded failure details. Each returned failure identifier (`test`, `class`, `reported_type`), message and detail is limited to 2048 UTF-8 bytes; the raw artifact retains the original text. Set `max_failures: 0` for counts and outcome only, or a limit from 1 to 100 for sampled failures. Omission preserves the 100-detail default. `failures_truncated` signals omitted details; the entire report is still parsed, so the limit never hides a failing or malformed report from the verdict. Incomplete/malformed XML, invalid suite or testcase nesting, elements nested inside `<skipped>` / `<failure>` / `<error>`, empty or duplicate counters, counter disagreements, unrelated formats and reports with no executed testcases return `verification_outcome: "unknown"`, never `passed`. Reports carrying disabled, suppressed or unrecognized testcase `status` / `result` attributes or positive suite `disabled` counters return `unknown`. Disabled or unsupported-attribute testcases do not count as passed. Native CTest JUnit explicit skips for `SKIP_RETURN_CODE` and `SKIP_REGULAR_EXPRESSION` count as skipped when the report includes the measured `notrun` and skip markers. Other `notrun` evidence, including missing executables and fixture setup failures, remains `unknown`. Normal GoogleTest reports (`status="run"`, `result="completed"`, `disabled="0"`) and ordinary JUnit without these attributes remain supported. Malformed or duplicate `disabled` counters return `unknown`. A valid report with failures returns `failed` as an ordinary successful MCP retrieval. Access or read failures are MCP tool errors.

Use `format: "go_test_json"` with the unmodified output of `go test -json ./...` to avoid a conversion program. The parser requires package and testcase lifecycle completion, preserves package/build failures separately in `package_failures`, accepts non-failing toolchain `build-output` diagnostics (such as `-gcflags=-m` output), and returns `unknown` for malformed, incomplete, contradictory or unsupported event streams (including `FailedBuild` references to a `build-output` stream missing its terminal `build-fail`). Test counters count testcase executions; package failures are additional and can fail a report even when all testcases passed. As with JUnit, this is report evidence, not an independent execution receipt. Benchmark (`bench`) events are currently unsupported and return `unknown`, including when mixed with tests. No filename-based format guessing occurs. Native Node.js JUnit reports with testcases immediately inside the root `testsuites` element are also accepted; invalid deeper nesting remains unknown.

Native Node.js JUnit summary comments are checked when present. Node 24.22.2 can omit a failed parent assertion from testcase elements while retaining it in the footer; such unrepresented failures or cancellations, incomplete/duplicate summaries, and inconsistent summary totals return `unknown`. Summary test totals may legitimately include successful parent tests absent from the testcase elements. General JUnit does not require these comments. A todo testcase containing an explicit failure remains conservatively classified as failed.

Use `format: "tap"` for the restricted flat TAP emitted by simple C or Makefile test harnesses. The parser accepts an optional initial `TAP version 13`, one leading or trailing `1..N` plan, sequential explicitly numbered `ok` / `not ok` cases with optional ` - name`, comments, and `# SKIP` directives. An escaped hash in a description is literal; only a hash preceded by an even number of consecutive backslashes can begin a directive. This preserves failures in native Test::More 1.302194 output whose test names contain `\# SKIP`. Plans must match the testcase count. Empty, skip-only, malformed, invalid UTF-8, truncated, bailout, TODO, nested subtest and YAML diagnostic reports return `unknown`. This is a measured subset, not a general TAP implementation. Failure names and returned details retain the same bounds as other formats; `max_failures: 0` returns only the summary without changing the verdict.

For low-output verification, redirect test stdout/stderr into a separate evidence directory and retain the command's exit code. Print one short summary on success. On failure, retrieve only the failing names and bounded diagnostic excerpts; keep the full log on disk. Start with `agy_wait(include_result: false)` and `agy_verification(max_failures: 0)`, then request a few failure details or use `agy_logs` byte pages if needed. Store large diagnostic fixtures outside the implementation workspace to avoid unnecessary agent reads. These controls reduce response content, not the tests executed or the verdict checks.

These outcomes remain independent:

| Evidence | What it establishes |
|---|---|
| `state` / `job_state` | agy-mcp's job lifecycle, including timeout/cancellation |
| `agent_outcome` | `success` or `error` from the CLI terminal result; otherwise `unknown` |
| `process` | The supervisor's observed **agy CLI** exit code, OS signal termination and supervisor stop flags. Spawning failure has a null exit code |
| Event `payload` | Provider-emitted fields. A tool step's `DONE` state does not establish shell exit code zero. Command details and nested exit codes are unknown unless the provider emits them |
| `verification_outcome` | A parsed report's result, not a diagnosis that the product caused a failure |

For example, a job may be `done`, have `agent_outcome: "success"` and CLI `process.exit_code: 0`, while a JUnit report says `failed` with an explicit `subprocess.TimeoutExpired` type. That describes a reported harness timeout; it does not prove a product fault or that a child process stopped. Narrative text is never converted into execution receipts or passing tests. Report contents also do not independently attest that commands ran or that the file was produced by this job.

If the supervisor has stopped and its completion marker is unavailable, the server uses a valid process receipt to recover the job outcome, including a recorded spawn failure. With a matching process receipt, ordinary CLI exits 124 and 127 remain CLI failures. New receipts also distinguish ordinary exits 130 and 143 from SIGINT or SIGTERM. Older receipts without signal provenance retain the prior classification for those two codes. A CLI launch error is available through the job's stderr log. A saved answer is still available for inspection. Jobs with only a saved response keep the legacy recovery behavior.

The supervisor appends timestamped CLI events while the job runs and records its process outcome before the completion sentinel. These files survive ordinary timeout/cancellation with the job's existing retention. The journal cap is 64 MiB per job. `capture.truncated`, `capture.dropped_events`, `capture.malformed_lines`, `skipped_records` and `incomplete_record` expose lost evidence. `capture.closed` describes capture closure, not task success. Incomplete live records are withheld at the same cursor; a terminal incomplete record is counted as lost and skipped. A complete record above the structured reader limit, or one whose metadata alone exceeds the page budget, increments `skipped_records`. Later records remain readable. The budget covers the JSON events array. Inline payloads that exceed it can be omitted while retaining metadata. Use `agy_logs` to read the original bytes. Cursors bind job, stream, file identity, generation and nearby preceding content. Use them for append-only streams. They detect replacements and truncations that change those checks. For rewritten files, start a new read. None of these reads marks the final model answer collected. Older jobs can have `available: false` and no process receipt; their missing history is not reconstructed by another model turn.

### Planned execution controls

Verified read-only filesystem/network/credential policies, pre-deadline cooperative checkpoints, atomic workspace/head locks and provider-backed token budgets are separate follow-up work. This release exposes evidence and its limits; it does not promise resumable shell processes or token-budget enforcement. The installed agy 1.4.12 `--help` was inspected without starting a model turn; its listed flags alone do not establish a supported per-command receipt or token-budget contract. A general `agy_exec` interface remains deferred until its execution policy is specified.

### Start errors

Start refusal outputs include `start_error_code`, equal to the native
`error_code`. Use `start_error_code` when a connector replaces `error_code` with a
generic transport category. The production connector was observed on 2026-11-07
to replace native `idempotency_conflict` and `conversation_busy` with
`INVALID_ARGUMENT` while retaining `job_started` and `next_action`.

Rejected `agy_run` and `agy_run_sync` calls remain MCP tool errors (`isError: true`) with the original text, and also return structured `{ error_code, message, job_id?, job_started, retryable, next_action, retry_after?, quota_group? }`. The output schema accepts either the existing success object or this error object. These operation errors do not add job states or change the closed `failure_reason` set.

| `error_code` | Recommended action |
|---|---|
| `invalid_argument`, `idempotency_conflict` | Fix the request; do not change keys merely to evade a conflict. |
| `workspace_unavailable` | Select or create an existing working directory; no job was started. Existing idempotent replays do not require their former directory to remain present. |
| `workspace_mismatch` | Recheck the selected Git branch/full HEAD and update the expected preconditions only when the changed workspace is intended; no job was started. |
| `capacity_exceeded` | Wait for capacity before retrying the same request. |
| `conversation_busy` | Wait for the conversation's active job. |
| `conversation_cache_unavailable` | No new job started. Retry the same request when the conversation cache is readable, or deliberately select an explicit `conversation_id`. |
| `quota_low` | Report that optional work was skipped; `retry_after` carries a reset timestamp when known. |
| `quota_mapping_unknown` | Select an explicitly mapped model if the optional work is still appropriate. |
| `agy_unavailable`, `unsupported_platform` | Fix the host configuration or environment. |
| `idempotency_in_progress` | Retry only the same normalized request with the same key; creation may already be underway. |
| `wait_interrupted` | Inspect the returned `job_id`; work has already started. |
| `start_indeterminate`, `start_failed`, unknown future values | Inspect existing work before considering a retry. |

`job_started: false` means this request started no new work; an earlier job may own a conflicting key. `true` means a job was created, for example before its inline wait was interrupted. `null` means execution is uncertain. A newly launched supervisor waits until its PID is recorded before starting AGY. If the response to a start is lost, the admitted job can already be running. Preserve the key, look up the existing job, and replay the exact request with that key if needed. `retryable` is advisory within `next_action` and never supplies user authorization. `retry_after` is absent when unknown, rather than an invented delay.

When a keyed start records a symlinked cwd, an exact retry can recover the job
if the alias is missing or dangling. An alias that resolves to a different directory
still conflicts. For older records created before the alias was recorded, use
the original canonical cwd or recover the handle with `agy_list_jobs`.

### Result pages

Omitting `include_result` preserves the full-result `agy_status`, `agy_wait` and `agy_run_sync` response and its terminal collected marker. `include_result: false` omits `result`, sets `result_included: false`, and leaves the job uncollected. It bounds response text, not disk I/O: deriving terminal status still reads the stored result.

`agy_get_result` returns at most `max_bytes` UTF-8 bytes of result text per page (default 16 KiB, minimum 4 bytes, maximum 1 MiB); metadata is additional. Invalid UTF-8 is replaced with the replacement character. Concatenate `content` in order until `complete` is true. A running job returns `next_action: "wait"` without result text. Cursors are bound to the job ID, whole-result SHA-256, job state and partial flag. Malformed cursors, different jobs, changed snapshots and invalid byte offsets fail rather than mix answers. An expired job cannot be recovered by a cursor. Schema-constrained JSON results remain text; parse them after assembling every page. The final page marks a terminal result collected, while intermediate pages do not. A collection marker records handler completion and does not prove transport delivery.

`list_sessions` lists known conversations. By default, `exclude_transient` is true, omitting defunct or missing directories as well as ephemeral scratch and test directories (/tmp, /var/tmp, scratch, and shim directories). Pass `exclude_transient: false` to include all cached entries. Supports pagination with `limit` and `offset`, returning `total_count` of matching entries before slicing.

For new jobs, `expected_git_branch` can require an exact branch name and
`expected_git_head` can require a full 40- or 64-character hexadecimal commit ID.
Use the branch name as stored under `refs/heads/` even when a tag shares it.
Use `head_full` from `agy_resolve_workspace` (or `git rev-parse HEAD`); the
existing short `head` display remains unchanged and is not a valid full-ID
precondition. An unborn branch can resolve by name without either HEAD field.
`head_full` is omitted when the commit cannot be resolved. Hex case is
normalized. The manager checks these
expectations before creating a fresh job and refuses mismatches or unverifiable
Git state with `workspace_mismatch` and `job_started: false`. Both expectations
are included in idempotency identity and retained in job metadata. Same-key
replay returns the existing job even after that job changed HEAD or the former
working directory was removed. These checks do not lock the repository or
prevent a later change between preflight and agent execution.

Terminal timing uses the exit-code sentinel, the final result file, or the
publication time of a valid supervisor process receipt, in that order. If those
receipts are missing, status inspection or GC creates `terminal_observed.<pid>.<start_ticks>` exclusively in the
job directory. Binding the marker to the recorded supervisor prevents a stale
PID-zero startup observation from shortening a later interrupted run. The marker persists across reads and restarts and starts a full
retention window; output/progress timestamps do not prove completion. In that
case frozen `elapsed` is measured to the first observation of death, an estimate
rather than the exact termination time. Empty output created at spawn no longer
implies that an interrupted long-running job took zero seconds.

`execution_cwd`, `git_branch`, `git_head` (short commit SHA), and `git_dirty` in job status outputs capture the concrete directory and Git metadata at job invocation time, allowing callers to verify which branch and commit were active during execution.

`agy_list_projects` and `agy_resolve_workspace` allow clients to discover registered Antigravity projects and `/data` repositories, inspect linked worktrees, and resolve ambiguous project or branch names to concrete working directory paths.

`agy_adopt_conversation(conversation_id, project, title?, restart_daemon?)` attaches
an existing local conversation to a registered cloud project. Use a project ID
from `agy_list_projects(sync_filter: "cloud_only")`. Omit the project to reuse
the conversation's existing cloud project. The conversation must be idle. It
keeps its ID, history, and recorded workspace URIs. Omit the title to keep it
or derive one from the preview.

Pass `restart_daemon: true` to refresh the systemd-managed Remote Control
daemon when idle and wait briefly for it to pick up the change. The refresh
removes only the conversation's stale entries from the CLI and daemon
summary snapshots while the daemon is stopped. AGY rebuilds these entries on
startup. With the default
`false`, repeat the adoption with `restart_daemon: true` when a refresh is
needed. Repeating an already confirmed adoption skips the refresh unless
`force_restart` is set. `agy_get_conversation` and `agy_list_conversations` report
`is_cloud_synced: true` when the daemon's persisted summary matches the
conversation ID, project and title and covers the latest revision observed in
SQLite or the CLI snapshot. MEASURED against agy 1.5.3, the daemon populates
this snapshot during startup reconciliation. Repeat adoption with
`restart_daemon: true` to refresh the summary after a later CLI turn.

`cloud_sync_state` is `confirmed` when that evidence matches, `pending`
when a readable snapshot lacks the binding or latest observed revision, and `unavailable` when the
snapshot cannot be read. Conversation lookups also return `local_only` for
conversations outside cloud projects or without a title. Adoption
`success` reports the local change. `post_restart_verified` records that
the call restarted the daemon and observed the updated summary.

See [AGY conversation adoption and Remote Control summaries](docs/agy-conversation-adoption.md)
for the measured storage layout, startup reconciliation, and history checks.

Project discovery retains one entry per registered resource root. A multi-root
project therefore repeats its project ID; resolving that ID requires an absolute
directory selection instead of silently choosing the last resource. Distinct
registrations that share a root retain their own IDs. Local `file:` URIs decode
percent escapes once (including spaces and Unicode); `localhost` is accepted,
while remote file authorities, relative paths and nonlocal URI schemes are
omitted. Both ordinary repositories and shared-bare layouts enumerate linked
worktrees. NUL-delimited Git output preserves spaces, newlines and trailing
whitespace in worktree paths. Listings and ambiguity candidates are sorted.

Workspace resolution returns a usable `path` only when `resolved: true` and
`ambiguous: false`. Multiple project/worktree matches return `resolved: false`,
`ambiguous: true`, candidate descriptions and advice, with no `path`, `branch`
or `head`. The resolver never implicitly picks main or the default branch.
Specify a full branch name or an absolute candidate directory and resolve again;
candidate display strings themselves are not paths to pass as cwd. Missing
branches/directories return both flags false and an error, with no fallback path.
If a selected worktree changes branch or HEAD during discovery, resolve it again.
The `ambiguous` boolean is always present.

Exact case-sensitive Git branch names take precedence over convenience aliases.
When no exact branch matches, case-insensitive branch names and worktree folder
basenames are accepted only if they identify one directory. A duplicated folder
basename such as review requires a full branch name or absolute path. An absolute
project directory pins that path: an accompanying branch must match its branch
or folder alias; it never causes an implicit checkout or branch switch. Existing
callers that relied on the former default-branch fallback must now select the
branch explicitly. Duplicate project IDs likewise require a more specific path.

`models` holds ids alone, so its entries can be passed straight to `model`; `model_details` pairs each id with the display label `agy` prints for it, in the same order, for showing a readable name. Through v2.1.0 `models` carried each `agy models` row whole, which was a tab-joined `id<TAB>label` string that `agy` does not accept as a model at all, so a client had to split it and pick a column ([#135](https://github.com/tphakala/agy-mcp/issues/135)).

`usage` is agy's own token accounting (`input_tokens`, `output_tokens`, `thinking_tokens`, `cache_read_tokens`, `total_tokens`), and together with `num_turns` it appears once a run reports a terminal result. On resumed multi-turn runs (`num_turns > 1` or continuation), `usage` is conversation-cumulative, while `job_usage` reports the tokens billed specifically for that single job run, and `usage_scope` indicates whether `usage` is `"job"` or `"conversation_cumulative"`.

`model` (kept for backward compatibility) and `requested_model` echo the normalized model id agy-mcp resolved for the run and passed to `agy` via `--model` (the one you passed, or `AGY_MCP_DEFAULT_MODEL`, reduced to its id); both are absent when no model was pinned and `agy` ran on its own default. When `agy` emits `init.model` in its pre-execution `stream-json` `init` event, `selected_model` carries that pre-execution CLI selection with `selected_model_source: "cli_init"`. Because `agy`'s `stream-json` output does not include post-generation execution model metadata, `effective_model` is `null` with `effective_model_source: "unknown"` rather than fabricated from request or `init` flags.

A run that ends badly still hands back whatever it managed to say, so `result` is set on `failed` and `cancelled` jobs too and is worth reading rather than discarding. `partial` describes whether it has authoritative final-result provenance, and follows from where the text came from (and from `agy`'s stderr notices, described below) rather than from the state. When such a run produced no text at all but still named a conversation, `recovery` usually carries a short note on continuing that conversation instead of restating the task. A `background_aborted` run gets no such note, because simply carrying on with the same task would launch the same background command again; its `error` says to re-run with the verification in the foreground instead. A `quota_exhausted` failure is an exception the other way: its `recovery` note advises waiting for the reset and retrying even when no conversation was named (see `failure_reason` below). A run that agy could not start or continue because it is not signed in, or because its sign-in expired, is another: it also gets a `recovery` note without a conversation, saying a person must sign agy in on the host running agy-mcp, and its `failure_reason` stays `agy_error`. When `retryable` is `false` (see below), the note instead says to fix the cause rather than continue the conversation (a sign-in note keeps its sign-in advice), and a `quota_exhausted` note keeps its wait-for-reset advice without `conversation_id` but adds that the cause may need fixing first when `error` names no reset time.

`failure_reason` classifies a `failed` job into one stable, machine-readable category so a caller can branch on the cause without scraping `error`. It is present only when `state` is `failed` (a cancelled job is already named by its state, and a running or done one has no failure), and the set is closed, so treat any value you do not recognize as `unknown`. The values are `quota_exhausted` (agy hit a provider quota, rate-limit or AI credits wall), `timeout` (the run outlived its timeout: agy-mcp killed it, or agy's own `--print-timeout` expired mid-turn and agy returned only what it had, possibly nothing), `spawn_failed` (the agy binary could not be started, or a legacy 127 marker has no matching process receipt), `agy_error` (agy itself reported an error, exited non-zero, or returned an indeterminate result), `interrupted` (the job process vanished without writing a result), `background_aborted` (agy ended the run itself, by a clean exit or in a job recovered from its result after its supervisor died, but its stderr shows it went idle with outstanding background shell tasks and killed them at exit, so the response is only progress narration rather than completed work; re-run with any verification in the foreground), and `unknown` (a failure fitting none of the above, for example its output could not be read). `quota_exhausted` is the one that is transient rather than fatal (unless agy reports it with `retryable` false, see below): `error` keeps agy's message, including the reset window when agy gives one (agy-mcp does not parse it out; `agy_usage` reports quota reset times), and `recovery` says to wait for the reset and then retry, reusing `conversation_id` when one was named, unless agy reported the error with `retryable` false. So a caller that hits a quota wall can back off until it clears instead of treating the run as a hard failure.

`retryable` and `error_id` come from the structured `AGY_ERROR:` line agy writes to stderr when it fails a request (MEASURED against agy 1.2.9 and 1.3.1 for a model not available in the account's region: exit 3, `retryable: false`; [#183](https://github.com/tphakala/agy-mcp/issues/183)). They are present only on `failed` jobs whose stderr carried a well-formed line; absent means agy gave no verdict, not that the failure is permanent. When that line's canonical status is `RESOURCE_EXHAUSTED`, the job is reported as `quota_exhausted` even if the message wording is not recognized; this follows agy's status naming and has not been checked against a captured quota error. agy's raw `short_error` text carries the cloud project and region, so it is not copied into these fields, and when `error` falls back to the stderr tail (a non-zero exit whose payload gave no message, or exit 127) each line in that tail that starts with `AGY_ERROR:` is reduced to its canonical status, for example `AGY_ERROR: NOT_FOUND (code 404)`, or left out when it cannot be read; `failure_reason` is still classified from the unreduced text.

It is true when the text was reconstructed from the streamed events, which happens when agy never reported a terminal result (a clean exit whose final event never arrived, a cancel, a timeout, a supervisor that died mid-stream) and also when agy did report one but the authoritative result channel was empty, so fallback text is all that remains. It is likewise true when agy reported a terminal result that was not a success, in which case the text is only what the run had produced by the time it stopped, and when agy marked the result a success but its stderr shows it ended the run early (its own `--print-timeout` expired mid-turn, or it killed outstanding background tasks). That applies even after a hard kill: a response agy wrote when its own `--print-timeout` expired is flagged partial. For a non-schema run, a SUCCESS `response` is authoritative; for a `json_schema` run, only SUCCESS `structured_output` is authoritative, and a retained `response` is diagnostic/partial when that structured result is missing. An authoritative result stays non-partial even if the process is then cancelled or killed; the exceptions are a timed-out run whose stderr shows `agy`'s own `--print-timeout` had already expired, and a job recovered from its result after its supervisor died whose stderr shows `agy` ended the run early.

Parameter and result fields carry their own descriptions in the tool schemas, so a client sees
them without consulting this file. The constraints worth knowing up front: `conversation_id`
and `continue_latest` are mutually exclusive (setting `continue_latest` true alongside a
`conversation_id` is an error); `timeout` is a Go duration and a value above 24h is rejected
outright, and on expiry the job normally ends in state `failed` with `failure_reason: timeout`,
because agy-mcp kills the `agy` process tree (if `agy`'s own
`--print-timeout` fires first, `agy` stops the turn itself and any text it had comes back as a
`partial` result); `effort`, when set, must be
`low`, `medium`, `high`, `xhigh` or `max`, and `mode`, when set, must be `accept-edits` or `auto`, with any other
value rejected before the run starts. `plan` is refused because agy ignores `--mode plan` while
slash command expansion is disabled, which agy-mcp does on every run so prompts stay literal, so the
run would apply edits instead of only planning (MEASURED against agy 1.1.15, 1.1.27, 1.4.6 and 1.4.8,
[#219](https://github.com/tphakala/agy-mcp/issues/219)); to keep a run from editing, say so in the
prompt. `accept-edits` is forwarded as given; under `--dangerously-skip-permissions`, which every run
passes, it made no observable difference compared with a run without
`--mode` (MEASURED against agy 1.4.8: a file write and a shell command were applied either way). `wait` defaults
to 90s and is silently clamped to that same ceiling. That 90s default is kept below the roughly
120s per-call timeout common MCP clients (Claude Code included) impose, so agy-mcp's still-running result reaches
the caller inline instead of being lost to a client-synthesized timeout (set `AGY_MCP_SYNC_WAIT_CAP`
to a Go duration to raise or lower the ceiling for a client with a different per-call limit), and it
bounds only the inline wait, never the job itself.
If other calls on a shared connection arrive after a long wait, use a shorter
`wait` or a single `agy_status` snapshot with the same job ID.
`agy_cancel` is asynchronous, so it usually returns `running` and the job settles to `cancelled`
a moment later. A `prompt` is sent to agy literally: a leading slash-command or skill name
(a prompt beginning with `/`) is treated as text rather than expanded, so caller input means
exactly what it says regardless of the installed agy version.

A fresh `agy_run` waits after admission for agy's `init` event, within the
`AGY_MCP_CONVERSATION_ID_WAIT` budget (default 5s). If the wait expires first,
the response still includes the `job_id`. Use that handle with `agy_status` to
learn the `conversation_id` once available. `agy_run_sync` gets the conversation
ID from the status it already polls, within its own inline wait.

**Fresh runs are not serialized.** Any number of them can run in one directory at the same
time, bounded only by the global concurrency cap. Earlier versions refused a second fresh run
in a directory that already had one, because the conversation id had to be inferred by diffing
agy's shared conversation cache and that inference was only sound while nothing else could
touch the same entry. Reading the id from agy's own stream removes the ambiguity and the
restriction with it.

What is still serialized is a **conversation**: two runs continuing the same `conversation_id`
cannot overlap, because concurrent agy sessions on one conversation trigger a known session-lock
hang in agy itself. The second is refused with a conflict error rather than queued. That gate is
rebuilt at startup from jobs whose supervisor outlived a server restart, so it holds across
restarts.

Conversation serialization also holds across separate `agy-mcp` processes, which matters in
stdio mode, where each MCP client session spawns its own process sharing one
`AGY_MCP_STATE_DIR`. A per-conversation advisory lock (`flock` on a file under
`<state-dir>/locks/`) stops two sibling sessions from continuing the same conversation at once.
The global concurrency cap, by contrast, is enforced per process: with N client sessions each
capped at the configured limit, up to N times that many runs can be in flight. Lock files are
left in place by design (unlinking a `flock` file races), so `locks/` keeps one empty file per
conversation ever locked.

Two caveats, both now scoped to continuations only. `AGY_MCP_STATE_DIR` must live on a local
filesystem that supports `flock` (not NFS, where `flock` may be a no-op or fail); a continuation
is refused rather than started if the lock cannot be taken, so cross-process exclusion can never
silently lapse. And across a server restart there is a brief window, while the manager process is
down, before it re-takes the locks for jobs whose supervisor outlived it; a sibling process that
continues the same conversation during that window is not blocked. The in-process gate is always
restored at startup, so this gap is limited to the restart window itself. Fresh runs take no lock
at all and are unaffected by either caveat.

### Quota

`agy_usage` runs `agy --output-format json -p /usage` and returns the quota per group (for example `Gemini Models` for Gemini Flash and Pro, `Gemini 4 Argon` when Argon quota is reported as a separate group, and `Claude and GPT models` for Claude Opus, Claude Sonnet and GPT-OSS, MEASURED against agy 1.2.16 and agy 1.4.10; disabled buckets are skipped, and known Gemini, Argon, Claude and GPT models are each matched to their group). That invocation starts no agent turn and spends no model quota (agy changelog 1.1.11; MEASURED against agy 1.2.16, which also measured that adding `--disable-slash-commands` turns `/usage` into a model prompt, so the probe never carries it, and a reply that shows a model turn switches probing off for the rest of the server process). The probe runs in its own session because agy 1.2.x otherwise stops on SIGTTOU when it shares a controlling terminal.

The CLI supplies `groups` and any plan wording in `description`. A reading can report separate Gemini Flash and Argon groups. Supply `models: ["gemini-3.8-flash-medium", "gemini-4-argon"]` to get `model_groups` entries `{ model, status, group? }` against that same snapshot, with no additional model-list call or agent turn. This uses the same model-family and CLI group-metadata inference as optional-run admission, not provider-guaranteed membership. `exact` means one group matched; `unknown` means an unrecognized model or missing group; `ambiguous` means multiple candidates; `unpinned` means an empty model. Only `exact` includes `group`. Mapping is not evidence that a model is available or executable. At most 128 models may be requested; omitted models return an empty array.

Use `list_models` to discover CLI model IDs and pass them to `agy_usage(models=...)` for mapping against one snapshot. Listing and quota metadata provide no execution check.

Each group has a `level` from its tightest window (usually the 5h or the weekly limit): `exhausted` at 0, `critical` below 5 percent remaining, `low` below 25 percent, otherwise `ok`. The server assumes a full window after the CLI-reported `reset_time`. The bucket retains its observed `remaining_fraction`, and `refilled` identifies the assumption. `remaining_percent` rounds down the effective value. `checked_at` records when the server recorded the successful reading. Thresholds are set with `AGY_MCP_USAGE_LOW` and `AGY_MCP_USAGE_CRITICAL`.

Readings are cached in memory per server process (60 seconds by default; `max_age` sets how old a reading may be, from 10 seconds up, and a younger reading is reused), concurrent callers share one probe, and a failed probe is remembered for 30 seconds. With `AGY_MCP_USAGE_INTERVAL` above zero (5 minutes by default) the server also refreshes the reading in the background and skips refresh ticks while it tracks running jobs. An explicit `agy_usage` can request a fresher reading. `agy_run`, `agy_run_sync`, `agy_status` and `agy_wait` results carry a compact `quota` field read from that cache; it never waits on agy and is absent when there is no reading younger than twice the interval (at least 10 minutes). Several servers (one per client session) each probe on their own.

For capacity planning that tolerates a reading up to ten minutes old, use
`max_age: "10m"` and check `checked_at`. A cache hit avoids starting the CLI,
while end-to-end latency also depends on the MCP connection.

`priority: optional` on `agy_run` or `agy_run_sync` marks work that may be skipped, such as a peer review or a second opinion. When a trusted reading is present and the run's model maps to a group at `low`, `critical` or `exhausted`, the run is refused before any job starts, as a tool error naming the group, level, remaining percent and reset time. When a trusted reading is present but the model's quota group mapping cannot be established (an omitted model with no `AGY_MCP_DEFAULT_MODEL`, an unknown model, or an ambiguous/missing group in `/usage`), an `optional` run is also refused/deferred clearly without claiming quota is exhausted. The refusal creates no job, so `failure_reason` is unchanged; report the skip instead of retrying. When there is no trusted reading yet (no reading, a reading that is too old, or a failed probe with no trusted earlier reading), the guard fails open (an earlier reading inside that window still applies), and `priority: normal` (the default) always runs. `priority` is not part of the `idempotency_key` match: a retry whose key already names a job returns that job whatever its priority and the current quota.

## Completion wake for Claude Code

Claude Code does not surface MCP server notifications to the model: progress notifications are UI-only, and there is no server-initiated push that can wake the model when an async job finishes. Polling `agy_status` after `agy_run` is the protocol-level baseline. agy-mcp ships a hook bridge that turns job completion into a real wake instead, built on Claude Code's `asyncRewake` hook mechanism: a background PostToolUse hook that exits with code 2 wakes the model with the hook's stderr as a system reminder.

Add to your Claude Code `settings.json`:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "mcp__agy__agy_run(_sync)?",
        "hooks": [
          { "type": "command", "command": "agy-mcp hook-wait", "asyncRewake": true, "timeout": 3700 }
        ]
      }
    ]
  }
}
```

How it behaves:

- After every `agy_run`, the hook waits (in the background, off the session's critical path) for the job's completion sentinel and then wakes Claude with a one-line message naming the `job_id` and final state, prompting the model to call `agy_status` when the result is actually ready.
- `agy_run_sync` calls that returned their result inline produce no wake; a sync call that overran its wait cap (and so returned a still-running job) gets the same completion wake as `agy_run`.
- A finished job produces no wake when a tool has already returned its outcome to the session: `agy_wait` and `agy_status` record a `collected` marker in the job directory when they return a terminal state, and hook-wait exits 0 silently if the marker is present. Summary-only status, wait and sync responses and intermediate result pages do not write it; the final `agy_get_result` page does. The marker is written as the tool returns the outcome, so hook-wait allows up to one second for it to appear before waking. The marker records that the tool returned the outcome, not that the client received it, so a call abandoned in transit can leave a finished job without a wake. `agy_cancel` records a separate `dismissed` marker when it asks a job to stop, and hook-wait honours it only if the job then ends `cancelled`, so a job that finishes on its own before the stop lands still wakes. One narrow case follows from this: a job that had already produced its complete answer and then exits non-zero under the cancel signal still reads as `cancelled` (with that answer kept, see `partial`), so it gets no wake either; read a job you cancelled with `agy_status` before re-running it. Every other finished job still wakes, including a job nobody collected, a job cancelled without an `agy_cancel` request, and one whose wait overran while it was still running. The marker is per job, not per session, so a job collected by one session (or subagent) is not woken for in another.
- When the PostToolUse payload names the calling subagent (`agent_type`), the wake message says the job was started by that subagent, so a parent session can tell it is not its own. Per Claude Code's hooks documentation the field is present only inside a subagent call. MEASURED against Claude Code 2.1.289: absent from a main-thread payload, `general-purpose` in a foreground or background subagent's payload, for both a Bash and an `mcp__agy__` tool call.
- On timeout (default 1h, `-timeout` to change) the hook still wakes Claude, reporting the job as still running, so a long job is never silently lost, provided the outer hook `timeout` exceeds hook-wait's own `-timeout` (see the `settings.json` note below). An externally interrupted wait (a SIGINT or SIGTERM delivered to hook-wait) also wakes, with a distinct "wait interrupted" message pointing at `agy_wait` and `agy_status`, rather than exiting silently and dropping the owed wake. On any internal error the hook exits 0 silently; it can never disrupt the tool call it observes. The hook-level `timeout` in `settings.json` (3700 above) must exceed hook-wait's own `-timeout` (1h by default) by more than the one-second marker grace plus a poll interval (a quarter of a second), because a job that finishes on the last poll spends both after the timeout; the 100 seconds of margin above is ample. With that margin hook-wait's internal timeout always fires first and reports the wake itself; if the outer `timeout` is equal or lower, Claude Code kills the hook process first and the exit-2 wake is lost, silently defeating that guarantee.
- The `agy` in the matcher pattern `mcp__agy__agy_run(_sync)?` is the server name you passed to `claude mcp add agy`; if you registered the server under a different name, change both the `claude mcp add` name and that `agy` segment of the matcher.
- Claude Code runs hooks with the user's shell environment, not the MCP server entry's own env block, so if the server is registered with a custom `AGY_MCP_STATE_DIR` the same value must also be exported in the shell (or passed inline in the hook command) or hook-wait will silently look in the wrong state directory and never wake.

Two related subcommands, useful beyond Claude Code:

- `agy-mcp wait-job [-timeout 1h] <job_id>` blocks until the job is terminal and prints the final state word (`done`, `failed`, or `cancelled`) to stdout. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout, 130 interrupted. It needs only the job state directory, not the agy binary, so it works in minimal environments.
- `agy-mcp hook-wait [-timeout 1h]` is the hook entrypoint described above: it reads the PostToolUse payload from stdin, so it is not useful to invoke by hand, but it is a single self-contained binary call, no shell wrapper or jq required, and it works on Linux, macOS, and Windows. The file-based wake contract is exercised by tests on all three platforms; the signal-interrupt wake (SIGINT/SIGTERM) is a POSIX behavior and is tested there.

## Preflight (`agy-mcp doctor`)

`agy-mcp doctor` runs the read-only checks a run depends on and prints a one-line verdict for each, so a broken setup names its own cause instead of surfacing as a downstream tool error (for example `list_models` failing when `agy` is missing from PATH or not authenticated). It checks that the `agy` binary resolves, meets the version floor, and is reachable and authenticated (a model listing); that the state directory is writable; names where each effective setting came from (an environment variable or the default) without printing any secret value; warns when the environment selects ADC sign-in in a way that does not work as intended (`USE_ADC` without `AGY_ADC_AUTH`, or `AGY_ADC_AUTH` on an agy below 1.4.7; see the auth note above); and reports any stale jobs a prior crash left behind. It never starts a job or mutates job state.

The exit code is meant for a setup script or CI: `0` when nothing is broken (a warning does not count, and a fresh install with no jobs passes), `1` when a check failed, `2` on a usage error. A `WARN` line (leftover jobs the periodic GC will reap, or an ADC environment caveat) is information, not a failure.

On POSIX, both wait subcommands install their SIGINT/SIGTERM handler only after parsing flags, resolving the job state directory and building the wait manager (and hook-wait also reads its payload from stdin first), so a signal sent immediately after launch can land before the handler exists and kill the process outright, losing the interrupt exit code. A parent that intends to interrupt a wait can close that window by setting `AGY_MCP_WAIT_READY_FILE`: the subcommand creates that file the moment the handler is in place, so waiting for it to appear makes the signal deliverable. Leave it unset and nothing is written. Three caveats, because existence is the entire signal: the path must be absolute (a relative one resolves against each process's own working directory, and hook-wait runs from the session cwd), it must be fresh and unique per invocation (a file left from an earlier run satisfies the wait immediately and hands back the race), and the parent still needs its own timeout, since both subcommands have exit paths that return before any handler is armed. An existing file at that path is refused rather than overwritten, so pointing the variable at something that matters destroys nothing; that protects the file, not the handshake, which is why the path has to be fresh.

MCP clients other than Claude Code get the same no-polling benefit in-protocol: call `agy_wait` with a `job_id` from `agy_run` (or from an `agy_run_sync` that outlived its inline wait) and the tool blocks (bounded by `wait`, default and cap 90s, raise or lower with `AGY_MCP_SYNC_WAIT_CAP`) until the job finishes. Re-issue `agy_wait` with the same `job_id` if a single bounded call is not enough; each call blocks another window rather than re-running the job.

## HTTP mode

```bash
agy-mcp -http 127.0.0.1:8765
```

HTTP mode is opt-in and only accepts a loopback bind address (`localhost`, `127.0.0.1`, or `::1`). A non-loopback address (including `:8765`, which binds all interfaces) is refused at startup, so it cannot be accidentally exposed.

Two extra hardening layers are always available:

- **Origin/CSRF protection** is always on. A state-changing cross-origin browser request (one carrying a cross-site `Sec-Fetch-Site` or a mismatched `Origin`) is rejected with `403`. Normal non-browser MCP clients (Claude Code, Cursor, the go-sdk client) send no `Origin` header and are unaffected.
- **Optional bearer token.** Set `-http-token <token>` (or `AGY_MCP_HTTP_TOKEN`) to require `Authorization: Bearer <token>` on every request; a missing or wrong token gets `401`. The flag overrides the env var. Off by default, so the bare loopback mode stays unauthenticated.

```bash
agy-mcp -http 127.0.0.1:8765 -http-token "$(openssl rand -hex 32)"
```

## Upgrading from v2.1

**Behaviour change to check your client against:** `list_models` used to return each `agy models` row whole in `models`, as a tab-joined `id<TAB>label` string. It now returns the id alone, and pairs each id with its display label in the new `model_details` field. No type on the wire changed, only the values, so a client that renders `models` to a human now shows a slug and should read `model_details[].label` instead, and one that split each entry on the tab will find nothing to split. A stored old value passed back as `model` still works: the server reduces a whole row to its id before running it.

## Upgrading from v1

v2 requires agy 1.1.15 and drives it through `--output-format stream-json`. The upgrade is safe to do in place, but two things are worth knowing.

**Stop the server before replacing the binary.** The supervisor is the same `agy-mcp` binary re-executed as a private `run-job` subprocess, so replacing it under a live v1 server pairs an old manager with a new supervisor. The supervisor detects that case and asks agy for the event stream anyway, so no result is lost, but a v1 manager and a v2 process sharing one `AGY_MCP_STATE_DIR` briefly disagree about cross-process locking, and a v1 run's conversation id can be misattributed in that window.

**Jobs already in the state directory keep working.** A job dir written by v1 loads cleanly (the two removed `meta.json` fields are simply ignored) and its result is still reported in full. Two degradations apply until those jobs age out of the 24h TTL:

- A v1 fresh run may report no `conversation_id`. v1 recovered it by diffing agy's conversation cache, and v2 has no such path. Recover the thread with `list_sessions` if you need it.
- Cancelled and interrupted v1 jobs behave as before; only the id is affected.

**Behaviour changes to check your client against.** `agy_run` waits within the configured conversation ID budget after admission. Fresh runs in the same directory can run concurrently and edit one working tree under `--dangerously-skip-permissions`. `list_models` requires agy 1.1.15 or later.

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `AGY_MCP_AGY_PATH` | `agy` on PATH | path to the agy binary |
| `AGY_MCP_STATE_DIR` | `$XDG_STATE_HOME/agy-mcp` or `~/.local/state/agy-mcp` | job state directory |
| `AGY_MCP_DEFAULT_MODEL` | agy default | default model, as an id (`gemini-3.1-pro-high`), not a display label |
| `AGY_MCP_HTTP_TOKEN` | (none) | optional bearer token for HTTP mode; empty = unauthenticated |
| `AGY_MCP_SYNC_WAIT_CAP` | `90s` | inline-wait ceiling for `agy_run_sync` / `agy_wait` (the default when no `wait` is given, and the clamp for a larger one); the 90s default stays below common MCP clients' ~120s per-call timeout, and this variable raises or lowers it; a Go duration that does not parse as positive is ignored |
| `AGY_MCP_CONVERSATION_ID_WAIT` | `5s` | how long `agy_run` blocks waiting for `agy` to emit its stream `init` event and name the conversation id; a Go duration that does not parse as positive is ignored |
| `AGY_MCP_PROGRESS_HEARTBEAT` | `10s` | heartbeat interval for MCP progress notifications during `agy_wait` / `agy_run_sync` while step type is unchanged; notifications still fire immediately on step changes |
| `AGY_MCP_USAGE_INTERVAL` | `5m` | how often the server refreshes its quota reading in the background (a Go duration of at least 1m); `0` disables background probing, leaving only explicit `agy_usage` calls to probe; a bad value fails startup |
| `AGY_MCP_USAGE_LOW` | `25` | remaining quota percent (`25` or `25%`) below which a group reports `low`; a bad value, or `AGY_MCP_USAGE_CRITICAL` above it, fails startup |
| `AGY_MCP_USAGE_CRITICAL` | `5` | remaining quota percent below which a group reports `critical` |
| `AGY_MCP_WAIT_READY_FILE` | (none) | absolute path `wait-job` / `hook-wait` create once their SIGINT/SIGTERM handler is installed, so a parent can signal without racing it; must be fresh per invocation, an existing file is refused rather than overwritten; empty = nothing is written |

## Development

Build, test and lint commands, the code layout and the conventions a change follows are in [AGENTS.md](AGENTS.md).

## License

MIT. See [LICENSE](LICENSE).

## Service icon

The original artwork is `web/favicon.svg`; PNG and maskable exports are in
`web/icons/`, with a real multi-size `web/favicon.ico` for legacy browsers.
`web/server-icons.json` contains the public MCP implementation icon metadata,
with PNG first and SVG as a scalable alternative. Keep its version queries in
sync with changes to the artwork. The icon images are public static assets;
MCP and application access policies are configured separately.

`deploy/icons.caddy` provides the `agy_mcp_icons` snippet. Import its
file and then invoke the snippet with the absolute `web/` directory inside the
existing site block. Preserve the existing MCP and application access rules.

### Bounded artifact line reads

`agy_read_artifact` hashes and counts the full stable file in fixed-size chunks,
then retains only the requested byte-bounded selection. A long earlier line does
not prevent reading a short final diagnostic, and large tail counts do not
allocate a line array. CRLF becomes LF and an unterminated final line gets an LF.
`start_line` beyond EOF returns empty content, retains the requested start index,
and reports `end_line: total_lines` (zero for an empty file). `truncated` still
indicates the byte limit shortened the selected content. File changes detected
during these passes fail the read; use `agy_logs` for growing files.

### Include the enclosing command's exit status

A child test report can pass even when a later Make recipe fails. Save the
enclosing command's actual status alongside that invocation's report:

```sh
make_status=0
make test > evidence/make.log 2>&1 || make_status=$?
printf '%s\n' "$make_status" > evidence/make.exit
```

Register `evidence` in the job's `artifact_dirs`, then explicitly select both
files when inspecting the result:

```json
{"job_id":"<job-id>","path":"poststep-go.json","format":"go_test_json","exit_code_path":"make.exit","max_failures":0}
```

For the measured Make workflow, the child Go report passed 37 tests and Make
returned `2` after a later recipe failed. Selecting that `make.exit` changes
`verification_outcome` to `failed`, preserves `report_outcome: "passed"`, and
returns `command_exit` with `source: "exit_code_artifact"`, the selected path,
the exact status `2`, and the SHA-256 of its original bytes. Testcase counts
still describe the child report.

The exit artifact must contain one signed 32-bit decimal integer with optional
surrounding whitespace, at most 64 bytes in total. Any nonzero code prevents a
report pass. An incomplete or malformed report remains `unknown` even when the
selected code is zero; missing, malformed, oversized or inaccessible selected
exit evidence is a tool error. Omitting `exit_code_path` leaves
report-only behavior and omits `report_outcome` and `command_exit`.

To catch an accidental mix of invocations, prefix the JSON receipt path with
`receipt:` in `exit_code_path`. After the command finishes, write a JSON receipt alongside
its report using the saved exit status. Use a fresh report path per invocation.
For the example above, with `evidence` as the artifact root:

```sh
python3 - evidence/poststep-go.json poststep-go.json "$make_status" > evidence/make.receipt.json <<'PY'
import hashlib, json, pathlib, sys

file, path, status = sys.argv[1:]
print(json.dumps({"report_path": path,
                  "report_sha256": hashlib.sha256(pathlib.Path(file).read_bytes()).hexdigest(),
                  "exit_code": int(status)}))
PY
```

```json
{"job_id":"<job-id>","path":"poststep-go.json","format":"go_test_json","exit_code_path":"receipt:make.receipt.json","max_failures":0}
```

The path after `receipt:` must be nonempty. A literal filename starting with
`receipt:` can be passed as an ordinary path beginning with `./`. The receipt
is a JSON object up to 8 KiB with exactly the three fields shown.
`report_path` must use the same spelling as `path`, and `report_sha256` must
match the exact report bytes. A mismatch, missing field or duplicate key
returns a tool error before any verdict. `command_exit` then identifies the
receipt with source `command_receipt_artifact` and the SHA-256 of its bytes.
This checks the producer's binding of report to exit status. The producer
remains responsible for recording the actual invocation and exit status.

These are caller-selected artifacts, not independently witnessed command
execution. Associate the report and status with the same invocation, use fresh
paths, and retain the complete command log. A job's lifecycle state and agent
narrative never supply the exit code. Artifact hashes identify exact bytes;
they do not establish which command created the files.
