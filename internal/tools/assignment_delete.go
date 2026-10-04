package tools

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Deleting an assignment for good (docs/schema.md §2.5, An assignment is
// deleted for good). assignment.delete_preview says what would go with it,
// and whether its caller may; assignment.delete takes it, given back the
// counts the preview showed, so that a page or an agent that looked before
// more was added cannot delete more than it was shown.

const (
	ToolAssignmentDelete        = "assignment.delete"
	ToolAssignmentDeletePreview = "assignment.delete_preview"

	// The feed's one record of a deletion: of an assignment students could
	// see, for everyone who reads the course; of one they could not, for
	// those who write assignments.
	EventAssignmentDeleted           = "assignment.deleted"
	EventAssignmentDeletedUnreleased = "assignment.deleted_unreleased"

	// PurgedWithAssignment is the reason a document purged with its
	// assignment says it was purged, and each of its versions.
	PurgedWithAssignment = "assignment_deleted"
)

// Why an assignment is not deleted, in error.details.reason, beside the
// authorization's own (course_archived, student_out_of_scope, ...).
const (
	// An agent deletes none that has work or grades: a person does.
	DeletePeopleOnly = "people_only"
	// More would go with it than the caller was shown.
	DeleteConfirmStale = "confirm_stale"
	// There are files to remove and no file store to remove them from.
	DeleteNoFileStorage = "no_file_storage"
	// The assignment was deleted already.
	DeleteDeleted = "deleted"
)

func assignmentDeleteTools(d Deps) []tool.Tool {
	return []tool.Tool{assignmentDeletePreview(d), assignmentDelete(d)}
}

// DeletionCounts is what goes with an assignment, counted: what the preview
// shows, and what assignment.delete is given back (confirm).
type DeletionCounts struct {
	Submissions int `json:"submissions" jsonschema:"every submission to it: every attempt and state, handed in, a draft, or recorded as missing"`
	HandedIn    int `json:"handed_in" jsonschema:"of those, handed in, on time or late"`
	Drafts      int `json:"drafts" jsonschema:"of those, drafts"`
	Missing     int `json:"missing" jsonschema:"of those, recorded as missing"`
	Grades      int `json:"grades" jsonschema:"live grades given on them, drafts and posted; the history of each goes with it and is not counted"`
	Posted      int `json:"posted" jsonschema:"of those, posted"`
	Files       int `json:"files" jsonschema:"files deleted from storage: those handed in and given as feedback, every version's, and those of the documents purged with it; their PDF renditions go too and are not counted"`
	Documents   int `json:"documents" jsonschema:"its instructions and rubric purged with it, since nothing else uses them"`
	Proposals   int `json:"proposals" jsonschema:"proposals about it waiting for a decision, which are cancelled"`
	Totals      int `json:"totals" jsonschema:"students whose posted totals are worked out again without it, the change recorded"`
}

// above names the counts of c that are larger than of was: what more would
// go than was shown.
func (c DeletionCounts) above(was DeletionCounts) bool {
	return c.Submissions > was.Submissions || c.HandedIn > was.HandedIn || c.Drafts > was.Drafts ||
		c.Missing > was.Missing || c.Grades > was.Grades || c.Posted > was.Posted || c.Files > was.Files ||
		c.Documents > was.Documents || c.Proposals > was.Proposals || c.Totals > was.Totals
}

func (c DeletionCounts) check() error {
	if min(c.Submissions, c.HandedIn, c.Drafts, c.Missing, c.Grades, c.Posted, c.Files, c.Documents, c.Proposals, c.Totals) < 0 {
		return apperr.Invalid("confirm's counts are none of them below zero: send back the counts assignment.delete_preview gave")
	}
	return nil
}

// DeletionDocument is an instructions or rubric document an assignment
// names.
type DeletionDocument struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind" jsonschema:"instructions or rubric"`
	Title string    `json:"title"`
}

