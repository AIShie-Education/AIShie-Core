package tools_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

func (b *built) events(t *testing.T, typ string, subject uuid.UUID) int {
	t.Helper()
	return b.Count(`SELECT count(*) FROM event WHERE type = $1 AND subject_id = $2`, typ, subject)
}

// A document is renamed, or moved in its list, by whoever may write that
// kind of document, and an archived one is brought back.
func TestADocumentIsRenamedAndBroughtBack(t *testing.T) {
	b := build(t)
	doc := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Lecture 1", "body_md": "Loops."})).DocumentID
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	update := func(args m) m {
		args["course_id"], args["document_id"] = b.course, doc
		return args
	}

	if !testkit.Result[tools.DocumentChangeOut](t, b.do(t, b.sato, "document.update", update(m{"title": "Lecture 1: Loops"}))).Changed {
		t.Fatal("the rename says it changed nothing")
	}
	if got := b.get(t, b.yuki, m{"document_id": doc}); got.Title != "Lecture 1: Loops" || got.Version == nil || *got.Version.BodyMD != "Loops." {
		t.Fatalf("Yuki reads %+v", got)
	}
	if b.events(t, "document.updated", doc) != 1 {
		t.Fatal("no document.updated event")
	}
	if testkit.Result[tools.DocumentChangeOut](t, b.do(t, b.sato, "document.update", update(m{"title": "Lecture 1: Loops", "sort_order": 0}))).Changed {
		t.Fatal("the same title and place again was a change")
	}
	b.do(t, b.sato, "document.update", update(m{"sort_order": 3}))
	if got := b.get(t, b.sato, m{"document_id": doc}); got.SortOrder != 3 || got.Title != "Lecture 1: Loops" {
		t.Fatalf("moving it in the list: %+v", got)
	}
	if out := b.MustCall(b.yuki, "document.update", update(m{"title": "Mine"}), "yuki"); out.Status != domain.StatusDenied {
		t.Fatalf("a student renaming the course's material: %+v", out)
	}
	b.try(t, b.sato, "document.update", update(m{"title": " "}), apperr.InvalidArgument)
	b.try(t, b.sato, "document.update", update(m{}), apperr.InvalidArgument)

	// Archived, it is withdrawn; brought back, it is read again.
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": doc})
	if _, err := b.Call(b.yuki, "document.get", m{"course_id": b.course, "document_id": doc}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("an archived document read by a student: %v", err)
	}
	b.do(t, b.sato, "document.update", update(m{"title": "Lecture 1 (old)"})) // renamed while archived
	if out := b.MustCall(b.yuki, "document.unarchive", m{"course_id": b.course, "document_id": doc}, "yuki-back"); out.Status != domain.StatusDenied {
		t.Fatalf("a student bringing material back: %+v", out)
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": doc})
	if got := b.get(t, b.yuki, m{"document_id": doc}); got.Status != "active" || got.Version == nil {
		t.Fatalf("brought back, Yuki reads %+v", got)
	}
	if b.events(t, "document.unarchived", doc) != 1 {
		t.Fatal("no document.unarchived event")
	}
	b.try(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": doc}, apperr.Conflict)
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc, "body_md": "Loops, again."})

	// Instructions are their assignment's, and so is news of renaming them.
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW9 brief", "body_md": "Write."})).DocumentID
	b.do(t, b.sato, "document.update", m{"course_id": b.course, "document_id": brief, "title": "HW9: the brief"})
	if b.events(t, "document.updated_unreleased", brief) != 1 || b.events(t, "document.updated", brief) != 0 {
		t.Fatal("renaming instructions no published assignment uses is not news for those who write assignments alone")
	}
	for _, e := range feed(t, b, b.yuki) {
		if e.SubjectID != nil && *e.SubjectID == brief {
			t.Fatalf("Yuki learns of next week's brief: %+v", e)
		}
	}
}

