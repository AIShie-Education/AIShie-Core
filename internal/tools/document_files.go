package tools

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

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
// Each file of material, instructions or a rubric has a text version of its
// own.
//
// The version's own columns (storage_key, content_type, byte_size, checksum)
// still name its first file, for the release before, which reads them. They
// are deprecated.

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

// errNoFile is what a file that does not exist, and one of a version the
// caller may not read, both answer.
var errNoFile = apperr.Missing("no such file of this document")
