package tools

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// The agent runtime's renditions (docs/schema.md §2.4, Renditions; §2.1,
// The agent runtime's tools). The site's agent runtime, the site service
// agent_runtime, converts what waits to be converted, across the site, as
// the transcriber transcribes (text_service.go): a claim holds one
// rendition for it alone until its lease runs out, and hands it the file by
// a short-lived URL; it converts the file to PDF, asks for somewhere to
// upload the PDF (rendition_upload_url), PUTs it there, and writes back what
// became of it while its claim holds: done, naming the upload, or failed or
// skipped, saying why. Core holds the PDF to what one may be — a PDF, no
// larger than RenditionLimits.MaxBytes — and keeps it, under its own key, as
// it was made. Nothing else is the runtime's to read or write here: no
// course, no seat, no person, and no file but those of what it has claimed.
//
// A rendition is converted in an archived course too (OnArchived), as it is
// of an archived document: it is plumbing, not a write of anyone's, and
// whoever still reads the file there reads its PDF.

const (
	// MaxRenditionAttempts is how many claims a rendition is given before it
	// is failed (attempts_exhausted) rather than claimed again.
	MaxRenditionAttempts = 5
	// MaxRenditionClaim is the most one claim takes.
	MaxRenditionClaim = 10
)

// renditionReasons are why a rendition fails or is skipped, as the runtime
// says it; attempts_exhausted is Core's own.
var renditionReasons = []string{"password_protected", "timeout", "conversion_failed", "too_large", "unsupported"}

// renditionSource says whose file a rendition is of: a version's or a
// message's.
func renditionSource(file *uuid.UUID) string {
	if file != nil {
		return "document_file"
	}
	return "attachment"
}

type ClaimedRendition struct {
	RenditionID       uuid.UUID  `json:"rendition_id" jsonschema:"give it to agent_runtime.rendition_file, .rendition_renew, .rendition_upload_url and .rendition_complete"`
	LeaseID           uuid.UUID  `json:"lease_id" jsonschema:"the claim's: give it to every call about the rendition"`
	LeaseExpiresAt    time.Time  `json:"lease_expires_at" jsonschema:"when the claim lapses, and the rendition may be claimed again, unless it is renewed"`
	Attempt           int32      `json:"attempt" jsonschema:"how many times it has been claimed since it was queued, this one included; a rendition claimed 5 times and not finished is failed (attempts_exhausted)"`
	Backfill          bool       `json:"backfill" jsonschema:"queued when renditions came in, rather than as its file was recorded"`
	CourseID          uuid.UUID  `json:"course_id"`
	Source            string     `json:"source" jsonschema:"document_file, a file of a document's version; or attachment, a file a message of a conversation carries"`
	FileID            *uuid.UUID `json:"file_id,omitempty" jsonschema:"the version's file, for document_file"`
	AttachmentID      *uuid.UUID `json:"attachment_id,omitempty" jsonschema:"the message's file, for attachment"`
	Filename          string     `json:"filename" jsonschema:"the file's name, whose extension says what it is"`
	ContentType       string     `json:"content_type" jsonschema:"the media type its uploader declared"`
	ByteSize          int64      `json:"byte_size"`
	Checksum          *string    `json:"checksum,omitempty"`
	DownloadURL       string     `json:"download_url" jsonschema:"a short-lived URL for the file; agent_runtime.rendition_file gives another while the claim holds"`
	DownloadExpiresAt time.Time  `json:"download_expires_at"`
	MaxBytes          int64      `json:"max_bytes" jsonschema:"the largest PDF that is taken, in bytes: complete a larger one skipped, too_large"`
}

type RenditionClaimIn struct {
	Max    int `json:"max,omitempty" jsonschema:"how many renditions to claim at most, 1 to 10; 1 if omitted"`
	LeaseS int `json:"lease_s,omitempty" jsonschema:"how long each claim holds, 60 to 3600 seconds; 600 if omitted. agent_runtime.rendition_renew holds it longer"`
	tool.CanWait
}

