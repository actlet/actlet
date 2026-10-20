package agent

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Compaction: when a worker's context nears its model's window, a model
// summarises the worker's older turns, and the worker carries on from its
// first message, which holds the task, the summary and its most recent
// turns. A checkpoint is written before the intent and after the
// outcome; recovery in between makes the summary's call again with the
// same intent ID.

// Compaction defaults.
const (
	DefaultCompactionThreshold = 80
	DefaultCompactionKeepTurns = 4
)

// Compaction limits.
const (
	maxTranscript     = 512 << 10 // the transcript a summary is written from
	maxTranscriptPart = 8 << 10   // one tool call's input or result in it
	maxSummary        = 64 << 10
)

// CompactionPolicy is when and how workers compact their context.
type CompactionPolicy struct {
	// Off turns compaction off.
	Off bool
	// ThresholdPercent of the model's context window starts a compaction;
	// 0 means DefaultCompactionThreshold.
	ThresholdPercent uint32
	// Model writes the summary; empty means the worker's own model.
	Model string
	// KeepTurns are the recent model turns kept as they are; 0 means
	// DefaultCompactionKeepTurns.
	KeepTurns int
}

const compactionSystem = `You summarise an AI agent's work so far, so that it can carry on with less context. The agent keeps its task and its most recent turns; your summary replaces the turns in between. Write for the agent, not for a person.`

const compactionRequest = `Summarise the conversation above so that the agent can carry on from your summary and its most recent turns. Include:

- the task's requirements and constraints, as they became clear;
- what has been done, naming the files created or changed and the commands run;
- what was found: results, values and decisions, with their reasons;
- errors met and how they were resolved, or that they weren't;
- what remains to do, and the next step.

Be specific and brief. Write only the summary.`

// summaryHeading starts the summary in the first message.
const summaryHeading = "[Summary of the earlier turns of this conversation, which were removed to save context]\n\n"

// compactionDue reports whether a worker's context has reached the
// threshold, and returns where to cut its conversation.
func (b *box) compactionDue(st *agentpb.WorkerState) (at, summarized, kept int, ok bool) {
	p := b.env.Settings.Compaction
	window := b.env.Settings.ContextWindows[st.GetModel()]
	if p.Off || window == 0 || st.GetContextTokens() == 0 {
		return 0, 0, 0, false
	}
	threshold := uint64(p.ThresholdPercent)
	if threshold == 0 {
		threshold = DefaultCompactionThreshold
	}
	if st.GetContextTokens()*100 < window*threshold {
		return 0, 0, 0, false
	}
	keep := p.KeepTurns
	if keep <= 0 {
		keep = DefaultCompactionKeepTurns
	}
	at, summarized, kept = compactionCut(st.GetMessages(), keep)
	return at, summarized, kept, at > 0
}

// compactionCut returns the index of the first message to keep, after the
// first: the assistant message keep turns from the end, or fewer turns if
// the conversation is shorter, so that at least one turn is summarised and
// one kept. It returns 0 when there is nothing to summarise.
func compactionCut(msgs []*gatewayv0.Message, keep int) (at, summarized, kept int) {
	var answers []int
	for i, m := range msgs {
		if i > 0 && m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			answers = append(answers, i)
		}
	}
	if len(answers) < 2 {
		return 0, 0, 0
	}
	kept = min(keep, len(answers)-1)
	return answers[len(answers)-kept], len(answers) - kept, kept
}

