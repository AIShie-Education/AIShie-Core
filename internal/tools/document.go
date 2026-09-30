package tools

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Everything readable is a document, and a version of one is text, a file, or
// both. Two shapes (docs/schema.md §2.4):
//
//   - one, versioned, pinned — material, instructions, rubrics. Editing adds a
//     version; publishing moves a pointer; students read what is published.
//   - many, unversioned, owned — submitted files and feedback files. Each is
//     a document with exactly one version, pointing at its owner.
//
// Which permission governs a document depends on its kind, so these tools are
// gated by "any of" and let the document say which one applies.

func documentTools(d Deps) []tool.Tool {
	return []tool.Tool{documentUploadURL(d), documentCreate(d), documentAddVersion(d), documentPublish(), documentArchive(),
		documentList(), documentGet(d), documentVersions(), documentUpdate(), documentUnarchive(), documentPurge(d)}
}

const (
	kindMaterial, kindInstructions, kindRubric = "material", "instructions", "rubric"
	kindSubmission, kindFeedback               = "submission", "feedback"

	EventDocumentCreated        = "document.created"
	EventDocumentVersionAdded   = "document.version_added"
	EventDocumentPublished      = "document.published"
	EventRubricPublished        = "document.rubric_published"
	EventDocumentArchived       = "document.archived"
	EventSubmissionFileAdded    = "submission.file_added"
	EventSubmissionFileArchived = "submission.file_archived"
	EventFeedbackFileAdded      = "grade.feedback_added"
	EventFeedbackFileArchived   = "grade.feedback_archived"
	EventDocumentUpdated        = "document.updated"
	EventDocumentUnarchived     = "document.unarchived"
	EventDocumentPurged         = "document.purged"
	EventSubmissionFileUpdated  = "submission.file_updated"
	EventSubmissionFileRestored = "submission.file_unarchived"
	EventFeedbackFileUpdated    = "grade.feedback_updated"
	EventFeedbackFileRestored   = "grade.feedback_unarchived"

	// What the document events above are called while they are about
	// instructions or a rubric that no published assignment refers to yet
	// (see emitDocumentEvent).
	EventDocumentCreatedUnreleased      = "document.created_unreleased"
	EventDocumentVersionAddedUnreleased = "document.version_added_unreleased"
	EventDocumentPublishedUnreleased    = "document.published_unreleased"
	EventRubricPublishedUnreleased      = "document.rubric_published_unreleased"
	EventDocumentArchivedUnreleased     = "document.archived_unreleased"
	EventDocumentUpdatedUnreleased      = "document.updated_unreleased"
	EventDocumentUnarchivedUnreleased   = "document.unarchived_unreleased"
	EventDocumentPurgedUnreleased       = "document.purged_unreleased"

	uploadWindow = 15 * time.Minute
	downloadTTL  = 15 * time.Minute
)

var (
	anyDocumentRead = tool.Gate{Any: true, Perms: []domain.Perm{domain.PermDocumentRead, domain.PermRubricRead,
		domain.PermSubmissionRead, domain.PermGradeRead}}
	anyDocumentWrite = tool.Gate{Any: true, Perms: []domain.Perm{domain.PermDocumentWrite, domain.PermSubmissionWrite,
		domain.PermGradeSubmit}}
)

// readPerm and writePerm are the kind-to-permission table.
func readPerm(kind string) domain.Perm {
	switch kind {
	case kindRubric:
		return domain.PermRubricRead
	case kindSubmission:
		return domain.PermSubmissionRead
	case kindFeedback:
		return domain.PermGradeRead
	}
	return domain.PermDocumentRead
}

func writePerm(kind string) domain.Perm {
	switch kind {
	case kindSubmission:
		return domain.PermSubmissionWrite
	case kindFeedback:
		return domain.PermGradeSubmit
	}
	return domain.PermDocumentWrite
}

func courseLevel(kind string) bool {
	return kind == kindMaterial || kind == kindInstructions || kind == kindRubric
}

// ownerScope is whose an owned document is, for steps 4 and 5.
func ownerScope(d dbq.GetDocumentWithOwnerRow) authz.Target {
	switch {
	case d.SubmissionStudent != nil:
		return authz.Target{StudentMemberIDs: []uuid.UUID{*d.SubmissionStudent}, AssignmentIDs: []uuid.UUID{*d.SubmissionAssignment}}
	case d.GradeStudent != nil && d.GradeAssignment != nil:
		return authz.Target{StudentMemberIDs: []uuid.UUID{*d.GradeStudent}, AssignmentIDs: []uuid.UUID{*d.GradeAssignment}}
	case d.GradeStudent != nil:
		return authz.Target{StudentMemberIDs: []uuid.UUID{*d.GradeStudent}, SpansAssignments: true}
	}
	return authz.Target{}
}

func loadDocument(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (dbq.GetDocumentWithOwnerRow, error) {
	d, err := q.GetDocumentWithOwner(ctx, dbq.GetDocumentWithOwnerParams{ID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return d, apperr.Missing("no such document in this course")
	}
	return d, err
}

func documentTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID, perm func(string) domain.Perm) (tool.Target, error) {
	d, err := loadDocument(ctx, q, courseID, id)
	if err != nil {
		return tool.Target{}, err
	}
	t := tool.Target{CourseID: courseID, Type: "document", ID: &id, Scope: ownerScope(d), Perms: []domain.Perm{perm(d.Kind)}}
	if d.Kind == kindFeedback && d.GradePostedAt != nil && perm(kindFeedback) == domain.PermGradeSubmit {
		t.Perms = feedbackWritePerms(true)
	}
	return t, nil
}

// feedbackWritePerms: feedback is part of a grade. While the grade is a draft,
// writing it takes what writing the grade takes. Once the grade is posted, a
// change to its feedback is visible to the student the moment it is made, so
// it takes the right to post as well, and runs at the lower of the two levels
// — the same reasoning as grade.regrade. Otherwise a TA who "grades but does
// not post" could put new feedback in front of a student, or take the
// instructor's away, on their own.
func feedbackWritePerms(posted bool) []domain.Perm {
	if posted {
		return []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}
	}
	return []domain.Perm{domain.PermGradeSubmit}
}

// unreleased is each document event's unreleased name.
var unreleased = map[string]string{
	EventDocumentCreated:      EventDocumentCreatedUnreleased,
	EventDocumentVersionAdded: EventDocumentVersionAddedUnreleased,
	EventDocumentPublished:    EventDocumentPublishedUnreleased,
	EventRubricPublished:      EventRubricPublishedUnreleased,
	EventDocumentArchived:     EventDocumentArchivedUnreleased,
	EventDocumentUpdated:      EventDocumentUpdatedUnreleased,
	EventDocumentUnarchived:   EventDocumentUnarchivedUnreleased,
	EventDocumentPurged:       EventDocumentPurgedUnreleased,
	EventTextUpdated:          EventTextUpdatedUnreleased,
	EventRubricTextUpdated:    EventRubricTextUpdatedUnreleased,
	EventDraftTextUpdated:     EventDraftTextUpdatedUnreleased,
}

