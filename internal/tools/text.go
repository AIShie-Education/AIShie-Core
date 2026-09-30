package tools

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Text versions (docs/schema.md §2.4, Text versions). A version with a file
// of a course's material, instructions or rubric has a text version: its
// file transcribed into Markdown by the site's service (source ai), or
// written by the course's staff (source staff), which no transcription
// writes over. It is queued as the version is added, by the database, and
// deleted with the file when the version is purged. Whoever may read the
// version reads its text, and nobody else: document.get and document.versions
// say where it stands, document.text reads it, in parts. Whoever may write
// the document edits it (document.text_update) and sends it back to be
// transcribed again (document.text_retranscribe), each an action like any
// other write of a document's.

func textTools(d Deps) []tool.Tool {
	return []tool.Tool{documentText(), documentTextUpdate(), documentTextRetranscribe(),
		textQueue(d), textFile(d), textRenew(), textComplete()}
}

const (
	// MaxTextBytes bounds a text version: 2 MiB of Markdown, several
	// hundred pages of dense text. The database holds the same
	// (document_version_text_body_size).
	MaxTextBytes = 2 << 20
	// TextPartBytes is the most of a text one read of it gives
	// (document.text): whole pages, where they fit.
	TextPartBytes = 64 << 10
	// textRequestBytes is the most a REST request carrying a whole text may
	// be: the text, written as JSON, and the rest of the call.
	textRequestBytes = 5 << 20

	textPending, textWorking, textDone, textFailed, textSkipped = "pending", "working", "done", "failed", "skipped"
	textByAI, textByStaff                                       = "ai", "staff"

	// News of a text version, told as a version's news is: of the published
	// version, to whoever reads that kind of document; of any other, to
	// whoever reads drafts; and for instructions or a rubric, filed under
	// each published assignment that refers to it, or unreleased while none
	// does (emitDocumentEvent).
	EventTextUpdated                 = "document.text_updated"
	EventRubricTextUpdated           = "document.rubric_text_updated"
	EventDraftTextUpdated            = "document.draft_text_updated"
	EventTextUpdatedUnreleased       = "document.text_updated_unreleased"
	EventRubricTextUpdatedUnreleased = "document.rubric_text_updated_unreleased"
	EventDraftTextUpdatedUnreleased  = "document.draft_text_updated_unreleased"
)

// TextView is a text version as its readers are shown it.
type TextView struct {
	Status           string     `json:"status" jsonschema:"pending: waiting to be transcribed; working: being transcribed; done: there is a text; failed or skipped: there is none, and reason says why"`
	Source           *string    `json:"source,omitempty" jsonschema:"whose the text is, once it is done: ai, a transcription, or staff, written or corrected by a member of staff, which no transcription writes over"`
	Model            *string    `json:"model,omitempty" jsonschema:"the model that transcribed it, as the site names it"`
	Pages            *int32     `json:"pages,omitempty" jsonschema:"how many pages or slides the transcription found in the file"`
	Reason           *string    `json:"reason,omitempty" jsonschema:"why it failed or was skipped"`
	ProducedAt       *time.Time `json:"produced_at,omitempty" jsonschema:"when it was transcribed"`
	EditedByMemberID *uuid.UUID `json:"edited_by_member_id,omitempty" jsonschema:"who wrote or last edited it, for staff's"`
	EditedByName     *string    `json:"edited_by_name,omitempty"`
	EditedAt         *time.Time `json:"edited_at,omitempty"`
	Revision         int32      `json:"revision" jsonschema:"counts the changes to the text: an edit names the revision it was made from (base_revision), and a long text is read part by part at one revision"`
	UpdatedAt        time.Time  `json:"updated_at"`
	Bytes            int32      `json:"bytes" jsonschema:"how long the text is, in bytes; 0 while there is none"`
	Body             *string    `json:"body,omitempty" jsonschema:"the whole text, Markdown, when it is done and no longer than one part (65536 bytes); a longer one is read with document.text"`
}

