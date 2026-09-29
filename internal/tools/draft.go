package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// An answer's draft (docs/schema.md §2.8, Drafts): while a respondent's
// runtime writes an answer, it says what it is doing (its steps) and, as the
// model writes it, the text so far, so that whoever reads the conversation
// watches the answer come, and the posted answer then takes its place.
//
// A draft is not a record. conversation.draft is an ephemeral write
// (tool.Ephemeral): no action, no idempotency key, never a proposal, no
// event, and it bounds its own rate, per conversation, beside which it costs
// its caller nothing of theirs. It is kept, one per conversation, in an
// UNLOGGED table, the last write winning, and is gone once the answer is
// posted or proposed, the conversation is closed, or its runtime says the
// attempt is over; one nobody has written for DraftTTL is no draft.
//
// Who sees its text is who would see the answer: the opener, and whoever
// else reads the conversation, while the respondent's answers are posted as
// they are written (conversation_answer autonomous); otherwise only those
// who could decide the answer once it is proposed, and the respondent, who
// wrote it. The others see its steps, and that its text is held back
// (text_hidden).

// DraftTTL is how long a draft stands without being written again: a
// runtime that stopped without a word leaves nothing up for longer.
const DraftTTL = 120 * time.Second

// DraftWritesPerSecond bounds how often one conversation's draft is written,
// in each instance: a runtime sends one every 300 ms or so.
const DraftWritesPerSecond = 10

const (
	maxDraftAttemptChars = 64
	maxDraftSteps        = 20
	maxStepTargetChars   = 120
)

// What a step of a draft may be, and be in.
var (
	draftStepKinds = []string{"thinking", "reading_document", "listing_documents", "reading_assignment", "reading_submission",
		"searching_memory", "writing", "tool"}
	draftStepStates = []string{"running", "done"}
)

// DraftStep is one thing a runtime did, or does, towards an answer.
type DraftStep struct {
	Kind   string  `json:"kind" jsonschema:"thinking, reading_document, listing_documents, reading_assignment, reading_submission, searching_memory, writing or tool"`
	Target *string `json:"target,omitempty" jsonschema:"what it is about, as plain text on one line, at most 120 characters: a document's title, say"`
	State  string  `json:"state" jsonschema:"running or done"`
}

type ConversationDraftIn struct {
	tool.InCourse
	ConversationID uuid.UUID   `json:"conversation_id"`
	Attempt        string      `json:"attempt" jsonschema:"your id for this attempt at the answer, 1 to 64 characters; a new attempt replaces the draft of another"`
	Version        int64       `json:"version" jsonschema:"1, 2, 3, ... rising with each write of the attempt: a write not newer than the draft kept is passed over (stored false), so one that arrives late undoes nothing"`
	Text           *string     `json:"text,omitempty" jsonschema:"the whole answer so far, at most 20000 characters, replacing what was kept; left out, the attempt's text is kept"`
	Steps          []DraftStep `json:"steps,omitempty" jsonschema:"every step so far, at most 20, replacing those kept; left out, the attempt's steps are kept"`
	Done           bool        `json:"done,omitempty" jsonschema:"true: the attempt is over, given up or finished, and its draft is gone; give a version no lower than the last"`
}

type ConversationDraftOut struct {
	Stored  bool  `json:"stored" jsonschema:"whether this write was kept: false when the draft kept is newer (a higher version of the attempt, or the attempt already over), or this ends an attempt that is not the one kept"`
	Version int64 `json:"version" jsonschema:"the version of the draft as a reader now finds it, what seen_draft_version names: this one's when it was stored; 0 for none, as once an attempt is over"`
}