// emitDocumentEvent emits an event about a document of the given kind.
// Instructions and a rubric are their assignment's (assignmentWithheld), and
// so is news of them: the event is filed under each published assignment that
// refers to the document, so that the feed's assignment scope applies to it,
// and while none does it goes out under its unreleased name, which only those
// who see unpublished work, and would be shown the event by its own name, are
// shown (seesType). Otherwise a student would learn from the feed that next
// week's exam exists, and when it was finished. Every other event goes out as
// it is.
func emitDocumentEvent(ctx context.Context, ec *tool.ExecCtx, kind string, ev events.Event) error {
	if kind != kindInstructions && kind != kindRubric {
		ec.Emit(ev)
		return nil
	}
	published, err := ec.Q.ListPublishedAssignmentsUsingDocument(ctx, ev.SubjectID)
	if err != nil {
		return err
	}
	if len(published) == 0 {
		ev.Type = unreleased[ev.Type]
		ec.Emit(ev)
		return nil
	}
	for _, a := range published {
		ev.AssignmentID = &a
		ec.Emit(ev)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

type UploadURLIn struct {
	tool.InCourse
	Kind        string `json:"kind" jsonschema:"what the file is for: material, instructions, rubric, submission or feedback"`
	ContentType string `json:"content_type" jsonschema:"the file's media type, e.g. application/pdf; the upload must send the same"`
	Filename    string `json:"filename,omitempty" jsonschema:"the file's name, e.g. week1-slides.pdf: 1 to 255 characters on one line, a name and not a path. The file is called so when it is attached without a name of its own"`
}

type UploadURLOut struct {
	UploadURL   string            `json:"upload_url" jsonschema:"PUT the file's bytes here, once, within the window"`
	Headers     map[string]string `json:"headers" jsonschema:"headers the PUT must carry"`
	UploadToken string            `json:"upload_token" jsonschema:"name this in files of document.create or document.add_version, or in feedback_files of grade.submit, to attach what you uploaded"`
	ExpiresAt   time.Time         `json:"expires_at"`
	MaxBytes    int64             `json:"max_bytes" jsonschema:"the largest file, in bytes, that can be attached. It is checked when the file is attached, which refuses a larger one (file_too_large); where the URL is an object store's, a larger upload is not stopped as it arrives"`
	MaxFiles    int               `json:"max_files" jsonschema:"the most files one version of a document holds (too_many_files)"`
	// MaxVersionBytes is what a front end checks a version's files against
	// before it is refused for them.
	MaxVersionBytes int64 `json:"max_version_bytes" jsonschema:"the most one version's files come to, in bytes, all together (version_too_large)"`
}

// DocumentPrefix begins the key of every upload for a document:
// documents/<course>/<upload>. These keys, and the final keys that attaching
// moves them to, are with those of UploadPrefix and AttachmentPrefix all the
// server ever writes to the store. The orphan sweep looks at nothing else,
// so a bucket or directory that holds other things as well loses none of
// them.
//
// UploadPrefix, courses/<course>/<upload>, is where documents' uploads went
// before a version held several files. The release before that sweeps it
// alone, and knows of a version's first file alone: under it, the other
// files of a version would look to it like uploads nothing came to point
// at. So they go under a prefix of their own, which it leaves be.
const (
	DocumentPrefix = "documents/"
	UploadPrefix   = "courses/"
)

// OrphanGrace is how long past the proposal TTL the orphan sweep keeps an
// upload that nothing has attached (see jobs.sweepBlobs), and so how old an
// upload may be when a proposal that would attach it is made: a proposal is
// decided within the TTL, and the file it names is there all that time.
const OrphanGrace = 48 * time.Hour

// proposalsExpire says whether a proposal is cancelled once it has waited the
// TTL. Only then does the orphan sweep remove uploads, and only then can a
// proposal outlive the file it names.
func proposalsExpire(d Deps) bool {
	return d.Pipeline.Config().ProposalTTL > 0
}

func documentUploadURL(d Deps) tool.Tool {
	description := "Get somewhere to upload a file. Files do not travel through tool calls: PUT the bytes to the URL this " +
		"returns, then pass the upload_token to the tool that attaches it. Nothing is recorded until then, and an upload " +
		"that is never attached is eventually discarded."
	if proposalsExpire(d) {
		description += " A call that would attach it by way of a proposal is refused once the upload is more than " +
			strconv.Itoa(int(OrphanGrace.Hours())) + " hours old."
	}
	return tool.Define(tool.Spec[UploadURLIn, UploadURLOut]{
		Name:        "document.upload_url",
		Description: description,
		Kind:        tool.Read, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/upload-url"},
		Resolve: func(_ context.Context, _ dbq.Querier, in UploadURLIn) (tool.Target, error) {
			if !courseLevel(in.Kind) && in.Kind != kindSubmission && in.Kind != kindFeedback {
				return tool.Target{}, apperr.Invalid("kind must be material, instructions, rubric, submission or feedback")
			}
			// Permission only. Whose submission or grade the file ends up on
			// is checked, with scope, by the call that attaches it.
			return tool.Target{CourseID: in.CourseID, Type: "upload", Perms: []domain.Perm{writePerm(in.Kind)}}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in UploadURLIn) (UploadURLOut, error) {
			if d.Blob == nil {
				return UploadURLOut{}, errNoFileStorage
			}
			if strings.TrimSpace(in.ContentType) == "" || len(in.ContentType) > 200 {
				return UploadURLOut{}, apperr.Invalid("content_type is required")
			}
			filename := ""
			if in.Filename != "" {
				var err error
				if filename, err = checkFilename(in.Filename); err != nil {
					return UploadURLOut{}, err
				}
			}
			// This is a read — it records nothing — but what it hands out is
			// the means to write, and an archived course refuses every write.
			if c, err := rc.Q.GetCourse(ctx, in.CourseID); err != nil {
				return UploadURLOut{}, err
			} else if c.Status == domain.CourseArchived {
				return UploadURLOut{}, apperr.Forbid("the course is archived and takes no new files").With("reason", "course_archived")
			}
			// The key is ours and unguessable; nothing the uploader says goes
			// into it. The orphan sweep knows the server's own keys by this
			// shape.
			key := DocumentPrefix + in.CourseID.String() + "/" + ids.New().String()
			url, headers, err := d.Blob.PresignPut(ctx, key, in.ContentType, uploadWindow)
			if err != nil {
				return UploadURLOut{}, err
			}
			expires := rc.Now.Add(uploadWindow)
			return UploadURLOut{
				UploadURL: url, Headers: headers, ExpiresAt: expires, MaxBytes: d.MaxUploadBytes,
				MaxFiles: d.Documents.FilesPerVersion, MaxVersionBytes: d.Documents.VersionBytes,
				UploadToken: d.Uploads.SignUpload(blob.UploadClaim{Key: key, CourseID: in.CourseID, MemberID: rc.Member.ID,
					Purpose: in.Kind, ContentType: in.ContentType, Expires: expires.Unix(), Filename: filename}),
			}, nil
		},
	})
}

// upload is a file that has been uploaded and checked, ready to be recorded.
type upload struct {
	key  string
	info blob.Info
}

// errNoFileStorage refuses what would store a file where there is nowhere
// to store one (BLOB_STORE=none).
var errNoFileStorage = apperr.Precondition("this installation has no file storage configured").With("reason", "no_file_storage")

// errTooLarge refuses a file larger than the most one of its kind may be.
func errTooLarge(size, most int64) *apperr.Error {
	return apperr.Precondition("the file is %d bytes; the limit is %d", size, most).
		With("reason", "file_too_large").With("byte_size", size).With("max_bytes", most)
}

// maxBytes is the most a file of the kind may be: a conversation's
// attachment its own limit, never more than MaxUploadBytes, and a
// document's file MaxUploadBytes.
func (d Deps) maxBytes(kind string) int64 {
	if kind == kindAttachment {
		return d.Attachments.MaxBytes
	}
	return d.MaxUploadBytes
}

// claimUpload turns an upload token into a file to attach. The token proves
// that this member of this course was given the key for this purpose; the
// store is asked whether anything is actually there, and how big it is.
func claimUpload(ctx context.Context, d Deps, q dbq.Querier, m *domain.Member, courseID uuid.UUID, kind, token string, finalize bool) (upload, error) {
	if d.Blob == nil {
		return upload{}, errNoFileStorage
	}
	c, err := d.Uploads.VerifyUpload(token)
	if err != nil {
		return upload{}, apperr.Invalid("upload_token is not valid").With("reason", "bad_upload_token")
	}
	if c.CourseID != courseID || c.MemberID != m.ID || c.Purpose != kind {
		return upload{}, apperr.Forbid("that upload was issued to someone else, or for something else").With("reason", "not_your_upload")
	}
	most := d.maxBytes(kind)
	// An upload URL can be written to again for as long as it is valid, so
	// the object is moved, on attaching, to a final key that no upload URL
	// was ever issued for; that key is what the version records. The lock
	// makes "is it attached already?" and the move one step: without it a
	// second attach, racing the first, could copy different bytes over an
	// object a committed version already points at.
	//
	// Attached is asked of the upload's own key as well as its final key.
	// The disk's final key is the upload's own, so what was attached there
	// is recorded under it, and when the files are moved to a bucket they
	// keep their keys: there the same upload's final key is another, which
	// nothing names. Asked of that alone, a token replayed after the move
	// would be let by, and attaching would move the object a version points
	// at to the final key and delete it from under the version.
	final := d.Blob.FinalKey(c.Key)
	keys := lockStorageKeys(c.Key, final)
	for _, key := range keys {
		if err := q.LockStorageKey(ctx, key); err != nil {
			return upload{}, err
		}
	}
	for _, key := range keys {
		if used, err := q.StorageKeyInUse(ctx, &key); err != nil {
			return upload{}, err
		} else if used {
			to := "a document"
			if kind == kindAttachment {
				to = "a message"
			}
			return upload{}, apperr.Conflicts("that upload is already attached to %s", to).With("reason", "already_attached")
		}
	}
	staged, err := d.Blob.Stat(ctx, c.Key)
	if errors.Is(err, blob.ErrNotFound) {
		// Nothing staged: either nothing was uploaded, or an earlier attach
		// moved it and then its transaction did not commit — the move is
		// outside the transaction. The object is then at the final key,
		// which nothing points at (checked above, under the lock) and no
		// upload URL was ever issued for: it can only be this token's own
		// upload, and is attached as it is.
		info, err := d.Blob.Stat(ctx, final)
		if errors.Is(err, blob.ErrNotFound) {
			return upload{}, errNotUploaded
		}
		if err != nil {
			return upload{}, err
		}
		if info.ContentType == "" {
			info.ContentType = c.ContentType
		}
		if info.Size > most {
			// Moved, by an attach that did not commit, under a larger
			// limit than there is now.
			return upload{}, errTooLarge(info.Size, most)
		}
		return upload{key: final, info: info}, nil
	} else if err != nil {
		return upload{}, err
	} else if staged.Size > most {
		_ = d.Blob.Delete(ctx, c.Key)
		return upload{}, errTooLarge(staged.Size, most)
	}
	if !finalize {
		// A dry run, for Validate and Pin: everything is checked and nothing
		// moves. What is there is described as it is now.
		if staged.ContentType == "" {
			staged.ContentType = c.ContentType
		}
		return upload{key: final, info: staged}, nil
	}
	// What is recorded describes the final object, read after the move.
	info, err := d.Blob.Finalize(ctx, c.Key)
	if errors.Is(err, blob.ErrNotFound) {
		return upload{}, errNotUploaded
	}
	if err != nil {
		return upload{}, err
	}
	if info.Size > most {
		_ = d.Blob.Delete(ctx, final)
		return upload{}, errTooLarge(info.Size, most)
	}
	if info.ContentType == "" {
		info.ContentType = c.ContentType
	}
	return upload{key: final, info: info}, nil
}

// lockStorageKeys are the keys an attach locks, each once and in one order,
// the same for every caller, so that two attaching the same upload at once
// wait for each other and never each for the other. The sweep locks one key
// at a time.
func lockStorageKeys(keys ...string) []string {
	keys = slices.Clone(keys)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// errNotUploaded refuses a token whose upload URL has had nothing PUT to it.
var errNotUploaded = apperr.Precondition("nothing has been uploaded to that URL yet").With("reason", "not_uploaded")

// checkUploadAge refuses uploads that a proposal made now could outlive. A
// proposal waits up to the TTL for its decision, and the sweep removes an
// upload nothing has attached once it is TTL + OrphanGrace old, so what a
// proposal names must be no older than OrphanGrace when it is made. The age
// is the store's, which is what the sweep goes by.
//
// It is for Pin. A direct call attaches the file there and then, and an
// approval carries out a proposal that was held to this when it was made,
// however old the file is by the time it is approved. A token that is not
// good, or names nothing uploaded yet, is let by: the call that attaches it
// says what is wrong, and an upload still to come is younger than the
// proposal. Where proposals do not expire nothing is swept, and any upload
// may be proposed.
func checkUploadAge(ctx context.Context, d Deps, now time.Time, tokens ...string) error {
	if d.Blob == nil || !proposalsExpire(d) {
		return nil
	}
	for _, token := range tokens {
		c, err := d.Uploads.VerifyUpload(token)
		if err != nil {
			continue
		}
		info, err := d.Blob.Stat(ctx, c.Key)
		if errors.Is(err, blob.ErrNotFound) {
			// Moved already, by an attach that did not commit; see
			// claimUpload.
			info, err = d.Blob.Stat(ctx, d.Blob.FinalKey(c.Key))
		}
		if errors.Is(err, blob.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Modified.Before(now.Add(-OrphanGrace)) {
			return apperr.Precondition("that upload is more than %d hours old and may be discarded before the proposal is decided; upload the file again",
				int(OrphanGrace.Hours())).With("reason", "upload_too_old")
		}
	}
	return nil
}

// Content is what a version holds: text, files, or both.
type Content struct {
	BodyMD      *string  `json:"body_md,omitempty" jsonschema:"markdown text"`
	Files       []FileIn `json:"files,omitempty" jsonschema:"the version's files, in order, each uploaded first with document.upload_url; at most max_files of them, together at most max_version_bytes"`
	UploadToken *string  `json:"upload_token,omitempty" jsonschema:"deprecated: one file, as files with one, named as it was uploaded or else after the document's title; not with files"`
}

func (c Content) empty() bool {
	return (c.BodyMD == nil || *c.BodyMD == "") && c.UploadToken == nil && len(c.Files) == 0
}

// uploads are the upload tokens the content names, for checkUploadAge.
func (c Content) uploads() []string {
	if c.UploadToken != nil {
		return []string{*c.UploadToken}
	}
	tokens := make([]string, len(c.Files))
	for i, f := range c.Files {
		tokens[i] = f.UploadToken
	}
	return tokens
}

// insertVersion writes one version, with its files, named (Content.named)
// after title where nothing else names them. The author is the calling
// member, who is a member of the document's course because the call was
// authorized in it — which is the whole of the rule that a version's author
// belongs to its document's course. The version's own file columns name its
// first file, as the release before reads them. Each file of material,
// instructions or a rubric is queued to be transcribed as it is added
// (document_version_file_text_queued), and the service is woken to take it.
func insertVersion(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID, documentID uuid.UUID, kind, title string, seq int32,
	c Content) (uuid.UUID, []uuid.UUID, error) {
	named, err := c.named(d, title)
	if err != nil {
		return uuid.Nil, nil, err
	}
	files, err := claimFiles(ctx, d, ec.Q, ec.Member, courseID, kind, named, true)
	if err != nil {
		return uuid.Nil, nil, err
	}
	row := dbq.InsertDocumentVersionParams{ID: ids.New(), DocumentID: documentID, Seq: seq, BodyMd: c.BodyMD,
		AuthorMemberID: ec.Member.ID, CreatedAt: ec.Now}
	if len(files) > 0 {
		first := files[0]
		row.StorageKey, row.ContentType, row.ByteSize, row.Checksum = &first.key, &first.info.ContentType, &first.info.Size,
			nonEmpty(first.info.Checksum)
	}
	if err := ec.Q.InsertDocumentVersion(ctx, row); err != nil {
		return uuid.Nil, nil, err
	}
	fileIDs, err := insertFiles(ctx, ec, documentID, row.ID, files)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if len(files) > 0 && courseLevel(kind) {
		if err := notifyQueued(ctx, ec.Q, courseID); err != nil {
			return uuid.Nil, nil, err
		}
	}
	return row.ID, fileIDs, nil
}

// ---------------------------------------------------------------------------
// document.create
// ---------------------------------------------------------------------------

type DocumentCreateIn struct {
	tool.InCourse
	Kind         string     `json:"kind" jsonschema:"material, instructions, rubric, submission or feedback"`
	Title        string     `json:"title"`
	SubmissionID *uuid.UUID `json:"submission_id,omitempty" jsonschema:"required for kind submission: the draft this file belongs to"`
	GradeID      *uuid.UUID `json:"grade_id,omitempty" jsonschema:"required for kind feedback: the grade this file belongs to"`
	SortOrder    int32      `json:"sort_order,omitempty"`
	Content
}

type DocumentCreateOut struct {
	DocumentID uuid.UUID   `json:"document_id"`
	VersionID  *uuid.UUID  `json:"version_id,omitempty" jsonschema:"absent when the document was created empty"`
	FileIDs    []uuid.UUID `json:"file_ids,omitempty" jsonschema:"the version's files' ids, in order; absent when it has none"`
}

func documentCreate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentCreateIn, DocumentCreateOut]{
		Name: "document.create",
		Description: "Create a document. Material, instructions and rubrics are versioned and start unpublished — students " +
			"see nothing until document.publish. A submission file is attached to a draft submission, and a feedback file " +
			"to a grade, a computed total included; those have exactly one version and are given their content here. " +
			"A version is text (body_md), files, or both: upload each file first (document.upload_url) and name them, in " +
			"order, in files, each with its upload_token and filename; upload_token alone is one file, and is deprecated.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentCreateIn) (tool.Target, error) {
			t := tool.Target{CourseID: in.CourseID, Type: "document", Perms: []domain.Perm{writePerm(in.Kind)}}
			switch {
			case courseLevel(in.Kind):
				if in.SubmissionID != nil || in.GradeID != nil {
					return t, apperr.Invalid("%s does not belong to a submission or a grade", in.Kind)
				}
			case in.Kind == kindSubmission:
				if in.SubmissionID == nil || in.GradeID != nil {
					return t, apperr.Invalid("a submission file needs submission_id, and only that")
				}
				owner, err := submissionTarget(ctx, q, in.CourseID, *in.SubmissionID)
				if err != nil {
					return t, err
				}
				t.Scope = owner.Scope
			case in.Kind == kindFeedback:
				if in.GradeID == nil || in.SubmissionID != nil {
					return t, apperr.Invalid("a feedback file needs grade_id, and only that")
				}
				g, err := q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: *in.GradeID, CourseID: in.CourseID})
				if errors.Is(err, pgx.ErrNoRows) {
					return t, apperr.Missing("no such grade in this course")
				}
				if err != nil {
					return t, err
				}
				t.Perms = feedbackWritePerms(g.PostedAt != nil)
				t.Scope = authz.Target{StudentMemberIDs: []uuid.UUID{g.StudentMemberID}}
				if g.AssignmentID != nil {
					t.Scope.AssignmentIDs = []uuid.UUID{*g.AssignmentID}
				} else {
					t.Scope.SpansAssignments = true
				}
			default:
				return t, apperr.Invalid("kind must be material, instructions, rubric, submission or feedback")
			}
			return t, nil
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in DocumentCreateIn) error {
			return in.check(d)
		},
		// The files must be ones a version may hold, and still be there when
		// the proposal is approved.
		Pin: func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in DocumentCreateIn) (DocumentCreateIn, error) {
			return in, checkProposedVersion(ctx, d, q, m, now, in.CourseID, in.Kind, in.Title, in.Content)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentCreateIn) (DocumentCreateOut, error) {
			if strings.TrimSpace(in.Title) == "" {
				return DocumentCreateOut{}, apperr.Invalid("title is required")
			}
			ev := events.Event{Type: EventDocumentCreated, CourseID: &in.CourseID, SubjectType: "document", Payload: map[string]any{"kind": in.Kind}}
			switch in.Kind {
			case kindSubmission:
				// The freeze trigger guards the submission row, not the files
				// beside it. Once handed in, nothing more may be added. The
				// state is read under the row's lock, the one the hand-in
				// takes: a file that comes while the draft is being handed in
				// waits for it, and then finds it handed in.
				s, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: *in.SubmissionID, CourseID: in.CourseID})
				if err != nil {
					return DocumentCreateOut{}, err
				}
				if s.State != stateDraft {
					return DocumentCreateOut{}, apperr.Conflicts("the submission is %s; files are added to a draft, and a new attempt is a new draft", s.State)
				}
				ev.Type, ev.StudentMemberID, ev.AssignmentID = EventSubmissionFileAdded, &s.StudentMemberID, &s.AssignmentID
			case kindFeedback:
				g, err := ec.Q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: *in.GradeID, CourseID: in.CourseID})
				if err != nil {
					return DocumentCreateOut{}, err
				}
				// A computed total takes feedback files as an entered grade does:
				// whoever posts may say something about it (grade.comment_total),
				// and the totals written for it later carry them on.
				if g.SupersededBy != nil {
					return DocumentCreateOut{}, apperr.Conflicts("that grade has been replaced; attach feedback to the grade that replaced it")
				}
				ev.Type, ev.StudentMemberID, ev.AssignmentID = EventFeedbackFileAdded, &g.StudentMemberID, g.AssignmentID
			}
			if !courseLevel(in.Kind) && in.empty() {
				return DocumentCreateOut{}, apperr.Invalid("a %s file has one version and needs its content now: body_md or files", in.Kind)
			}

			out := DocumentCreateOut{DocumentID: ids.New()}
			if err := ec.Q.InsertDocument(ctx, dbq.InsertDocumentParams{ID: out.DocumentID, CourseID: in.CourseID, Kind: in.Kind,
				Title: in.Title, SubmissionID: in.SubmissionID, GradeID: in.GradeID, SortOrder: in.SortOrder, CreatedAt: ec.Now}); err != nil {
				return DocumentCreateOut{}, err
			}
			if !in.empty() {
				v, files, err := insertVersion(ctx, d, ec, in.CourseID, out.DocumentID, in.Kind, in.Title, 1, in.Content)
				if err != nil {
					return DocumentCreateOut{}, err
				}
				out.VersionID, out.FileIDs = &v, files
				if !courseLevel(in.Kind) {
					// An owned file has no draft stage: it is what it is.
					if err := ec.Q.SetPublishedVersion(ctx, dbq.SetPublishedVersionParams{ID: out.DocumentID, PublishedVersionID: &v}); err != nil {
						return DocumentCreateOut{}, err
					}
				}
			}
			ev.SubjectID = &out.DocumentID
			return out, emitDocumentEvent(ctx, ec, in.Kind, ev)
		},
	})
}