type RenditionClaimOut struct {
	Claimed []ClaimedRendition `json:"claimed" jsonschema:"what was claimed, one file each: what was queued as its file was recorded first, the oldest first, then what was queued when renditions came in, the newest first; empty when nothing waits"`
}

func renditionClaim(d Deps) tool.Tool {
	return tool.Define(tool.Spec[RenditionClaimIn, RenditionClaimOut]{
		Name: "agent_runtime.rendition_claim",
		Description: "For the site's agent runtime alone: claim Office and OpenDocument files waiting to be converted to PDF, " +
			"across the site, a version's file or a message's: those recorded most lately waiting longest first, then those " +
			"queued when renditions came in, the newest first. Each claim holds its rendition for you alone until " +
			"lease_expires_at, and comes with a short-lived URL for the file. Convert it, get somewhere to put the PDF " +
			"(agent_runtime.rendition_upload_url), PUT it there and say what became of it (agent_runtime.rendition_complete) " +
			"before then, or hold it longer with agent_runtime.rendition_renew. A claim that lapses may be claimed again; a " +
			"rendition claimed 5 times and not finished is failed (attempts_exhausted). With wait_s, a call that finds " +
			"nothing waits up to that many seconds for a file to be queued, and claims it as soon as it is. Recorded " +
			"nowhere; the claims are the record.",
		Kind: tool.Ephemeral, Gate: agentRuntime,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/renditions/claim"},
		Resolve: noTarget[RenditionClaimIn]("file_rendition"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RenditionClaimIn) (RenditionClaimOut, error) {
			if in.Max < 0 || in.Max > MaxRenditionClaim {
				return RenditionClaimOut{}, apperr.Invalid("max is 1 to %d", MaxRenditionClaim)
			}
			lease, err := leaseOf(in.LeaseS)
			if err != nil {
				return RenditionClaimOut{}, err
			}
			if d.Blob == nil {
				return RenditionClaimOut{}, errNoFileStorage
			}
			if err := ec.Q.ExhaustRenditions(ctx, dbq.ExhaustRenditionsParams{Now: ec.Now, MaxAttempts: MaxRenditionAttempts}); err != nil {
				return RenditionClaimOut{}, err
			}
			until := ec.Now.Add(lease)
			rows, err := ec.Q.ClaimRenditions(ctx, dbq.ClaimRenditionsParams{ClaimedUntil: &until, CredentialID: &ec.CredentialID,
				Now: &ec.Now, MaxAttempts: MaxRenditionAttempts, MaxRows: int32(max(in.Max, 1))})
			if err != nil {
				return RenditionClaimOut{}, err
			}
			// In the queue's order: what RETURNING gives back is in none.
			slices.SortFunc(rows, func(a, b dbq.ClaimRenditionsRow) int {
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
				}
				return strings.Compare(a.ID.String(), b.ID.String())
			})
			out := RenditionClaimOut{Claimed: make([]ClaimedRendition, 0, len(rows))}
			for _, r := range rows {
				if r.LeaseID == nil || r.ClaimedUntil == nil {
					return RenditionClaimOut{}, errors.New("a claimed rendition has no lease")
				}
				url, err := d.Blob.PresignDownload(ctx, r.StorageKey, r.Filename, downloadTTL)
				if err != nil {
					return RenditionClaimOut{}, err
				}
				out.Claimed = append(out.Claimed, ClaimedRendition{RenditionID: r.ID, LeaseID: *r.LeaseID, LeaseExpiresAt: *r.ClaimedUntil,
					Attempt: r.Attempts, Backfill: r.Backfill, CourseID: r.CourseID, Source: renditionSource(r.FileID),
					FileID: r.FileID, AttachmentID: r.AttachmentID, Filename: r.Filename, ContentType: r.ContentType,
					ByteSize: r.ByteSize, Checksum: r.Checksum, DownloadURL: url, DownloadExpiresAt: ec.Now.Add(downloadTTL),
					MaxBytes: d.Renditions.MaxBytes})
			}
			return out, nil
		},
		// A file queued anywhere wakes a claim that found nothing.
		Wait: &tool.Waiting[RenditionClaimIn, RenditionClaimOut]{
			For: func(_ *tool.ReadCtx, _ RenditionClaimIn) wake.Filter {
				return wake.Filter{AnyCourse: true, Kinds: []string{wake.KindRenditionQueued}}
			},
			Nothing: func(_ RenditionClaimIn, _, now RenditionClaimOut) bool { return len(now.Claimed) == 0 },
		},
	})
}