// A submitted file is renamed, archived and brought back only while its
// submission is a draft: handed in, its files are frozen with it. Feedback
// on a posted grade is renamed or brought back only by whoever may post.
func TestOwnedFilesAreRenamedAsTheirOwnersAllow(t *testing.T) {
	b := build(t)
	sub := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "essay"})).SubmissionID
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create",
		m{"course_id": b.course, "kind": "submission", "title": "essay.md", "submission_id": sub, "body_md": "My essay."})).DocumentID
	b.do(t, b.yuki, "document.update", m{"course_id": b.course, "document_id": file, "title": "final-essay.md"})
	b.do(t, b.yuki, "document.archive", m{"course_id": b.course, "document_id": file})
	b.do(t, b.yuki, "document.unarchive", m{"course_id": b.course, "document_id": file})
	if b.events(t, "submission.file_updated", file) != 1 || b.events(t, "submission.file_unarchived", file) != 1 {
		t.Fatal("news of a submitted file goes out under its owner's names")
	}
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": sub})
	b.try(t, b.yuki, "document.update", m{"course_id": b.course, "document_id": file, "title": "sneaky.md"}, apperr.Conflict)
	b.try(t, b.sato, "document.update", m{"course_id": b.course, "document_id": file, "title": "renamed-by-sato.md"}, apperr.Conflict)
	if got := b.get(t, b.sato, m{"document_id": file}); got.Title != "final-essay.md" {
		t.Fatalf("a handed-in file was renamed: %q", got.Title)
	}

	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": sub, "score": 80})).GradeID
	note := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "feedback", "title": "notes", "grade_id": g, "body_md": "Good."})).DocumentID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{g}})
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": note})
	ta := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "TA"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ta, "preset": "ta"}) // grades, does not post
	for name, args := range map[string]m{
		"document.update":    {"course_id": b.course, "document_id": note, "title": "TA's notes"},
		"document.unarchive": {"course_id": b.course, "document_id": note},
	} {
		if out := b.MustCall(ta, name, args, "ta-"+name); out.Status != domain.StatusDenied {
			t.Fatalf("%s on posted feedback by someone who cannot post: %+v", name, out)
		}
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": note})
	if got := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": g})); len(got.FeedbackFiles) != 1 {
		t.Fatal("feedback brought back is not released again")
	}
}