// ---------------------------------------------------------------------------
// document.add_version, publish, archive
// ---------------------------------------------------------------------------

type DocumentAddVersionIn struct {
	tool.InCourse
	DocumentID uuid.UUID `json:"document_id"`
	Content
	Publish bool `json:"publish,omitempty" jsonschema:"publish the new version at once"`
}

type DocumentVersionOut struct {
	VersionID uuid.UUID   `json:"version_id"`
	Seq       int32       `json:"seq"`
	Published bool        `json:"published"`
	FileIDs   []uuid.UUID `json:"file_ids,omitempty" jsonschema:"document.add_version: the new version's files' ids, in order; absent when it has none"`
}

func documentAddVersion(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentAddVersionIn, DocumentVersionOut]{
		Name: "document.add_version",
		Description: "Edit material, instructions or a rubric by adding a version. Versions are never changed or removed, " +
			"but for an administrator's purge of one uploaded by mistake (document.purge). " +
			"The new version is a draft until it is published; what students read does not change until then. " +
			"It is text (body_md), files, or both, as document.create takes them: files, in order, each with its " +
			"upload_token and filename; upload_token alone is one file, and is deprecated.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/versions"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentAddVersionIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in DocumentAddVersionIn) error {
			return in.check(d)
		},
		// As document.create.
		Pin: func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in DocumentAddVersionIn) (DocumentAddVersionIn, error) {
			doc, err := loadDocument(ctx, q, in.CourseID, in.DocumentID)
			if err != nil {
				return in, err
			}
			return in, checkProposedVersion(ctx, d, q, m, now, in.CourseID, doc.Kind, doc.Title, in.Content)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentAddVersionIn) (DocumentVersionOut, error) {
			if err := ec.Q.LockDocument(ctx, in.DocumentID); err != nil {
				return DocumentVersionOut{}, err
			}
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentVersionOut{}, err
			}
			switch {
			case !courseLevel(doc.Kind):
				return DocumentVersionOut{}, apperr.Precondition("a %s file has exactly one version; replace it by archiving it and adding another", doc.Kind)
			case doc.Status != "active":
				return DocumentVersionOut{}, apperr.Conflicts("the document is archived")
			case in.empty():
				return DocumentVersionOut{}, apperr.Invalid("a version needs content: body_md or files")
			}
			last, err := ec.Q.MaxVersionSeq(ctx, doc.ID)
			if err != nil {
				return DocumentVersionOut{}, err
			}
			out := DocumentVersionOut{Seq: last + 1, Published: in.Publish}
			if out.VersionID, out.FileIDs, err = insertVersion(ctx, d, ec, in.CourseID, doc.ID, doc.Kind, doc.Title, out.Seq, in.Content); err != nil {
				return DocumentVersionOut{}, err
			}
			if err := emitDocumentEvent(ctx, ec, doc.Kind, events.Event{Type: EventDocumentVersionAdded, CourseID: &in.CourseID,
				SubjectType: "document", SubjectID: &doc.ID, Payload: map[string]any{"kind": doc.Kind, "seq": out.Seq,
					"files": len(out.FileIDs)}}); err != nil {
				return DocumentVersionOut{}, err
			}
			if in.Publish {
				if err := publish(ctx, ec, in.CourseID, doc, out.VersionID); err != nil {
					return DocumentVersionOut{}, err
				}
			}
			return out, nil
		},
	})
}