func textView(status string, source *string, pages *int32, model, reason *string, revision int32, producedAt *time.Time,
	editedBy *uuid.UUID, editedAt *time.Time, updatedAt time.Time, bytes int32, editedByName *string) *TextView {
	return &TextView{Status: status, Source: source, Model: model, Pages: pages, Reason: reason, ProducedAt: producedAt,
		EditedByMemberID: editedBy, EditedByName: editedByName, EditedAt: editedAt, Revision: revision, UpdatedAt: updatedAt,
		Bytes: bytes}
}

// versionText is a version's text version, for document.get: the text with it
// when it is one part. nil for a version that has none.
func versionText(ctx context.Context, q dbq.Querier, version uuid.UUID) (*TextView, error) {
	r, err := q.GetTextView(ctx, version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := textView(r.Status, r.Source, r.Pages, r.Model, r.Reason, r.Revision, r.ProducedAt, r.EditedByMemberID, r.EditedAt,
		r.UpdatedAt, r.Bytes, r.EditedByName)
	if r.Status == textDone && r.Bytes <= TextPartBytes {
		b, err := q.GetTextBody(ctx, version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // purged meanwhile
		}
		if err != nil {
			return nil, err
		}
		// Read again with its text: what is shown is of one revision.
		v = textView(b.Status, b.Source, b.Pages, b.Model, b.Reason, b.Revision, b.ProducedAt, b.EditedByMemberID, b.EditedAt,
			b.UpdatedAt, b.Bytes, b.EditedByName)
		if b.Bytes <= TextPartBytes {
			v.Body = b.Body
		}
	}
	return v, nil
}

// textsOf are the text versions of a document's versions, without their
// text, by version.
func textsOf(ctx context.Context, q dbq.Querier, document uuid.UUID) (map[uuid.UUID]*TextView, error) {
	rows, err := q.ListTextViews(ctx, document)
	out := make(map[uuid.UUID]*TextView, len(rows))
	for _, r := range rows {
		out[r.VersionID] = textView(r.Status, r.Source, r.Pages, r.Model, r.Reason, r.Revision, r.ProducedAt, r.EditedByMemberID,
			r.EditedAt, r.UpdatedAt, r.Bytes, r.EditedByName)
	}
	return out, err
}

// textParts cuts a text into the parts document.text reads it in, each at
// most TextPartBytes: whole pages where they fit, a page being what starts
// at a heading of the second level ("## "), as the transcription heads each
// page or slide; otherwise whole lines; otherwise, for a line longer than a
// part, whole characters. A part ends before the last such break that leaves
// it at least half full. The same text is always cut the same way.
func textParts(body string) []string {
	var parts []string
	for len(body) > TextPartBytes {
		cut := textCut(body[:TextPartBytes+1])
		parts = append(parts, body[:cut])
		body = body[cut:]
	}
	if body != "" {
		parts = append(parts, body)
	}
	return parts
}

// textCut is where a part of window, which is one byte longer than a part,
// ends.
func textCut(window string) int {
	half := TextPartBytes / 2
	// A page heading's line starts the next part.
	if i := strings.LastIndex(window[:TextPartBytes], "\n## "); i+1 >= half {
		return i + 1
	}
	// Else the part ends after a whole line.
	if i := strings.LastIndexByte(window[:TextPartBytes], '\n'); i+1 >= half {
		return i + 1
	}
	// Else after a whole character; a part is never empty, whatever the
	// bytes are.
	cut := TextPartBytes
	for cut > TextPartBytes-utf8.UTFMax && !utf8.RuneStart(window[cut]) {
		cut--
	}
	return cut
}

// notifyQueued wakes the service's claims waiting on the queue
// (document_text.queue) once the transaction commits: something is waiting
// to be transcribed in each course named. It is told by no event.
func notifyQueued(ctx context.Context, q *dbq.Queries, courses ...uuid.UUID) error {
	arg := dbq.NotifyWakeParams{Channel: wake.Channel}
	seen := map[uuid.UUID]bool{}
	for _, c := range courses {
		if seen[c] {
			continue
		}
		seen[c] = true
		arg.CourseIds, arg.Kinds, arg.Seqs = append(arg.CourseIds, c), append(arg.Kinds, wake.KindTextQueued), append(arg.Seqs, 0)
		arg.ConversationIds, arg.ActionIds = append(arg.ConversationIds, uuid.Nil), append(arg.ActionIds, uuid.Nil)
	}
	if len(arg.CourseIds) == 0 {
		return nil
	}
	return q.NotifyWake(ctx, arg)
}

// emitTextEvent tells a text version's news as its version's is told: of the
// published version to whoever reads that kind of document, and of any other
// to whoever reads drafts. It carries ids and where the text stands, never
// the text.
func emitTextEvent(ctx context.Context, ec *tool.ExecCtx, doc dbq.GetDocumentWithOwnerRow, version uuid.UUID, status string,
	source *string, revision int32) error {
	typ := EventDraftTextUpdated
	if doc.PublishedVersionID != nil && *doc.PublishedVersionID == version {
		typ = EventTextUpdated
		if doc.Kind == kindRubric {
			typ = EventRubricTextUpdated
		}
	}
	payload := map[string]any{"kind": doc.Kind, "version_id": version, "status": status, "revision": revision}
	if source != nil {
		payload["source"] = *source
	}
	return emitDocumentEvent(ctx, ec, doc.Kind, events.Event{Type: typ, CourseID: &doc.CourseID, SubjectType: "document",
		SubjectID: &doc.ID, Payload: payload})
}

// checkText holds a text to what a text version may be: something, and at
// most MaxTextBytes.
func checkText(body string) error {
	switch {
	case strings.TrimSpace(body) == "":
		return apperr.Invalid("the text is empty")
	case len(body) > MaxTextBytes:
		return apperr.Invalid("the text is %d bytes; the most is %d", len(body), MaxTextBytes).With("reason", "text_too_long")
	}
	return nil
}

func errNoText() *apperr.Error {
	return apperr.Missing("this version has no text version: only a version with a file of material, instructions or a "+
		"rubric has one").With("reason", "no_text")
}

// ---------------------------------------------------------------------------
// document.text
// ---------------------------------------------------------------------------

type DocumentTextIn struct {
	tool.InCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty" jsonschema:"a specific version; otherwise the one document.get gives: the published one, or the latest for members who can read drafts"`
	Part       int        `json:"part,omitempty" jsonschema:"which part of the text, from 1; 1 if omitted"`
}

type DocumentTextOut struct {
	DocumentID uuid.UUID `json:"document_id"`
	VersionID  uuid.UUID `json:"version_id"`
	Seq        int32     `json:"seq"`
	Published  bool      `json:"published"`
	Text       TextView  `json:"text" jsonschema:"where the text version stands; its body is the part's"`
	Part       int       `json:"part,omitempty" jsonschema:"which part this is, while there is a text"`
	Parts      int       `json:"parts" jsonschema:"how many parts the text is read in; 0 while there is none"`
}

func documentText() tool.Tool {
	return tool.Define(tool.Spec[DocumentTextIn, DocumentTextOut]{
		Name: "document.text",
		Description: "Read a document version's text version: its file (slides, a PDF, a Word file) transcribed into Markdown, " +
			"pictures and diagrams described in brackets, each page or slide under a heading of its own; or written by staff. " +
			"Read it before the file: it is the same for every model. For whoever may read the version, as document.get: " +
			"students read the published one. A long text is read in parts of at most 65536 bytes, whole pages where " +
			"they fit, from part 1 to parts; read them all at one revision. Until the text is done, it says where it " +
			"stands (pending, working, failed or skipped, with reason) and has no body.",
		Kind: tool.Read, Gate: anyDocumentRead,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/documents/{document_id}/text"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentTextIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, readPerm)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DocumentTextIn) (DocumentTextOut, error) {
			if in.Part < 0 {
				return DocumentTextOut{}, apperr.Invalid("part counts from 1")
			}
			doc, v, err := readableVersion(ctx, rc, in.CourseID, in.DocumentID, in.VersionID)
			if err != nil {
				return DocumentTextOut{}, err
			}
			if v == nil {
				return DocumentTextOut{}, apperr.Missing("the document has no version for you to read")
			}
			b, err := rc.Q.GetTextBody(ctx, v.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				return DocumentTextOut{}, errNoText()
			}
			if err != nil {
				return DocumentTextOut{}, err
			}
			out := DocumentTextOut{DocumentID: doc.ID, VersionID: v.ID, Seq: v.Seq,
				Published: doc.PublishedVersionID != nil && *doc.PublishedVersionID == v.ID,
				Text: *textView(b.Status, b.Source, b.Pages, b.Model, b.Reason, b.Revision, b.ProducedAt, b.EditedByMemberID,
					b.EditedAt, b.UpdatedAt, b.Bytes, b.EditedByName)}
			if b.Status != textDone || b.Body == nil {
				return out, nil
			}
			parts := textParts(*b.Body)
			part := max(in.Part, 1)
			if part > len(parts) {
				return DocumentTextOut{}, apperr.Invalid("the text has %d parts", len(parts)).With("parts", len(parts))
			}
			out.Part, out.Parts, out.Text.Body = part, len(parts), &parts[part-1]
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// document.text_update, document.text_retranscribe
// ---------------------------------------------------------------------------

type DocumentTextUpdateIn struct {
	tool.InCourse
	DocumentID   uuid.UUID `json:"document_id"`
	VersionID    uuid.UUID `json:"version_id"`
	Body         string    `json:"body" jsonschema:"the whole text, Markdown, at most 2 MiB; it takes the place of what there was"`
	BaseRevision *int32    `json:"base_revision,omitempty" jsonschema:"the revision of the text the edit was made from, as the views give it: if the text has changed since, the edit is refused (text_changed) rather than put over the change"`
}

type DocumentTextRetranscribeIn struct {
	tool.InCourse
	DocumentID   uuid.UUID `json:"document_id"`
	VersionID    uuid.UUID `json:"version_id"`
	DiscardEdit  bool      `json:"discard_edit,omitempty" jsonschema:"true to discard staff's text: required when the text is staff's (staff_edit)"`
	BaseRevision *int32    `json:"base_revision,omitempty" jsonschema:"the revision of the text the request was made from: if the text has changed since, it is refused (text_changed)"`
}

type DocumentTextChangeOut struct {
	VersionID uuid.UUID `json:"version_id"`
	Changed   bool      `json:"changed" jsonschema:"false when the text already was so: nothing was done"`
	Status    string    `json:"status"`
	Revision  int32     `json:"revision"`
}

// textTarget is what a write of a text version is about: the version, of a
// document the caller may write, which says which permission governs it,
// as every write of a document's.
func textTarget(ctx context.Context, q dbq.Querier, courseID, documentID, versionID uuid.UUID) (tool.Target, error) {
	t, err := documentTarget(ctx, q, courseID, documentID, writePerm)
	if err != nil {
		return t, err
	}
	t.Type, t.ID = "document_version", &versionID
	return t, nil
}

// pinRevision fixes, for a proposal, the revision of the text it was made
// about, unless it names one: a text changed while it waits is not what
// whoever asked for the change saw, and approving it refuses (text_changed).
func pinRevision(ctx context.Context, q dbq.Querier, version uuid.UUID, given *int32) (*int32, error) {
	if given != nil {
		return given, nil
	}
	r, err := q.GetTextView(ctx, version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r.Revision, nil
}

// staffText is a text version held for staff's change to it, and its
// document, which is held first, as adding and purging a version hold it.
type staffText struct {
	doc  dbq.GetDocumentWithOwnerRow
	text dbq.DocumentVersionText
	// queued says the text version was made by this call: its version has
	// a file from before text versions, and was never queued.
	queued bool
}

// lockText holds a text version for staff's change to it: of a version of
// material, instructions or a rubric, not archived, with a file. A version
// with a file from before text versions, never queued, is given one here,
// pending: staff may write it, or ask for it to be transcribed.
func lockText(ctx context.Context, ec *tool.ExecCtx, courseID, documentID, versionID uuid.UUID) (staffText, error) {
	var st staffText
	if err := ec.Q.LockDocument(ctx, documentID); err != nil {
		return st, err
	}
	var err error
	if st.doc, err = loadDocument(ctx, ec.Q, courseID, documentID); err != nil {
		return st, err
	}
	switch {
	case !courseLevel(st.doc.Kind):
		return st, errNoText()
	case st.doc.Status != "active":
		return st, apperr.Conflicts("the document is archived").With("reason", "document_archived")
	}
	st.text, err = ec.Q.LockText(ctx, dbq.LockTextParams{VersionID: versionID, DocumentID: st.doc.ID})
	if !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	v, err := ec.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: versionID, DocumentID: st.doc.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return st, apperr.Missing("no such version of this document")
	}
	if err != nil {
		return st, err
	}
	if v.StorageKey == nil || v.PurgedAt != nil {
		return st, errNoText()
	}
	if err := ec.Q.QueueNewText(ctx, dbq.QueueNewTextParams{VersionID: v.ID, DocumentID: st.doc.ID, CourseID: st.doc.CourseID,
		Now: ec.Now}); err != nil {
		return st, err
	}
	st.queued = true
	st.text, err = ec.Q.LockText(ctx, dbq.LockTextParams{VersionID: versionID, DocumentID: st.doc.ID})
	return st, err
}

// changedSince refuses a change made from a revision of the text that is not
// the one there now.
func changedSince(text dbq.DocumentVersionText, base *int32) error {
	if base != nil && *base != text.Revision {
		return apperr.Conflicts("the text has changed since revision %d; it is at revision %d", *base, text.Revision).
			With("reason", "text_changed").With("revision", text.Revision)
	}
	return nil
}

func documentTextUpdate() tool.Tool {
	return tool.Define(tool.Spec[DocumentTextUpdateIn, DocumentTextChangeOut]{
		Name: "document.text_update",
		Description: "Write a document version's text version, in place of what there was: correct a transcription, or write " +
			"one by hand. The text is staff's from then on: no transcription writes over it, and one under way is " +
			"refused when it finishes. For whoever may write the document, as a new version is written; the text, at most " +
			"2 MiB of Markdown, is recorded with the action, for whoever decides or reviews it. base_revision refuses the " +
			"edit if the text has changed since (text_changed). Giving the text it already is changes nothing (changed: false).",
		Kind: tool.Write, Gate: anyDocumentWrite, MaxRequestBytes: textRequestBytes,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/versions/{version_id}/text"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentTextUpdateIn) (tool.Target, error) {
			return textTarget(ctx, q, in.CourseID, in.DocumentID, in.VersionID)
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in DocumentTextUpdateIn) error {
			return checkText(in.Body)
		},
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in DocumentTextUpdateIn) (DocumentTextUpdateIn, error) {
			var err error
			in.BaseRevision, err = pinRevision(ctx, q, in.VersionID, in.BaseRevision)
			return in, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentTextUpdateIn) (DocumentTextChangeOut, error) {
			if err := checkText(in.Body); err != nil {
				return DocumentTextChangeOut{}, err
			}
			st, err := lockText(ctx, ec, in.CourseID, in.DocumentID, in.VersionID)
			if err != nil {
				return DocumentTextChangeOut{}, err
			}
			text := st.text
			if err := changedSince(text, in.BaseRevision); err != nil {
				return DocumentTextChangeOut{}, err
			}
			out := DocumentTextChangeOut{VersionID: text.VersionID, Status: textDone, Revision: text.Revision}
			if text.Source != nil && *text.Source == textByStaff && text.Body != nil && *text.Body == in.Body {
				return out, nil
			}
			if out.Revision, err = ec.Q.EditText(ctx, dbq.EditTextParams{VersionID: text.VersionID, Body: &in.Body,
				EditedByMemberID: &ec.Member.ID, Now: &ec.Now}); err != nil {
				return DocumentTextChangeOut{}, err
			}
			out.Changed = true
			source := textByStaff
			return out, emitTextEvent(ctx, ec, st.doc, text.VersionID, textDone, &source, out.Revision)
		},
	})
}

func documentTextRetranscribe() tool.Tool {
	return tool.Define(tool.Spec[DocumentTextRetranscribeIn, DocumentTextChangeOut]{
		Name: "document.text_retranscribe",
		Description: "Send a document version's text version to be transcribed again, or for the first time for a version " +
			"added before there were text versions: it is pending again, ahead of anything queued when text versions came " +
			"in, and what it said is gone until the new transcription is done; one under way is refused when it finishes. " +
			"A text staff wrote or corrected is discarded only with discard_edit true (staff_edit). For whoever may write " +
			"the document. base_revision refuses it if the text has changed since (text_changed). A text already waiting " +
			"its turn changes nothing (changed: false).",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/versions/{version_id}/text/retranscribe"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentTextRetranscribeIn) (tool.Target, error) {
			return textTarget(ctx, q, in.CourseID, in.DocumentID, in.VersionID)
		},
		// Nobody is asked to approve discarding staff's text without saying
		// so; whoever approves it is asked again when it is carried out.
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, in DocumentTextRetranscribeIn) error {
			r, err := q.GetTextView(ctx, in.VersionID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return keepsStaffText(r.Source, in.DiscardEdit)
		},
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in DocumentTextRetranscribeIn) (DocumentTextRetranscribeIn, error) {
			var err error
			in.BaseRevision, err = pinRevision(ctx, q, in.VersionID, in.BaseRevision)
			return in, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentTextRetranscribeIn) (DocumentTextChangeOut, error) {
			st, err := lockText(ctx, ec, in.CourseID, in.DocumentID, in.VersionID)
			if err != nil {
				return DocumentTextChangeOut{}, err
			}
			doc, text := st.doc, st.text
			if err := changedSince(text, in.BaseRevision); err != nil {
				return DocumentTextChangeOut{}, err
			}
			if err := keepsStaffText(text.Source, in.DiscardEdit); err != nil {
				return DocumentTextChangeOut{}, err
			}
			out := DocumentTextChangeOut{VersionID: text.VersionID, Status: textPending, Revision: text.Revision}
			switch {
			case st.queued:
				out.Changed = true
				return out, notifyQueued(ctx, ec.Q, doc.CourseID)
			case text.Status == textPending && !text.Backfill:
				return out, nil
			}
			if out.Revision, err = ec.Q.RequeueText(ctx, dbq.RequeueTextParams{VersionID: text.VersionID, Now: ec.Now}); err != nil {
				return DocumentTextChangeOut{}, err
			}
			out.Changed = true
			if err := notifyQueued(ctx, ec.Q, doc.CourseID); err != nil {
				return DocumentTextChangeOut{}, err
			}
			if out.Revision == text.Revision {
				return out, nil // there was no text to go
			}
			return out, emitTextEvent(ctx, ec, doc, text.VersionID, textPending, nil, out.Revision)
		},
	})
}

// keepsStaffText refuses to discard staff's text unless told to.
func keepsStaffText(source *string, discard bool) error {
	if source != nil && *source == textByStaff && !discard {
		return apperr.Precondition("the text was written by staff: say discard_edit to discard it").With("reason", "staff_edit")
	}
	return nil
}
