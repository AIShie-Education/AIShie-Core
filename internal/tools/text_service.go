package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// The transcription service's tools (docs/schema.md §2.4, The queue). The
// runtime's transcriber, a site service with a credential of its own
// (service.issue_credential), claims what waits to be transcribed, across
// the site: a claim holds the text version of one file of a version for it
// alone until its lease runs out, and hands it the file, by a short-lived
// URL, as document.get hands one to a reader. A call about a claim names its
// version, its file (file_id) and its lease. It writes the text back while
// its claim holds: done, with the text, or failed or skipped, saying why;
// never over staff's text. Nothing else is its to read or write: no course,
// no seat, no person, and no file but those of what it has claimed.

var transcriber = tool.Gate{Service: domain.ServiceDocumentText}

const (
	// MaxTextAttempts is how many claims a text version is given before it
	// is failed (attempts_exhausted) rather than claimed again: a file the
	// service cannot get through, or that stops it each time.
	MaxTextAttempts = 5
	// MaxTextClaim is the most one claim takes.
	MaxTextClaim = 10

	DefaultTextLease = 10 * time.Minute
	MinTextLease     = time.Minute
	MaxTextLease     = time.Hour
)

type ClaimedText struct {
	VersionID         uuid.UUID `json:"version_id"`
	FileID            uuid.UUID `json:"file_id" jsonschema:"the file whose text is claimed: give it to document_text.file, .renew and .complete"`
	Position          int32     `json:"position" jsonschema:"the file's place among its version's files, from 1"`
	Filename          string    `json:"filename"`
	DocumentID        uuid.UUID `json:"document_id"`
	CourseID          uuid.UUID `json:"course_id"`
	LeaseID           uuid.UUID `json:"lease_id" jsonschema:"the claim's: give it to document_text.file, .renew and .complete"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at" jsonschema:"when the claim lapses, and the version may be claimed again, unless it is renewed"`
	Attempt           int32     `json:"attempt" jsonschema:"how many times it has been claimed since it was queued, this one included; a file claimed 5 times and not finished is failed (attempts_exhausted)"`
	Backfill          bool      `json:"backfill" jsonschema:"queued when text versions came in, rather than as its file was added"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          *string   `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url" jsonschema:"a short-lived URL for the file; document_text.file gives another while the claim holds"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
}

type TextQueueIn struct {
	Max    int `json:"max,omitempty" jsonschema:"how many versions to claim at most, 1 to 10; 1 if omitted"`
	LeaseS int `json:"lease_s,omitempty" jsonschema:"how long each claim holds, 60 to 3600 seconds; 600 if omitted. document_text.renew holds it longer"`
	tool.CanWait
}

type TextQueueOut struct {
	Claimed []ClaimedText `json:"claimed" jsonschema:"what was claimed, one file's text each, uploads before what was queued when text versions came in, a version's files in order; empty when nothing waits"`
}

func leaseOf(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return DefaultTextLease, nil
	}
	lease := time.Duration(seconds) * time.Second
	if lease < MinTextLease || lease > MaxTextLease {
		return 0, apperr.Invalid("lease_s is 60 to 3600")
	}
	return lease, nil
}

