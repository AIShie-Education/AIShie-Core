package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Renditions (docs/schema.md §2.4, Renditions). Every Office or OpenDocument
// file Core keeps — each file of a document's version, of every kind, and
// each file a message of a conversation carries — is given a PDF rendition,
// for the site to preview it in its PDF viewer: converted once, on the
// server, by the site's agent runtime, which takes it from a queue
// (agent_runtime.rendition_*, rendition_service.go) and uploads the PDF it
// makes. Which files are converted is the database's one table
// (file_rendition_convertible, migration 0026), and the database queues a
// file as it is recorded, whichever release records it.
//
// Who may read a rendition is exactly who may read its file, by the same
// checks: document.get and document.file give each file's (rendition), with
// a short-lived URL that shows the PDF, once it is done; document.versions
// says where each stands; conversation.attachment gives a message's file's,
// with its URL, and conversation.messages where each stands. To anyone else
// it does not exist, as its file does not. A file that is not converted has
// none.
//
// Whoever may write a document sends a failed rendition of one of its files
// back to the queue (document.rendition_retry); a message's author, or
// whoever decides actions for the conversation's opener, a message's file's
// (conversation.rendition_retry). Neither is news: a rendition is plumbing.

func renditionTools(d Deps) []tool.Tool {
	return []tool.Tool{documentRenditionRetry(), conversationRenditionRetry(),
		renditionClaim(d), renditionFile(d), renditionRenew(), renditionUploadURL(d), renditionComplete(d)}
}

const (
	renditionQueued, renditionClaimed, renditionDone, renditionFailed, renditionSkipped = "queued", "claimed", "done", "failed", "skipped"

	// RenditionPrefix begins the key of every rendition's PDF:
	// renditions/<course>/<upload>, beside the uploads' prefixes, which the
	// orphan sweep looks under as well. The release before 0026 does not
	// look under it.
	RenditionPrefix = "renditions/"

	// RenditionType is what a rendition is, and is served as.
	RenditionType = "application/pdf"
	// pdfMagic is how every PDF begins, and what Core asks of one uploaded.
	pdfMagic = "%PDF-"

	// DefaultRenditionMaxBytes is the largest PDF a rendition may be where
	// nothing else is said (RENDITION_MAX_BYTES): 100 MiB.
	DefaultRenditionMaxBytes = 100 << 20
)

// RenditionLimits bounds the PDFs files are converted into.
type RenditionLimits struct {
	// MaxBytes bounds one PDF.
	MaxBytes int64
}

func (l RenditionLimits) withDefaults() RenditionLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultRenditionMaxBytes
	}
	return l
}

// RenditionView is a file's PDF rendition, as the file's readers are shown
// it.
type RenditionView struct {
	State             string     `json:"state" jsonschema:"queued: waiting to be converted; claimed: being converted; done: the PDF is there; failed or skipped: there is none, and reason says why"`
	PageCount         *int32     `json:"page_count,omitempty" jsonschema:"how many pages the PDF has, once it is done"`
	ByteSize          *int64     `json:"byte_size,omitempty" jsonschema:"how large the PDF is, in bytes, once it is done"`
	Reason            *string    `json:"reason,omitempty" jsonschema:"why there is none, for failed and skipped: password_protected, timeout, conversion_failed, too_large, unsupported or attempts_exhausted"`
	DownloadURL       *string    `json:"download_url,omitempty" jsonschema:"once it is done, and where the read gives URLs: a short-lived URL that shows the PDF where it is opened (inline), named as the file with .pdf; GET it with no Authorization header"`
	DownloadExpiresAt *time.Time `json:"download_expires_at,omitempty" jsonschema:"when download_url stops working, about 15 minutes from now; read again for another"`
}

// renditionView is a rendition as its file's readers are shown it, without
// a URL.
func renditionView(r dbq.FileRendition) *RenditionView {
	v := &RenditionView{State: r.Status, PageCount: r.PageCount, Reason: r.Reason}
	if r.Status == renditionDone {
		v.ByteSize = r.ByteSize
	}
	return v
}

// renditionState is a rendition as a list of versions shows it: where it
// stands, and nothing else.
func renditionState(status string) *RenditionView { return &RenditionView{State: status} }

