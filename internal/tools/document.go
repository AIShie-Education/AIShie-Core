package tools

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
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
		documentList(), documentGet(d), documentVersions()}
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

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

type UploadURLIn struct {
	tool.InCourse
	Kind        string `json:"kind" jsonschema:"what the file is for: material, instructions, rubric, submission or feedback"`
	ContentType string `json:"content_type" jsonschema:"the file's media type, e.g. application/pdf; the upload must send the same"`
}

type UploadURLOut struct {
	UploadURL   string            `json:"upload_url" jsonschema:"PUT the file's bytes here, once, within the window"`
	Headers     map[string]string `json:"headers" jsonschema:"headers the PUT must carry"`
	UploadToken string            `json:"upload_token" jsonschema:"hand this to document.create, document.add_version or grade.submit to attach what you uploaded"`
	ExpiresAt   time.Time         `json:"expires_at"`
	MaxBytes    int64             `json:"max_bytes"`
}

// UploadPrefix begins the key of every upload: courses/<course>/<upload>.
// These keys, and the final keys that attaching moves them to, are all the
// server ever writes to the store. The orphan sweep looks at nothing else, so
// a bucket or directory that holds other things as well loses none of them.
const UploadPrefix = "courses/"

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
				return UploadURLOut{}, apperr.Precondition("this installation has no file storage configured")
			}
			if strings.TrimSpace(in.ContentType) == "" || len(in.ContentType) > 200 {
				return UploadURLOut{}, apperr.Invalid("content_type is required")
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
			key := UploadPrefix + in.CourseID.String() + "/" + ids.New().String()
			url, headers, err := d.Blob.PresignPut(ctx, key, in.ContentType, uploadWindow)
			if err != nil {
				return UploadURLOut{}, err
			}
			expires := rc.Now.Add(uploadWindow)
			return UploadURLOut{
				UploadURL: url, Headers: headers, ExpiresAt: expires, MaxBytes: d.MaxUploadBytes,
				UploadToken: d.Uploads.SignUpload(blob.UploadClaim{Key: key, CourseID: in.CourseID, MemberID: rc.Member.ID,
					Purpose: in.Kind, ContentType: in.ContentType, Expires: expires.Unix()}),
			}, nil
		},
	})
}

// upload is a file that has been uploaded and checked, ready to be recorded.
type upload struct {
	key  string
	info blob.Info
}

// claimUpload turns an upload token into a file to attach. The token proves
// that this member of this course was given the key for this purpose; the
// store is asked whether anything is actually there, and how big it is.
func claimUpload(ctx context.Context, d Deps, q dbq.Querier, m *domain.Member, courseID uuid.UUID, kind, token string, finalize bool) (upload, error) {
	if d.Blob == nil {
		return upload{}, apperr.Precondition("this installation has no file storage configured")
	}
	c, err := d.Uploads.VerifyUpload(token)
	if err != nil {
		return upload{}, apperr.Invalid("upload_token is not valid")
	}
	if c.CourseID != courseID || c.MemberID != m.ID || c.Purpose != kind {
		return upload{}, apperr.Forbid("that upload was issued to someone else, or for something else")
	}
	// An upload URL can be written to again for as long as it is valid, so
	// the object is moved, on attaching, to a final key that no upload URL
	// was ever issued for; that key is what the version records. The lock
	// makes "is it attached already?" and the move one step: without it a
	// second attach, racing the first, could copy different bytes over an
	// object a committed version already points at.
	final := d.Blob.FinalKey(c.Key)
	if err := q.LockStorageKey(ctx, final); err != nil {
		return upload{}, err
	}
	if used, err := q.StorageKeyInUse(ctx, &final); err != nil {
		return upload{}, err
	} else if used {
		return upload{}, apperr.Conflicts("that upload is already attached to a document")
	}
	if staged, err := d.Blob.Stat(ctx, c.Key); errors.Is(err, blob.ErrNotFound) {
		// Nothing staged: either nothing was uploaded, or an earlier attach
		// moved it and then its transaction did not commit — the move is
		// outside the transaction. The object is then at the final key,
		// which nothing points at (checked above, under the lock) and no
		// upload URL was ever issued for: it can only be this token's own
		// upload, and is attached as it is.
		info, err := d.Blob.Stat(ctx, final)
		if errors.Is(err, blob.ErrNotFound) {
			return upload{}, apperr.Precondition("nothing has been uploaded to that URL yet")
		}
		if err != nil {
			return upload{}, err
		}
		if info.ContentType == "" {
			info.ContentType = c.ContentType
		}
		return upload{key: final, info: info}, nil
	} else if err != nil {
		return upload{}, err
	} else if staged.Size > d.MaxUploadBytes {
		_ = d.Blob.Delete(ctx, c.Key)
		return upload{}, apperr.Precondition("the file is %d bytes; the limit is %d", staged.Size, d.MaxUploadBytes)
	}
	if !finalize {
		// A dry run, for Validate: everything is checked and nothing moves.
		return upload{key: final}, nil
	}
	// What is recorded describes the final object, read after the move.
	info, err := d.Blob.Finalize(ctx, c.Key)
	if errors.Is(err, blob.ErrNotFound) {
		return upload{}, apperr.Precondition("nothing has been uploaded to that URL yet")
	}
	if err != nil {
		return upload{}, err
	}
	if info.Size > d.MaxUploadBytes {
		_ = d.Blob.Delete(ctx, final)
		return upload{}, apperr.Precondition("the file is %d bytes; the limit is %d", info.Size, d.MaxUploadBytes)
	}
	if info.ContentType == "" {
		info.ContentType = c.ContentType
	}
	return upload{key: final, info: info}, nil
}

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
				int(OrphanGrace.Hours()))
		}
	}
	return nil
}