// publish moves the pointer. That is all publishing is: the versions are all
// there already, and students read whichever one the pointer names.
func publish(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, doc dbq.GetDocumentWithOwnerRow, version uuid.UUID) error {
	if err := ec.Q.SetPublishedVersion(ctx, dbq.SetPublishedVersionParams{ID: doc.ID, PublishedVersionID: &version}); err != nil {
		return err
	}
	typ := EventDocumentPublished
	if doc.Kind == kindRubric {
		typ = EventRubricPublished // rubrics have readers of their own
	}
	return emitDocumentEvent(ctx, ec, doc.Kind, events.Event{Type: typ, CourseID: &courseID, SubjectType: "document", SubjectID: &doc.ID,
		Payload: map[string]any{"kind": doc.Kind, "version_id": version}})
}

type DocumentPublishIn struct {
	tool.InCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty" jsonschema:"which version to publish; if omitted, the latest when the call was made (for a proposal, when it was proposed)"`
}

func documentPublish() tool.Tool {
	return tool.Define(tool.Spec[DocumentPublishIn, DocumentVersionOut]{
		Name: "document.publish",
		Description: "Make a version of a document the one students read: the latest, or any earlier one. " +
			"Work already submitted stays pinned to the version that was published when it was handed in.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/publish"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentPublishIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		// "The latest" is the latest the proposer read. A version added while
		// the proposal waits has been read by nobody who asked for it to be
		// published, and approving must not put it in front of the class, so
		// the proposal names the version it was made about.
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in DocumentPublishIn) (DocumentPublishIn, error) {
			if in.VersionID != nil {
				return in, nil
			}
			v, err := q.GetLatestVersion(ctx, in.DocumentID)
			if errors.Is(err, pgx.ErrNoRows) {
				return in, apperr.Precondition("there is no such version of this document to publish")
			}
			if err != nil {
				return in, err
			}
			in.VersionID = &v.ID
			return in, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentPublishIn) (DocumentVersionOut, error) {
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentVersionOut{}, err
			}
			if !courseLevel(doc.Kind) {
				return DocumentVersionOut{}, apperr.Precondition("a %s file is not published; it is visible to whoever may see its owner", doc.Kind)
			}
			if doc.Status != "active" {
				return DocumentVersionOut{}, apperr.Conflicts("the document is archived")
			}
			var v dbq.DocumentVersion
			if in.VersionID != nil {
				v, err = ec.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: *in.VersionID, DocumentID: doc.ID})
			} else {
				v, err = ec.Q.GetLatestVersion(ctx, doc.ID)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return DocumentVersionOut{}, apperr.Precondition("there is no such version of this document to publish")
			}
			if err != nil {
				return DocumentVersionOut{}, err
			}
			if doc.PublishedVersionID != nil && *doc.PublishedVersionID == v.ID {
				return DocumentVersionOut{}, apperr.Conflicts("that version is already the published one")
			}
			if v.PurgedAt != nil {
				return DocumentVersionOut{}, apperr.Precondition("that version was purged and holds nothing to publish").With("reason", "purged")
			}
			return DocumentVersionOut{VersionID: v.ID, Seq: v.Seq, Published: true}, publish(ctx, ec, in.CourseID, doc, v.ID)
		},
	})
}