// withURL gives a done rendition's view a URL that shows its PDF, named as
// its file is, with .pdf.
func (d Deps) withURL(ctx context.Context, v *RenditionView, r dbq.FileRendition, filename string, now time.Time) error {
	if r.Status != renditionDone || r.StorageKey == nil || d.Blob == nil {
		return nil
	}
	url, err := d.Blob.PresignView(ctx, *r.StorageKey, pdfName(filename), RenditionType, downloadTTL)
	if err != nil {
		return err
	}
	expires := now.Add(downloadTTL)
	v.DownloadURL, v.DownloadExpiresAt = &url, &expires
	return nil
}

// pdfName is what a file's rendition is called: its name with .pdf in place
// of its extension, which a converted file has.
func pdfName(filename string) string {
	if i := strings.LastIndexByte(filename, '.'); i > 0 {
		filename = filename[:i]
	}
	return filename + ".pdf"
}

// renditionsOfFiles are the renditions of the given files of versions, by
// file.
func renditionsOfFiles(ctx context.Context, q dbq.Querier, files []uuid.UUID) (map[uuid.UUID]dbq.FileRendition, error) {
	out := map[uuid.UUID]dbq.FileRendition{}
	if len(files) == 0 {
		return out, nil
	}
	rows, err := q.ListRenditionsOfFiles(ctx, files)
	for _, r := range rows {
		out[*r.FileID] = r
	}
	return out, err
}

// renditionsOfAttachments are the renditions of the given files of messages,
// by file.
func renditionsOfAttachments(ctx context.Context, q dbq.Querier, files []uuid.UUID) (map[uuid.UUID]dbq.FileRendition, error) {
	out := map[uuid.UUID]dbq.FileRendition{}
	if len(files) == 0 {
		return out, nil
	}
	rows, err := q.ListRenditionsOfAttachments(ctx, files)
	for _, r := range rows {
		out[*r.AttachmentID] = r
	}
	return out, err
}

// withRenditions gives the files of messages where their renditions stand,
// without URLs.
func withRenditions(ctx context.Context, q dbq.Querier, files map[uuid.UUID][]AttachmentView) error {
	var ids []uuid.UUID
	for _, fs := range files {
		for _, f := range fs {
			ids = append(ids, f.ID)
		}
	}
	renditions, err := renditionsOfAttachments(ctx, q, ids)
	if err != nil {
		return err
	}
	for _, fs := range files {
		for i, f := range fs {
			if r, ok := renditions[f.ID]; ok {
				fs[i].Rendition = renditionView(r)
			}
		}
	}
	return nil
}

// notifyRenditionsQueued wakes the agent runtime's claims waiting on the
// queue (agent_runtime.rendition_claim) once the transaction commits:
// something is waiting to be converted in each course named. It is told by
// no event.
func notifyRenditionsQueued(ctx context.Context, q *dbq.Queries, courses ...uuid.UUID) error {
	arg := dbq.NotifyWakeParams{Channel: wake.Channel}
	seen := map[uuid.UUID]bool{}
	for _, c := range courses {
		if seen[c] {
			continue
		}
		seen[c] = true
		arg.CourseIds, arg.Kinds, arg.Seqs = append(arg.CourseIds, c), append(arg.Kinds, wake.KindRenditionQueued), append(arg.Seqs, 0)
		arg.ConversationIds, arg.ActionIds = append(arg.ConversationIds, uuid.Nil), append(arg.ActionIds, uuid.Nil)
	}
	if len(arg.CourseIds) == 0 {
		return nil
	}
	return q.NotifyWake(ctx, arg)
}

// queuedFiles wakes the runtime for the files of a version just recorded, if
// the database queued any of them to be converted.
func queuedFiles(ctx context.Context, q *dbq.Queries, course uuid.UUID, files []uuid.UUID) error {
	queued, err := renditionsOfFiles(ctx, q, files)
	if err != nil || len(queued) == 0 {
		return err
	}
	return notifyRenditionsQueued(ctx, q, course)
}