// DraftView is a draft as the read tools show it (conversation.get,
// conversation.messages).
type DraftView struct {
	Attempt   string      `json:"attempt"`
	Version   int64       `json:"version" jsonschema:"what seen_draft_version names"`
	UpdatedAt time.Time   `json:"updated_at"`
	Steps     []DraftStep `json:"steps"`
	// Text is the answer so far, to whom it would be shown once posted;
	// TextHidden says it is held back from the caller, whose steps it sees.
	Text       *string `json:"text,omitempty" jsonschema:"the answer so far, when you may see it; absent while none is written, or when text_hidden"`
	TextHidden bool    `json:"text_hidden,omitempty" jsonschema:"true when its text is not shown to you while it is written: the respondent's answers are not posted as they are written (its answer_level is not autonomous), and you would not decide them; you read the answer once it is posted"`
}

var (
	errNotTheRespondent = apperr.Forbid("only the member a conversation is addressed to writes its answer's draft").
				With("reason", "not_the_respondent")
	errNotAwaiting = apperr.Conflicts("the conversation waits for no answer now: it is answered, an answer waits for approval, "+
		"or it is closed; a draft is written only while it waits").With("reason", "conversation_not_awaiting")
)

// errDraftTooSoon refuses a draft written faster than DraftWritesPerSecond,
// as the rate limit refuses a call too soon.
func errDraftTooSoon(wait time.Duration) *apperr.Error {
	secs := max(int(math.Ceil(wait.Seconds())), 1)
	return apperr.New(apperr.RateLimited, "a conversation's draft is written at most %d times a second; try again in %d seconds",
		DraftWritesPerSecond, secs).With("reason", "draft_rate").With("retry_after_seconds", secs)
}