type DocumentIDIn struct {
	tool.InCourse
	DocumentID uuid.UUID `json:"document_id"`
}

func documentArchive() tool.Tool {
	return tool.Define(tool.Spec[DocumentIDIn, OK]{
		Name: "document.archive",
		Description: "Retire a document. It disappears from lists and can no longer be edited until it is brought back with " +
			"document.unarchive; nothing is deleted, and " +
			"anything pinned to one of its versions still reads it. A submitted file can be archived only while its submission is a draft.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/archive"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentIDIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentIDIn) (OK, error) {
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return OK{}, err
			}
			// A submitted file is archived only while its submission is a
			// draft, and the state is read under the submission's lock, as
			// document.create reads it: an archive during the hand-in waits
			// for it, rather than taking away a file the hand-in counted.
			if doc.SubmissionID != nil {
				s, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: *doc.SubmissionID, CourseID: in.CourseID})
				if err != nil {
					return OK{}, err
				}
				if s.State != stateDraft {
					return OK{}, apperr.Conflicts("the submission has been handed in; its files no longer change")
				}
			}
			n, err := ec.Q.SetDocumentStatus(ctx, dbq.SetDocumentStatusParams{ID: doc.ID, Status: "archived"})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the document is already archived")
			}
			if err := emitFileEvent(ctx, ec, in.CourseID, doc, EventDocumentArchived, EventSubmissionFileArchived, EventFeedbackFileArchived, nil); err != nil {
				return OK{}, err
			}
			return OK{OK: true}, nil
		},
	})
}

// emitFileEvent emits news of a document under the type for its kind: a
// course-level document's by its own name, through emitDocumentEvent; an
// owned file's under its owner's type, and belonging to its student and
// assignment, as its creation did, so that feed scope applies to it and those
// who read the submission — not those who read drafts — are told.
func emitFileEvent(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, doc dbq.GetDocumentWithOwnerRow,
	courseType, submissionType, feedbackType string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["kind"] = doc.Kind
	ev := events.Event{Type: courseType, CourseID: &courseID, SubjectType: "document", SubjectID: &doc.ID, Payload: payload}
	switch doc.Kind {
	case kindSubmission:
		ev.Type, ev.StudentMemberID, ev.AssignmentID = submissionType, doc.SubmissionStudent, doc.SubmissionAssignment
	case kindFeedback:
		ev.Type, ev.StudentMemberID, ev.AssignmentID = feedbackType, doc.GradeStudent, doc.GradeAssignment
	}
	return emitDocumentEvent(ctx, ec, doc.Kind, ev)
}

// handedIn refuses a change to a submitted file once its submission has
// been handed in: its files are frozen with it. The state is read under the
// submission's lock, the one the hand-in takes, as document.create reads it.
func handedIn(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, doc dbq.GetDocumentWithOwnerRow) error {
	if doc.SubmissionID == nil {
		return nil
	}
	s, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: *doc.SubmissionID, CourseID: courseID})
	if err != nil {
		return err
	}
	if s.State != stateDraft {
		return apperr.Conflicts("the submission has been handed in; its files no longer change")
	}
	return nil
}

// ---------------------------------------------------------------------------
// document.update, unarchive, purge
// ---------------------------------------------------------------------------

type DocumentUpdateIn struct {
	tool.InCourse
	DocumentID uuid.UUID `json:"document_id"`
	Title      *string   `json:"title,omitempty"`
	SortOrder  *int32    `json:"sort_order,omitempty"`
}

type DocumentChangeOut struct {
	Changed bool `json:"changed" jsonschema:"false when the document already was so: nothing was done"`
}

// documentUpdate renames a document or moves it in its list. It is what the
// document is called, not what it says: its versions are untouched, so an
// archived or purged document may be renamed too — a title that named what
// was purged, say. A submitted file keeps its name once handed in, as
// everything else about it.
func documentUpdate() tool.Tool {
	return tool.Define(tool.Spec[DocumentUpdateIn, DocumentChangeOut]{
		Name: "document.update",
		Description: "Rename a document, or change its place in the list (sort_order), for whoever may write that kind of " +
			"document: material, instructions and rubrics with document_write, a submitted file with submission_write while " +
			"its submission is a draft, a feedback file as its grade is written. Its versions are untouched; an archived or " +
			"purged document may be renamed as well. Giving what it already is changes nothing (changed: false).",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentUpdateIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentUpdateIn) (DocumentChangeOut, error) {
			if in.Title == nil && in.SortOrder == nil {
				return DocumentChangeOut{}, apperr.Invalid("give title, sort_order or both")
			}
			if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
				return DocumentChangeOut{}, apperr.Invalid("title cannot be empty")
			}
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentChangeOut{}, err
			}
			if err := handedIn(ctx, ec, in.CourseID, doc); err != nil {
				return DocumentChangeOut{}, err
			}
			// Under the document's lock, and read again: two renames take
			// turns, and neither puts back what the other changed.
			if err := ec.Q.LockDocument(ctx, doc.ID); err != nil {
				return DocumentChangeOut{}, err
			}
			if doc, err = loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID); err != nil {
				return DocumentChangeOut{}, err
			}
			title, order := doc.Title, doc.SortOrder
			if in.Title != nil {
				title = *in.Title
			}
			if in.SortOrder != nil {
				order = *in.SortOrder
			}
			if title == doc.Title && order == doc.SortOrder {
				return DocumentChangeOut{}, nil
			}
			if err := ec.Q.UpdateDocumentDetails(ctx, dbq.UpdateDocumentDetailsParams{ID: doc.ID, Title: title, SortOrder: order}); err != nil {
				return DocumentChangeOut{}, err
			}
			payload := map[string]any{"title_changed": title != doc.Title, "sort_order_changed": order != doc.SortOrder}
			return DocumentChangeOut{Changed: true}, emitFileEvent(ctx, ec, in.CourseID, doc,
				EventDocumentUpdated, EventSubmissionFileUpdated, EventFeedbackFileUpdated, payload)
		},
	})
}

