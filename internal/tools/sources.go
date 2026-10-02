package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// What an answer relied on (docs/schema.md §2.8, Sources of an answer). An
// agent answering may say which of the course's materials it read for the
// answer (conversation.answer's sources): each a version of a document — a
// material, instructions or a rubric — and, if it says so, one of the
// version's files, a page or a slide of it and a part of its text. They are
// kept with the answer (conversation_message_source), in order, a row each.
//
// What it names it must be able to read as it answers: each version is one
// document.get would show the answering seat, named by id, at that moment,
// and not purged; each file one of that version's. A source that is not is
// refused, saying which (sources[i]), before anything is recorded or
// proposed, and again when a proposal is approved, as the proposer's.
//
// Who reads the answer reads its sources as they may read the documents now,
// whoever they are: what document.get would show them of each version, named
// by id, with the document's title as it is now. A version they may not
// open, of a document they may read — an earlier version, replaced since, to
// a student who reads the published one — is said to be of that document,
// with no version, file or page (other_version). A source they may not read
// at all — a document not published to them, archived, withheld with its
// assignment, or of a kind they do not read — is said to be there, and
// nothing else (restricted): they learn that the answer relied on something,
// not what. A purged version, or a purged document, is restricted to every
// reader: what it said is gone, and so is anything that would say what it
// was.

// maxSources bounds the sources one answer names, as the database does.
const maxSources = 20

// maxSourceLocator bounds a page, a slide or a part, as the database does.
const maxSourceLocator = 100000

// SourceIn is one thing an answer relied on, as conversation.answer is told
// it.
type SourceIn struct {
	DocumentID uuid.UUID  `json:"document_id" jsonschema:"the document read: a material, instructions or a rubric of the course"`
	VersionID  uuid.UUID  `json:"version_id" jsonschema:"the version read: version.id in document.get, or version_id in document.text"`
	FileID     *uuid.UUID `json:"file_id,omitempty" jsonschema:"one of the version's files, when the answer relied on that file: its id in version.files of document.get, or file_id in document.text"`
	Page       *int32     `json:"page,omitempty" jsonschema:"a page of the file, as its text heads it (## 第 N 頁 / ## Page N), from 1; with file_id, not with slide"`
	Slide      *int32     `json:"slide,omitempty" jsonschema:"a slide of the file, as its text heads it (## Slide N), from 1; with file_id, not with page"`
	Part       *int32     `json:"part,omitempty" jsonschema:"the part of the file's text read, as document.text numbers its parts, from 1; with file_id"`
}

// SourceView is a source of an answer as one reader is shown it.
type SourceView struct {
	Restricted   bool       `json:"restricted,omitempty" jsonschema:"true: the answer relied on a course material you may not open now (not published to you, archived, withheld with its assignment, or purged); nothing else is said of it"`
	OtherVersion bool       `json:"other_version,omitempty" jsonschema:"true: the answer relied on a version of this document you may not open, such as an earlier one since replaced; document.get without version_id gives the one you read; no version, file or page is said"`
	DocumentID   *uuid.UUID `json:"document_id,omitempty"`
	Kind         *string    `json:"kind,omitempty" jsonschema:"material, instructions or rubric"`
	Title        *string    `json:"title,omitempty" jsonschema:"the document's title as it is now"`
	VersionID    *uuid.UUID `json:"version_id,omitempty" jsonschema:"the version the answer read; document.get takes it as version_id"`
	Seq          *int32     `json:"seq,omitempty" jsonschema:"that version's seq"`
	Published    *bool      `json:"published,omitempty" jsonschema:"whether that version is the document's published one now"`
	FileID       *uuid.UUID `json:"file_id,omitempty" jsonschema:"the file of the version the answer read, when it named one; document.file takes it"`
	Filename     *string    `json:"filename,omitempty"`
	Page         *int32     `json:"page,omitempty"`
	Slide        *int32     `json:"slide,omitempty"`
	Part         *int32     `json:"part,omitempty" jsonschema:"the part of the file's text, as document.text numbers them"`
}

// sourceField is where in a call a source is, for a refusal to say.
func sourceField(i int) string { return fmt.Sprintf("sources[%d]", i) }

// errSource refuses the source at i, saying which and why.
func errSource(i int, reason, format string, args ...any) *apperr.Error {
	return apperr.Invalid("%s: %s", sourceField(i), fmt.Sprintf(format, args...)).
		With("field", sourceField(i)).With("index", i).With("reason", reason)
}

