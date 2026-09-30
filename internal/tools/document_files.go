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
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// A version of a document holds several files (docs/schema.md §2.4, Files of
// a version): a lecture's slides, its handout and a sample program, with
// text beside them or none. Each is uploaded first (document.upload_url) and
// named, in order, by its upload token and its name in the call that writes
// the version (files), and recorded with the version, in its transaction,
// as part of it. The one upload_token the version took before is one file,
// named after its upload or the document, and is kept for a release.
//
// Who may read a file is who may read its version: document.get lists a
// version's files, each with a URL to download it under its name, and
// document.file gives one by its id. Each file of material, instructions or
// a rubric has a text version of its own.
//
// The version's own columns (storage_key, content_type, byte_size, checksum)
// still name its first file, and the reads give the first file's in the
// fields they gave before, for the runtimes and front ends of the release
// before, which read them. Both are deprecated.

const (
	// DefaultFilesPerVersion and DefaultVersionBytes are the limits a
	// version's files are held to where nothing else is said: twenty files,
	// and 200 MiB in all.
	DefaultFilesPerVersion = 20
	DefaultVersionBytes    = 200 << 20
	// MaxFilesPerVersion is the most any version holds, whatever is said:
	// what the database holds it to (document_version_file).
	MaxFilesPerVersion = 100
)

// DocumentLimits bounds the files one version of a document holds. Each file
// is held to MaxUploadBytes, as ever.
type DocumentLimits struct {
	// FilesPerVersion bounds how many.
	FilesPerVersion int
	// VersionBytes bounds how much, all of them together.
	VersionBytes int64
}

func (l DocumentLimits) withDefaults() DocumentLimits {
	if l.FilesPerVersion <= 0 {
		l.FilesPerVersion = DefaultFilesPerVersion
	}
	l.FilesPerVersion = min(l.FilesPerVersion, MaxFilesPerVersion)
	if l.VersionBytes <= 0 {
		l.VersionBytes = DefaultVersionBytes
	}
	return l
}

// FileIn is one file a version is to hold, named in the call that writes it.
type FileIn struct {
	UploadToken string `json:"upload_token" jsonschema:"from document.upload_url, once the file's bytes are PUT to its upload_url"`
	Filename    string `json:"filename,omitempty" jsonschema:"the file's name, as readers are shown it and as it downloads, e.g. week1-slides.pdf: 1 to 255 characters on one line, a name and not a path. If omitted, the name given to document.upload_url; one of the two is required"`
}