// queuedAttachments wakes the runtime for the files of a message just
// written, if the database queued any of them to be converted.
func queuedAttachments(ctx context.Context, q *dbq.Queries, course uuid.UUID, files []uuid.UUID) error {
	queued, err := renditionsOfAttachments(ctx, q, files)
	if err != nil || len(queued) == 0 {
		return err
	}
	return notifyRenditionsQueued(ctx, q, course)
}

// ---------------------------------------------------------------------------
// document.rendition_retry, conversation.rendition_retry
// ---------------------------------------------------------------------------

// errNoRendition refuses a retry of a file that is not converted.
func errNoRendition() *apperr.Error {
	return apperr.Missing("that file has no PDF rendition: only an Office or OpenDocument file has one").With("reason", "no_rendition")
}

// RenditionRetryOut says what became of a retry.
type RenditionRetryOut struct {
	RenditionID uuid.UUID `json:"rendition_id"`
	Changed     bool      `json:"changed" jsonschema:"false when it was waiting or being converted already: nothing was done"`
	State       string    `json:"state" jsonschema:"where it stands now: queued, or claimed when it was being converted already"`
}

// errRenditionDone refuses a retry of a rendition that is done: its PDF is
// there.
func errRenditionDone() *apperr.Error {
	return apperr.Precondition("the PDF is there already; it is made once").With("reason", "rendition_done")
}

// retryable says why a retry of a file's rendition would be refused as it
// stands, read without its lock, or nil when it would not: rs is the file's,
// none for a file that has none. A retry asks it before it is proposed,
// carried out or approved (Validate), and requeue again under the
// rendition's lock. A rendition waiting or being converted when a retry is
// proposed may be done by the time it is approved, which approving then
// refuses.
func retryable(rs []dbq.FileRendition) error {
	switch {
	case len(rs) == 0:
		return errNoRendition()
	case rs[0].Status == renditionDone:
		return errRenditionDone()
	}
	return nil
}

// requeue sends a failed or skipped rendition back to the queue, as an
// upload is queued; one waiting or being converted is left as it is, and a
// done one is refused: its PDF is there.
func requeue(ctx context.Context, ec *tool.ExecCtx, r dbq.FileRendition) (RenditionRetryOut, error) {
	out := RenditionRetryOut{RenditionID: r.ID, State: r.Status}
	switch r.Status {
	case renditionQueued, renditionClaimed:
		return out, nil
	case renditionDone:
		return out, errRenditionDone()
	}
	if err := ec.Q.RequeueRendition(ctx, dbq.RequeueRenditionParams{ID: r.ID, Now: ec.Now}); err != nil {
		return out, err
	}
	out.Changed, out.State = true, renditionQueued
	return out, notifyRenditionsQueued(ctx, ec.Q, r.CourseID)
}

type DocumentRenditionRetryIn struct {
	tool.InCourse
	DocumentID uuid.UUID `json:"document_id"`
	FileID     uuid.UUID `json:"file_id" jsonschema:"a file of one of the document's versions, from document.get or document.versions"`
}

func documentRenditionRetry() tool.Tool {
	return tool.Define(tool.Spec[DocumentRenditionRetryIn, RenditionRetryOut]{
		Name: "document.rendition_retry",
		Description: "Send the PDF rendition of a document's file that failed or was skipped back to be converted again, " +
			"ahead of anything queued when renditions came in, its attempts starting again. For whoever may write the " +
			"document, as a new version is written. A rendition waiting or being converted already changes nothing " +
			"(changed: false); a done one is refused (rendition_done); a file that is not an Office or OpenDocument file " +
			"has none (no_rendition). No news is told of it.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/files/{file_id}/rendition/retry"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentRenditionRetryIn) (tool.Target, error) {
			t, err := documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
			if err != nil {
				return t, err
			}
			t.Type, t.ID = "document_version_file", &in.FileID
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in DocumentRenditionRetryIn) error {
			if err := documentFileExists(ctx, q, in); err != nil {
				return err
			}
			rs, err := q.ListRenditionsOfFiles(ctx, []uuid.UUID{in.FileID})
			if err != nil {
				return err
			}
			return retryable(rs)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentRenditionRetryIn) (RenditionRetryOut, error) {
			if err := documentFileExists(ctx, ec.Q, in); err != nil {
				return RenditionRetryOut{}, err
			}
			r, err := ec.Q.LockRenditionOfFile(ctx, &in.FileID)
			if errors.Is(err, pgx.ErrNoRows) {
				return RenditionRetryOut{}, errNoRendition()
			}
			if err != nil {
				return RenditionRetryOut{}, err
			}
			return requeue(ctx, ec, r)
		},
	})
}

