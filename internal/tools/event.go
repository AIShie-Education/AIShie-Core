package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/members"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func eventTools() []tool.Tool { return []tool.Tool{eventList()} }

// visibility says who may see each type of event: holding ANY of the listed
// permissions is enough, except for an unreleased type, which also asks for
// what its released type asks for (seesType). Scope is applied on top, per
// row, in SQL.
//
// A type that is not in this table is visible to nobody but the member whose
// action caused it. News of a conversation is the exception both ways: it is
// its participants', and only theirs (ListEvents). A new event type is therefore private until someone
// decides otherwise here, which is the safe way round; a test checks that
// every type the tools emit has made that decision.
var visibility = map[string][]domain.Perm{
	// The action log belongs to those who decide. A proposer still sees what
	// became of its own proposals, by the own-action rule.
	events.ActionProposed: {domain.PermActionDecide}, events.ActionApproved: {domain.PermActionDecide},
	events.ActionRejected: {domain.PermActionDecide}, events.ActionCancelled: {domain.PermActionDecide},
	events.ActionReviewed: {domain.PermActionDecide}, events.ActionEscalated: {domain.PermActionDecide},

	// A draft grade is for graders; a posted one for whoever may read grades.
	events.GradeCreated:      {domain.PermGradeSubmit, domain.PermGradePost},
	events.GradePosted:       {domain.PermGradeRead},
	events.GradeRegraded:     {domain.PermGradeRead},
	events.GradeTotalUpdated: {domain.PermGradeRead},

	EventSubmissionSubmitted: {domain.PermSubmissionRead},
	EventSubmissionLateness:  {domain.PermSubmissionRead},
	EventSubmissionMissing:   {domain.PermSubmissionRead},

	// Unpublished work is for those who write assignments.
	EventAssignmentCreated:   {domain.PermAssignmentWrite},
	EventAssignmentUpdated:   {domain.PermAssignmentWrite},
	EventAssignmentPublished: {domain.PermDocumentRead},
	// Those who were told it was published are told it was taken back.
	EventAssignmentUnpublished: {domain.PermDocumentRead},
	EventAssignmentDuePassed:   {domain.PermDocumentRead},

	EventComponentCreated: {domain.PermGradeRead}, EventComponentUpdated: {domain.PermGradeRead},
	EventComponentMoved: {domain.PermGradeRead},

	members.EventAdded: {domain.PermMemberRead}, members.EventUpdated: {domain.PermMemberRead},
	members.EventPaused: {domain.PermMemberRead}, members.EventResumed: {domain.PermMemberRead},
	members.EventRemoved: {domain.PermMemberRead}, members.EventRescoped: {domain.PermMemberRead},

	EventCourseCreated: {domain.PermDocumentRead}, EventCourseUpdated: {domain.PermDocumentRead},
	EventCourseActivated: {domain.PermDocumentRead}, EventCourseArchived: {domain.PermDocumentRead},

	// A draft of course material is for those who read drafts; publishing is
	// for everyone who reads that kind of document.
	EventDocumentCreated: {domain.PermDocumentReadDraft}, EventDocumentVersionAdded: {domain.PermDocumentReadDraft},
	EventDocumentArchived:  {domain.PermDocumentReadDraft},
	EventDocumentPublished: {domain.PermDocumentRead}, EventRubricPublished: {domain.PermRubricRead},
	EventSubmissionFileAdded: {domain.PermSubmissionRead}, EventSubmissionFileArchived: {domain.PermSubmissionRead},
	EventFeedbackFileAdded: {domain.PermGradeSubmit, domain.PermGradePost}, EventFeedbackFileArchived: {domain.PermGradeSubmit, domain.PermGradePost},

	// Instructions and a rubric are their assignment's. News of them is filed
	// under each published assignment that refers to them, for scope to
	// apply, and until there is one it goes by these names instead: an
	// unpublished assignment is for those who write assignments, and of them
	// for those who would be told the news by its own name.
	EventDocumentCreatedUnreleased: {domain.PermAssignmentWrite}, EventDocumentVersionAddedUnreleased: {domain.PermAssignmentWrite},
	EventDocumentPublishedUnreleased: {domain.PermAssignmentWrite}, EventRubricPublishedUnreleased: {domain.PermAssignmentWrite},
	EventDocumentArchivedUnreleased: {domain.PermAssignmentWrite},

	// A conversation's news is for its two participants and nobody else,
	// whoever holds what: event.list shows it by the participant rule
	// (queries ListEvents), not by permission, and not even to whoever
	// caused it without taking part — a manager whose removal of a seat
	// closed it. So no permission is listed for it here.
	events.ConversationOpened: nil, events.ConversationMessagePosted: nil,
	events.ConversationClosed: nil, events.ConversationMessageRetracted: nil,

	// Platform events belong to no course, so they are in no course's feed.
	// They are listed so that leaving them out is visibly a decision.
	EventActorRegistered: nil, EventActorUpdated: nil, EventActorInvited: nil, EventActorCredentialRevoked: nil,
	EventActorSuspended: nil, EventActorReactivated: nil, EventAgentCreated: nil,
}