// FileView is a file of a version, as its readers are shown it: never where
// it is kept.
type FileView struct {
	ID          uuid.UUID `json:"id" jsonschema:"the file's id: document.file takes it, and document.text, for its text version"`
	Position    int32     `json:"position" jsonschema:"its place among the version's files, from 1, as they were given"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type" jsonschema:"the media type its uploader declared"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    *string   `json:"checksum,omitempty" jsonschema:"sha256:<hex> where the store worked it out from the bytes, etag:<value> where all it has is an object store's tag"`
	DownloadURL *string   `json:"download_url,omitempty" jsonschema:"document.get only: a short-lived URL that serves the file as a download, saved under its name; GET it with no Authorization header"`
	Text        *TextView `json:"text,omitempty" jsonschema:"the file's text version: the file transcribed into Markdown, for a file of material, instructions or a rubric; absent for any other. Its body, in document.get only, while the bodies given with the version come to at most 65536 bytes"`
}

func fileView(f dbq.DocumentVersionFile) FileView {
	return FileView{ID: f.ID, Position: f.Position, Filename: f.Filename, ContentType: f.ContentType, ByteSize: f.ByteSize,
		Checksum: f.Checksum}
}

var errFilesAndUploadToken = apperr.Invalid("give files, or upload_token for one file, not both").With("reason", "files_and_upload_token")

// errVersionTooLarge refuses files that come to more than a version holds.
func errVersionTooLarge(d Deps, size int64) *apperr.Error {
	return apperr.Precondition("the files come to %d bytes; a version holds at most %d", size, d.Documents.VersionBytes).
		With("reason", "version_too_large").With("byte_size", size).With("max_version_bytes", d.Documents.VersionBytes)
}

// check holds what a version is given to what one may hold, as far as can be
// told without asking the store: files or upload_token, not both; no more
// files than a version holds; no upload named twice; and each name given a
// name. It records nothing.
func (c Content) check(d Deps) error {
	if c.UploadToken != nil && len(c.Files) > 0 {
		return errFilesAndUploadToken
	}
	if len(c.Files) > d.Documents.FilesPerVersion {
		return apperr.Invalid("a version holds at most %d files; this names %d", d.Documents.FilesPerVersion, len(c.Files)).
			With("reason", "too_many_files").With("max_files", d.Documents.FilesPerVersion)
	}
	seen := map[string]bool{}
	for _, f := range c.Files {
		if seen[f.UploadToken] {
			return apperr.Invalid("the same upload is named twice").With("reason", "duplicate_file")
		}
		seen[f.UploadToken] = true
		if f.Filename != "" {
			if _, err := checkFilename(f.Filename); err != nil {
				return err
			}
		}
	}
	return nil
}

// namedUpload is a file a version is to hold, by its upload token, and the
// name it is to be kept under.
type namedUpload struct {
	token, filename string
}

// named says what each file of the content is to be called, in order: the
// name given with it; else the name given when it was uploaded; and for the
// one file of upload_token, which names none, else its document's title,
// made a name (legacyFilename). A file of files with neither is refused.
func (c Content) named(d Deps, title string) ([]namedUpload, error) {
	if err := c.check(d); err != nil {
		return nil, err
	}
	if c.UploadToken != nil {
		claim, err := d.Uploads.VerifyUpload(*c.UploadToken)
		if err != nil {
			return nil, errBadUploadToken
		}
		name := legacyFilename(title, claim.ContentType)
		if n, err := checkFilename(claim.Filename); claim.Filename != "" && err == nil {
			name = n
		}
		return []namedUpload{{token: *c.UploadToken, filename: name}}, nil
	}
	out := make([]namedUpload, len(c.Files))
	for i, f := range c.Files {
		name := f.Filename
		if name == "" {
			claim, err := d.Uploads.VerifyUpload(f.UploadToken)
			if err != nil {
				return nil, errBadUploadToken
			}
			if name = claim.Filename; name == "" {
				return nil, apperr.Invalid("file %d has no name: give its filename, here or to document.upload_url", i+1).
					With("reason", "filename_required")
			}
		}
		n, err := checkFilename(name)
		if err != nil {
			return nil, err
		}
		out[i] = namedUpload{token: f.UploadToken, filename: n}
	}
	return out, nil
}

// errBadUploadToken is what claimUpload says of a token that is not good.
var errBadUploadToken = apperr.Invalid("upload_token is not valid").With("reason", "bad_upload_token")

// versionFile is a file of a version, claimed, or checked and ready to be.
type versionFile struct {
	upload
	filename string
}

// claimFiles claims the files a version is to hold, in order: each checked
// and, when finalize is set, moved where no upload URL reaches it
// (claimUpload); and holds them, together, to what a version holds. A dry
// run (finalize false), for a proposal, moves nothing.
func claimFiles(ctx context.Context, d Deps, q dbq.Querier, m *domain.Member, courseID uuid.UUID, kind string,
	files []namedUpload, finalize bool) ([]versionFile, error) {
	out := make([]versionFile, len(files))
	var total int64
	for i, f := range files {
		up, err := claimUpload(ctx, d, q, m, courseID, kind, f.token, finalize)
		if err != nil {
			return nil, err
		}
		out[i] = versionFile{upload: up, filename: f.filename}
		total += up.info.Size
	}
	if total > d.Documents.VersionBytes {
		return nil, errVersionTooLarge(d, total)
	}
	return out, nil
}

// checkProposedVersion is what a version that is to wait for a decision is
// held to, as to its files, when it is proposed (Pin): that they may be
// held, as far as they can be told now, and are young enough to outlast
// the proposal (checkUploadAge). All of it is asked again when it is
// approved, and the files are named then.
func checkProposedVersion(ctx context.Context, d Deps, q dbq.Querier, m *domain.Member, now time.Time, courseID uuid.UUID,
	kind, title string, c Content) error {
	files, err := c.named(d, title)
	if err != nil {
		return err
	}
	if _, err := claimFiles(ctx, d, q, m, courseID, kind, files, false); err != nil {
		return err
	}
	return checkUploadAge(ctx, d, now, c.uploads()...)
}

// insertFiles records a version's files, claimed already, in order, with it.
func insertFiles(ctx context.Context, ec *tool.ExecCtx, documentID, versionID uuid.UUID, files []versionFile) ([]uuid.UUID, error) {
	fileIDs := make([]uuid.UUID, len(files))
	for i, f := range files {
		fileIDs[i] = ids.New()
		if err := ec.Q.InsertDocumentVersionFile(ctx, dbq.InsertDocumentVersionFileParams{ID: fileIDs[i], VersionID: versionID,
			DocumentID: documentID, Position: int32(i + 1), Filename: f.filename, StorageKey: f.key,
			ContentType: f.info.ContentType, ByteSize: f.info.Size, Checksum: nonEmpty(f.info.Checksum), CreatedAt: ec.Now}); err != nil {
			return nil, err
		}
	}
	return fileIDs, nil
}

// fileExtensions is the extension a file of a type is given when it is named
// after its document's title, which says none. The database names a file
// the same way (document_file_name, migration 0023), and the two lists are
// the same.
var fileExtensions = map[string]string{
	"application/pdf":    ".pdf",
	"application/msword": ".doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.ms-powerpoint":                                             ".ppt",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/vnd.ms-excel":                                                  ".xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.oasis.opendocument.text":                                   ".odt",
	"application/vnd.oasis.opendocument.presentation":                           ".odp",
	"application/vnd.oasis.opendocument.spreadsheet":                            ".ods",
	"application/zip":  ".zip",
	"application/json": ".json",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"text/html":        ".html",
	"text/x-python":    ".py",
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
	"image/svg+xml":    ".svg",
	"audio/mpeg":       ".mp3",
	"video/mp4":        ".mp4",
}

// legacyFilename is what a file is called that is given no name: its
// document's title, made a name — each run of control characters, of
// characters that turn the text round and of slashes a space, trimmed —
// with the extension of its type where the title does not end in it, at
// most 255 characters; "file" for a title that leaves nothing. As the
// database's document_file_name.
func legacyFilename(title, contentType string) string {
	var b strings.Builder
	gap := false
	for _, r := range title {
		if r == '/' || r == '\\' || hidden(r) {
			gap = true
			continue
		}
		if gap {
			b.WriteByte(' ')
			gap = false
		}
		b.WriteRune(r)
	}
	if gap {
		b.WriteByte(' ')
	}
	name := strings.Trim(b.String(), " ")
	if name == "" {
		name = "file"
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	ext := fileExtensions[strings.ToLower(strings.TrimSpace(mediaType))]
	most := maxFilenameChars
	if ext != "" && !strings.HasSuffix(strings.ToLower(name), ext) {
		most -= len(ext)
	} else {
		ext = ""
	}
	if utf8.RuneCountInString(name) > most {
		name = string([]rune(name)[:most])
	}
	return strings.Trim(name, " ") + ext
}

// ---------------------------------------------------------------------------
// Reading a version's files
// ---------------------------------------------------------------------------

// versionFiles are a version's files as document.get shows them: each with a
// URL to download it under its name, and its text version, with its text
// while the texts given with the version come to at most one part.
func versionFiles(ctx context.Context, d Deps, q dbq.Querier, version uuid.UUID) ([]FileView, error) {
	rows, err := q.ListVersionFiles(ctx, version)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	texts, err := q.ListVersionTextViews(ctx, version)
	if err != nil {
		return nil, err
	}
	byFile := make(map[uuid.UUID]dbq.ListVersionTextViewsRow, len(texts))
	for _, t := range texts {
		byFile[t.FileID] = t
	}
	budget := int32(TextPartBytes)
	out := make([]FileView, len(rows))
	for i, f := range rows {
		out[i] = fileView(f)
		if d.Blob != nil {
			url, err := d.Blob.PresignDownload(ctx, f.StorageKey, f.Filename, downloadTTL)
			if err != nil {
				return nil, err
			}
			out[i].DownloadURL = &url
		}
		t, ok := byFile[f.ID]
		if !ok {
			continue
		}
		out[i].Text = textView(t.Status, t.Source, t.Pages, t.Model, t.Reason, t.Revision, t.ProducedAt, t.EditedByMemberID,
			t.EditedAt, t.UpdatedAt, t.Bytes, t.EditedByName)
		if t.Status != textDone || t.Bytes > budget {
			continue
		}
		b, err := q.GetTextBody(ctx, dbq.GetTextBodyParams{VersionID: version, FileID: f.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			out[i].Text = nil // purged meanwhile
			continue
		}
		if err != nil {
			return nil, err
		}
		// Read again with its text: what is shown is of one revision.
		out[i].Text = textView(b.Status, b.Source, b.Pages, b.Model, b.Reason, b.Revision, b.ProducedAt, b.EditedByMemberID,
			b.EditedAt, b.UpdatedAt, b.Bytes, b.EditedByName)
		if b.Status == textDone && b.Bytes <= budget {
			out[i].Text.Body = b.Body
			budget -= b.Bytes
		}
	}
	return out, nil
}

// documentFiles are the files of every version of a document, by version,
// each version's in order, with their text versions, without their text.
func documentFiles(ctx context.Context, q dbq.Querier, document uuid.UUID) (map[uuid.UUID][]FileView, error) {
	rows, err := q.ListDocumentFiles(ctx, document)
	if err != nil {
		return nil, err
	}
	texts, err := textsOf(ctx, q, document)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]FileView{}
	for _, f := range rows {
		v := fileView(f)
		v.Text = texts[f.ID]
		out[f.VersionID] = append(out[f.VersionID], v)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// document.file
// ---------------------------------------------------------------------------

type DocumentFileIn struct {
	tool.InCourse
	DocumentID uuid.UUID `json:"document_id"`
	FileID     uuid.UUID `json:"file_id" jsonschema:"a file's id, from a version's files in document.get or document.versions"`
}

// DocumentFileOut is a file as FileView shows it, with its version, and a URL
// of its own.
type DocumentFileOut struct {
	ID          uuid.UUID `json:"id"`
	Position    int32     `json:"position" jsonschema:"its place among its version's files, from 1"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type" jsonschema:"the media type its uploader declared"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    *string   `json:"checksum,omitempty"`
	Text        *TextView `json:"text,omitempty" jsonschema:"the file's text version, without the text: document.text reads it; absent for a file of a submission or of feedback"`
	DocumentID  uuid.UUID `json:"document_id"`
	VersionID   uuid.UUID `json:"version_id"`
	Seq         int32     `json:"seq" jsonschema:"its version's seq"`
	Published   bool      `json:"published" jsonschema:"whether its version is the published one"`
	DownloadURL string    `json:"download_url" jsonschema:"a short-lived URL that serves the file as a download, saved under its name: GET it as it is, with no Authorization header"`
	ExpiresAt   time.Time `json:"expires_at" jsonschema:"when download_url stops working, about 15 minutes from now; ask again for another"`
}