// Content is what a version holds: text, a file, or both.
type Content struct {
	BodyMD      *string `json:"body_md,omitempty" jsonschema:"markdown text"`
	UploadToken *string `json:"upload_token,omitempty" jsonschema:"from document.upload_url, after uploading the file"`
}

func (c Content) empty() bool { return (c.BodyMD == nil || *c.BodyMD == "") && c.UploadToken == nil }

// uploads is the upload token the content names, if any, for checkUploadAge.
func (c Content) uploads() []string {
	if c.UploadToken == nil {
		return nil
	}
	return []string{*c.UploadToken}
}

// insertVersion writes one version. The author is the calling member, who is
// a member of the document's course because the call was authorized in it —
// which is the whole of the rule that a version's author belongs to its
// document's course.
func insertVersion(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID, documentID uuid.UUID, kind string, seq int32, c Content) (uuid.UUID, error) {
	row := dbq.InsertDocumentVersionParams{ID: ids.New(), DocumentID: documentID, Seq: seq, BodyMd: c.BodyMD,
		AuthorMemberID: ec.Member.ID, CreatedAt: ec.Now}
	if c.UploadToken != nil {
		up, err := claimUpload(ctx, d, ec.Q, ec.Member, courseID, kind, *c.UploadToken, true)
		if err != nil {
			return uuid.Nil, err
		}
		row.StorageKey, row.ContentType, row.ByteSize, row.Checksum = &up.key, &up.info.ContentType, &up.info.Size, &up.info.Checksum
	}
	return row.ID, ec.Q.InsertDocumentVersion(ctx, row)
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
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty" jsonschema:"absent when the document was created empty"`
}

func documentCreate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentCreateIn, DocumentCreateOut]{
		Name: "document.create",
		Description: "Create a document. Material, instructions and rubrics are versioned and start unpublished — students " +
			"see nothing until document.publish. A submission file is attached to a draft submission, and a feedback file " +
			"to a grade; those have exactly one version and are given their content here.",
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
		// The file must still be there when the proposal is approved.
		Pin: func(ctx context.Context, _ dbq.Querier, now time.Time, in DocumentCreateIn) (DocumentCreateIn, error) {
			return in, checkUploadAge(ctx, d, now, in.uploads()...)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DocumentCreateIn) (DocumentCreateOut, error) {
			if strings.TrimSpace(in.Title) == "" {
				return DocumentCreateOut{}, apperr.Invalid("title is required")
			}
			ev := events.Event{Type: EventDocumentCreated, CourseID: &in.CourseID, SubjectType: "document", Payload: map[string]any{"kind": in.Kind}}
			switch in.Kind {
			case kindSubmission:
				// The freeze trigger guards the submission row, not the files
				// beside it. Once handed in, nothing more may be added.
				s, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: *in.SubmissionID, CourseID: in.CourseID})
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
				if g.SupersededBy != nil {
					return DocumentCreateOut{}, apperr.Conflicts("that grade has been replaced; attach feedback to the grade that replaced it")
				}
				if g.Origin != "entered" {
					return DocumentCreateOut{}, apperr.Precondition("a computed total is arithmetic, not a judgement; feedback goes on the grades beneath it")
				}
				ev.Type, ev.StudentMemberID, ev.AssignmentID = EventFeedbackFileAdded, &g.StudentMemberID, g.AssignmentID
			}
			if !courseLevel(in.Kind) && in.empty() {
				return DocumentCreateOut{}, apperr.Invalid("a %s file has one version and needs its content now: body_md or upload_token", in.Kind)
			}

			out := DocumentCreateOut{DocumentID: ids.New()}
			if err := ec.Q.InsertDocument(ctx, dbq.InsertDocumentParams{ID: out.DocumentID, CourseID: in.CourseID, Kind: in.Kind,
				Title: in.Title, SubmissionID: in.SubmissionID, GradeID: in.GradeID, SortOrder: in.SortOrder, CreatedAt: ec.Now}); err != nil {
				return DocumentCreateOut{}, err
			}
			if !in.empty() {
				v, err := insertVersion(ctx, d, ec, in.CourseID, out.DocumentID, in.Kind, 1, in.Content)
				if err != nil {
					return DocumentCreateOut{}, err
				}
				out.VersionID = &v
				if !courseLevel(in.Kind) {
					// An owned file has no draft stage: it is what it is.
					if err := ec.Q.SetPublishedVersion(ctx, dbq.SetPublishedVersionParams{ID: out.DocumentID, PublishedVersionID: &v}); err != nil {
						return DocumentCreateOut{}, err
					}
				}
			}
			ev.SubjectID = &out.DocumentID
			ec.Emit(ev)
			return out, nil
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
	VersionID uuid.UUID `json:"version_id"`
	Seq       int32     `json:"seq"`
	Published bool      `json:"published"`
}