// documentUnarchive is document.archive undone: the document is back in
// lists and editable, and a published version is read again. For feedback on
// a posted grade that is a release, as archiving it was a withdrawal, and it
// takes what that took. A submitted file comes back only while its
// submission is a draft, as it was archived; a purged document stays
// archived, with nothing in it to bring back.
func documentUnarchive() tool.Tool {
	return tool.Define(tool.Spec[DocumentIDIn, OK]{
		Name: "document.unarchive",
		Description: "Bring back an archived document: it is in lists again, can be edited, and its published version is " +
			"read again by whoever may read that kind of document. For whoever may archive it: feedback on a posted grade " +
			"takes grade_post as well, and a submitted file comes back only while its submission is a draft. A purged " +
			"document stays archived.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/unarchive"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentIDIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentIDIn) (OK, error) {
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return OK{}, err
			}
			if err := handedIn(ctx, ec, in.CourseID, doc); err != nil {
				return OK{}, err
			}
			if err := ec.Q.LockDocument(ctx, doc.ID); err != nil {
				return OK{}, err
			}
			if doc, err = loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID); err != nil {
				return OK{}, err
			}
			if doc.PurgedAt != nil {
				return OK{}, apperr.Precondition("the document was purged, and stays archived").With("reason", "purged")
			}
			n, err := ec.Q.SetDocumentStatus(ctx, dbq.SetDocumentStatusParams{ID: doc.ID, Status: "active"})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the document is not archived")
			}
			return OK{OK: true}, emitFileEvent(ctx, ec, in.CourseID, doc,
				EventDocumentUnarchived, EventSubmissionFileRestored, EventFeedbackFileRestored, nil)
		},
	})
}

type DocumentPurgeIn struct {
	CourseID   uuid.UUID  `json:"course_id"`
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty" jsonschema:"one version to purge; every version of the document, and the document with them, if omitted"`
	Reason     string     `json:"reason" jsonschema:"why, 1 to 500 characters: kept on the tombstone, and shown to whoever reads what was purged"`
}

type DocumentPurgeOut struct {
	PurgedVersions int `json:"purged_versions"`
	FilesRemoved   int `json:"files_removed" jsonschema:"how many files were deleted from storage"`
}

// documentPurge removes what was uploaded by mistake — personal data, say —
// from a course's material, instructions or rubrics: a version, or a whole
// document. Its text and its file go, the file deleted from storage; the
// version stays as a tombstone, saying who removed it, when and why, so the
// history shows that something was there. It is an administrator's, from
// outside the course, as removing data is not something a seat's permissions
// reach: a platform administrator anywhere, a department administrator in
// the courses of the departments they cover. It is allowed in an archived
// course too, where nothing else is written: what must go must go.
//
// What pinned a purged version — a submission its instructions, a grade its
// rubric, the document its published version — still names it, and reads
// the tombstone; the work and the grade are unchanged. A submitted file and a
// feedback file are their submission's and grade's, and are not purged here.
//
// The file is deleted last, once the rows say it is gone. Should the call
// then fail to commit, the file is gone and the rows still name it; the call
// is made again with the same key, and deleting what is gone already is not
// an error.
func documentPurge(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentPurgeIn, DocumentPurgeOut]{
		Name: "document.purge",
		Description: "Purge a version of a course's material, instructions or rubric, or the whole document, uploaded by " +
			"mistake: its text and file are removed, the file deleted from storage, and a tombstone says who removed them, " +
			"when and why. A purged document is archived for good. Work handed in under a purged version of the " +
			"instructions still names it and reads the tombstone; grades are untouched. Submitted and feedback files are " +
			"not purged. For a platform administrator, or a department administrator for the courses of the departments " +
			"they administer; it works in an archived course too.",
		Kind: tool.Write, Gate: administrators, OnArchived: true,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/purge"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentPurgeIn) (tool.Target, error) {
			t, err := platformCourse(ctx, q, in.CourseID)
			if err != nil {
				return t, err
			}
			if _, err := loadDocument(ctx, q, in.CourseID, in.DocumentID); err != nil {
				return tool.Target{}, err
			}
			t.Type, t.ID = "document", &in.DocumentID
			return t, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentPurgeIn) (DocumentPurgeOut, error) {
			reason := strings.TrimSpace(in.Reason)
			if reason == "" || utf8.RuneCountInString(reason) > 500 {
				return DocumentPurgeOut{}, apperr.Invalid("reason is 1 to 500 characters")
			}
			// The document's lock, as a new version takes it: nothing is
			// added to it while it is purged.
			if err := ec.Q.LockDocument(ctx, in.DocumentID); err != nil {
				return DocumentPurgeOut{}, err
			}
			doc, err := loadDocument(ctx, ec.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentPurgeOut{}, err
			}
			if !courseLevel(doc.Kind) {
				return DocumentPurgeOut{}, apperr.Precondition("a %s file is its %s's, and is archived with it, not purged", doc.Kind, map[string]string{
					kindSubmission: "submission", kindFeedback: "grade"}[doc.Kind]).With("reason", "owned_file")
			}
			if doc.PurgedAt != nil {
				return DocumentPurgeOut{}, apperr.Conflicts("the document has been purged already").With("reason", "already_purged")
			}
			versions, err := ec.Q.ListVersionsToPurge(ctx, doc.ID)
			if err != nil {
				return DocumentPurgeOut{}, err
			}
			if in.VersionID != nil {
				v, err := ec.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: *in.VersionID, DocumentID: doc.ID})
				if errors.Is(err, pgx.ErrNoRows) {
					return DocumentPurgeOut{}, apperr.Missing("no such version of this document")
				}
				if err != nil {
					return DocumentPurgeOut{}, err
				}
				if v.PurgedAt != nil {
					return DocumentPurgeOut{}, apperr.Conflicts("that version has been purged already").With("reason", "already_purged")
				}
				files, err := ec.Q.ListVersionFiles(ctx, v.ID)
				if err != nil {
					return DocumentPurgeOut{}, err
				}
				keys := make([]string, len(files))
				for i, f := range files {
					keys[i] = f.StorageKey
				}
				versions = []dbq.ListVersionsToPurgeRow{{ID: v.ID, StorageKeys: keys}}
			}
			var files []string
			for _, v := range versions {
				files = append(files, v.StorageKeys...)
			}
			if len(files) > 0 && d.Blob == nil {
				return DocumentPurgeOut{}, apperr.Precondition("this installation has no file storage configured, so the file cannot be removed")
			}
			out := DocumentPurgeOut{}
			for _, v := range versions {
				n, err := ec.Q.PurgeVersion(ctx, dbq.PurgeVersionParams{ID: v.ID, PurgedAt: &ec.Now, PurgedByActorID: &ec.Actor.ID, PurgeReason: &reason})
				if err != nil {
					return DocumentPurgeOut{}, err
				}
				out.PurgedVersions += int(n)
			}
			payload := map[string]any{"versions": out.PurgedVersions}
			if in.VersionID != nil {
				payload["version_id"] = *in.VersionID
			} else {
				if _, err := ec.Q.PurgeDocument(ctx, dbq.PurgeDocumentParams{ID: doc.ID, PurgedAt: &ec.Now, PurgedByActorID: &ec.Actor.ID,
					PurgeReason: &reason}); err != nil {
					return DocumentPurgeOut{}, err
				}
			}
			if err := emitFileEvent(ctx, ec, in.CourseID, doc, EventDocumentPurged, "", "", payload); err != nil {
				return DocumentPurgeOut{}, err
			}
			for _, key := range files {
				if err := d.Blob.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
					return DocumentPurgeOut{}, err
				}
				out.FilesRemoved++
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