// checkSources is what an answer's sources say alone (Check): at most
// maxSources; each naming a document and a version; a page, a slide or a
// part only with a file, a page or a slide and not both, each from 1; and
// none named twice.
func checkSources(sources []SourceIn) error {
	if len(sources) > maxSources {
		return apperr.Invalid("an answer names at most %d sources; this one names %d", maxSources, len(sources)).
			With("field", "sources").With("reason", "too_many_sources").With("max_sources", maxSources)
	}
	seen := map[string]int{}
	for i, s := range sources {
		if s.DocumentID == uuid.Nil || s.VersionID == uuid.Nil {
			return errSource(i, "bad_source", "a source names its document_id and its version_id")
		}
		if s.FileID == nil && (s.Page != nil || s.Slide != nil || s.Part != nil) {
			return errSource(i, "bad_source", "a page, a slide or a part is of a file: give file_id as well")
		}
		if s.Page != nil && s.Slide != nil {
			return errSource(i, "bad_source", "give a page or a slide, not both")
		}
		for _, n := range []struct {
			what string
			n    *int32
		}{{"page", s.Page}, {"slide", s.Slide}, {"part", s.Part}} {
			if n.n != nil && (*n.n < 1 || *n.n > maxSourceLocator) {
				return errSource(i, "bad_source", "a %s counts from 1 to %d", n.what, maxSourceLocator)
			}
		}
		k := strings.Join([]string{s.DocumentID.String(), s.VersionID.String(), idText(s.FileID), locator(s.Page),
			locator(s.Slide), locator(s.Part)}, " ")
		if j, twice := seen[k]; twice {
			return errSource(i, "duplicate_source", "the same source as %s", sourceField(j))
		}
		seen[k] = i
	}
	return nil
}

// locator is a page, a slide or a part as a source's key writes it.
func locator(n *int32) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(int(*n))
}