// compact compacts a worker's conversation, or finishes a compaction a
// crash or pause interrupted.
func (w *worker) compact(st *agentpb.WorkerState, at, summarized, kept int) error {
	b := w.b
	in := st.GetCompacting()
	if in == nil {
		if err := b.checkpoint(); err != nil {
			return err
		}
		model := b.env.Settings.Compaction.Model
		if model == "" {
			model = st.GetModel()
		}
		in = &agentpb.CompactionIntent{IntentId: newID(), WorkerId: w.id, Cut: uint32(at), SummarizedTurns: uint32(summarized),
			KeptTurns: uint32(kept), Model: model, ContextTokens: st.GetContextTokens()}
		if err := b.commit(in); err != nil {
			return err
		}
		b.text(w.id, "compacting the context: %d tokens; summarising %d turns with %s, keeping %d", in.GetContextTokens(),
			summarized, model, kept)
	} else {
		b.text(w.id, "compacting the context again after an interruption (intent %s)", in.GetIntentId())
	}
	if crashHit("compaction.intent", w.id) {
		return errStopped
	}
	if err := b.waitBudget(); err != nil {
		return err
	}
	msgs := st.GetMessages()
	if int(in.GetCut()) > len(msgs) {
		return w.end("", fmt.Errorf("compaction %s cuts at message %d of %d", in.GetIntentId(), in.GetCut(), len(msgs)))
	}
	req := &gatewayv0.InferenceRequest{
		SessionId: b.env.SessionID, Epoch: b.env.Epoch, IntentId: in.GetIntentId(), Model: in.GetModel(),
		System:   compactionSystem,
		Messages: []*gatewayv0.Message{userText(transcript(msgs[:in.GetCut()]) + "\n\n" + compactionRequest)},
		Purpose:  "compaction", Worker: w.id,
	}
	ans, err := b.env.Infer(b.env.Soft, req)
	if err != nil {
		if b.env.Soft.Err() != nil {
			return errStopped // the next run makes the call again
		}
		return w.end("", fmt.Errorf("compacting the context: %w", err))
	}
	summary := strings.TrimSpace(messageText(ans.Message))
	if summary == "" {
		return w.end("", errors.New("compacting the context: the model wrote no summary"))
	}
	outcome := &agentpb.CompactionOutcome{IntentId: in.GetIntentId(), WorkerId: w.id, Summary: cut(summary, maxSummary),
		Usage: ans.Done.GetUsage(), Attempts: uint32(ans.Attempts)}
	if err := b.commit(outcome); err != nil {
		return err
	}
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Compaction{Compaction: &gatewayv0.Compaction{
		Model: in.GetModel(), SummarizedTurns: in.GetSummarizedTurns(), KeptTurns: in.GetKeptTurns(),
		ContextTokens: in.GetContextTokens(), Summary: cut(summary, maxEventText), Usage: ans.Done.GetUsage(),
	}}})
	if crashHit("compaction.outcome", w.id) {
		return errStopped
	}
	return b.checkpoint()
}

// compacted applies a compaction's summary: the first message, with the
// summary in place of any earlier one, then the messages from the cut on.
func compacted(w *agentpb.WorkerState, o *agentpb.CompactionOutcome) error {
	in := w.GetCompacting()
	if in == nil || in.GetIntentId() != o.GetIntentId() {
		return fmt.Errorf("compaction outcome %s without its intent", o.GetIntentId())
	}
	c := int(in.GetCut())
	if c < 1 || c > len(w.Messages) {
		return fmt.Errorf("compaction %s cuts at message %d of %d", in.GetIntentId(), c, len(w.Messages))
	}
	first := proto.Clone(w.Messages[0]).(*gatewayv0.Message)
	if w.Compactions > 0 && len(first.Parts) > 0 {
		first.Parts = first.Parts[:len(first.Parts)-1]
	}
	first.Parts = append(first.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: summaryHeading + o.GetSummary()}})
	w.Messages = append([]*gatewayv0.Message{first}, w.Messages[c:]...)
	addUsage(w.Usage, o.GetUsage())
	w.Compacting = nil
	w.Compactions++
	w.ContextTokens = 0
	return nil
}

// transcript writes messages as text for the summary's model call, with
// long tool inputs and results cut short. Past maxTranscript, it leaves
// out the oldest messages after the first.
func transcript(msgs []*gatewayv0.Message) string {
	texts := make([]string, len(msgs))
	for i, m := range msgs {
		var sb strings.Builder
		role := "User"
		if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			role = "Agent"
		}
		fmt.Fprintf(&sb, "## %s\n\n", role)
		for _, p := range m.GetParts() {
			switch {
			case p.GetToolCall() != nil:
				c := p.GetToolCall()
				fmt.Fprintf(&sb, "[call %s %s: %s]\n", c.GetId(), c.GetName(), cut(c.GetInput(), maxTranscriptPart))
			case p.GetToolResult() != nil:
				r := p.GetToolResult()
				label := "result"
				if r.GetIsError() {
					label = "error"
				}
				fmt.Fprintf(&sb, "[%s of %s: %s]\n", label, r.GetCallId(), cut(r.GetContent(), maxTranscriptPart))
			case p.GetText() != "":
				sb.WriteString(cut(p.GetText(), maxTranscript/4))
				sb.WriteString("\n")
			}
		}
		texts[i] = sb.String()
	}
	total := 0
	for _, t := range texts {
		total += len(t) + 1
	}
	skip := 0
	for total > maxTranscript && skip+2 < len(texts) {
		skip++
		total -= len(texts[skip]) + 1
	}
	if skip > 0 {
		omitted := fmt.Sprintf("[%d earlier messages left out]\n", skip)
		texts = append([]string{texts[0], omitted}, texts[skip+1:]...)
	}
	return strings.Join(texts, "\n")
}