// plainLine says whether s is text on one line: no control character.
func plainLine(s string) bool {
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// checkDraft holds a draft to what the database takes, and returns its steps
// as they are kept, trimmed, or nil when the write keeps the attempt's.
func checkDraft(in ConversationDraftIn) ([]byte, error) {
	if n := utf8.RuneCountInString(in.Attempt); n < 1 || n > maxDraftAttemptChars || !plainLine(in.Attempt) {
		return nil, apperr.Invalid("attempt is 1 to %d characters of plain text", maxDraftAttemptChars).With("field", "attempt")
	}
	if in.Version < 1 {
		return nil, apperr.Invalid("version is 1 or more").With("field", "version")
	}
	if in.Text != nil {
		if n := utf8.RuneCountInString(*in.Text); n > maxMessageChars {
			return nil, apperr.Invalid("the text is %d characters long; the most is %d", n, maxMessageChars).With("field", "text")
		}
	}
	if in.Steps == nil {
		return nil, nil
	}
	if len(in.Steps) > maxDraftSteps {
		return nil, apperr.Invalid("at most %d steps", maxDraftSteps).With("field", "steps")
	}
	steps := make([]DraftStep, len(in.Steps))
	for i, s := range in.Steps {
		if !slices.Contains(draftStepKinds, s.Kind) {
			return nil, apperr.Invalid("a step's kind is one of %s", strings.Join(draftStepKinds, ", ")).With("field", "steps")
		}
		if !slices.Contains(draftStepStates, s.State) {
			return nil, apperr.Invalid("a step's state is running or done").With("field", "steps")
		}
		steps[i] = DraftStep{Kind: s.Kind, State: s.State}
		if s.Target != nil {
			t := strings.TrimSpace(*s.Target)
			if utf8.RuneCountInString(t) > maxStepTargetChars || !plainLine(t) {
				return nil, apperr.Invalid("a step's target is plain text on one line, at most %d characters", maxStepTargetChars).
					With("field", "steps")
			}
			if t != "" {
				steps[i].Target = &t
			}
		}
	}
	return json.Marshal(steps)
}

// conversationDraft is gated as answering is: by conversation_answer, a
// person told that they answer nothing. Who may write is the conversation's
// respondent, while it waits for an answer and its opener may still address
// the respondent, as answering it is; at any level above denied, since a
// draft is never proposed: what the level holds back is its text, from
// those who may not see the answer before it is approved.
func conversationDraft(limit *ratelimit.Limiter) tool.Tool {
	return tool.Define(tool.Spec[ConversationDraftIn, ConversationDraftOut]{
		Name: "conversation.draft",
		Description: "For an agent runtime, never for a model: say what you are doing towards an answer in a conversation " +
			"addressed to you, and the answer's text so far, for whoever reads the conversation to watch it come. Each " +
			"write replaces the draft whole: steps (thinking, reading_document, ... each running or done) and text, both " +
			"the whole list and the whole text so far; either left out keeps the attempt's. A write whose attempt and " +
			"version are not newer than the draft kept is passed over (stored false). done ends the attempt, given up or " +
			"finished. Posting or proposing the answer, or closing the conversation, clears it; one not written for 120 " +
			"seconds is gone. Only the respondent writes, while the conversation waits for its answer " +
			"(not_the_respondent, conversation_not_awaiting). It is recorded nowhere: no action, no idempotency key, " +
			"never proposed; at most 10 writes a second per conversation (rate_limited), and a write carried out does " +
			"not count against your rate limit. The text is shown to the opener only while your answers are posted " +
			"without approval; otherwise to those who would approve them.",
		Kind: tool.Ephemeral, Gate: answers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/conversations/{conversation_id}/draft"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ConversationDraftIn) (tool.Target, error) {
			return conversationTarget(ctx, q, in.CourseID, in.ConversationID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ConversationDraftIn) (ConversationDraftOut, error) {
			steps, err := checkDraft(in)
			if err != nil {
				return ConversationDraftOut{}, err
			}
			c, err := findConversation(ctx, ec.Q, in.CourseID, in.ConversationID)
			if err != nil {
				return ConversationDraftOut{}, err
			}
			if c.RespondentMemberID != ec.Member.ID {
				return ConversationDraftOut{}, errNotTheRespondent
			}
			// Counted once the caller is known to be the one who writes it,
			// so that nobody else spends a conversation's allowance.
			if ok, wait := limit.Allow(c.ID.String()); !ok {
				return ConversationDraftOut{}, errDraftTooSoon(wait)
			}
			if _, err := ec.Q.LockConversationForDraft(ctx, dbq.LockConversationForDraftParams{ID: c.ID, CourseID: c.CourseID}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ConversationDraftOut{}, errNoConversation
				}
				return ConversationDraftOut{}, err
			}
			awaiting, err := ec.Q.ConversationAwaitsAnswer(ctx, c.ID)
			if err != nil {
				return ConversationDraftOut{}, err
			}
			if !awaiting {
				return ConversationDraftOut{}, errNotAwaiting
			}
			// Nobody gains through a draft more than through the answer:
			// once the opener may no longer address the respondent, what it
			// writes is not shown to them, as it would not be posted.
			opener, err := authz.LoadMember(ctx, ec.Q, c.OpenerMemberID)
			if err != nil {
				return ConversationDraftOut{}, err
			}
			why, err := newAddressing(ec.Q, ec.Now).refusal(ctx, opener, ec.Member)
			if err != nil {
				return ConversationDraftOut{}, err
			}
			if why != "" {
				return ConversationDraftOut{}, notAddressable("you may no longer answer in this conversation", why)
			}

			stale := ec.Now.Add(-DraftTTL)
			version, err := ec.Q.PutDraft(ctx, dbq.PutDraftParams{ConversationID: c.ID, CourseID: c.CourseID, Attempt: in.Attempt,
				Version: in.Version, Done: in.Done, Body: in.Text, Steps: steps, At: ec.Now, StaleBefore: stale})
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				kept, err := ec.Q.DraftVersion(ctx, dbq.DraftVersionParams{ConversationID: c.ID, FreshAfter: stale})
				return ConversationDraftOut{Version: kept}, err
			case err != nil:
				return ConversationDraftOut{}, err
			}
			// Whoever waits on the conversation, watching its draft, reads
			// it again once this commits (package wake). No event: a draft
			// is in no feed.
			if err := ec.Q.NotifyWake(ctx, dbq.NotifyWakeParams{Channel: wake.Channel, CourseIds: []uuid.UUID{c.CourseID},
				Kinds: []string{wake.KindDraft}, Seqs: []int64{0}, ConversationIds: []uuid.UUID{c.ID},
				ActionIds: []uuid.UUID{uuid.Nil}}); err != nil {
				return ConversationDraftOut{}, err
			}
			if in.Done {
				version = 0 // the attempt is over: there is no draft to see
			}
			return ConversationDraftOut{Stored: true, Version: version}, nil
		},
	})
}