func textQueue(d Deps) tool.Tool {
	return tool.Define(tool.Spec[TextQueueIn, TextQueueOut]{
		Name: "document_text.queue",
		Description: "For the transcription service alone: claim files of document versions waiting to be transcribed, " +
			"across the site, each file of a version on its own: those added most lately waiting longest first, then those " +
			"queued when text versions came in, the newest first, a version's files in order. Each claim holds its file's " +
			"text version for you alone until lease_expires_at, and comes with a short-lived URL for the file; write the " +
			"text back with document_text.complete before then, or hold it longer with document_text.renew, naming its " +
			"version_id, file_id and lease_id. A claim that lapses may be claimed again, by you or another; a file " +
			"claimed 5 times and not finished is failed (attempts_exhausted). Nothing in an archived course, or of an " +
			"archived document, is claimed. With wait_s, a call that finds nothing waits up to that many seconds for a " +
			"file to be queued, and claims it as soon as it is. Recorded nowhere; the claims are the record.",
		Kind: tool.Ephemeral, Gate: transcriber,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/services/document_text/queue"},
		Resolve: noTarget[TextQueueIn]("document_version"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in TextQueueIn) (TextQueueOut, error) {
			if in.Max < 0 || in.Max > MaxTextClaim {
				return TextQueueOut{}, apperr.Invalid("max is 1 to %d", MaxTextClaim)
			}
			lease, err := leaseOf(in.LeaseS)
			if err != nil {
				return TextQueueOut{}, err
			}
			if d.Blob == nil {
				return TextQueueOut{}, apperr.Precondition("this installation has no file storage configured")
			}
			if err := ec.Q.ExhaustTexts(ctx, dbq.ExhaustTextsParams{Now: ec.Now, MaxAttempts: MaxTextAttempts}); err != nil {
				return TextQueueOut{}, err
			}
			until := ec.Now.Add(lease)
			rows, err := ec.Q.ClaimTexts(ctx, dbq.ClaimTextsParams{ClaimedUntil: &until, CredentialID: &ec.CredentialID, Now: &ec.Now,
				MaxAttempts: MaxTextAttempts, MaxRows: int32(max(in.Max, 1))})
			if err != nil {
				return TextQueueOut{}, err
			}
			// In the queue's order: what RETURNING gives back is in none.
			slices.SortFunc(rows, func(a, b dbq.ClaimTextsRow) int {
				switch {
				case a.Backfill != b.Backfill:
					if a.Backfill {
						return 1
					}
					return -1
				case !a.QueuedAt.Equal(b.QueuedAt):
					if a.Backfill {
						return b.QueuedAt.Compare(a.QueuedAt)
					}
					return a.QueuedAt.Compare(b.QueuedAt)
				case a.VersionID != b.VersionID:
					return strings.Compare(a.VersionID.String(), b.VersionID.String())
				}
				return int(a.Position - b.Position)
			})
			out := TextQueueOut{Claimed: make([]ClaimedText, 0, len(rows))}
			for _, r := range rows {
				if r.LeaseID == nil || r.ClaimedUntil == nil {
					return TextQueueOut{}, errors.New("a claimed text version has no lease")
				}
				url, err := d.Blob.PresignDownload(ctx, r.StorageKey, r.Filename, downloadTTL)
				if err != nil {
					return TextQueueOut{}, err
				}
				out.Claimed = append(out.Claimed, ClaimedText{VersionID: r.VersionID, FileID: r.FileID, Position: r.Position,
					Filename: r.Filename, DocumentID: r.DocumentID, CourseID: r.CourseID,
					LeaseID: *r.LeaseID, LeaseExpiresAt: *r.ClaimedUntil, Attempt: r.Attempts, Backfill: r.Backfill,
					ContentType: r.ContentType, ByteSize: r.ByteSize, Checksum: r.Checksum,
					DownloadURL: url, DownloadExpiresAt: ec.Now.Add(downloadTTL)})
			}
			return out, nil
		},
		// A version queued anywhere wakes a claim that found nothing.
		Wait: &tool.Waiting[TextQueueIn, TextQueueOut]{
			For: func(_ *tool.ReadCtx, _ TextQueueIn) wake.Filter {
				return wake.Filter{AnyCourse: true, Kinds: []string{wake.KindTextQueued}}
			},
			Nothing: func(_ TextQueueIn, _, now TextQueueOut) bool { return len(now.Claimed) == 0 },
		},
	})
}

// serviceText is what a call about one claimed text version is about: the
// version, in its course, where every write is refused while the course is
// archived.
func serviceText(ctx context.Context, q dbq.Querier, version uuid.UUID) (tool.Target, error) {
	r, err := q.GetTextForService(ctx, version)
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such text version")
	}
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: r.CourseID, Type: "document_version", ID: &r.VersionID}, nil
}