// documentFileExists refuses a file that is not one of the document's.
func documentFileExists(ctx context.Context, q dbq.Querier, in DocumentRenditionRetryIn) error {
	_, err := q.GetDocumentFile(ctx, dbq.GetDocumentFileParams{ID: in.FileID, DocumentID: in.DocumentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoFile
	}
	return err
}

type ConversationRenditionRetryIn struct {
	tool.InCourse
	AttachmentID uuid.UUID `json:"attachment_id" jsonschema:"a file a message carries, from conversation.messages"`
}

func conversationRenditionRetry() tool.Tool {
	return tool.Define(tool.Spec[ConversationRenditionRetryIn, RenditionRetryOut]{
		Name: "conversation.rendition_retry",
		Description: "Send the PDF rendition of a file a message carries that failed or was skipped back to be converted " +
			"again, ahead of anything queued when renditions came in, its attempts starting again. For the message's " +
			"author, and for whoever decides actions for the conversation's opener, as retracting a message is; to anyone " +
			"who may not read the conversation the file does not exist. A retracted message's files are withheld " +
			"(retracted). A rendition waiting or being converted already changes nothing (changed: false); a done one is " +
			"refused (rendition_done); a file that is not an Office or OpenDocument file has none (no_rendition).",
		Kind: tool.Write, Gate: converses,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/conversation-attachments/{attachment_id}/rendition/retry"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ConversationRenditionRetryIn) (tool.Target, error) {
			if _, err := findAttachment(ctx, q, in.CourseID, in.AttachmentID); err != nil {
				return tool.Target{}, err
			}
			return tool.Target{CourseID: in.CourseID, Type: "conversation_attachment", ID: &in.AttachmentID}, nil
		},
		// Whether the caller may read the file, and send it back, and
		// whether it has a rendition that is not done: asked before a retry
		// is proposed, and again as it is made.
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in ConversationRenditionRetryIn) error {
			if err := attachmentToRetry(ctx, q, m, now, in); err != nil {
				return err
			}
			rs, err := q.ListRenditionsOfAttachments(ctx, []uuid.UUID{in.AttachmentID})
			if err != nil {
				return err
			}
			return retryable(rs)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ConversationRenditionRetryIn) (RenditionRetryOut, error) {
			if err := attachmentToRetry(ctx, ec.Q, ec.Member, ec.Now, in); err != nil {
				return RenditionRetryOut{}, err
			}
			r, err := ec.Q.LockRenditionOfAttachment(ctx, &in.AttachmentID)
			if errors.Is(err, pgx.ErrNoRows) {
				return RenditionRetryOut{}, errNoRendition()
			}
			if err != nil {
				return RenditionRetryOut{}, err
			}
			return requeue(ctx, ec, r)
		},
	})
}

// attachmentToRetry refuses a retry of a message's file by m at now: one m
// may not read is not there to it, one of a retracted message is withheld,
// and only the message's author, or someone who decides actions for the
// conversation's opener, sends it back to be converted.
func attachmentToRetry(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in ConversationRenditionRetryIn) error {
	a, err := findAttachment(ctx, q, in.CourseID, in.AttachmentID)
	if err != nil {
		return err
	}
	c, err := findConversation(ctx, q, in.CourseID, a.ConversationID)
	if err != nil {
		return err
	}
	if ok, err := newAddressing(q, now).mayRead(ctx, m, c); err != nil {
		return err
	} else if !ok {
		return errNoAttachment
	}
	if a.Retracted {
		return errAttachmentRetracted
	}
	if a.AuthorMemberID != m.ID {
		staff, err := oversees(ctx, q, m, c.OpenerMemberID)
		if err != nil {
			return err
		}
		if !staff {
			return apperr.Forbid("only the message's author, or someone who decides actions for the "+
				"conversation's opener, sends its file back to be converted").With("reason", "not_your_message")
		}
	}
	return nil
}
