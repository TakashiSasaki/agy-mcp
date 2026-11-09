# AGY conversation adoption and Remote Control summaries

MEASURED against AGY 1.5.3 on Linux on 2026-11-09, with the Remote Control
daemon running as the systemd user service `antigravity-cli-daemon.service`.
The final MCP build was `v2.10.0+local.3858843`. The experiment checked the
daemon's local snapshot and continuation through MCP using the same conversation
ID. Browser visibility and remote storage require separate observations.

Remote Control exposes a headless CLI instance running on the host.
[Antigravity Remote Control documentation](https://antigravity.google/docs/remote-control/#remote-control-headless-daemon)

## Where AGY stores conversation data

The observed files are relative to `~/.gemini/antigravity-cli/`. A
conversation's ID identifies its trajectory and the corresponding records in
the summary stores.

| File | Role in the observed build |
| --- | --- |
| `conversations/<id>.db` | The conversation trajectory. The `steps` table stores execution history. The `trajectory_metadata_blob` row with ID `main` carries the project binding. |
| `annotations/<id>.pbtxt` | Conversation annotations, including the title. |
| `conversation_summaries.db` | The `conversation_summaries` table supplies the local list and lookup fields, including title, project ID, workspace URIs, step count, and modification time. A `raw_summary` blob also carries summary metadata where present. |
| `jetbox_summaries_proto.pb` | The CLI's persisted protobuf snapshot of conversation summaries. |
| `agyhub_summaries_proto.pb` | The Remote Control daemon's persisted protobuf snapshot of conversation summaries. |

Both protobuf snapshots can contain a summary for the same ID, with their own
project, title, timestamp, and step count. AGY's daemon log reported background
summary reconciliation on startup. A summary row and the conversation's actual
steps can therefore describe different revisions.

## What changed on restart

The baseline agy-mcp build `7060e27` based `post_restart_verified: true` on
a local SQLite update. The call requested no restart, and the ID was absent
from the daemon snapshot.

On startup, an existing daemon entry restored an earlier title.
Removing only that entry let the daemon rebuild the updated title. A later
observation found the old title in the CLI snapshot and again in SQLite while
the daemon still held the updated title. AGY also restored the original
trajectory workspace URIs after a summary-only append.

A CLI continuation changed the step count while the title and project stayed
the same. The measurements paired MCP responses with read-only SQLite and
protobuf snapshot reads for one test conversation.

| Observation | Trajectory steps | SQLite summary | CLI snapshot | Daemon snapshot |
| --- | ---: | ---: | ---: | ---: |
| Already indexed before adoption | 6 | 6 | 6 | 6 |
| After continuation through MCP on the original ID | 9 | 9 | 9 | 6 |
| After a daemon restart to adopt another conversation | 9 | 6 | 9 | 6 |
| After a further CLI turn and targeted refresh | 12 | 12 | 12 | 12 |

A targeted refresh reconciled the nine-step revision, and the next MCP
continuation recalled a marker from the original history on the same ID.
A separate fresh conversation with an omitted title was adopted in one call.
Its two-step summary appeared in the daemon snapshot while the CLI snapshot
had no entry for that ID. The earlier twelve-step conversation survived
the restart for this fresh adoption.

## How agy-mcp refreshes and verifies adoption

1. `agy_adopt_conversation` persists the target project and title for the
   original ID. It updates the local summary, trajectory project metadata, and
   title annotation, retaining the recorded workspace URIs and execution steps.
2. With `restart_daemon: true`, an idle-checked refresh stops the daemon,
   rechecks the local binding, and removes stale entries for that ID from the
   CLI and daemon snapshots where present. The rewrite preserves the other
   protobuf fields. On startup, AGY rebuilds the daemon entry.
3. agy-mcp matches the daemon entry by conversation ID, project, and title. It
   compares the daemon's revision against the newest timestamp observed in
   SQLite or the CLI snapshot. A newer daemon timestamp covers that revision.
   Equal timestamps require equal step counts. When no local timestamp is
   available, the binding and title provide the comparison.

`success: true` reports local persistence. `cloud_sync_state: confirmed`
reports the daemon match. A readable snapshot with a missing, mismatched, or
older entry gives `pending`. A snapshot that cannot be read gives
`unavailable`. `post_restart_verified: true` records that this call both
restarted the daemon and observed the match. `already_converted: true`
describes the local binding and title, so a subsequent CLI turn can still
leave a newer revision pending. Repeating the adoption with
`restart_daemon: true` refreshes that revision. A repeat against matching
local metadata and current snapshots returned without restarting the daemon
in the final experiment.

The comparison and targeted snapshot rewrite live in
[`conversation_sync.go`](../internal/manager/conversation_sync.go).
Local persistence and refresh sequencing live in
[`conversation_adoption.go`](../internal/manager/conversation_adoption.go).

## Synchronization through job submission

With the sync worker installed, both run tools require a registered cloud
`project`. The worker picks up durable job records after execution and
verifies the project, title and history. A continuation records the preceding
revision at admission and verifies that the new turn advanced it. The worker
compares the SQLite and CLI revisions with the actual step count in the
trajectory before pinning the target in the job's sync receipt.

MEASURED against AGY 1.5.3 through MCP. Passing `--project` on a fresh
headless run recorded the requested project in the trajectory and CLI
summary. The running Remote Control daemon still had no entry for that
conversation. The initial integrated build therefore uses the worker's
post-execution adoption. It skips a restart when the daemon already covers
the pinned revision and the CLI snapshot contains no stale entry for it.

On the deployed Linux/systemd build `a3977f5`, two parallel jobs reached
`confirmed` after one daemon restart. A continuation reused the original ID,
recalled the first response and advanced its history from two to five steps.
Both conversations remained confirmed after a later batch. The user verified
the continued response in the browser. When the worker was killed after a
simulated stop boundary, systemd restarted it and the worker restored Remote
Control from the durable marker. Removing a disposable job's confirmation
receipt after recovery let the worker confirm it again with the same daemon
PID.

See [deployment and recovery](self-hosting.md#required-cloud-job-sync) for
the shared store and the watchdog guard.
