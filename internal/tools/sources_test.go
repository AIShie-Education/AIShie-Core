package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// What an answer relied on (docs/schema.md §2.8, Sources of an answer): the
// respondent names the course materials it read, each one it may read as it
// answers; whoever reads the answer is shown each as they may read it now.

// sourced answers question in conversation as the tutor, naming sources.
func (b *built) sourced(t *testing.T, conversation, question uuid.UUID, sources []m) uuid.UUID {
	t.Helper()
	args := answerArgs(b, conversation, question, "Loops repeat a block; see the slides.")
	args["sources"] = sources
	return testkit.Result[tools.MessageIDOut](t, b.do(t, b.tutor, "conversation.answer", args)).MessageID
}

// sourcesOf are the sources of a message, as actor reads the conversation.
func (b *built) sourcesOf(t *testing.T, actor, conversation, message uuid.UUID) []tools.SourceView {
	t.Helper()
	for _, msg := range b.messages(t, actor, conversation) {
		if msg.ID == message {
			return msg.Sources
		}
	}
	t.Fatalf("message %s is not among those %s reads", message, actor)
	return nil
}

func sameID(a *uuid.UUID, b uuid.UUID) bool { return a != nil && *a == b }

func TestAnAnswerKeepsWhatItReliedOnForEachReader(t *testing.T) {
	b := build(t)
	lecture := b.lecture(t) // Week 3: Loops, published, three files
	v1 := *lecture.VersionID
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a loop?")
	page, part := 2, 1
	answer := b.sourced(t, conv, q, []m{
		{"document_id": lecture.DocumentID, "version_id": v1, "file_id": lecture.FileIDs[0], "page": page, "part": part},
		{"document_id": lecture.DocumentID, "version_id": v1},
	})
	if n := b.Count(`SELECT count(*) FROM conversation_message_source WHERE message_id = $1`, answer); n != 2 {
		t.Fatalf("%d sources kept, want 2", n)
	}

	// Yuki, who asked, and Sato, who oversees her, read both, as they are.
	for who, reader := range map[string]uuid.UUID{"Yuki": b.yuki, "Sato": b.sato} {
		got := b.sourcesOf(t, reader, conv, answer)
		if len(got) != 2 {
			t.Fatalf("%s reads %d sources: %+v", who, len(got), got)
		}
		first, second := got[0], got[1]
		if first.Restricted || first.OtherVersion || !sameID(first.DocumentID, lecture.DocumentID) || !sameID(first.VersionID, v1) ||
			!sameID(first.FileID, lecture.FileIDs[0]) || first.Filename == nil || *first.Filename != "week3-slides.pdf" ||
			first.Title == nil || *first.Title != "Week 3: Loops" || first.Kind == nil || *first.Kind != "material" ||
			first.Published == nil || !*first.Published || first.Seq == nil || *first.Seq != 1 ||
			first.Page == nil || *first.Page != 2 || first.Part == nil || *first.Part != 1 || first.Slide != nil {
			t.Fatalf("%s reads the first source as %+v", who, first)
		}
		if second.Restricted || !sameID(second.VersionID, v1) || second.FileID != nil || second.Page != nil {
			t.Fatalf("%s reads the second source as %+v", who, second)
		}
	}
	// The question names none; nor does an answer that relied on nothing.
	plain := b.ask(t, b.yuki, conv, "And a while loop?")
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, conv, plain, "The same, until."))
	for _, msg := range b.messages(t, b.yuki, conv) {
		if msg.ID != answer && msg.Sources != nil {
			t.Fatalf("message %d names sources: %+v", msg.Seq, msg.Sources)
		}
	}
	// Ken, another student, reads nothing of Yuki's conversation, its
	// sources with it.
	if _, err := b.Call(b.ken, "conversation.messages", m{"course_id": b.course, "conversation_id": conv}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("Ken reading Yuki's conversation: %v", err)
	}

	// Renamed, it is shown by its name now.
	b.do(t, b.sato, "document.update", m{"course_id": b.course, "document_id": lecture.DocumentID, "title": "Week 3: Loops and ranges"})
	if got := b.sourcesOf(t, b.yuki, conv, answer)[0]; got.Title == nil || *got.Title != "Week 3: Loops and ranges" {
		t.Fatalf("after the rename: %+v", got)
	}

	// A new version published: Yuki may open the lecture, but not the
	// version the answer read; she is told which document it was, and
	// nothing of the version. Sato, who reads drafts, reads it all.
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": lecture.DocumentID, "body_md": "Loops, rewritten."}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": lecture.DocumentID, "version_id": v2.VersionID})
	got := b.sourcesOf(t, b.yuki, conv, answer)[0]
	if !got.OtherVersion || got.Restricted || !sameID(got.DocumentID, lecture.DocumentID) || got.Title == nil ||
		got.VersionID != nil || got.FileID != nil || got.Filename != nil || got.Page != nil || got.Part != nil || got.Seq != nil {
		t.Fatalf("Yuki, the version replaced: %+v", got)
	}
	if got := b.sourcesOf(t, b.sato, conv, answer)[0]; got.OtherVersion || !sameID(got.VersionID, v1) || got.Published == nil || *got.Published {
		t.Fatalf("Sato, the version replaced: %+v", got)
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": lecture.DocumentID, "version_id": v1})

	// Archived, the lecture is withdrawn from Yuki: she is told the answer
	// relied on something, and not what. Sato still reads it.
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": lecture.DocumentID})
	for _, s := range b.sourcesOf(t, b.yuki, conv, answer) {
		if s != (tools.SourceView{Restricted: true}) {
			t.Fatalf("Yuki, the lecture archived: %+v", s)
		}
	}
	if got := b.sourcesOf(t, b.sato, conv, answer)[0]; got.Restricted || !sameID(got.VersionID, v1) {
		t.Fatalf("Sato, the lecture archived: %+v", got)
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": lecture.DocumentID})
	if got := b.sourcesOf(t, b.yuki, conv, answer)[0]; got.Restricted || !sameID(got.VersionID, v1) {
		t.Fatalf("Yuki, the lecture back: %+v", got)
	}

	// Retracted, the answer keeps its sources from its readers, as its text.
	b.do(t, b.tutor, "conversation.retract", m{"course_id": b.course, "message_id": answer})
	if got := b.sourcesOf(t, b.sato, conv, answer); got != nil {
		t.Fatalf("a retracted answer's sources: %+v", got)
	}
}

// A purged version's source is restricted to every reader, staff too: what
// it said is gone, and so is anything that would say what it was. The
// source is kept, and names no file.
func TestAPurgedVersionsSourceIsRestrictedToEveryone(t *testing.T) {
	b := build(t)
	lecture := b.lecture(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a loop?")
	answer := b.sourced(t, conv, q, []m{{"document_id": lecture.DocumentID, "version_id": lecture.VersionID, "file_id": lecture.FileIDs[2], "page": 1}})
	b.do(t, b.admin, "document.purge", m{"course_id": b.course, "document_id": lecture.DocumentID, "version_id": lecture.VersionID,
		"reason": "A student's name was in the program."})
	for who, reader := range map[string]uuid.UUID{"Yuki": b.yuki, "Sato": b.sato} {
		if got := b.sourcesOf(t, reader, conv, answer); len(got) != 1 || got[0] != (tools.SourceView{Restricted: true}) {
			t.Fatalf("%s, the version purged: %+v", who, got)
		}
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message_source WHERE message_id = $1 AND file_id IS NULL AND page = 1`, answer); n != 1 {
		t.Fatal("the purged version's source is not kept, naming no file")
	}
	// And a new answer relies on it no more.
	q2 := b.ask(t, b.yuki, conv, "Again?")
	args := answerArgs(b, conv, q2, "Again.")
	args["sources"] = []m{{"document_id": lecture.DocumentID, "version_id": lecture.VersionID}}
	b.refusedAs(t, b.tutor, "conversation.answer", args, apperr.InvalidArgument, "source_purged")
}

// What an answer names is checked before anything is recorded: what its
// sources say alone as the call is read, and whether the respondent may
// read each before the answer is posted or proposed, and again when a
// proposal is approved. The refusal says which source.
func TestAnAnswersSourcesAreCheckedBeforeAnythingIsRecorded(t *testing.T) {
	b := build(t)
	lecture := b.lecture(t)
	v1 := *lecture.VersionID
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a loop?")
	good := m{"document_id": lecture.DocumentID, "version_id": v1}
	// A draft nobody has published, which the tutor may not read.
	draft := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Week 4", "body_md": "Not yet."}))
	// Yuki's essay: her work, not the course's material.
	yukiDraft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create", m{"course_id": b.course,
		"kind": "submission", "title": "essay.md", "submission_id": yukiDraft, "body_md": "My essay."}))
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": lecture.DocumentID, "body_md": "Loops, rewritten."}))

	many := make([]m, 21)
	for i := range many {
		many[i] = m{"document_id": lecture.DocumentID, "version_id": v1, "file_id": lecture.FileIDs[0], "page": i + 1}
	}
	cases := []struct {
		name    string
		sources []m
		code    apperr.Code
		reason  string
		field   string
	}{
		{"more than 20", many, apperr.InvalidArgument, "too_many_sources", "sources"},
		{"no version", []m{{"document_id": lecture.DocumentID, "version_id": uuid.Nil}}, apperr.InvalidArgument, "bad_source", "sources[0]"},
		{"a page of no file", []m{good, {"document_id": lecture.DocumentID, "version_id": v1, "page": 3}},
			apperr.InvalidArgument, "bad_source", "sources[1]"},
		{"a page and a slide", []m{{"document_id": lecture.DocumentID, "version_id": v1, "file_id": lecture.FileIDs[0], "page": 1, "slide": 1}},
			apperr.InvalidArgument, "bad_source", "sources[0]"},
		{"a page before the first", []m{{"document_id": lecture.DocumentID, "version_id": v1, "file_id": lecture.FileIDs[0], "page": 0}},
			apperr.InvalidArgument, "bad_source", "sources[0]"},
		{"the same twice", []m{good, {"document_id": lecture.DocumentID, "version_id": v1, "file_id": lecture.FileIDs[0]}, good},
			apperr.InvalidArgument, "duplicate_source", "sources[2]"},
		{"a draft the tutor may not read", []m{good, {"document_id": draft.DocumentID, "version_id": draft.VersionID}},
			apperr.InvalidArgument, "source_unreadable", "sources[1]"},
		{"a version not published", []m{{"document_id": lecture.DocumentID, "version_id": v2.VersionID}},
			apperr.InvalidArgument, "source_unreadable", "sources[0]"},
		{"a version of another document", []m{{"document_id": draft.DocumentID, "version_id": v1}},
			apperr.InvalidArgument, "source_unreadable", "sources[0]"},
		{"a document of no course", []m{{"document_id": uuid.New(), "version_id": v1}},
			apperr.InvalidArgument, "source_unreadable", "sources[0]"},
		{"a file of another version", []m{{"document_id": lecture.DocumentID, "version_id": v1, "file_id": uuid.New()}},
			apperr.InvalidArgument, "source_unreadable", "sources[0]"},
		{"a student's work", []m{good, {"document_id": essay.DocumentID, "version_id": essay.VersionID}},
			apperr.InvalidArgument, "source_unreadable", "sources[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := answerArgs(b, conv, q, "Loops repeat.")
			args["sources"] = tc.sources
			key := "sources-" + uuid.NewString()
			out, err := b.Call(b.tutor, "conversation.answer", args, key)
			e, ok := apperr.As(err)
			if !ok && out.Error != nil {
				e, ok = out.Error, true
			}
			if !ok || e.Code != tc.code || e.Details["reason"] != tc.reason || e.Details["field"] != tc.field {
				t.Fatalf("%+v %v, want %s %s at %s", out, err, tc.code, tc.reason, tc.field)
			}
			if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
				t.Fatal("the answer was posted")
			}
		})
	}

	// At confirm_required, a source the tutor may not read is refused
	// before a proposal is queued; one it may is proposed, and refused when
	// approved once the lecture is archived, as the tutor would be.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	args := answerArgs(b, conv, q, "Loops repeat.")
	args["sources"] = []m{{"document_id": draft.DocumentID, "version_id": draft.VersionID}}
	b.refusedAs(t, b.tutor, "conversation.answer", args, apperr.InvalidArgument, "source_unreadable")
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.answer' AND status = 'proposed'`); n != 0 {
		t.Fatalf("%d answers proposed naming a source the tutor may not read", n)
	}
	args["sources"] = []m{good}
	proposed := b.MustCall(b.tutor, "conversation.answer", args, "readable-proposal")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the answer, proposed: %+v", proposed)
	}
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": lecture.DocumentID})
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed || d.Error == nil || d.Error.Details["reason"] != "source_unreadable" {
		t.Fatalf("approving an answer whose source was archived since: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatal("the answer was posted")
	}
}

// An answer from a runtime that names no sources, as every one did before
// they were kept, is taken as it always was, and read with none.
func TestAnAnswerNamingNoSourcesIsAsBefore(t *testing.T) {
	b := build(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a loop?")
	for _, sources := range []any{nil, []m{}} {
		args := answerArgs(b, conv, q, "A block repeated.")
		if sources != nil {
			args["sources"] = sources
		}
		answer := testkit.Result[tools.MessageIDOut](t, b.do(t, b.tutor, "conversation.answer", args)).MessageID
		if got := b.sourcesOf(t, b.yuki, conv, answer); got != nil {
			t.Fatalf("an answer naming none reads %+v", got)
		}
		q = b.ask(t, b.yuki, conv, "Again?")
	}
}

// An export for audit holds every answer's sources, whoever may read them
// now, by their ids, with their documents' titles as they are now; and a
// proposed answer's as it named them.
func TestAnExportHoldsWhatEachAnswerReliedOn(t *testing.T) {
	a := newAudit(t)
	b := a.built
	lecture := b.lecture(t)
	conv, q := b.open(t, b.yuki, b.tutorM, "What is a loop?")
	posted := b.sourced(t, conv, q, []m{{"document_id": lecture.DocumentID, "version_id": lecture.VersionID,
		"file_id": lecture.FileIDs[0], "slide": 4}})
	waits, asked := b.open(t, b.yuki, a.courseTutorM, "And a range?")
	args := answerArgs(b, waits, asked, "A run of numbers.")
	args["sources"] = []m{{"document_id": lecture.DocumentID, "version_id": lecture.VersionID}}
	if out := b.MustCall(a.courseTutor, "conversation.answer", args, "proposed-with-sources"); out.Status != domain.StatusProposed {
		t.Fatalf("the course tutor's answer: %+v", out)
	}
	b.do(t, b.sato, "document.update", m{"course_id": b.course, "document_id": lecture.DocumentID, "title": "Week 3, again"})

	out, _ := a.export(t, a.Root, m{"course_id": a.course})
	x := a.files(t, out)
	var answer map[string]any
	for _, msg := range list(x.byID[conv.String()]["messages"]) {
		if obj(msg)["id"] == posted.String() {
			answer = obj(msg)
		}
	}
	sources := list(answer["sources"])
	if len(sources) != 1 {
		t.Fatalf("the answer's sources, exported: %v", answer)
	}
	s := obj(sources[0])
	if s["document_id"] != lecture.DocumentID.String() || s["version_id"] != lecture.VersionID.String() ||
		s["file_id"] != lecture.FileIDs[0].String() || s["filename"] != "week3-slides.pdf" || s["slide"] != 4.0 || s["page"] != nil ||
		s["title"] != "Week 3, again" || s["kind"] != "material" || s["version_seq"] != 1.0 || s["purged"] != false {
		t.Fatalf("the answer's source, exported: %v", s)
	}
	for _, msg := range list(x.byID[a.c1.String()]["messages"]) {
		if got, ok := obj(msg)["sources"].([]any); !ok || len(got) != 0 {
			t.Fatalf("a message naming none exports sources %v", obj(msg)["sources"])
		}
	}
	proposals := list(x.byID[waits.String()]["proposals"])
	if len(proposals) != 1 {
		t.Fatalf("the proposed answer, exported: %v", x.byID[waits.String()])
	}
	named := list(obj(proposals[0])["sources"])
	if len(named) != 1 || obj(named[0])["document_id"] != lecture.DocumentID.String() || obj(named[0])["title"] != nil {
		t.Fatalf("the proposed answer's sources, exported: %v", named)
	}
}