// errNoFile is what a file that does not exist, and one of a version the
// caller may not read, both answer.
var errNoFile = apperr.Missing("no such file of this document")

// documentFile is document.get for one file: a fresh URL for it, to whoever
// may read its version, as document.get decides that when the version is
// named (readableVersion), and to nobody else.
func documentFile(d Deps) tool.Tool {
	return tool.Define(tool.Spec[DocumentFileIn, DocumentFileOut]{
		Name: "document.file",
		Description: "One file of a version of a document: its name, type, size and text version (without the text), and a " +
			"short-lived URL that serves it as a download, under its name. For whoever may read its version, as document.get " +
			"with that version_id: students the published version's, and the one their own work was handed in under. " +
			"document.get and document.versions list each version's files, with their ids.",
		Kind: tool.Read, Gate: anyDocumentRead,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/documents/{document_id}/files/{file_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DocumentFileIn) (tool.Target, error) {
			return documentTarget(ctx, q, in.CourseID, in.DocumentID, readPerm)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DocumentFileIn) (DocumentFileOut, error) {
			f, err := rc.Q.GetDocumentFile(ctx, dbq.GetDocumentFileParams{ID: in.FileID, DocumentID: in.DocumentID})
			if errors.Is(err, pgx.ErrNoRows) {
				return DocumentFileOut{}, errNoFile
			}
			if err != nil {
				return DocumentFileOut{}, err
			}
			doc, v, err := readableVersion(ctx, rc, in.CourseID, in.DocumentID, &f.VersionID)
			if apperr.Is(err, apperr.NotFound) {
				return DocumentFileOut{}, errNoFile
			}
			if err != nil {
				return DocumentFileOut{}, err
			}
			if d.Blob == nil {
				return DocumentFileOut{}, errNoFileStorage
			}
			url, err := d.Blob.PresignDownload(ctx, f.StorageKey, f.Filename, downloadTTL)
			if err != nil {
				return DocumentFileOut{}, err
			}
			out := DocumentFileOut{ID: f.ID, Position: f.Position, Filename: f.Filename, ContentType: f.ContentType,
				ByteSize: f.ByteSize, Checksum: f.Checksum, DocumentID: doc.ID, VersionID: v.ID, Seq: v.Seq,
				Published:   doc.PublishedVersionID != nil && *doc.PublishedVersionID == v.ID,
				DownloadURL: url, ExpiresAt: rc.Now.Add(downloadTTL)}
			t, err := rc.Q.GetTextView(ctx, dbq.GetTextViewParams{VersionID: v.ID, FileID: f.ID})
			if err == nil {
				out.Text = textView(t.Status, t.Source, t.Pages, t.Model, t.Reason, t.Revision, t.ProducedAt, t.EditedByMemberID,
					t.EditedAt, t.UpdatedAt, t.Bytes, t.EditedByName)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return DocumentFileOut{}, err
			}
			return out, nil
		},
	})
}