// KnownEventTypes lists every event type that has a visibility rule.
func KnownEventTypes() []string {
	out := make([]string, 0, len(visibility))
	for t := range visibility {
		out = append(out, t)
	}
	return out
}

func visibleTypes(m *domain.Member) []string {
	out := []string{}
	for typ := range visibility {
		if seesType(m, typ) {
			out = append(out, typ)
		}
	}
	return out
}

// seesType: any one of the type's permissions is enough. An unreleased type
// is its released type's news, told early to those who see unpublished work,
// so it asks for both: a seat that writes assignments but may not read drafts
// or rubrics is not told here of a draft or a rubric that document.get would
// not show it.
func seesType(m *domain.Member, typ string) bool {
	for released, u := range unreleased {
		if u == typ && !seesType(m, released) {
			return false
		}
	}
	for _, p := range visibility[typ] {
		if m.Perm(p).Allowed() {
			return true
		}
	}
	return false
}

type EventListIn struct {
	tool.InCourse
	SinceSeq int64 `json:"since_seq,omitempty" jsonschema:"the seq of the last event already seen; 0 for the beginning"`
	Limit    int   `json:"limit,omitempty" jsonschema:"default 100, maximum 500"`
}

type EventView struct {
	Seq             int64           `json:"seq"`
	Type            string          `json:"type"`
	ActionID        *uuid.UUID      `json:"action_id,omitempty"`
	SubjectType     string          `json:"subject_type"`
	SubjectID       *uuid.UUID      `json:"subject_id,omitempty"`
	StudentMemberID *uuid.UUID      `json:"student_member_id,omitempty"`
	AssignmentID    *uuid.UUID      `json:"assignment_id,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	OccurredAt      time.Time       `json:"occurred_at"`
}

type EventListOut struct {
	Events []EventView `json:"events"`
	// NextSeq is what to pass as since_seq next time. It moves even when the
	// page is empty of events the caller may see.
	NextSeq int64 `json:"next_seq"`
	More    bool  `json:"more" jsonschema:"true when the page was full and there may be more right now"`
}

func eventList() tool.Tool {
	return tool.Define(tool.Spec[EventListIn, EventListOut]{
		Name: "event.list",
		Description: "The course's event feed from a cursor: everything that has happened since since_seq that the caller is " +
			"allowed to know about. Events carry ids, never content — fetch what they point to with the read tools. " +
			"The events of your own actions are always included, which is how you learn that a proposal was approved, " +
			"rejected or cancelled. Core never calls out: poll this.",
		Kind: tool.Read,
		// Any seat in the course can read the feed; what it shows is decided
		// per event type and per row.
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/events"},
		Resolve: func(_ context.Context, _ dbq.Querier, in EventListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "event"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in EventListIn) (EventListOut, error) {
			limit := int32(100)
			if in.Limit > 0 {
				limit = int32(min(in.Limit, 500))
			}
			rows, err := rc.Q.ListEvents(ctx, dbq.ListEventsParams{
				CourseID: &in.CourseID, SinceSeq: in.SinceSeq, MaxRows: limit, VisibleTypes: visibleTypes(rc.Member),
				MemberID: &rc.Scope.MemberID, StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll,
				PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll,
			})
			out := EventListOut{Events: make([]EventView, 0, len(rows)), NextSeq: in.SinceSeq, More: len(rows) == int(limit)}
			for _, r := range rows {
				out.Events = append(out.Events, EventView{Seq: r.Seq, Type: r.Type, ActionID: r.ActionID, SubjectType: r.SubjectType,
					SubjectID: r.SubjectID, StudentMemberID: r.StudentMemberID, AssignmentID: r.AssignmentID, Payload: r.Payload, OccurredAt: r.OccurredAt})
				out.NextSeq = r.Seq
			}
			return out, err
		},
	})
}