// deletion is what deleting an assignment takes with it, as the course
// stands: read by the preview and by Validate as it is, and by Execute under
// the locks it takes.
type deletion struct {
	a      dbq.GetAssignmentInCourseRow
	counts DeletionCounts
	// students have a submission row of any kind; totals have a live total
	// that is worked out again.
	students, totals []uuid.UUID
	// owned are the submitted and feedback files, deleted; purged, its
	// instructions and rubric that nothing else uses, purged; kept, those
	// it names that something else uses.
	owned         []uuid.UUID
	purged, kept  []DeletionDocument
	keys          []string
	actions       []dbq.ListActionsAboutAssignmentRow
	proposalCount int
}

// rewritesTotals says whether the totals are worked out again without it:
// it counted toward a component, which only a published assignment does.
func (d deletion) rewritesTotals() bool { return d.a.PublishedAt != nil && d.a.ComponentID != nil }

// gone is every document deleted or purged with it.
func (d deletion) gone() []uuid.UUID {
	out := slices.Clone(d.owned)
	for _, doc := range d.purged {
		out = append(out, doc.ID)
	}
	return out
}

func (d deletion) purgedIDs() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(d.purged))
	for _, doc := range d.purged {
		out = append(out, doc.ID)
	}
	return out
}

// scope is what deleting it reaches: the assignment, every student whose
// work goes with it, and, when the totals are worked out again, every
// student who has one, over the whole course, as a change to the scheme
// reaches them (schemeScope).
func (d deletion) scope() authz.Target {
	t := authz.Target{AssignmentIDs: []uuid.UUID{d.a.ID}, StudentMemberIDs: dedupe(append(slices.Clone(d.students), d.totals...))}
	t.SpansAssignments = d.rewritesTotals()
	return t
}

// hasWork says whether anyone has started on it: a submission row of any
// kind, or a grade.
func (d deletion) hasWork() bool { return d.counts.Submissions > 0 || d.counts.Grades > 0 }

// readDeletion reads what deleting a takes with it.
func readDeletion(ctx context.Context, q dbq.Querier, courseID uuid.UUID, a dbq.GetAssignmentInCourseRow) (deletion, error) {
	d := deletion{a: a}
	work, err := q.CountAssignmentWork(ctx, a.ID)
	if err != nil {
		return d, err
	}
	d.counts = DeletionCounts{Submissions: int(work.Submissions), HandedIn: int(work.HandedIn), Drafts: int(work.Drafts),
		Missing: int(work.Missing), Grades: int(work.Grades), Posted: int(work.Posted)}
	if d.students, err = q.ListStudentsWithSubmissionsTo(ctx, a.ID); err != nil {
		return d, err
	}
	if d.owned, err = q.ListOwnedDocumentsOfAssignment(ctx, a.ID); err != nil {
		return d, err
	}
	named, err := q.ListAssignmentDocuments(ctx, a.ID)
	if err != nil {
		return d, err
	}
	for _, doc := range named {
		v := DeletionDocument{ID: doc.ID, Kind: doc.Kind, Title: doc.Title}
		if doc.Exclusive {
			d.purged = append(d.purged, v)
		} else {
			d.kept = append(d.kept, v)
		}
	}
	gone := d.gone()
	files, err := q.ListFileKeysOfDocuments(ctx, gone)
	if err != nil {
		return d, err
	}
	pdfs, err := q.RenditionKeysOfDocuments(ctx, gone)
	if err != nil {
		return d, err
	}
	d.keys = append(files, pdfs...)
	d.counts.Files, d.counts.Documents = len(files), len(d.purged)
	if d.actions, err = q.ListActionsAboutAssignment(ctx, dbq.ListActionsAboutAssignmentParams{
		AssignmentID: a.ID, CourseID: courseID, DocumentIds: gone,
	}); err != nil {
		return d, err
	}
	for _, act := range d.actions {
		if act.Status == string(domain.StatusProposed) {
			d.proposalCount++
		}
	}
	d.counts.Proposals = d.proposalCount
	if d.rewritesTotals() {
		if d.totals, err = q.ListStudentsWithLiveTotals(ctx, courseID); err != nil {
			return d, err
		}
		d.counts.Totals = len(d.totals)
	}
	return d, nil
}