// serviceRendition is what a call about one claimed rendition is about: the
// rendition, in its file's course, where it is recorded.
func serviceRendition(ctx context.Context, q dbq.Querier, id uuid.UUID) (tool.Target, error) {
	r, err := q.GetRenditionForService(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such rendition")
	}
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: r.CourseID, Type: "file_rendition", ID: &r.ID}, nil
}

// lockClaimedRendition holds a rendition for a call of the runtime's about
// it, and says whether the caller's claim still holds it: it lapsed and was
// claimed again, the rendition was sent back to the queue, or the
// credential that made it was revoked, and it does not (lease_lost).
func lockClaimedRendition(ctx context.Context, q dbq.Querier, id, lease uuid.UUID) (dbq.FileRendition, error) {
	r, err := q.LockRendition(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return r, apperr.Missing("no such rendition") // its file was purged meanwhile
	case err != nil:
		return r, err
	case r.Status != renditionClaimed || r.LeaseID == nil || *r.LeaseID != lease:
		return r, errLeaseLost()
	}
	return r, nil
}

type RenditionLeaseIn struct {
	RenditionID uuid.UUID `json:"rendition_id"`
	LeaseID     uuid.UUID `json:"lease_id" jsonschema:"the claim's, from agent_runtime.rendition_claim"`
}

type RenditionFileOut struct {
	RenditionID       uuid.UUID `json:"rendition_id"`
	Filename          string    `json:"filename"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          *string   `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url" jsonschema:"a short-lived URL for the file"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
}