func documentAddVersion(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentAddVersionIn, DocumentVersionOut]{
		Name: "document.add_version",
		Description: "Edit material, instructions or a rubric by adding a version. Versions are never changed or removed. " +
			"The new version is a draft until it is published; what students read does not change until then.",
		Kind: tool.Write, Gate: anyDocumentWrite,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/documents/{document_id}/versions"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentAddVersionIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, writePerm)
		},
		// As document.create.
		Pin: func(ctx context.Context, _ dbq.Querier, now time.Time, in DocumentAddVersionIn) (DocumentAddVersionIn, error) {
			return in, checkUploadAge(ctx, d, now, in.uploads()...)
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
				return DocumentVersionOut{}, apperr.Invalid("a version needs content: body_md or upload_token")
			}
			last, err := ec.Q.MaxVersionSeq(ctx, doc.ID)
			if err != nil {
				return DocumentVersionOut{}, err
			}
			out := DocumentVersionOut{Seq: last + 1, Published: in.Publish}
			if out.VersionID, err = insertVersion(ctx, d, ec, in.CourseID, doc.ID, doc.Kind, out.Seq, in.Content); err != nil {
				return DocumentVersionOut{}, err
			}
			ec.Emit(events.Event{Type: EventDocumentVersionAdded, CourseID: &in.CourseID, SubjectType: "document", SubjectID: &doc.ID,
				Payload: map[string]any{"kind": doc.Kind, "seq": out.Seq}})
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
	ec.Emit(events.Event{Type: typ, CourseID: &courseID, SubjectType: "document", SubjectID: &doc.ID,
		Payload: map[string]any{"kind": doc.Kind, "version_id": version}})
	return nil
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
		Pin: func(ctx context.Context, q dbq.Querier, _ time.Time, in DocumentPublishIn) (DocumentPublishIn, error) {
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
		Description: "Retire a document. It disappears from lists and can no longer be edited; nothing is deleted, and " +
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
			if doc.SubmissionState != nil && *doc.SubmissionState != stateDraft {
				return OK{}, apperr.Conflicts("the submission has been handed in; its files no longer change")
			}
			n, err := ec.Q.SetDocumentStatus(ctx, dbq.SetDocumentStatusParams{ID: doc.ID, Status: "archived"})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the document is already archived")
			}
			// An owned file's event belongs to its student and assignment, as
			// its creation did, so that feed scope applies to it and those who
			// read the submission — not those who read drafts — see it go.
			ev := events.Event{Type: EventDocumentArchived, CourseID: &in.CourseID, SubjectType: "document", SubjectID: &doc.ID,
				Payload: map[string]any{"kind": doc.Kind}}
			switch doc.Kind {
			case kindSubmission:
				ev.Type, ev.StudentMemberID, ev.AssignmentID = EventSubmissionFileArchived, doc.SubmissionStudent, doc.SubmissionAssignment
			case kindFeedback:
				ev.Type, ev.StudentMemberID, ev.AssignmentID = EventFeedbackFileArchived, doc.GradeStudent, doc.GradeAssignment
			}
			ec.Emit(ev)
			return OK{OK: true}, nil
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
			})
			out := DocumentListOut{Documents: make([]DocumentSummary, 0, len(rows))}
			for _, r := range rows {
				out.Documents = append(out.Documents, DocumentSummary{ID: r.ID, Kind: r.Kind, Title: r.Title,
					PublishedVersionID: r.PublishedVersionID, SortOrder: r.SortOrder, Status: r.Status, CreatedAt: r.CreatedAt})
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
}