// refusal is what deleting it is refused for, by the caller, an agent or
// not, who was shown confirm (nil for the preview, which is shown nothing
// yet), on an installation with a file store or none: nil when nothing
// refuses it.
func (d deletion) refusal(agent bool, confirm *DeletionCounts, store bool) *apperr.Error {
	switch {
	case agent && d.hasWork():
		return apperr.Forbid("an agent does not delete an assignment that has work or grades; a person deletes it").
			With("reason", DeletePeopleOnly)
	case confirm != nil && d.counts.above(*confirm):
		return apperr.Conflicts("more would go with the assignment than you were shown; read what goes with it again "+
			"(assignment.delete_preview) and confirm that").With("reason", DeleteConfirmStale).With("current", d.counts)
	case len(d.keys) > 0 && !store:
		return apperr.Precondition("this installation has no file storage configured, so the assignment's files cannot be removed").
			With("reason", DeleteNoFileStorage)
	}
	return nil
}

// assignmentGone is what a call naming an assignment the course does not
// have is told: that it was deleted, when and by which action, if it was
// deleted for good; not found, otherwise.
func assignmentGone(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) error {
	gone, err := q.GetAssignmentDeletion(ctx, dbq.GetAssignmentDeletionParams{AssignmentID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoAssignment
	}
	if err != nil {
		return err
	}
	return apperr.Missing("the assignment was deleted for good").With("reason", DeleteDeleted).
		With("deleted_at", gone.DeletedAt).With("by_action_id", gone.ActionID)
}

// goneIfNoRows is assignmentGone for err, from a read of the assignment
// that found it no more, or err as it is.
func goneIfNoRows(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return assignmentGone(ctx, q, courseID, id)
	}
	return err
}

// callerIsAgent reads whether actor is an agent: to refuse, never to grant,
// as member.reset_password reads it.
func callerIsAgent(ctx context.Context, q dbq.Querier, actor uuid.UUID) (bool, error) {
	a, err := q.GetActor(ctx, actor)
	if err != nil {
		return false, err
	}
	return isAgent(a.Kind), nil
}

func viewDocuments(docs []DeletionDocument) []DeletionDocument {
	if docs == nil {
		return []DeletionDocument{}
	}
	return docs
}

type AssignmentDeletePreviewOut struct {
	AssignmentID  uuid.UUID          `json:"assignment_id"`
	Title         string             `json:"title"`
	Published     bool               `json:"published" jsonschema:"students can see it, and the feed will tell them it was deleted"`
	InGrade       bool               `json:"in_grade" jsonschema:"it counts toward a component of the grading scheme"`
	Counts        DeletionCounts     `json:"counts" jsonschema:"what goes with it; send these back unchanged as confirm to assignment.delete"`
	Documents     []DeletionDocument `json:"documents" jsonschema:"its instructions and rubric, purged with it: answers that relied on them say only that a source was removed"`
	KeptDocuments []DeletionDocument `json:"kept_documents" jsonschema:"instructions or a rubric it names that something else uses, kept as they are"`
	Refusal       *string            `json:"refusal" jsonschema:"what assignment.delete would refuse right now for you (people_only, student_out_of_scope, course_archived, no_file_storage, ...), or null"`
}