func renditionFile(d Deps) tool.Tool {
	return tool.Define(tool.Spec[RenditionLeaseIn, RenditionFileOut]{
		Name: "agent_runtime.rendition_file",
		Description: "For the site's agent runtime alone: another short-lived URL for the file of a rendition you have " +
			"claimed, while the claim holds. Any other file is not yours to read (lease_lost).",
		Kind: tool.Read, Gate: agentRuntime,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/services/agent_runtime/renditions/{rendition_id}/file"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RenditionLeaseIn) (tool.Target, error) {
			return serviceRendition(ctx, q, in.RenditionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in RenditionLeaseIn) (RenditionFileOut, error) {
			if d.Blob == nil {
				return RenditionFileOut{}, errNoFileStorage
			}
			f, err := rc.Q.GetClaimedRendition(ctx, dbq.GetClaimedRenditionParams{ID: in.RenditionID, LeaseID: &in.LeaseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return RenditionFileOut{}, errLeaseLost()
			}
			if err != nil {
				return RenditionFileOut{}, err
			}
			if f.ClaimedUntil == nil {
				return RenditionFileOut{}, errors.New("a claimed rendition has no lease")
			}
			url, err := d.Blob.PresignDownload(ctx, f.StorageKey, f.Filename, downloadTTL)
			if err != nil {
				return RenditionFileOut{}, err
			}
			return RenditionFileOut{RenditionID: f.ID, Filename: f.Filename, ContentType: f.ContentType, ByteSize: f.ByteSize,
				Checksum: f.Checksum, DownloadURL: url, DownloadExpiresAt: rc.Now.Add(downloadTTL), LeaseExpiresAt: *f.ClaimedUntil}, nil
		},
	})
}

type RenditionRenewIn struct {
	RenditionID uuid.UUID `json:"rendition_id"`
	LeaseID     uuid.UUID `json:"lease_id" jsonschema:"the claim's, from agent_runtime.rendition_claim"`
	LeaseS      int       `json:"lease_s,omitempty" jsonschema:"how long the claim holds from now, 60 to 3600 seconds; 600 if omitted"`
}

type RenditionRenewOut struct {
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func renditionRenew() tool.Tool {
	return tool.Define(tool.Spec[RenditionRenewIn, RenditionRenewOut]{
		Name: "agent_runtime.rendition_renew",
		Description: "For the site's agent runtime alone: hold a claim on a rendition longer, lease_s from now, while the " +
			"conversion goes on. Refused once the claim no longer holds (lease_lost): stop the work then. Recorded nowhere.",
		Kind: tool.Ephemeral, Gate: agentRuntime, OnArchived: true,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/renditions/{rendition_id}/renew"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RenditionRenewIn) (tool.Target, error) {
			return serviceRendition(ctx, q, in.RenditionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RenditionRenewIn) (RenditionRenewOut, error) {
			lease, err := leaseOf(in.LeaseS)
			if err != nil {
				return RenditionRenewOut{}, err
			}
			if _, err := lockClaimedRendition(ctx, ec.Q, in.RenditionID, in.LeaseID); err != nil {
				return RenditionRenewOut{}, err
			}
			until := ec.Now.Add(lease)
			if _, err := ec.Q.RenewRenditionLease(ctx, dbq.RenewRenditionLeaseParams{ID: in.RenditionID, LeaseID: &in.LeaseID,
				ClaimedUntil: &until, Now: ec.Now}); err != nil {
				return RenditionRenewOut{}, err
			}
			return RenditionRenewOut{LeaseExpiresAt: until}, nil
		},
	})
}

type RenditionUploadURLOut struct {
	UploadURL   string            `json:"upload_url" jsonschema:"PUT the PDF's bytes here, once, within the window"`
	Headers     map[string]string `json:"headers" jsonschema:"headers the PUT must carry: Content-Type application/pdf"`
	UploadToken string            `json:"upload_token" jsonschema:"name this in agent_runtime.rendition_complete, with status done, once the PDF is PUT"`
	ExpiresAt   time.Time         `json:"expires_at" jsonschema:"when upload_url stops taking the PDF"`
	MaxBytes    int64             `json:"max_bytes" jsonschema:"the largest PDF that is taken, in bytes; complete a larger one skipped, too_large"`
}

func renditionUploadURL(d Deps) tool.Tool {
	return tool.Define(tool.Spec[RenditionLeaseIn, RenditionUploadURLOut]{
		Name: "agent_runtime.rendition_upload_url",
		Description: "For the site's agent runtime alone: somewhere to upload the PDF a rendition you have claimed was " +
			"converted into, while the claim holds: PUT its bytes to upload_url with the headers given, then name " +
			"upload_token in agent_runtime.rendition_complete. Each call gives a new URL, to a key of its own; one that is " +
			"never named is discarded. Refused once the claim no longer holds (lease_lost).",
		Kind: tool.Read, Gate: agentRuntime,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/services/agent_runtime/renditions/{rendition_id}/upload-url"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RenditionLeaseIn) (tool.Target, error) {
			return serviceRendition(ctx, q, in.RenditionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in RenditionLeaseIn) (RenditionUploadURLOut, error) {
			if d.Blob == nil {
				return RenditionUploadURLOut{}, errNoFileStorage
			}
			f, err := rc.Q.GetClaimedRendition(ctx, dbq.GetClaimedRenditionParams{ID: in.RenditionID, LeaseID: &in.LeaseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return RenditionUploadURLOut{}, errLeaseLost()
			}
			if err != nil {
				return RenditionUploadURLOut{}, err
			}
			// The key is ours and unguessable, and new each time, so that
			// nothing is ever written over; the orphan sweep knows it by its
			// shape.
			key := RenditionPrefix + f.CourseID.String() + "/" + ids.New().String()
			url, headers, err := d.Blob.PresignPutUpTo(ctx, key, RenditionType, d.Renditions.MaxBytes, uploadWindow)
			if err != nil {
				return RenditionUploadURLOut{}, err
			}
			expires := rc.Now.Add(uploadWindow)
			return RenditionUploadURLOut{UploadURL: url, Headers: headers, ExpiresAt: expires, MaxBytes: d.Renditions.MaxBytes,
				UploadToken: d.Uploads.SignRendition(blob.RenditionUpload{Key: key, Rendition: f.ID, Lease: in.LeaseID,
					Expires: expires.Unix()})}, nil
		},
	})
}

type RenditionCompleteIn struct {
	RenditionID uuid.UUID `json:"rendition_id"`
	LeaseID     uuid.UUID `json:"lease_id" jsonschema:"the claim's, from agent_runtime.rendition_claim"`
	Status      string    `json:"status" jsonschema:"done, with the PDF uploaded; failed, when it could not be converted; skipped, when it was not to be"`
	UploadToken *string   `json:"upload_token,omitempty" jsonschema:"for done: from agent_runtime.rendition_upload_url, once the PDF is PUT to its upload_url"`
	PageCount   *int32    `json:"page_count,omitempty" jsonschema:"for done: how many pages the PDF has, 1 to 100000"`
	Reason      *string   `json:"reason,omitempty" jsonschema:"for failed and skipped: why, one of password_protected, timeout, conversion_failed, too_large, unsupported"`
}

type RenditionCompleteOut struct {
	RenditionID uuid.UUID `json:"rendition_id"`
	State       string    `json:"state"`
	ByteSize    *int64    `json:"byte_size,omitempty" jsonschema:"for done: the PDF's size, as the store has it"`
	Checksum    *string   `json:"checksum,omitempty" jsonschema:"for done: sha256:<hex> where the store worked it out from the bytes, etag:<value> where all it has is an object store's tag"`
}

// checkRenditionCompletion holds what the runtime writes back to its shape.
func checkRenditionCompletion(in RenditionCompleteIn) error {
	switch in.Status {
	case renditionDone:
		if in.UploadToken == nil || *in.UploadToken == "" {
			return apperr.Invalid("done needs upload_token")
		}
		if in.PageCount == nil || *in.PageCount < 1 || *in.PageCount > 100000 {
			return apperr.Invalid("done needs page_count, 1 to 100000")
		}
		if in.Reason != nil {
			return apperr.Invalid("done gives no reason")
		}
		return nil
	case renditionFailed, renditionSkipped:
		if in.UploadToken != nil || in.PageCount != nil {
			return apperr.Invalid("%s gives no upload_token or page_count", in.Status)
		}
		if in.Reason == nil || !slices.Contains(renditionReasons, *in.Reason) {
			return apperr.Invalid("%s needs a reason: %s", in.Status, strings.Join(renditionReasons, ", ")).With("field", "reason")
		}
		return nil
	}
	return apperr.Invalid("status must be done, failed or skipped")
}

// errRenditionTooLarge refuses a PDF larger than a rendition may be.
func errRenditionTooLarge(size, most int64) *apperr.Error {
	return apperr.Precondition("the PDF is %d bytes; the largest taken is %d: complete it skipped, too_large", size, most).
		With("reason", "rendition_too_large").With("byte_size", size).With("max_bytes", most)
}

// errNotPDF refuses an upload that is not a PDF.
var errNotPDF = apperr.Precondition("what was uploaded is not a PDF: it does not begin %q; upload the PDF to a new URL", pdfMagic).
	With("reason", "not_a_pdf")

func renditionComplete(d Deps) tool.Tool {
	return tool.Define(tool.Spec[RenditionCompleteIn, RenditionCompleteOut]{
		Name: "agent_runtime.rendition_complete",
		Description: "For the site's agent runtime alone: say what became of a rendition you have claimed, while the claim " +
			"holds: done, naming the upload_token of the PDF you PUT and its page_count; or failed or skipped, with " +
			"reason: password_protected, timeout, conversion_failed, too_large or unsupported. A PDF is taken once it " +
			"begins %PDF- (not_a_pdf) and is no larger than max_bytes (rendition_too_large); refused, it is discarded, the " +
			"claim holds, and you may upload again or say failed or skipped. Refused, and nothing written, once the claim " +
			"no longer holds (lease_lost). Done is for good: its readers are shown the PDF. Recorded as an action of the " +
			"service's, but for the upload token; retry it with the same idempotency key.",
		Kind: tool.Write, Gate: agentRuntime, OnArchived: true,
		HTTP:     tool.Route{Method: "POST", Pattern: "/v1/services/agent_runtime/renditions/{rendition_id}/complete"},
		SecretIn: []string{"upload_token"},
		Resolve: func(ctx context.Context, q dbq.Querier, in RenditionCompleteIn) (tool.Target, error) {
			return serviceRendition(ctx, q, in.RenditionID)
		},
		Validate: func(_ context.Context, _ dbq.Querier, _ *domain.Member, in RenditionCompleteIn) error {
			return checkRenditionCompletion(in)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RenditionCompleteIn) (RenditionCompleteOut, error) {
			if err := checkRenditionCompletion(in); err != nil {
				return RenditionCompleteOut{}, err
			}
			r, err := lockClaimedRendition(ctx, ec.Q, in.RenditionID, in.LeaseID)
			if err != nil {
				return RenditionCompleteOut{}, err
			}
			out := RenditionCompleteOut{RenditionID: r.ID, State: in.Status}
			if in.Status != renditionDone {
				return out, ec.Q.FinishRenditionUndone(ctx, dbq.FinishRenditionUndoneParams{ID: r.ID, Status: in.Status,
					Reason: in.Reason, Now: ec.Now})
			}
			pdf, err := takePDF(ctx, d, ec.Q, r, in.LeaseID, *in.UploadToken)
			if err != nil {
				return RenditionCompleteOut{}, err
			}
			if err := ec.Q.FinishRenditionDone(ctx, dbq.FinishRenditionDoneParams{ID: r.ID, StorageKey: &pdf.key,
				ByteSize: &pdf.info.Size, Checksum: nonEmpty(pdf.info.Checksum), PageCount: in.PageCount, Now: &ec.Now}); err != nil {
				return RenditionCompleteOut{}, err
			}
			out.ByteSize, out.Checksum = &pdf.info.Size, nonEmpty(pdf.info.Checksum)
			return out, nil
		},
	})
}

// takePDF takes the PDF the runtime uploaded for a rendition under its claim:
// the token proves this claim of this rendition was given the key, the store
// is asked what is there, and the object is moved where no upload URL
// reaches it (Finalize) before it is held to being a PDF no larger than a
// rendition may be. What is refused is removed, and the claim holds. The
// key's lock, which the orphan sweep takes as well, makes "is it there?" and
// the move one step with the transaction that records it.
func takePDF(ctx context.Context, d Deps, q dbq.Querier, r dbq.FileRendition, lease uuid.UUID, token string) (upload, error) {
	if d.Blob == nil {
		return upload{}, errNoFileStorage
	}
	c, err := d.Uploads.VerifyRendition(token)
	if err != nil {
		return upload{}, errBadUploadToken
	}
	if c.Rendition != r.ID || c.Lease != lease {
		return upload{}, apperr.Forbid("that upload was issued for another claim").With("reason", "not_your_upload")
	}
	final := d.Blob.FinalKey(c.Key)
	for _, key := range lockStorageKeys(c.Key, final) {
		if err := q.LockStorageKey(ctx, key); err != nil {
			return upload{}, err
		}
	}
	info, err := d.Blob.Stat(ctx, c.Key)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		// Nothing staged: nothing was uploaded, or a completion that did
		// not commit moved it already, to a key no URL was issued for.
		if info, err = d.Blob.Stat(ctx, final); errors.Is(err, blob.ErrNotFound) {
			return upload{}, errNotUploaded
		}
	case err == nil:
		info, err = d.Blob.Finalize(ctx, c.Key)
		if errors.Is(err, blob.ErrNotFound) {
			return upload{}, errNotUploaded
		}
	}
	if err != nil {
		return upload{}, err
	}
	if info.Size > d.Renditions.MaxBytes {
		_ = d.Blob.Delete(ctx, final)
		return upload{}, errRenditionTooLarge(info.Size, d.Renditions.MaxBytes)
	}
	head, err := d.Blob.Head(ctx, final, len(pdfMagic))
	if err != nil {
		return upload{}, err
	}
	if !bytes.Equal(head, []byte(pdfMagic)) {
		_ = d.Blob.Delete(ctx, final)
		return upload{}, errNotPDF
	}
	return upload{key: final, info: info}, nil
}