// An administrator purges what was uploaded by mistake: the file is deleted
// from storage and its text is gone, and a tombstone says who removed it,
// when and why. Work handed in under a purged version still names it, and
// reads the tombstone.
func TestAPurgeLeavesATombstone(t *testing.T) {
	b := build(t)
	token := b.upload(t, b.sato, "instructions", "application/pdf", []byte("%PDF the brief, with the class list"))
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW5 brief", "files": oneFile(token)}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief.DocumentID})
	hw5 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW5",
		"points_possible": 10, "instructions_document_id": brief.DocumentID})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw5})
	sub := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": hw5, "body": "done"})).SubmissionID
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": sub})
	v1 := *brief.VersionID
	fixed := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": brief.DocumentID, "body_md": "The brief.", "publish": true}))
	var key string
	if err := b.Pool.QueryRow(t.Context(), `SELECT storage_key FROM document_version_file WHERE version_id = $1 AND position = 1`, v1).Scan(&key); err != nil {
		t.Fatal(err)
	}
	purge := func(args m) m {
		args["course_id"], args["document_id"] = b.course, brief.DocumentID
		return args
	}

	// Not an instructor's: removing data is done from outside the course.
	if out := b.MustCall(b.sato, "document.purge", purge(m{"version_id": v1, "reason": "x"}), "sato"); reason(out) != "platform_role_required" {
		t.Fatalf("an instructor purging: %+v", out)
	}
	b.try(t, b.admin, "document.purge", purge(m{"version_id": v1, "reason": "  "}), apperr.InvalidArgument)

	out := b.do(t, b.admin, "document.purge", purge(m{"version_id": v1, "reason": "The class list was attached by mistake."}))
	if res := testkit.Result[tools.DocumentPurgeOut](t, out); res.PurgedVersions != 1 || res.FilesRemoved != 1 {
		t.Fatalf("purge: %+v", res)
	}
	if _, err := b.Blob.Stat(t.Context(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the file is still in storage: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM document_version v WHERE id = $1 AND body_md IS NULL AND purged_by_actor_id = $2
		AND purge_reason = 'The class list was attached by mistake.'
		AND NOT EXISTS (SELECT 1 FROM document_version_file f WHERE f.version_id = v.id)`, v1, b.admin); n != 1 {
		t.Fatal("no tombstone where the version was")
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND action_type = 'document.purge' AND authority = 'platform' AND course_id = $2`,
		*out.ActionID, b.course); n != 1 {
		t.Fatal("the purge is not on record as an administrator's")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'document.purged' AND subject_id = $1 AND assignment_id = $2`, brief.DocumentID, hw5); n != 1 {
		t.Fatal("no document.purged event, filed under the assignment")
	}
	// Yuki's work still names the version it was handed in under, and she
	// reads what became of it.
	mine := testkit.Result[tools.SubmissionView](t, b.do(t, b.yuki, "submission.get", m{"course_id": b.course, "submission_id": sub}))
	if mine.InstructionsVersionID == nil || *mine.InstructionsVersionID != v1 {
		t.Fatalf("the submission no longer names its instructions: %+v", mine)
	}
	got := b.get(t, b.yuki, m{"document_id": brief.DocumentID, "version_id": v1})
	if got.Version == nil || got.Version.Purged == nil || got.Version.Purged.ByActorID != b.admin ||
		len(got.Version.Files) != 0 || got.Version.BodyMD != nil {
		t.Fatalf("Yuki reads the purged version as %+v", got.Version)
	}
	versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions", m{"course_id": b.course, "document_id": brief.DocumentID}))
	if versions.Versions[0].PurgedAt == nil || len(versions.Versions[0].Files) != 0 || versions.Versions[1].PurgedAt != nil {
		t.Fatalf("the version list: %+v", versions.Versions)
	}
	// Once is enough; there is nothing in it to publish.
	if out := b.MustCall(b.admin, "document.purge", purge(m{"version_id": v1, "reason": "again"}), "again"); reason(out) != "already_purged" {
		t.Fatalf("purging a version twice: %+v", out)
	}
	if out := b.MustCall(b.sato, "document.publish", m{"course_id": b.course, "document_id": brief.DocumentID, "version_id": v1}, "republish"); reason(out) != "purged" {
		t.Fatalf("publishing a purged version: %+v", out)
	}
	if got := b.get(t, b.yuki, m{"document_id": brief.DocumentID}); got.Version == nil || got.Version.ID != fixed.VersionID {
		t.Fatalf("the published version: %+v", got.Version)
	}

	// Submitted and feedback files are their owners', and not purged.
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create",
		m{"course_id": b.course, "kind": "submission", "title": "draft.md", "submission_id": testkit.Result[tools.SubmissionCreateOut](t,
			b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "x"})).SubmissionID, "body_md": "x"})).DocumentID
	if out := b.MustCall(b.admin, "document.purge", m{"course_id": b.course, "document_id": file, "reason": "x"}, "owned"); reason(out) != "owned_file" {
		t.Fatalf("purging a submitted file: %+v", out)
	}
}

// A whole document purged is archived for good: nothing is brought back or
// added, though it may be renamed. A department's administrator purges in
// the courses of the departments they administer, and nowhere else; and it
// is done in an archived course as well.
func TestAPurgedDocumentStaysArchived(t *testing.T) {
	b := build(t)
	notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Grades of 2025 (Yuki, Ken, ...)", "body_md": "Yuki 71, Ken 64."})).DocumentID
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": notes,
		"files": oneFile(b.upload(t, b.sato, "material", "text/csv", []byte("Yuki,71\nKen,64\n"))), "publish": true})

	// A department administrator: of Computer Science, whose course this is,
	// and of another department, whose it is not.
	ada := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Ada"})).ActorID
	bob := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Bob"})).ActorID
	other := testkit.Result[tools.IDOut](t, b.do(t, b.admin, "department.create", m{"name": "Physics"})).ID
	b.do(t, b.admin, "department.add_admin", m{"dept_id": b.dept, "actor_id": ada})
	b.do(t, b.admin, "department.add_admin", m{"dept_id": other, "actor_id": bob})
	whole := m{"course_id": b.course, "document_id": notes, "reason": "Other students' marks."}
	if out := b.MustCall(bob, "document.purge", whole, "bob"); reason(out) != "department_out_of_scope" {
		t.Fatalf("a department administrator elsewhere: %+v", out)
	}
	// In an archived course too: what must go must go.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	out := b.do(t, ada, "document.purge", whole)
	if res := testkit.Result[tools.DocumentPurgeOut](t, out); res.PurgedVersions != 2 || res.FilesRemoved != 1 {
		t.Fatalf("purging the whole document: %+v", res)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND authority = 'department' AND authority_dept_id = $2`, *out.ActionID, b.dept); n != 1 {
		t.Fatal("the purge is not on record in the department administrator's capacity")
	}
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})
	if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND status = 'archived' AND purged_by_actor_id = $2`, notes, ada); n != 1 {
		t.Fatal("the document is not archived with its tombstone")
	}
	if n := b.Count(`SELECT count(*) FROM document_version WHERE document_id = $1 AND purged_at IS NULL`, notes); n != 0 {
		t.Fatal("a version of the purged document kept its content")
	}
	if out := b.MustCall(b.sato, "document.unarchive", m{"course_id": b.course, "document_id": notes}, "back"); reason(out) != "purged" {
		t.Fatalf("bringing a purged document back: %+v", out)
	}
	b.try(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": notes, "body_md": "again"}, apperr.Conflict)
	b.do(t, b.sato, "document.update", m{"course_id": b.course, "document_id": notes, "title": "Removed"})
	if out := b.MustCall(ada, "document.purge", whole, "twice"); reason(out) != "already_purged" {
		t.Fatalf("purging a purged document: %+v", out)
	}
	got := b.get(t, b.sato, m{"document_id": notes})
	if got.Purged == nil || got.Purged.ByActorID != ada || got.PurgedAt == nil || got.Title != "Removed" {
		t.Fatalf("the purged document reads %+v", got)
	}
	list := testkit.Result[tools.DocumentListOut](t, b.do(t, b.sato, "document.list", m{"course_id": b.course, "include_archived": true}))
	if !slices.ContainsFunc(list.Documents, func(d tools.DocumentSummary) bool { return d.ID == notes && d.PurgedAt != nil }) {
		t.Fatal("the list does not say the document was purged")
	}
}