// errLeaseLost refuses a call about a claim that no longer holds: it lapsed
// and the file was claimed again, the file was sent back to be transcribed
// again, or the credential that made it was revoked.
func errLeaseLost() *apperr.Error {
	return apperr.Conflicts("the claim no longer holds: the file was claimed again, or sent back to the queue").
		With("reason", "lease_lost")
}

// lockClaimed holds the text version a call of the service's is about: the
// named file's. Whether the caller's claim still holds it, leaseHeld says.
func lockClaimed(ctx context.Context, q dbq.Querier, version, file uuid.UUID) (dbq.DocumentVersionText, error) {
	t, err := q.LockTextForService(ctx, dbq.LockTextForServiceParams{VersionID: version, FileID: file})
	if errors.Is(err, pgx.ErrNoRows) {
		return t, apperr.Missing("no such text version")
	}
	return t, err
}

// leaseHeld says whether the caller's claim still holds the text version,
// and if not, why: staff's text, which the service never writes over, or a
// claim that is not the caller's any more.
func leaseHeld(t dbq.DocumentVersionText, lease uuid.UUID) error {
	switch {
	case t.Source != nil && *t.Source == textByStaff:
		return apperr.Conflicts("staff have written the text; it is theirs, and is not written over").With("reason", "edited_by_staff")
	case t.Status != textWorking || t.LeaseID == nil || *t.LeaseID != lease:
		return errLeaseLost()
	}
	return nil
}

type TextLeaseIn struct {
	VersionID uuid.UUID `json:"version_id"`
	LeaseID   uuid.UUID `json:"lease_id" jsonschema:"the claim's, from document_text.queue"`
	FileID    uuid.UUID `json:"file_id" jsonschema:"the claim's file, from document_text.queue"`
}

type TextFileOut struct {
	VersionID         uuid.UUID `json:"version_id"`
	FileID            uuid.UUID `json:"file_id"`
	Position          int32     `json:"position"`
	Filename          string    `json:"filename"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          *string   `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url" jsonschema:"a short-lived URL for the file"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
}

