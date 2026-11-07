# Self-host agy-mcp with ChatGPT

This guide builds the fork on a Linux host, verifies the local MCP server, and connects it to ChatGPT through OpenAI Secure MCP Tunnel. The host runs `tunnel-client`, which launches `agy-mcp` over stdio. A separate supervisor runs Antigravity CLI (`agy`) for each job and saves its state on the host.

```text
ChatGPT <-> Secure MCP Tunnel <-> tunnel-client <-> agy-mcp
                                                       |
                                              job supervisor -> agy
```

## Prerequisites

- A Linux account with a systemd user session, Bash, Git, Go as specified in [go.mod](../go.mod), and `curl` and `jq` for the health checks. The local MCP check uses Python 3. Linux job cleanup requires a readable `/proc` and kernel support for subreapers and pidfds. See [Linux job cleanup](../README.md#linux-job-cleanup-and-systemd-tunnel-restarts).
- Antigravity CLI 1.1.15 or later, signed in under the account that will run the service. Install the CLI from [Antigravity](https://antigravity.google/) and launch `agy` interactively to sign in. For a headless host using Google Application Default Credentials, see the authentication note in [Requirements](../README.md#requirements).
- For ChatGPT, install `tunnel-client` on the same host. Get a binary for the host architecture from [OpenAI's tunnel-client releases](https://github.com/openai/tunnel-client/releases/latest) and put it on `PATH`. The commands below were checked with tunnel-client 0.0.18. The stdio tunnel uses outbound HTTPS to OpenAI and needs no public MCP listener. See the [Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels).

Use an account and workspace you trust with the host user's files. agy-mcp runs AGY with permission prompts disabled. An explicit job working directory selects the workspace, while the process retains the host user's filesystem permissions. See [execution boundaries](../README.md#agent-delegation-and-recovery).

## Build and check the local server

Run the shell blocks from the same Bash session so the path variables remain available. The clone command selects the branch used for this guide. Record the commit if you need to reproduce a deployment later.

```bash
git clone --branch codex/local-enhancements https://github.com/TakashiSasaki/agy-mcp.git
cd agy-mcp
git rev-parse HEAD

agy_bin="$(realpath "$(type -P agy)")"
agy_mcp_bin="$HOME/.local/bin/agy-mcp"
agy_state_dir="$HOME/.local/state/agy-mcp"
install -d -m 700 "$agy_state_dir"
mkdir -p "$HOME/.local/bin"
"$agy_bin" --version
go version
```

On a fresh install, build and stage the executable next to its final path so the move is atomic. If you already have a running installation at this path, use [Update and roll back](#update-and-roll-back).

```bash
(
  set -e
  test ! -e "$agy_mcp_bin"
  build_dir="$(mktemp -d "$HOME/.local/bin/.agy-mcp-build.XXXXXX")"
  trap 'rm -f -- "$build_dir/agy-mcp"; rmdir -- "$build_dir"' EXIT
  go build -trimpath -o "$build_dir/agy-mcp" .
  "$build_dir/agy-mcp" --version
  mv -- "$build_dir/agy-mcp" "$agy_mcp_bin"
)

export AGY_MCP_AGY_PATH="$agy_bin"
export AGY_MCP_STATE_DIR="$agy_state_dir"
"$agy_mcp_bin" doctor
```

Expect a fork version with `+local.<commit>` and a passing doctor check. If automatic Go selection cannot find a release for the language version in `go.mod`, install a released patch version satisfying it and set `GOTOOLCHAIN` to that version. See [source build details](../README.md#build-from-source-forks-and-local-branches) and [doctor checks](../README.md#preflight-agy-mcp-doctor).

Verify discovery and a read-only call over a fresh stdio connection. This check lists models to verify AGY sign-in and starts no agent turn.

```bash
timeout 30s python3 - "$agy_mcp_bin" <<'PY'
import json
import subprocess
import sys

with subprocess.Popen([sys.argv[1]], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True) as server:
    def send(message):
        server.stdin.write(json.dumps({"jsonrpc": "2.0", **message}) + "\n")
        server.stdin.flush()

    def request(request_id, method, params):
        send({"id": request_id, "method": method, "params": params})
        for line in server.stdout:
            reply = json.loads(line)
            if reply.get("id") == request_id:
                if "error" in reply:
                    raise RuntimeError(reply["error"])
                return reply["result"]
        raise RuntimeError(f"MCP closed before replying to {method}")

    initialized = request(1, "initialize", {
        "protocolVersion": "2025-03-26", "capabilities": {},
        "clientInfo": {"name": "agy-mcp-setup-check", "version": "1"},
    })
    send({"method": "notifications/initialized"})
    tools = request(2, "tools/list", {})
    assert any(tool["name"] == "agy_info" for tool in tools["tools"])
    response = request(3, "tools/call", {"name": "agy_info", "arguments": {"check_auth": True}})
    assert not response.get("isError"), response
    info = response["structuredContent"]
    assert info["runtime"]["auth_status"] == "ok", info["runtime"]
    assert info["server_version"] == initialized["serverInfo"]["version"]
    print(info["server_version"], info["catalog_sha256"])
PY
```

## Set up a private tunnel

In [Platform tunnel settings](https://platform.openai.com/settings/organization/tunnels), create or select a tunnel and copy its ID. Associate it with the Platform organization and the ChatGPT workspace that will use it. Creating a tunnel requires Tunnels Read and Manage. The runtime credential and the person selecting the tunnel in ChatGPT need Tunnels Read and Use. ChatGPT workspace controls also have to allow custom MCP servers. Create a restricted runtime key in [Platform API keys](https://platform.openai.com/settings/organization/api-keys). Keep management credentials separate. [OpenAI permission and workspace guidance](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels#permissions-and-access)

Create a named profile and give it a private file reference for the runtime key. The commands use a new `agy` profile. If that name is already in use, inspect the existing configuration before reusing or choosing a different name. Keep the instance name, profile argument, health path and drop-in directory consistent when renaming.

```bash
tunnel_client_bin="$(realpath "$(type -P tunnel-client)")"
profile_dir="$HOME/.config/tunnel-client"
runtime_dir="$HOME/.local/state/tunnel-client"
health_url_file="$runtime_dir/agy-health.url"
key_file="$profile_dir/secrets/agy-runtime.key"
install -d -m 700 "$profile_dir/secrets" "$runtime_dir"

(
  set -e
  umask 077
  set -o noclobber
  read -rsp 'Tunnel runtime key: ' runtime_key
  printf '\n'
  test -n "$runtime_key"
  printf '%s\n' "$runtime_key" > "$key_file"
  unset runtime_key

  read -rp 'Tunnel ID: ' tunnel_id
  "$tunnel_client_bin" init --sample sample_mcp_stdio_local \
    --profile-dir "$profile_dir" --profile agy --tunnel-id "$tunnel_id" \
    --control-plane-api-key-ref "file:$key_file" \
    --mcp-command "\"$agy_mcp_bin\"" --health-listen-addr 127.0.0.1:0
  "$tunnel_client_bin" doctor --profile-dir "$profile_dir" --profile agy --explain
)
```

## Run the tunnel as a user service

For a new host, install the following instance unit and its [job lifecycle drop-in](../deploy/agy-tunnel-job-lifecycle.conf). If you already manage the `agy` profile with systemd, compare these settings with `systemctl --user cat tunnel-client@agy.service` and adapt that service. Run one tunnel client per tunnel ID.

```bash
(
  set -e
  set -o noclobber
  unit_dir="$HOME/.config/systemd/user"
  unit_path="$unit_dir/tunnel-client@agy.service"
  mkdir -p "$unit_dir"
  cat > "$unit_path" <<UNIT
[Unit]
Description=Secure MCP Tunnel for agy-mcp

[Service]
Type=simple
WorkingDirectory=%h
Environment="AGY_MCP_AGY_PATH=$agy_bin"
Environment="AGY_MCP_STATE_DIR=$agy_state_dir"
ExecStart="$tunnel_client_bin" run --profile-dir "$profile_dir" --profile agy --health.url-file "$health_url_file"
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
UNIT

  install -D -m 644 deploy/agy-tunnel-job-lifecycle.conf \
    "$unit_dir/tunnel-client@agy.service.d/20-job-lifecycle.conf"
  systemctl --user daemon-reload
  systemctl --user enable --now tunnel-client@agy.service
  systemctl --user show tunnel-client@agy.service -p MainPID -p KillMode
)
```

For ADC, add `AGY_ADC_AUTH=1` and the required credential environment to the service. If jobs use tools installed outside the service's `PATH`, include their directories in the unit's environment. Reload and restart the service after editing it.

The instance-specific `KillMode=process` lets active job supervisors outlive the tunnel. Use `agy_cancel` when the job itself needs to stop. For startup at boot without an interactive login, check the account's linger setting with `loginctl show-user "$USER" -p Linger` and use your host's user-service policy.

```bash
"$tunnel_client_bin" health --url-file "$health_url_file" --require-control-plane-poll
```

If the first poll is still pending, check again when it completes. Use `journalctl --user -u tunnel-client@agy.service -n 50` for startup errors. For a healthy client, confirm readiness and a successful control-plane poll before connecting ChatGPT.

## Connect and run a small job

In [ChatGPT Plugins](https://chatgpt.com/#settings/Connectors), add a custom MCP server, choose **Tunnel** under Connection, and select the tunnel or enter its ID. Use **No authentication** for this private stdio server when no app-level authentication layer has been added. OpenAI still enforces tunnel and workspace access, and AGY uses its separate sign-in on the host. Create and install the plugin, then select it in a new conversation and call `agy_info`. Compare `server_version` with the installed binary and check that the expected tools are available. [OpenAI's ChatGPT connection steps](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels#connect-from-chatgpt)

Create an empty test directory on the host, for example `mkdir -p "$HOME/agy-mcp-smoke"`. Use its absolute path as `cwd`. Call `agy_run` with the following inputs, replacing the path and choosing a fresh idempotency key for this run. This step spends AGY model quota.

```json
{
  "prompt": "Reply with AGY MCP smoke check. Do not edit files.",
  "cwd": "/home/your-user/agy-mcp-smoke",
  "timeout": "2m",
  "task_label": "setup-smoke",
  "idempotency_key": "setup-smoke-your-unique-key"
}
```

Save the exact arguments and returned `job_id`. Use `agy_wait` with `wait: "30s"` and `include_result: false`, then `agy_get_result` with `max_bytes: 4096`. Check for `state: "done"`, `partial: false` and the expected answer. Follow `next_cursor` until `complete: true` if the result spans pages. If a wait returns `running`, wait on the same ID. To continue a finished conversation, pass its `conversation_id`, the intended `cwd`, and a new key to `agy_run`. [Result paging and state semantics](../README.md#result-pages)

## Update and roll back

Before the update, save the running `server_version` and a finished job ID for comparison. Check out the intended commit, record `git rev-parse HEAD`, and run the [repository checks](../AGENTS.md#build-test-lint).

Pause new tool calls. Use the health command below to check the dispatcher, and inspect live jobs separately with `agy_info` or `agy_list_jobs`. Wait for active dispatches to finish. If work must continue through the restart, record the live supervisor's PID and start time for comparison afterward. A service restart with the drop-in can preserve live jobs. A host reboot ends their processes.

```bash
(
  set -e
  stage="$(mktemp -d "$HOME/.local/bin/.agy-mcp-update.XXXXXX")"
  go build -trimpath -o "$stage/agy-mcp" .
  "$stage/agy-mcp" --version
  cp -p -- "$agy_mcp_bin" "$stage/previous"
  mv -- "$stage/previous" "$agy_mcp_bin.previous"
  mv -- "$stage/agy-mcp" "$agy_mcp_bin"
  rmdir -- "$stage"
  systemctl --user restart tunnel-client@agy.service
)
```

Confirm the new service PID and a single owner for the profile with `systemctl --user show tunnel-client@agy.service -p MainPID` and `pgrep -a -x tunnel-client`. Wait for a successful control-plane poll, then call `agy_info` and retrieve the saved job with `agy_get_result` through ChatGPT. Keep the state directory and tunnel profile in place to preserve existing job IDs and keys.

To roll back, pause new calls and wait for dispatches to finish, move `"$agy_mcp_bin.previous"` into `"$agy_mcp_bin"` with `mv`, and restart the same service. Repeat the version and result checks. After changing tool schemas, open the existing ChatGPT app, select **Refresh**, and test a changed tool in a new conversation. [Metadata refresh procedure](https://developers.openai.com/plugins/deploy/connect-chatgpt#refresh-metadata)

## Troubleshoot and recover

```bash
health_url="$(cat "$health_url_file")"
curl -fsS "$health_url/health?details=true" | jq '.components'
journalctl --user -u tunnel-client@agy.service -n 50
```

| Symptom | Check |
| --- | --- |
| AGY is unavailable or sign-in fails | Run `agy-mcp doctor` as the service user with its `AGY_MCP_AGY_PATH` and state path. Check the CLI version and auth in that environment. |
| Tunnel is absent from ChatGPT | Check the workspace association, the caller's tunnel permissions and the workspace's custom MCP policy. |
| Local MCP responds but a cloud call times out | Check tunnel readiness, last successful poll, then tool dispatch and response delivery. Discovery and successful polling can occur before an actual tool call reaches the server. Record the error and time, then make a bounded read-only check with `agy_info`. |
| The response to a job start was lost | Search using `agy_list_jobs` with the saved `idempotency_key`. Inspect the returned job ID with `agy_status`. If creation is still uncertain, replay the exact request with the same key. The binding lasts only while the original job remains in the same state store. |
| The process finished but output is incomplete | Inspect `state`, `failure_reason` and `partial` separately. Use `agy_events` to inspect the process outcome and `agy_get_result` to recover saved text. A failed job can carry an answer. |
| The reported version or tool list is stale | Compare `agy_info.server_version` and `catalog_sha256` with the deployed build and expected catalog. On Linux, `runtime.executable_state: "replaced"` identifies a running process using the old executable. Reconnect it and refresh app metadata after schema changes. |

The default job store is `$XDG_STATE_HOME/agy-mcp` or `~/.local/state/agy-mcp`. A stable absolute `AGY_MCP_STATE_DIR` keeps different launches on the same store. Completed jobs become eligible for cleanup after 24 hours by default. Export results you need beyond that window and protect the store, which contains job input and output. [Retention and recovery contract](../README.md#result-pages)