type DocumentSummary struct {
	ID                 uuid.UUID  `json:"id"`
	Kind               string     `json:"kind"`
	Title              string     `json:"title"`
	PublishedVersionID *uuid.UUID `json:"published_version_id,omitempty"`
	SortOrder          int32      `json:"sort_order"`
	Status             string     `json:"status"`
	CreatedAt          time.Time  `json:"created_at"`
	PurgedAt           *time.Time `json:"purged_at,omitempty" jsonschema:"when every version of it was purged: it is archived for good"`
}

// Purge is the tombstone of what an administrator removed: who, when, why.
type Purge struct {
	At        time.Time `json:"at"`
	ByActorID uuid.UUID `json:"by_actor_id" jsonschema:"the administrator who purged it"`
	Reason    string    `json:"reason"`
}

func purgeOf(at *time.Time, by *uuid.UUID, reason *string) *Purge {
	if at == nil || by == nil || reason == nil {
		return nil
	}
	return &Purge{At: *at, ByActorID: *by, Reason: *reason}
}

type DocumentListIn struct {
	tool.InCourse
	Kind            *string `json:"kind,omitempty" jsonschema:"material, instructions or rubric; all that the caller may read if omitted"`
	IncludeArchived bool    `json:"include_archived,omitempty"`
	Page
}

type DocumentListOut struct {
	Documents []DocumentSummary `json:"documents"`
	Next      *uuid.UUID        `json:"next,omitempty"`
}