func assignmentDeletePreview(d Deps) tool.Tool {
	return tool.Define(tool.Spec[AssignmentIDIn, AssignmentDeletePreviewOut]{
		Name: ToolAssignmentDeletePreview,
		Description: "What deleting an assignment for good would take with it, counted: its submissions, its grades, its " +
			"files, its instructions and rubric where nothing else uses them, the proposals about it waiting, and the " +
			"students whose totals are worked out again; never names. refusal says what assignment.delete would refuse " +
			"you right now, or is null. Send counts back unchanged as confirm to assignment.delete.",
		Kind: tool.Read, Gate: writeAssignments,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/delete-preview"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentIDIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AssignmentIDIn) (AssignmentDeletePreviewOut, error) {
			a, err := rc.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return AssignmentDeletePreviewOut{}, goneIfNoRows(ctx, rc.Q, in.CourseID, in.AssignmentID, err)
			}
			del, err := readDeletion(ctx, rc.Q, in.CourseID, a)
			if err != nil {
				return AssignmentDeletePreviewOut{}, err
			}
			out := AssignmentDeletePreviewOut{
				AssignmentID: a.ID, Title: a.Title, Published: a.PublishedAt != nil, InGrade: a.ComponentID != nil,
				Counts: del.counts, Documents: viewDocuments(del.purged), KeptDocuments: viewDocuments(del.kept),
			}
			refusal := func(reason string) { out.Refusal = &reason }
			// As assignment.delete would be refused, in its order: the
			// course, the reach, then what Validate asks.
			course, err := rc.Q.GetCourseForAuthz(ctx, in.CourseID)
			if err != nil {
				return out, err
			}
			if course.Status == domain.CourseArchived {
				refusal(string(authz.ReasonCourseArchived))
				return out, nil
			}
			reason, err := authz.CheckScope(ctx, rc.Q, rc.Member, del.scope())
			if err != nil {
				return out, err
			}
			if reason != authz.ReasonNone {
				refusal(string(reason))
				return out, nil
			}
			agent, err := callerIsAgent(ctx, rc.Q, rc.Actor.ID)
			if err != nil {
				return out, err
			}
			if e := del.refusal(agent, nil, d.Blob != nil); e != nil {
				why, _ := e.Details["reason"].(string)
				refusal(why)
			}
			return out, nil
		},
	})
}

type AssignmentDeleteIn struct {
	tool.InCourse
	AssignmentID uuid.UUID      `json:"assignment_id"`
	Confirm      DeletionCounts `json:"confirm" jsonschema:"the counts assignment.delete_preview gave, sent back unchanged: what you were shown goes, and if more would go now the call is refused (confirm_stale)"`
}

// DeletionRemoved is what went with an assignment.
type DeletionRemoved struct {
	Submissions int `json:"submissions"`
	Grades      int `json:"grades"`
	Files       int `json:"files"`
	Documents   int `json:"documents" jsonschema:"its instructions and rubric, purged"`
}

type AssignmentDeleteOut struct {
	Deleted            bool            `json:"deleted"`
	AssignmentID       uuid.UUID       `json:"assignment_id"`
	Title              string          `json:"title"`
	Removed            DeletionRemoved `json:"removed"`
	ProposalsCancelled int             `json:"proposals_cancelled"`
	Snapshots          int             `json:"snapshots" jsonschema:"how many posted totals were written down again"`
	FilesQueued        int             `json:"files_queued" jsonschema:"files queued to be deleted from storage, their PDF renditions among them, once the deletion is in"`
}

// deletionTarget is what assignment.delete is about, and what it reaches.
func deletionTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (tool.Target, error) {
	a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: id, CourseID: courseID})
	if err != nil {
		return tool.Target{}, goneIfNoRows(ctx, q, courseID, id, err)
	}
	d := deletion{a: a}
	if d.students, err = q.ListStudentsWithSubmissionsTo(ctx, id); err != nil {
		return tool.Target{}, err
	}
	if d.rewritesTotals() {
		if d.totals, err = q.ListStudentsWithLiveTotals(ctx, courseID); err != nil {
			return tool.Target{}, err
		}
	}
	return tool.Target{CourseID: courseID, Type: "assignment", ID: &id, Scope: d.scope()}, nil
}