// checkSourcesReadable holds an answer's sources to what its respondent, m,
// may read at now (Validate, and Execute): each a version document.get would
// show m, named by id, of a course's material, instructions or rubric, not
// purged; each file one of that version's. A version m may not read is
// refused as one that does not exist, so that nobody learns which.
func checkSourcesReadable(ctx context.Context, q dbq.Querier, m *domain.Member, courseID uuid.UUID, sources []SourceIn) error {
	r := readerAs(q, m)
	for i, s := range sources {
		unreadable := errSource(i, "source_unreadable", "names no version of a course material (a material, instructions or a "+
			"rubric) that you may read")
		doc, err := loadDocument(ctx, q, courseID, s.DocumentID)
		if apperr.Is(err, apperr.NotFound) {
			return unreadable
		}
		if err != nil {
			return err
		}
		// A student's work and a grader's feedback are no source, and are
		// refused as a document that is not there, whoever may read them.
		if !courseLevel(doc.Kind) || !m.Perm(readPerm(doc.Kind)).Allowed() {
			return unreadable
		}
		_, v, err := r.versionOf(ctx, doc, &s.VersionID)
		if apperr.Is(err, apperr.NotFound) || (err == nil && v == nil) {
			return unreadable
		}
		if err != nil {
			return err
		}
		if v.PurgedAt != nil || doc.PurgedAt != nil {
			return errSource(i, "source_purged", "that version was purged: nothing of it is left to rely on")
		}
		if s.FileID != nil {
			f, err := q.GetDocumentFile(ctx, dbq.GetDocumentFileParams{ID: *s.FileID, DocumentID: doc.ID})
			if err == nil && f.VersionID != v.ID {
				err = errNoSuchFile
			}
			if apperr.Is(err, apperr.NotFound) || errors.Is(err, pgx.ErrNoRows) {
				return errSource(i, "source_unreadable", "file_id names no file of that version")
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSources records an answer's sources, in order, with it: in its
// transaction, dated as it is.
func writeSources(ctx context.Context, ec *tool.ExecCtx, courseID, message uuid.UUID, sources []SourceIn) error {
	for i, s := range sources {
		if err := ec.Q.InsertMessageSource(ctx, dbq.InsertMessageSourceParams{MessageID: message, CourseID: courseID,
			Position: int32(i + 1), DocumentID: s.DocumentID, VersionID: s.VersionID, FileID: s.FileID, Page: s.Page,
			Slide: s.Slide, Part: s.Part, CreatedAt: ec.Now}); err != nil {
			return err
		}
	}
	return nil
}

// sourceReader shows one reader the sources of answers, as it may read
// their documents now. What it learns of each document and version it
// keeps: the sources of a page of messages name few.
type sourceReader struct {
	r        docReader
	courseID uuid.UUID
	docs     map[uuid.UUID]*dbq.GetDocumentWithOwnerRow // nil: not one the reader may read
	versions map[uuid.UUID]bool
	current  map[uuid.UUID]bool // whether the reader reads any version of the document
}

func newSourceReader(rc *tool.ReadCtx, courseID uuid.UUID) *sourceReader {
	return &sourceReader{r: readerOf(rc), courseID: courseID, docs: map[uuid.UUID]*dbq.GetDocumentWithOwnerRow{},
		versions: map[uuid.UUID]bool{}, current: map[uuid.UUID]bool{}}
}

// sourcesOf are the sources of the given messages, by message, as the
// reader is shown them; a message with none has no entry.
func (s *sourceReader) sourcesOf(ctx context.Context, messages []uuid.UUID) (map[uuid.UUID][]SourceView, error) {
	out := map[uuid.UUID][]SourceView{}
	if len(messages) == 0 {
		return out, nil
	}
	rows, err := s.r.q.ListMessageSources(ctx, messages)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		v, err := s.view(ctx, row)
		if err != nil {
			return nil, err
		}
		out[row.MessageID] = append(out[row.MessageID], v)
	}
	return out, nil
}

// document is the source's document, if the reader may read documents of
// its kind at all; nil otherwise.
func (s *sourceReader) document(ctx context.Context, row dbq.ListMessageSourcesRow) (*dbq.GetDocumentWithOwnerRow, error) {
	if d, known := s.docs[row.DocumentID]; known {
		return d, nil
	}
	var d *dbq.GetDocumentWithOwnerRow
	if s.r.m.Perm(readPerm(row.DocumentKind)).Allowed() {
		doc, err := loadDocument(ctx, s.r.q, s.courseID, row.DocumentID)
		if err != nil && !apperr.Is(err, apperr.NotFound) {
			return nil, err
		}
		if err == nil {
			d = &doc
		}
	}
	s.docs[row.DocumentID] = d
	return d, nil
}

// reads says whether the reader may read the version named, or, with
// none, any version of the document.
func (s *sourceReader) reads(ctx context.Context, doc *dbq.GetDocumentWithOwnerRow, version *uuid.UUID) (bool, error) {
	known, memo := s.current, doc.ID
	if version != nil {
		known, memo = s.versions, *version
	}
	if ok, done := known[memo]; done {
		return ok, nil
	}
	_, v, err := s.r.versionOf(ctx, *doc, version)
	if err != nil && !apperr.Is(err, apperr.NotFound) {
		return false, err
	}
	ok := err == nil && v != nil
	known[memo] = ok
	return ok, nil
}

func (s *sourceReader) view(ctx context.Context, row dbq.ListMessageSourcesRow) (SourceView, error) {
	restricted := SourceView{Restricted: true}
	if row.VersionPurgedAt != nil || row.DocumentPurgedAt != nil {
		return restricted, nil
	}
	doc, err := s.document(ctx, row)
	if err != nil || doc == nil {
		return restricted, err
	}
	kind, title := doc.Kind, doc.Title
	version := row.VersionID
	ok, err := s.reads(ctx, doc, &version)
	if err != nil {
		return restricted, err
	}
	if ok {
		published := doc.PublishedVersionID != nil && *doc.PublishedVersionID == row.VersionID
		seq := row.VersionSeq
		v := SourceView{DocumentID: &doc.ID, Kind: &kind, Title: &title, VersionID: &version, Seq: &seq, Published: &published,
			Page: row.Page, Slide: row.Slide, Part: row.Part}
		if row.FileID != nil {
			file := *row.FileID
			v.FileID, v.Filename = &file, row.Filename
		}
		return v, nil
	}
	if ok, err = s.reads(ctx, doc, nil); err != nil || !ok {
		return restricted, err
	}
	return SourceView{OtherVersion: true, DocumentID: &doc.ID, Kind: &kind, Title: &title}, nil
}