type DocumentGetOut struct {
	DocumentSummary
	SubmissionID *uuid.UUID   `json:"submission_id,omitempty"`
	GradeID      *uuid.UUID   `json:"grade_id,omitempty"`
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
			doc, err := loadDocument(ctx, rc.Q, in.CourseID, in.DocumentID)
			if err != nil {
				return DocumentGetOut{}, err
			}
			// Feedback on a grade the student cannot see yet is not theirs to
			// read either. Nor is feedback archived from a posted grade:
			// archiving it takes back a release (feedbackWritePerms), so like
			// withdrawn material below it is withdrawn from anyone who kept
			// the id. Those who grade still read it.
			if doc.Kind == kindFeedback && (doc.GradePostedAt == nil || doc.GradeSupersededBy != nil || doc.Status == "archived") && !seesDrafts(rc.Member) {
				return DocumentGetOut{}, apperr.Missing("no such document in this course")
			}
			drafts := rc.Member.Perm(domain.PermDocumentReadDraft).Allowed()
			if courseLevel(doc.Kind) && doc.PublishedVersionID == nil && !drafts {
				return DocumentGetOut{}, apperr.Missing("no such document in this course")
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
					return DocumentGetOut{}, err
				}
			}
			if withdrawn && in.VersionID == nil {
				return DocumentGetOut{}, apperr.Missing("no such document in this course")
			}
			out := DocumentGetOut{SubmissionID: doc.SubmissionID, GradeID: doc.GradeID,
				DocumentSummary: DocumentSummary{ID: doc.ID, Kind: doc.Kind, Title: doc.Title, PublishedVersionID: doc.PublishedVersionID,
					SortOrder: doc.SortOrder, Status: doc.Status, CreatedAt: doc.CreatedAt}}

			var v dbq.DocumentVersion
			switch {
			case in.VersionID != nil:
				v, err = rc.Q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: *in.VersionID, DocumentID: doc.ID})
				if err == nil && (withdrawn || (!drafts && (doc.PublishedVersionID == nil || *doc.PublishedVersionID != v.ID))) {
					// Not the published one and no right to drafts. One more
					// way in: it is what the caller's own work was pinned to.
					pinned, perr := rc.Q.VersionPinnedInScope(ctx, dbq.VersionPinnedInScopeParams{VersionID: &v.ID,
						StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID})
					if perr != nil {
						return DocumentGetOut{}, perr
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
				if in.VersionID != nil {
					return DocumentGetOut{}, apperr.Missing("no such version of this document")
				}
				return out, nil
			}
			if err != nil {
				return DocumentGetOut{}, err
			}
			view := VersionView{ID: v.ID, Seq: v.Seq, BodyMD: v.BodyMd, ContentType: v.ContentType, ByteSize: v.ByteSize,
				Checksum: v.Checksum, AuthorMemberID: v.AuthorMemberID, CreatedAt: v.CreatedAt,
				Published: doc.PublishedVersionID != nil && *doc.PublishedVersionID == v.ID}
			if v.StorageKey != nil && d.Blob != nil {
				url, err := d.Blob.PresignGet(ctx, *v.StorageKey, downloadTTL)
				if err != nil {
					return DocumentGetOut{}, err
				}
				view.DownloadURL = &url
			}
			out.Version = &view
			return out, nil
		},
	})
}

type VersionSummary struct {
	ID             uuid.UUID `json:"id"`
	Seq            int32     `json:"seq"`
	HasFile        bool      `json:"has_file"`
	ContentType    *string   `json:"content_type,omitempty"`
	ByteSize       *int64    `json:"byte_size,omitempty"`
	AuthorMemberID uuid.UUID `json:"author_member_id"`
	CreatedAt      time.Time `json:"created_at"`
	Published      bool      `json:"published"`
}

type DocumentVersionsOut struct {
	Versions []VersionSummary `json:"versions"`
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
		DocumentID: &docID, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID})
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
			if courseLevel(doc.Kind) && doc.Kind != kindMaterial {
				if withheld, err := assignmentWithheld(ctx, rc, doc.ID); err != nil {
					return DocumentVersionsOut{}, err
				} else if withheld {
					return DocumentVersionsOut{}, apperr.Missing("no such document in this course")
				}
			}
			rows, err := rc.Q.ListVersions(ctx, doc.ID)
			out := DocumentVersionsOut{Versions: make([]VersionSummary, 0, len(rows))}
			for _, r := range rows {
				out.Versions = append(out.Versions, VersionSummary{ID: r.ID, Seq: r.Seq, HasFile: r.HasFile, ContentType: r.ContentType,
					ByteSize: r.ByteSize, AuthorMemberID: r.AuthorMemberID, CreatedAt: r.CreatedAt,
					Published: doc.PublishedVersionID != nil && *doc.PublishedVersionID == r.ID})
			}
			return out, err
		},
	})
}
