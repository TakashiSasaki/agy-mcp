package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// streamOutcome is what the supervisor learned from one run's stream-json
// output: the terminal result if the run reached one, and how many unusable
// lines were skipped along the way.
//
// It carries no conversation id of its own, because nothing outside the tests
// ever read one, and the id already leaves here by the two routes callers do
// read: live in progress.json, written as soon as the init event names it, which
// is what lets the manager report it mid-run; and after the fact inside
// result.json, since result carries agy's own ConversationID (the manager reads
// that as a fallback, see carryResultMetadata).
type streamOutcome struct {
	result    *streamjson.Result
	malformed int
}

// consumeStream decodes agy's event stream to completion, mirroring it into the
// job directory as it goes.
//
// Evidence is written live because cancellation or timeout can happen before
// any final answer:
//
//   - progress.json, so the manager can report the conversation id (carried by
//     the init event, which arrives before any model work) and the current step
//     while the job is still running.
//   - out, which accumulates the assistant's text. It is the fallback the
//     manager reports as a partial result when no terminal event ever arrives.
//     Only agent_response deltas contribute: agy's stream carries no thinking
//     text (only a usage.thinking_tokens counter) and a tool step carries no
//     response text, so a run killed while still reasoning or mid-tool, before
//     it streams any answer, leaves out empty by design, not by defect
//     (MEASURED against agy 1.1.15).
//   - events.ndjson preserves timestamped provider events, including fields
//     the response decoder does not interpret. capture.json records its
//     generation, storage cap and any lost/malformed input.
//
// The terminal result is returned rather than written here, so the caller can
// order it against the exit-code sentinel.
func consumeStream(jobDir string, r io.Reader, out io.Writer) streamOutcome {
	return consumeStreamObserved(jobDir, r, out, time.Now)
}

func consumeStreamObserved(jobDir string, r io.Reader, out io.Writer, now func() time.Time) streamOutcome {
	var oc streamOutcome
	sr := streamjson.NewReader(r)
	journal, captureErr := jobstore.OpenJournal(jobDir)
	if captureErr == nil {
		defer journal.Close()
	}
	var prog jobstore.Progress
	stepUsages := make(map[int]streamjson.Usage)
	for {
		ev, raw, err := sr.NextRaw()
		if err != nil {
			// io.EOF on a clean end, or a read error once the process group was
			// torn down. Either way there is nothing further to decode, and the
			// exit-code sentinel is what classifies the outcome.
			break
		}
		if journal != nil {
			journal.Append(raw)
		}
		changed := false
		if cid := ev.ConversationIDOf(); cid != "" && cid != prog.ConversationID {
			prog.ConversationID = cid
			changed = true
		}
		switch ev.Kind {
		case streamjson.EventInit:
			if ev.Init != nil && ev.Init.Model != "" && ev.Init.Model != prog.InitModel {
				prog.InitModel = ev.Init.Model
				changed = true
			}
		case streamjson.EventStepUpdate:
			if su := ev.StepUpdate; su != nil {
				if su.Usage != nil {
					stepUsages[su.StepIndex] = *su.Usage
				}
				// Identical ACTIVE updates are still a received heartbeat. Preserve
				// their observation time instead of making a live tool look silent.
				prog.StepIndex = su.StepIndex
				prog.StepType = su.StepType
				prog.StepState = su.State
				changed = true
				// Append every delta rather than tracking which steps have already
				// contributed. agy streams one agent_response step as many text_delta
				// events (the ACTIVE states carry successive chunks, the DONE state the
				// final tail), so appending every one in arrival order reproduces the
				// response.
				//
				// MEASURED against agy 1.1.15, and re-measured against 1.1.22: a
				// 9448-byte response arrived as 38 ACTIVE deltas of 125 to 226 bytes
				// each plus a 2248-byte DONE tail, all on one step_index, and
				// concatenating all 39 equalled the terminal result byte for byte
				// (non-ASCII text included). A response short enough to fit one chunk
				// arrives as a single DONE delta instead, with no ACTIVE events at all
				// (also MEASURED against 1.1.22), so both shapes have to work.
				//
				// A run with several agent_response steps accumulates all of them, which
				// is more context than the terminal result carries. The manager reads it
				// when no terminal result reached disk, and also when one did but carried
				// no response of its own, since then this is the only text there is.
				//
				// Do NOT dedup by step_index to "skip repeats": the deltas are not
				// repeats and agy does not emit one per step, so a step-indexed filter
				// would keep one delta of the step and drop the rest, whichever way it
				// dedups. TestConsumeStreamAccumulatesChunksWithinOneStep pins exactly that.
				if su.StepType == streamjson.StepTypeAgentResponse && su.TextDelta != "" {
					// A failed append costs partial-result fidelity, nothing more: the
					// terminal result event is the authoritative response and is written
					// separately. Losing the stream would be worse than losing this text.
					_, _ = io.WriteString(out, su.TextDelta)
				}
			}
		case streamjson.EventResult:
			if ev.Result != nil {
				oc.result = ev.Result
			}
		}
		if changed {
			prog.UpdatedAt = now().UTC()
			// Best effort for the same reason: progress is a hint the manager treats
			// as absent when unreadable, never a correctness gate.
			_ = jobstore.WriteProgressDir(jobDir, prog)
		}
	}
	if oc.result != nil {
		if len(stepUsages) > 0 {
			var ju streamjson.Usage
			for _, u := range stepUsages {
				ju.InputTokens += u.InputTokens
				ju.OutputTokens += u.OutputTokens
				ju.ThinkingTokens += u.ThinkingTokens
				ju.CacheReadTokens += u.CacheReadTokens
				ju.TotalTokens += u.TotalTokens
			}
			oc.result.JobUsage = &ju
		} else if oc.result.Usage != nil && oc.result.NumTurns <= 1 {
			oc.result.JobUsage = oc.result.Usage
		}
		if oc.result.NumTurns > 1 {
			oc.result.UsageScope = streamjson.UsageScopeConversationCumulative
		} else {
			oc.result.UsageScope = streamjson.UsageScopeJob
		}
	}
	oc.malformed = sr.Malformed()
	if journal != nil {
		journal.NoteMalformed(oc.malformed)
	}
	return oc
}

// persist writes the terminal result to the job directory and notes any skipped
// lines on stderr. A run that produced no terminal event writes no result file,
// which is exactly the signal the manager uses to mark its captured output
// partial.
func (oc streamOutcome) persist(jobDir string, errW io.Writer) {
	if oc.malformed > 0 {
		// Surface corruption where a reader will actually see it. Skipping a line
		// is recoverable, so it must not fail the job, but silently discarding
		// output would make a truncated answer look complete.
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: skipped %d unreadable line(s) in agy's stream-json output\n", oc.malformed)
	}
	if oc.result == nil {
		return
	}
	b, err := json.Marshal(oc.result)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: could not record agy's result payload: %v\n", err)
		return
	}
	if err := jobstore.WriteResultDir(jobDir, b); err != nil {
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: could not write agy's result payload: %v\n", err)
	}
}