func assignmentDelete(d Deps) tool.Tool {
	return tool.Define(tool.Spec[AssignmentDeleteIn, AssignmentDeleteOut]{
		Name: ToolAssignmentDelete,
		Description: "Delete an assignment for good. It cannot be undone: the assignment, every submission to it with its " +
			"files, and every grade given on them go; its instructions and rubric are purged where nothing else uses " +
			"them; proposals about it waiting are cancelled; and the posted totals it counted in are worked out again " +
			"without it, the change recorded. Read assignment.delete_preview first and send its counts back unchanged as " +
			"confirm: if more would go than you were shown, the call is refused (confirm_stale), and you read the preview " +
			"again. An agent deletes only an assignment nobody has started on, with no submission of any kind and no " +
			"grade (people_only); a person deletes one with work. It reaches every student whose work or total it " +
			"changes. An archived course refuses it.",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/delete"},
		Check: func(in AssignmentDeleteIn) error { return in.Confirm.check() },
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentDeleteIn) (tool.Target, error) {
			return deletionTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		// Asked before a call is carried out or a proposal queued, again as
		// the proposer when it is approved, and for its proposer's owner;
		// Execute asks it again under its locks.
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in AssignmentDeleteIn) error {
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
			}
			del, err := readDeletion(ctx, q, in.CourseID, a)
			if err != nil {
				return err
			}
			agent, err := callerIsAgent(ctx, q, m.ActorID)
			if err != nil {
				return err
			}
			if e := del.refusal(agent, &in.Confirm, d.Blob != nil); e != nil {
				return e
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AssignmentDeleteIn) (AssignmentDeleteOut, error) {
			return deleteAssignment(ctx, d, ec, in)
		},
	})
}