func documentList() tool.Tool {
	return tool.Define(tool.Spec[DocumentListIn, DocumentListOut]{
		Name: "document.list",
		Description: "The course's material, instructions and rubrics — those kinds the caller has permission to read. " +
			"Documents with no published version are shown only to members who can read drafts. Instructions and rubrics " +
			"follow their assignment: unless the caller writes assignments, they are shown only once a published assignment " +
			"in the caller's scope refers to them. Submitted files and feedback files are not here: they come with " +
			"submission.get and grade.get.",
		Kind: tool.Read, Gate: tool.Gate{Any: true, Perms: []domain.Perm{domain.PermDocumentRead, domain.PermRubricRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/documents"},
		Resolve: func(_ context.Context, _ dbq.Querier, in DocumentListIn) (tool.Target, error) {
			t := tool.Target{CourseID: in.CourseID, Type: "document", Perms: []domain.Perm{domain.PermDocumentRead}}
			if in.Kind != nil {
				if !courseLevel(*in.Kind) {
					return t, apperr.Invalid("kind must be material, instructions or rubric")
				}
				t.Perms = []domain.Perm{readPerm(*in.Kind)}
			}
			return t, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DocumentListIn) (DocumentListOut, error) {
			// With no kind named, the list is every kind the caller may read,
			// rather than a refusal because one of them is out of reach.
			var kinds []string
			for _, k := range []string{kindMaterial, kindInstructions, kindRubric} {
				if (in.Kind == nil || *in.Kind == k) && rc.Member.Perm(readPerm(k)).Allowed() {
					kinds = append(kinds, k)
				}
			}
			rows, err := rc.Q.ListCourseDocuments(ctx, dbq.ListCourseDocumentsParams{
				CourseID: in.CourseID, After: in.after(), MaxRows: in.limit(), Kinds: kinds,
				IncludeUnpublished: rc.Member.Perm(domain.PermDocumentReadDraft).Allowed(),
				IncludeArchived:    in.IncludeArchived && rc.Member.Perm(domain.PermDocumentReadDraft).Allowed(),
				WritesAssignments:  canSeeUnpublished(rc.Member),
				AssignmentAll:      rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
				PrincipalID: rc.Scope.PrincipalID, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll,
			})
			out := DocumentListOut{Documents: make([]DocumentSummary, 0, len(rows))}
			for _, r := range rows {
				out.Documents = append(out.Documents, DocumentSummary{ID: r.ID, Kind: r.Kind, Title: r.Title,
					PublishedVersionID: r.PublishedVersionID, SortOrder: r.SortOrder, Status: r.Status, CreatedAt: r.CreatedAt,
					PurgedAt: r.PurgedAt})
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type DocumentGetIn struct {
	tool.InCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty" jsonschema:"a specific version; otherwise the published one, or the latest for members who can read drafts"`
}

type VersionView struct {
	ID             uuid.UUID `json:"id"`
	Seq            int32     `json:"seq"`
	BodyMD         *string   `json:"body_md,omitempty"`
	DownloadURL    *string   `json:"download_url,omitempty" jsonschema:"a short-lived URL for the file, if the version has one"`
	ContentType    *string   `json:"content_type,omitempty"`
	ByteSize       *int64    `json:"byte_size,omitempty"`
	Checksum       *string   `json:"checksum,omitempty"`
	AuthorMemberID uuid.UUID `json:"author_member_id"`
	CreatedAt      time.Time `json:"created_at"`
	Published      bool      `json:"published"`
	Purged         *Purge    `json:"purged,omitempty" jsonschema:"the version was purged: its text and file are gone, and this says who removed them, when and why. Work handed in under it still names it"`
	Text           *TextView `json:"text,omitempty" jsonschema:"the version's text version: its file transcribed into Markdown, for a version with a file of material, instructions or a rubric; absent for any other"`
}

type DocumentGetOut struct {
	DocumentSummary
	SubmissionID *uuid.UUID   `json:"submission_id,omitempty"`
	GradeID      *uuid.UUID   `json:"grade_id,omitempty"`
	Purged       *Purge       `json:"purged,omitempty" jsonschema:"the whole document was purged: who, when and why"`
	Version      *VersionView `json:"version,omitempty" jsonschema:"absent when the document has no version the caller may read"`
}

func documentGet(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentGetIn, DocumentGetOut]{
		Name: "document.get",
		Description: "Read a document: its text, and a short-lived URL for its file if it has one. Students get the " +
			"published version; members who can read drafts get the latest. A specific version can be asked for by id — " +
			"always allowed if it is the one your own submission was handed in under.",
		Kind: tool.Read, Gate: anyDocumentRead,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/documents/{document_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentGetIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, readPerm)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DocumentGetIn) (DocumentGetOut, error) {
			doc, v, err := readableVersion(ctx, rc, in.CourseID, in.DocumentID, in.VersionID)
			if err != nil {
				return DocumentGetOut{}, err
			}
			out := DocumentGetOut{SubmissionID: doc.SubmissionID, GradeID: doc.GradeID,
				Purged: purgeOf(doc.PurgedAt, doc.PurgedByActorID, doc.PurgeReason),
				DocumentSummary: DocumentSummary{ID: doc.ID, Kind: doc.Kind, Title: doc.Title, PublishedVersionID: doc.PublishedVersionID,
					SortOrder: doc.SortOrder, Status: doc.Status, CreatedAt: doc.CreatedAt, PurgedAt: doc.PurgedAt}}
			if v == nil {
				return out, nil
			}
			view := VersionView{ID: v.ID, Seq: v.Seq, BodyMD: v.BodyMd, ContentType: v.ContentType, ByteSize: v.ByteSize,
				Checksum: v.Checksum, AuthorMemberID: v.AuthorMemberID, CreatedAt: v.CreatedAt,
				Published: doc.PublishedVersionID != nil && *doc.PublishedVersionID == v.ID,
				Purged:    purgeOf(v.PurgedAt, v.PurgedByActorID, v.PurgeReason)}
			if v.StorageKey != nil && d.Blob != nil {
				url, err := d.Blob.PresignGet(ctx, *v.StorageKey, downloadTTL)
				if err != nil {
					return DocumentGetOut{}, err
				}
				view.DownloadURL = &url
			}
			if view.Text, err = firstFileText(ctx, rc.Q, v.ID); err != nil {
				return DocumentGetOut{}, err
			}
			out.Version = &view
			return out, nil
		},
	})
}

// firstFileText is the text version of a version's first file, for
// document.get: with its text when it is one part. nil for a version that
// has none.
func firstFileText(ctx context.Context, q dbq.Querier, version uuid.UUID) (*TextView, error) {
	files, err := q.ListVersionFiles(ctx, version)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	key := dbq.GetTextViewParams{VersionID: version, FileID: files[0].ID}
	r, err := q.GetTextView(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := textView(r.Status, r.Source, r.Pages, r.Model, r.Reason, r.Revision, r.ProducedAt, r.EditedByMemberID, r.EditedAt,
		r.UpdatedAt, r.Bytes, r.EditedByName)
	if r.Status == textDone && r.Bytes <= TextPartBytes {
		b, err := q.GetTextBody(ctx, dbq.GetTextBodyParams(key))
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

// readableVersion is the version of a document a read shows its caller, and
// the document: the one named, if the caller may read it; otherwise the
// published one, or the latest for members who can read drafts. It is nil,
// with no error, for a document the caller may read that has no version for
// them. A document or a version the caller may not read is not found.
func readableVersion(ctx context.Context, rc *tool.ReadCtx, courseID, documentID uuid.UUID, versionID *uuid.UUID) (dbq.GetDocumentWithOwnerRow, *dbq.DocumentVersion, error) {
	doc, err := loadDocument(ctx, rc.Q, courseID, documentID)
	if err != nil {
		return doc, nil, err
	}
	if feedbackWithheld(doc, rc.Member) {
		return doc, nil, apperr.Missing("no such document in this course")
	}
	drafts := rc.Member.Perm(domain.PermDocumentReadDraft).Allowed()
	if courseLevel(doc.Kind) && doc.PublishedVersionID == nil && !drafts {
		return doc, nil, apperr.Missing("no such document in this course")
	}
	// To anyone who cannot read drafts, a course-level document is
	// there while it is published and not archived — archiving is
	// the only way to withdraw something published, so it has to
	// withdraw it from anyone who kept the id. Instructions and a
	// rubric are the assignment's, and follow it: to anyone who does
	// not write assignments they are there only while a published
	// assignment in their scope refers to them, drafts or no drafts.
	// What stays readable regardless is a version someone's
	// submission is pinned to, named by id below, because that is
	// the record of what they were told.
	withdrawn := courseLevel(doc.Kind) && doc.Status == "archived" && !drafts
	if courseLevel(doc.Kind) && doc.Kind != kindMaterial && !withdrawn {
		withdrawn, err = assignmentWithheld(ctx, rc, doc.ID)
		if err != nil {
			return doc, nil, err
		}
	}
	if withdrawn && versionID == nil {
		return doc, nil, apperr.Missing("no such document in this course")
	}

	var v dbq.DocumentVersion
	switch {
	case versionID != nil:
		v, err = rc.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: *versionID, DocumentID: doc.ID})
		if err == nil && (withdrawn || (!drafts && (doc.PublishedVersionID == nil || *doc.PublishedVersionID != v.ID))) {
			// Not the published one and no right to drafts. One more
			// way in: it is what the caller's own work was pinned to.
			pinned, perr := rc.Q.VersionPinnedInScope(ctx, dbq.VersionPinnedInScopeParams{VersionID: &v.ID,
				StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
				PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll})
			if perr != nil {
				return doc, nil, perr
			}
			if !pinned {
				err = pgx.ErrNoRows
			}
		}
	case drafts && courseLevel(doc.Kind):
		v, err = rc.Q.GetLatestVersion(ctx, doc.ID)
	case doc.PublishedVersionID != nil:
		v, err = rc.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: *doc.PublishedVersionID, DocumentID: doc.ID})
	default:
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if versionID != nil {
			return doc, nil, apperr.Missing("no such version of this document")
		}
		return doc, nil, nil
	}
	if err != nil {
		return doc, nil, err
	}
	return doc, &v, nil
}

type VersionSummary struct {
	ID             uuid.UUID  `json:"id"`
	Seq            int32      `json:"seq"`
	HasFile        bool       `json:"has_file"`
	ContentType    *string    `json:"content_type,omitempty"`
	ByteSize       *int64     `json:"byte_size,omitempty"`
	AuthorMemberID uuid.UUID  `json:"author_member_id"`
	CreatedAt      time.Time  `json:"created_at"`
	Published      bool       `json:"published"`
	PurgedAt       *time.Time `json:"purged_at,omitempty" jsonschema:"when its text and file were purged; document.get of it says who and why"`
	Text           *TextView  `json:"text,omitempty" jsonschema:"its text version, without the text: document.text reads it"`
}

type DocumentVersionsOut struct {
	Versions []VersionSummary `json:"versions"`
}

// feedbackWithheld: feedback on a grade the student cannot see yet is not
// theirs to read either. Nor is feedback archived from a posted grade:
// archiving it takes back a release (feedbackWritePerms), so like withdrawn
// material it is withdrawn from anyone who kept the id. Those who grade still
// read it. It holds for the version list as for the document.
func feedbackWithheld(doc dbq.GetDocumentWithOwnerRow, m *domain.Member) bool {
	return doc.Kind == kindFeedback && (doc.GradePostedAt == nil || doc.GradeSupersededBy != nil || doc.Status == "archived") && !seesDrafts(m)
}

// assignmentWithheld: an instructions or rubric document is withheld from a
// member who does not write assignments unless a published assignment in
// their scope refers to it — the document is visible exactly when the
// assignment is.
func assignmentWithheld(ctx context.Context, rc *tool.ReadCtx, docID uuid.UUID) (bool, error) {
	if canSeeUnpublished(rc.Member) {
		return false, nil
	}
	inUse, err := rc.Q.DocumentInUseByPublishedAssignment(ctx, dbq.DocumentInUseByPublishedAssignmentParams{
		DocumentID: &docID, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
		PrincipalID: rc.Scope.PrincipalID, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll})
	return !inUse, err
}

func documentVersions() tool.Tool {
	return tool.Define(tool.Spec[DocumentIDIn, DocumentVersionsOut]{
		Name:        "document.versions",
		Description: "Every version of a document, oldest first, with which one is published. For members who can read drafts.",
		Kind:        tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentReadDraft}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/documents/{document_id}/versions"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentIDIn) (tool.Target, error) {
			t, err := documentTarget(ctx, q, in.CourseID, in.DocumentID, readPerm)
			// Both: the right to drafts, and the right to this kind of document.
			t.Perms = append(t.Perms, domain.PermDocumentReadDraft)
			return t, err
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DocumentIDIn) (DocumentVersionsOut, error) {
			doc, err := loadDocument(ctx, rc.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentVersionsOut{}, err
			}
			if feedbackWithheld(doc, rc.Member) {
				return DocumentVersionsOut{}, apperr.Missing("no such document in this course")
			}
			if courseLevel(doc.Kind) && doc.Kind != kindMaterial {
				if withheld, err := assignmentWithheld(ctx, rc, doc.ID); err != nil {
					return DocumentVersionsOut{}, err
				} else if withheld {
					return DocumentVersionsOut{}, apperr.Missing("no such document in this course")
				}
			}
			rows, err := rc.Q.ListVersions(ctx, doc.ID)
			if err != nil {
				return DocumentVersionsOut{}, err
			}
			texts, err := textsOf(ctx, rc.Q, doc.ID)
			if err != nil {
				return DocumentVersionsOut{}, err
			}
			// Each version's text is its first file's.
			files, err := rc.Q.ListDocumentFiles(ctx, doc.ID)
			if err != nil {
				return DocumentVersionsOut{}, err
			}
			first := map[uuid.UUID]uuid.UUID{}
			for _, f := range files {
				if f.Position == 1 {
					first[f.VersionID] = f.ID
				}
			}
			out := DocumentVersionsOut{Versions: make([]VersionSummary, 0, len(rows))}
			for _, r := range rows {
				out.Versions = append(out.Versions, VersionSummary{ID: r.ID, Seq: r.Seq, HasFile: r.HasFile, ContentType: r.ContentType,
					ByteSize: r.ByteSize, AuthorMemberID: r.AuthorMemberID, CreatedAt: r.CreatedAt,
					Published: doc.PublishedVersionID != nil && *doc.PublishedVersionID == r.ID, PurgedAt: r.PurgedAt,
					Text: texts[first[r.ID]]})
			}
			return out, nil
		},
	})
}