// draftOf is the draft of the conversation v, as rc's caller may see it, or
// nil for none. Only a conversation that waits for its answer has one: a
// draft left behind by a write that passed its answer, or its closing, is
// none. The text is shown where the answer, posted, would be (seesDraftText).
func draftOf(ctx context.Context, rc *tool.ReadCtx, v ConversationView) (*DraftView, error) {
	if v.State != StateAwaitingAnswer {
		return nil, nil
	}
	row, err := rc.Q.GetDraft(ctx, dbq.GetDraftParams{ConversationID: v.ID, FreshAfter: rc.Now.Add(-DraftTTL)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &DraftView{Attempt: row.Attempt, Version: row.Version, UpdatedAt: row.UpdatedAt, Steps: []DraftStep{}}
	if err := json.Unmarshal(row.Steps, &d.Steps); err != nil {
		return nil, err
	}
	show, err := seesDraftText(ctx, rc.Q, rc.Member, rc.Actor.ID, v)
	if err != nil {
		return nil, err
	}
	if show {
		d.Text = row.Body
	} else {
		d.TextHidden = true
	}
	return d, nil
}

// seesDraftText says whether reader, whose actor is actor, sees the text of
// the answer being written in v: everyone who may read it, while the
// respondent's answers are posted as they are written (conversation_answer
// autonomous), as the opener would read the answer at once; otherwise the
// respondent, who writes it, and whoever could decide it once proposed
// (decidesAnswers). Anyone else — the opener, and whoever oversees the
// opener without deciding the answer — sees what the opener sees.
func seesDraftText(ctx context.Context, q dbq.Querier, reader *domain.Member, actor uuid.UUID, v ConversationView) (bool, error) {
	if v.Respondent.AnswerLevel == domain.Autonomous.String() || reader.ID == v.Respondent.MemberID {
		return true, nil
	}
	return decidesAnswers(ctx, q, reader, actor, v.respondentActor)
}

// decidesAnswers says whether reader, whose actor is actor, could decide an
// answer the respondent, whose actor is respondent, proposed: as
// action.decide would let them. That is anyone who decides actions here,
// perm_action_decide allowed, who is not of the respondent's party (four
// eyes, pipeline.sameParty): an agent does not decide its owner's or a
// sibling's. Of the party, the respondent's owner alone, where they decide
// actions without anyone's confirmation: what an owner is measured by for
// their agent's answers (tool.Spec.OwnerJudgedBy, pipeline.ownerJudges).
func decidesAnswers(ctx context.Context, q dbq.Querier, reader *domain.Member, actor, respondent uuid.UUID) (bool, error) {
	level := reader.Perm(domain.PermActionDecide)
	if !level.Allowed() {
		return false, nil
	}
	same, err := q.SameParty(ctx, dbq.SamePartyParams{A: actor, B: respondent})
	if err != nil || !same {
		return !same, err
	}
	if level != domain.Autonomous {
		return false, nil
	}
	agent, err := q.GetActor(ctx, respondent)
	if err != nil {
		return false, err
	}
	return agent.OwnerActorID != nil && *agent.OwnerActorID == actor, nil
}

// sameDraft says whether two reads found the same draft: none both times, or
// the same version of the same attempt.
func sameDraft(a, b *DraftView) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Attempt == b.Attempt && a.Version == b.Version
}

// draftVersion is what seen_draft_version names of a draft: its version,
// 0 for none.
func draftVersion(d *DraftView) int64 {
	if d == nil {
		return 0
	}
	return d.Version
}