// deleteAssignment carries assignment.delete out, in the call's
// transaction. The locks come in one order: the caller's seat (taken by
// the pipeline), the assignment FOR UPDATE, its submissions and then the
// grades given on them in id order, every document deleted or purged with
// it in id order, and each student's totals as they are written again.
func deleteAssignment(ctx context.Context, d Deps, ec *tool.ExecCtx, in AssignmentDeleteIn) (AssignmentDeleteOut, error) {
	q := ec.Q
	locked, err := q.LockAssignmentForDelete(ctx, dbq.LockAssignmentForDeleteParams{ID: in.AssignmentID, CourseID: in.CourseID})
	if err != nil {
		return AssignmentDeleteOut{}, goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
	}
	a := dbq.GetAssignmentInCourseRow(locked)
	if _, err := q.LockSubmissionsOfAssignment(ctx, a.ID); err != nil {
		return AssignmentDeleteOut{}, err
	}
	if _, err := q.LockGradesOfAssignment(ctx, a.ID); err != nil {
		return AssignmentDeleteOut{}, err
	}
	// Which files are the work's cannot change under the locks above, nor
	// which documents the assignment names under its own; each of them is
	// locked, as a new version or a purge takes it, before anything is
	// read of what they hold.
	docs, err := q.ListOwnedDocumentsOfAssignment(ctx, a.ID)
	if err != nil {
		return AssignmentDeleteOut{}, err
	}
	for _, id := range []*uuid.UUID{a.InstructionsDocumentID, a.RubricDocumentID} {
		if id != nil {
			docs = append(docs, *id)
		}
	}
	slices.SortFunc(docs, func(x, y uuid.UUID) int { return slices.Compare(x[:], y[:]) })
	for _, id := range slices.Compact(docs) {
		if err := q.LockDocument(ctx, id); err != nil {
			return AssignmentDeleteOut{}, err
		}
	}
	del, err := readDeletion(ctx, q, in.CourseID, a)
	if err != nil {
		return AssignmentDeleteOut{}, err
	}
	agent, err := callerIsAgent(ctx, q, ec.Actor.ID)
	if err != nil {
		return AssignmentDeleteOut{}, err
	}
	if e := del.refusal(agent, &in.Confirm, d.Blob != nil); e != nil {
		return AssignmentDeleteOut{}, e
	}
	// The reach, again, over the work and the totals as they are now: some
	// may have come since the call was authorized.
	if reason, err := authz.CheckScope(ctx, q, ec.Member, del.scope()); err != nil {
		return AssignmentDeleteOut{}, err
	} else if reason != authz.ReasonNone {
		return AssignmentDeleteOut{}, apperr.Forbid("deleting it changes the work or the totals of students outside your scope").
			With("reason", string(reason))
	}

	// The record, which opens the guarded path for this assignment in this
	// transaction.
	if err := q.InsertAssignmentDeletion(ctx, dbq.InsertAssignmentDeletionParams{
		AssignmentID: a.ID, CourseID: in.CourseID, Title: a.Title, WasPublished: a.PublishedAt != nil, ActionID: ec.ActionID,
		DeletedByActorID: ec.Actor.ID, DeletedByMemberID: ec.Member.ID, DeletedAt: ec.Now,
		Submissions: int32(del.counts.Submissions), Grades: int32(del.counts.Grades), Files: int32(del.counts.Files), //nolint:gosec // counts of rows
		Documents: int32(del.counts.Documents), Proposals: int32(del.counts.Proposals), Totals: int32(del.counts.Totals), //nolint:gosec // counts of rows
	}); err != nil {
		return AssignmentDeleteOut{}, err
	}
	out := AssignmentDeleteOut{Deleted: true, AssignmentID: a.ID, Title: a.Title,
		Removed: DeletionRemoved{Submissions: del.counts.Submissions, Grades: del.counts.Grades, Files: del.counts.Files,
			Documents: del.counts.Documents}}

	// The files leave the store once this has committed, from the queue.
	if len(del.keys) > 0 {
		n, err := q.QueueBlobDeletions(ctx, dbq.QueueBlobDeletionsParams{CourseID: in.CourseID, ActionID: ec.ActionID, Now: ec.Now,
			StorageKeys: del.keys})
		if err != nil {
			return AssignmentDeleteOut{}, err
		}
		out.FilesQueued = int(n)
	}

	// What was about it: the proposals waiting are cancelled, each telling
	// its proposer as any cancellation does; then all of it is emptied.
	redact := make([]uuid.UUID, 0, len(del.actions))
	for _, act := range del.actions {
		redact = append(redact, act.ID)
		if act.Status != string(domain.StatusProposed) {
			continue
		}
		_, stored := pipeline.Cancellation(pipeline.CancelTargetDeleted, map[string]any{"by_action_id": ec.ActionID})
		n, err := q.CancelProposal(ctx, dbq.CancelProposalParams{ID: act.ID, Result: stored})
		if err != nil {
			return AssignmentDeleteOut{}, err
		}
		if n == 0 {
			continue // decided just now, as its lock let go
		}
		out.ProposalsCancelled++
		id := act.ID
		ec.Emit(events.Event{Type: events.ActionCancelled, CourseID: act.CourseID, ActionID: &id, SubjectType: "action", SubjectID: &id,
			Payload: map[string]any{"action_type": act.ActionType, "reason": pipeline.CancelTargetDeleted, "by_action_id": ec.ActionID}})
	}
	if len(redact) > 0 {
		if _, err := q.RedactActions(ctx, dbq.RedactActionsParams{ByActionID: ec.ActionID, Ids: redact}); err != nil {
			return AssignmentDeleteOut{}, err
		}
	}

	// The submitted and feedback files: each version purged, which takes
	// its files and their renditions, and then the versions and the
	// documents themselves.
	reason := PurgedWithAssignment
	purge := func(doc uuid.UUID) error {
		versions, err := q.ListVersionsToPurge(ctx, doc)
		if err != nil {
			return err
		}
		for _, v := range versions {
			if _, err := q.PurgeVersion(ctx, dbq.PurgeVersionParams{ID: v.ID, PurgedAt: &ec.Now, PurgedByActorID: &ec.Actor.ID,
				PurgeReason: &reason}); err != nil {
				return err
			}
		}
		return nil
	}
	if len(del.owned) > 0 {
		for _, doc := range del.owned {
			if err := purge(doc); err != nil {
				return AssignmentDeleteOut{}, err
			}
		}
		if err := q.ClearPublishedVersions(ctx, del.owned); err != nil {
			return AssignmentDeleteOut{}, err
		}
		if _, err := q.DeleteDocumentVersions(ctx, del.owned); err != nil {
			return AssignmentDeleteOut{}, err
		}
		if _, err := q.DeleteDocuments(ctx, del.owned); err != nil {
			return AssignmentDeleteOut{}, err
		}
	}
	if _, err := q.DeleteGradesOfAssignment(ctx, a.ID); err != nil {
		return AssignmentDeleteOut{}, err
	}
	if _, err := q.DeleteSubmissionsOfAssignment(ctx, a.ID); err != nil {
		return AssignmentDeleteOut{}, err
	}
	if _, err := q.DeleteAssignmentScopes(ctx, a.ID); err != nil {
		return AssignmentDeleteOut{}, err
	}
	if _, err := q.DeleteEventsOfAssignment(ctx, dbq.DeleteEventsOfAssignmentParams{CourseID: &in.CourseID, AssignmentID: &a.ID}); err != nil {
		return AssignmentDeleteOut{}, err
	}
	// Its instructions and rubric, where nothing else uses them, purged as
	// document.purge purges: what pointed at them reads the tombstone.
	for _, doc := range del.purged {
		if err := purge(doc.ID); err != nil {
			return AssignmentDeleteOut{}, err
		}
		if _, err := q.PurgeDocument(ctx, dbq.PurgeDocumentParams{ID: doc.ID, PurgedAt: &ec.Now, PurgedByActorID: &ec.Actor.ID,
			PurgeReason: &reason}); err != nil {
			return AssignmentDeleteOut{}, err
		}
	}
	if n, err := q.DeleteAssignment(ctx, dbq.DeleteAssignmentParams{ID: a.ID, CourseID: in.CourseID}); err != nil {
		return AssignmentDeleteOut{}, err
	} else if n == 0 {
		return AssignmentDeleteOut{}, assignmentGone(ctx, q, in.CourseID, a.ID)
	}

	// The totals it counted in, worked out again without it, as a change to
	// the scheme works them out; those superseded keep their number, and
	// the line of their working that was its says it was deleted.
	if del.rewritesTotals() {
		if out.Snapshots, err = rewriteTotals(ctx, ec, in.CourseID, nil, *a.ComponentID); err != nil {
			return AssignmentDeleteOut{}, err
		}
		if _, err := q.RedactTotalsLine(ctx, dbq.RedactTotalsLineParams{CourseID: in.CourseID, AssignmentID: a.ID}); err != nil {
			return AssignmentDeleteOut{}, err
		}
	}

	typ := EventAssignmentDeleted
	if a.PublishedAt == nil {
		typ = EventAssignmentDeletedUnreleased
	}
	ec.Emit(events.Event{Type: typ, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID,
		Payload: map[string]any{"title": a.Title, "purged_document_ids": del.purgedIDs()}})
	return out, nil
}

// errWorkDeleted is what a call is told of a submission or a grade it found
// a moment ago and finds no more: it went with its assignment, deleted for
// good (assignment.delete), the one way either goes.
var errWorkDeleted = apperr.Missing("the work this call is about was deleted with its assignment just now").With("reason", DeleteDeleted)

// workGone is errWorkDeleted for err, from a read of a submission or a grade
// that found it no more, or err as it is.
func workGone(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errWorkDeleted
	}
	return err
}