func textFile(d Deps) tool.Tool {
	return tool.Define(tool.Spec[TextLeaseIn, TextFileOut]{
		Name: "document_text.file",
		Description: "For the transcription service alone: another short-lived URL for a file you have claimed, while the " +
			"claim holds. Any other file is not yours to read (lease_lost).",
		Kind: tool.Read, Gate: transcriber,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/services/document_text/versions/{version_id}/file"},
		Resolve: func(ctx context.Context, q dbq.Querier, in TextLeaseIn) (tool.Target, error) {
			return serviceText(ctx, q, in.VersionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in TextLeaseIn) (TextFileOut, error) {
			if d.Blob == nil {
				return TextFileOut{}, apperr.Precondition("this installation has no file storage configured")
			}
			f, err := rc.Q.GetClaimedFile(ctx, dbq.GetClaimedFileParams{VersionID: in.VersionID, LeaseID: &in.LeaseID, FileID: in.FileID})
			if errors.Is(err, pgx.ErrNoRows) {
				return TextFileOut{}, errLeaseLost()
			}
			if err != nil {
				return TextFileOut{}, err
			}
			if f.ClaimedUntil == nil {
				return TextFileOut{}, errors.New("a claimed text version has no lease")
			}
			url, err := d.Blob.PresignDownload(ctx, f.StorageKey, f.Filename, downloadTTL)
			if err != nil {
				return TextFileOut{}, err
			}
			return TextFileOut{VersionID: in.VersionID, FileID: f.FileID, Position: f.Position, Filename: f.Filename,
				ContentType: f.ContentType, ByteSize: f.ByteSize, Checksum: f.Checksum,
				DownloadURL: url, DownloadExpiresAt: rc.Now.Add(downloadTTL), LeaseExpiresAt: *f.ClaimedUntil}, nil
		},
	})
}

type TextRenewIn struct {
	VersionID uuid.UUID `json:"version_id"`
	LeaseID   uuid.UUID `json:"lease_id" jsonschema:"the claim's, from document_text.queue"`
	FileID    uuid.UUID `json:"file_id" jsonschema:"the claim's file, from document_text.queue"`
	LeaseS    int       `json:"lease_s,omitempty" jsonschema:"how long the claim holds from now, 60 to 3600 seconds; 600 if omitted"`
}

type TextRenewOut struct {
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func textRenew() tool.Tool {
	return tool.Define(tool.Spec[TextRenewIn, TextRenewOut]{
		Name: "document_text.renew",
		Description: "For the transcription service alone: hold a claim longer, lease_s from now, while the transcription " +
			"goes on. Refused once the claim no longer holds (lease_lost), or once staff have written the text " +
			"(edited_by_staff): stop the work then. Recorded nowhere.",
		Kind: tool.Ephemeral, Gate: transcriber,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/services/document_text/versions/{version_id}/renew"},
		Resolve: func(ctx context.Context, q dbq.Querier, in TextRenewIn) (tool.Target, error) {
			return serviceText(ctx, q, in.VersionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in TextRenewIn) (TextRenewOut, error) {
			lease, err := leaseOf(in.LeaseS)
			if err != nil {
				return TextRenewOut{}, err
			}
			t, err := lockClaimed(ctx, ec.Q, in.VersionID, in.FileID)
			if err != nil {
				return TextRenewOut{}, err
			}
			if err := leaseHeld(t, in.LeaseID); err != nil {
				return TextRenewOut{}, err
			}
			until := ec.Now.Add(lease)
			if _, err := ec.Q.RenewTextLease(ctx, dbq.RenewTextLeaseParams{VersionID: in.VersionID, FileID: t.FileID, LeaseID: &in.LeaseID,
				ClaimedUntil: &until, Now: ec.Now}); err != nil {
				return TextRenewOut{}, err
			}
			return TextRenewOut{LeaseExpiresAt: until}, nil
		},
	})
}

type TextCompleteIn struct {
	VersionID uuid.UUID `json:"version_id"`
	LeaseID   uuid.UUID `json:"lease_id" jsonschema:"the claim's, from document_text.queue"`
	FileID    uuid.UUID `json:"file_id" jsonschema:"the claim's file, from document_text.queue"`
	Status    string    `json:"status" jsonschema:"done, with the text; failed, when it could not be transcribed; skipped, when it was not to be"`
	Body      *string   `json:"body,omitempty" jsonschema:"for done: the whole text, Markdown, at most 2 MiB"`
	Pages     *int32    `json:"pages,omitempty" jsonschema:"for done: how many pages or slides the file has, 1 to 100000"`
	Model     *string   `json:"model,omitempty" jsonschema:"for done: the model that transcribed it, as staff are to be shown it, 1 to 200 characters"`
	Reason    *string   `json:"reason,omitempty" jsonschema:"for failed and skipped: why, 1 to 500 characters, as staff are to be shown it"`
}

type TextCompleteOut struct {
	VersionID uuid.UUID `json:"version_id"`
	FileID    uuid.UUID `json:"file_id"`
	Status    string    `json:"status"`
	Revision  int32     `json:"revision"`
}

// checkCompletion holds what the service writes back to its shape.
func checkCompletion(in TextCompleteIn) error {
	text := func(what string, s *string, most int) error {
		if s == nil || strings.TrimSpace(*s) == "" || utf8.RuneCountInString(*s) > most {
			return apperr.Invalid("%s is 1 to %d characters", what, most)
		}
		return nil
	}
	switch in.Status {
	case textDone:
		if in.Body == nil {
			return apperr.Invalid("done needs body")
		}
		if err := checkText(*in.Body); err != nil {
			return err
		}
		if in.Pages == nil || *in.Pages < 1 || *in.Pages > 100000 {
			return apperr.Invalid("done needs pages, 1 to 100000")
		}
		if in.Reason != nil {
			return apperr.Invalid("done gives no reason")
		}
		return text("model", in.Model, 200)
	case textFailed, textSkipped:
		if in.Body != nil || in.Pages != nil || in.Model != nil {
			return apperr.Invalid("%s gives no body, pages or model", in.Status)
		}
		return text("reason", in.Reason, 500)
	}
	return apperr.Invalid("status must be done, failed or skipped")
}

func textComplete() tool.Tool {
	return tool.Define(tool.Spec[TextCompleteIn, TextCompleteOut]{
		Name: "document_text.complete",
		Description: "For the transcription service alone: write back what became of a file you have claimed, while the " +
			"claim holds: done, with its text (Markdown, at most 2 MiB), its page count and the model's name; or failed " +
			"or skipped, with why. Refused, and nothing written, once staff have written the text (edited_by_staff), or " +
			"once the claim no longer holds (lease_lost): it lapsed and was claimed again, or the file was sent back " +
			"to the queue. Done tells the version's readers the text is there. Recorded as an action, but for the text, " +
			"which is kept only as the text version; retry it with the same idempotency key.",
		Kind: tool.Write, Gate: transcriber, MaxRequestBytes: textRequestBytes,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/services/document_text/versions/{version_id}/complete"},
		// Kept out of the action log, as memory's text is: the text version
		// is where it is kept.
		SecretIn: []string{"body"},
		Resolve: func(ctx context.Context, q dbq.Querier, in TextCompleteIn) (tool.Target, error) {
			return serviceText(ctx, q, in.VersionID)
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in TextCompleteIn) error {
			return checkCompletion(in)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in TextCompleteIn) (TextCompleteOut, error) {
			if err := checkCompletion(in); err != nil {
				return TextCompleteOut{}, err
			}
			r, err := ec.Q.GetTextForService(ctx, in.VersionID)
			if errors.Is(err, pgx.ErrNoRows) {
				return TextCompleteOut{}, apperr.Missing("no such text version") // purged meanwhile
			}
			if err != nil {
				return TextCompleteOut{}, err
			}
			// The document first, as every writer of its versions holds it:
			// archiving it waits for this, or this sees it.
			status, err := ec.Q.ShareDocument(ctx, r.DocumentID)
			if err != nil {
				return TextCompleteOut{}, err
			}
			if status != "active" {
				return TextCompleteOut{}, apperr.Conflicts("the document is archived").With("reason", "document_archived")
			}
			t, err := lockClaimed(ctx, ec.Q, in.VersionID, in.FileID)
			if err != nil {
				return TextCompleteOut{}, err
			}
			if err := leaseHeld(t, in.LeaseID); err != nil {
				return TextCompleteOut{}, err
			}
			out := TextCompleteOut{VersionID: t.VersionID, FileID: t.FileID, Status: in.Status, Revision: t.Revision}
			if in.Status != textDone {
				return out, ec.Q.FinishTextUndone(ctx, dbq.FinishTextUndoneParams{VersionID: t.VersionID, FileID: t.FileID,
					Status: in.Status, Reason: in.Reason, Now: ec.Now})
			}
			if out.Revision, err = ec.Q.FinishTextDone(ctx, dbq.FinishTextDoneParams{VersionID: t.VersionID, FileID: t.FileID,
				Body: in.Body, Pages: in.Pages, Model: in.Model, Now: &ec.Now}); err != nil {
				return TextCompleteOut{}, err
			}
			doc, err := loadDocument(ctx, ec.Q, t.CourseID, t.DocumentID)
			if err != nil {
				return TextCompleteOut{}, err
			}
			source := textByAI
			return out, emitTextEvent(ctx, ec, doc, t.VersionID, t.FileID, textDone, &source, out.Revision)
		},
	})
}
