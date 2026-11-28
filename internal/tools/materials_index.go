package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/memory"
)

// The passage index is derived data. Core writes it in the transaction that
// writes a body or a file's text, and the jobs runner rebuilds units that an
// older release left behind. Search permissions are never stored in it.

const (
	materialChunkRunes = 1000
	materialBodySource = "body_md"
)

// materialDocument is the part of a document its passages repeat.
type materialDocument struct {
	courseID uuid.UUID
	id       uuid.UUID
	title    string
}

func materialDoc(doc dbq.GetDocumentWithOwnerRow) materialDocument {
	return materialDocument{courseID: doc.CourseID, id: doc.ID, title: doc.Title}
}

// materialPassage is one row to put in document_search_passages.
type materialPassage struct {
	kind    string
	section int
	part    int
	content string
}

// replaceMaterialBody rebuilds a version's own Markdown. bodyRevision is 1
// because versions are immutable: purging one removes its passages by trigger.
func replaceMaterialBody(ctx context.Context, q dbq.Querier, doc materialDocument, version uuid.UUID, body *string, now time.Time) (int, error) {
	if err := q.DeleteMaterialBodyPassages(ctx, version); err != nil {
		return 0, err
	}
	if body == nil || strings.TrimSpace(*body) == "" {
		return 0, nil
	}
	return writeMaterialPassages(ctx, q, doc, version, nil, materialBodySource, 1, "", *body, now, false)
}

// rebuildMaterialBody reads the version again, as a backfill does after taking
// its document's lock, before replacing its body passages.
func rebuildMaterialBody(ctx context.Context, q dbq.Querier, doc materialDocument, version uuid.UUID, now time.Time) (int, error) {
	v, err := q.GetVersionOfDocument(ctx, dbq.GetVersionOfDocumentParams{ID: version, DocumentID: doc.id})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, q.DeleteMaterialBodyPassages(ctx, version)
	}
	if err != nil {
		return 0, err
	}
	if v.PurgedAt != nil {
		return 0, q.DeleteMaterialBodyPassages(ctx, version)
	}
	return replaceMaterialBody(ctx, q, doc, version, v.BodyMd, now)
}

// rebuildMaterialFile replaces one file's passages from its current completed
// text. A pending, failed, skipped, purged or absent text leaves no passages.
func rebuildMaterialFile(ctx context.Context, q dbq.Querier, doc materialDocument, version, file uuid.UUID, now time.Time) (int, error) {
	if err := q.DeleteMaterialFilePassages(ctx, dbq.DeleteMaterialFilePassagesParams{VersionID: version, FileID: &file}); err != nil {
		return 0, err
	}
	t, err := q.GetMaterialFileText(ctx, dbq.GetMaterialFileTextParams{VersionID: version, FileID: file})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if t.Body == nil || t.Source == nil {
		return 0, nil
	}
	return writeMaterialPassages(ctx, q, doc, version, &file, *t.Source, t.Revision, t.Filename, *t.Body, now, true)
}

// clearMaterialFile removes an index as soon as its text is sent back for
// transcription, so a search never returns the text that was withdrawn.
func clearMaterialFile(ctx context.Context, q dbq.Querier, version, file uuid.UUID) error {
	return q.DeleteMaterialFilePassages(ctx, dbq.DeleteMaterialFilePassagesParams{VersionID: version, FileID: &file})
}

// rebuildMaterialDocument rebuilds all the versions' derived passages, used
// when their shared title changes and when an archived document comes back.
func rebuildMaterialDocument(ctx context.Context, q dbq.Querier, doc materialDocument, now time.Time) (int, error) {
	if _, err := q.DeleteMaterialDocumentPassages(ctx, doc.id); err != nil {
		return 0, err
	}
	bodies, err := q.ListMaterialDocumentBodies(ctx, doc.id)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, v := range bodies {
		n, err := replaceMaterialBody(ctx, q, doc, v.VersionID, v.BodyMd, now)
		written += n
		if err != nil {
			return written, err
		}
	}
	texts, err := q.ListMaterialDocumentTexts(ctx, doc.id)
	if err != nil {
		return written, err
	}
	for _, t := range texts {
		if t.Body == nil || t.Source == nil {
			continue
		}
		n, err := writeMaterialPassages(ctx, q, doc, t.VersionID, &t.FileID, *t.Source, t.Revision, t.Filename, *t.Body, now, true)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func clearMaterialDocument(ctx context.Context, q dbq.Querier, document uuid.UUID) error {
	_, err := q.DeleteMaterialDocumentPassages(ctx, document)
	return err
}

// writeMaterialPassages writes body or file text after deleting its old rows.
// A document's title and a file's name are indexed with every passage, while
// content stays the original Markdown for the excerpt and read pointer.
func writeMaterialPassages(ctx context.Context, q dbq.Querier, doc materialDocument, version uuid.UUID, file *uuid.UUID,
	source string, revision int32, filename, body string, now time.Time, byTextPart bool) (int, error) {
	passages := bodyMaterialPassages(body)
	if byTextPart {
		passages = fileMaterialPassages(body)
	}
	written := 0
	for _, p := range passages {
		text := p.content
		if filename != "" {
			text = filename + "\n" + text
		}
		text = doc.title + "\n" + text
		arg := dbq.InsertMaterialPassageParams{CourseID: doc.courseID, DocumentID: doc.id, VersionID: version, FileID: file,
			TextSource: source, TextRevision: revision, SectionKind: p.kind, SectionNo: int32(p.section), PartNo: int32(p.part), //nolint:gosec // bounded by Core's document size limits
			Content: p.content, SearchText: memory.SearchText(text), UpdatedAt: now}
		if err := q.InsertMaterialPassage(ctx, arg); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// bodyMaterialPassages cuts a version's own Markdown at headings, paragraphs
// and lines, about 1000 runes at a time. Its part numbers are simply its
// passage numbers: document_get, rather than document_text, is its read call.
func bodyMaterialPassages(body string) []materialPassage {
	var out []materialPassage
	for _, piece := range splitMaterialPiece(body) {
		out = append(out, materialPassage{kind: "body", part: len(out) + 1, content: piece})
	}
	return out
}

var (
	slideHeading = regexp.MustCompile(`(?i)^slide[[:space:]]+([0-9]+)(?:$|[[:space:]:：.\-].*)$`)
	pageHeading  = regexp.MustCompile(`(?i)^page[[:space:]]+([0-9]+)(?:$|[[:space:]:：.\-].*)$`)
	hanPageHead  = regexp.MustCompile(`^第[[:space:]]*([0-9]+)[[:space:]]*頁(?:$|[[:space:]:：.\-].*)$`)
)

// fileMaterialPassages cuts with textParts first, making part_no exactly the
// part document.text returns. Recognised second-level headings start a page or
// slide; a continuation in the next part keeps the preceding section.
func fileMaterialPassages(body string) []materialPassage {
	var out []materialPassage
	kind, section := "body", 0
	for i, part := range textParts(body) {
		partNo := i + 1
		sectionStart := 0
		flush := func(end int) {
			if end <= sectionStart {
				return
			}
			for _, piece := range splitMaterialPiece(part[sectionStart:end]) {
				out = append(out, materialPassage{kind: kind, section: section, part: partNo, content: piece})
			}
			sectionStart = end
		}
		for cursor := 0; cursor < len(part); {
			next := len(part)
			if newline := strings.IndexByte(part[cursor:], '\n'); newline >= 0 {
				next = cursor + newline + 1
			}
			if newKind, number, heading := materialSection(part[cursor:next]); heading {
				flush(cursor)
				kind, section = newKind, number
			}
			cursor = next
		}
		flush(len(part))
	}
	return out
}

// materialSection recognises a second-level page or slide heading.
func materialSection(line string) (string, int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "##") || strings.HasPrefix(line, "###") {
		return "", 0, false
	}
	heading := strings.TrimSpace(line[2:])
	for _, h := range []struct {
		re   *regexp.Regexp
		kind string
	}{{slideHeading, "slide"}, {pageHeading, "page"}, {hanPageHead, "page"}} {
		if match := h.re.FindStringSubmatch(heading); match != nil {
			n, err := strconv.ParseInt(match[1], 10, 31)
			if err == nil && n > 0 {
				return h.kind, int(n), true
			}
		}
	}
	return "", 0, false
}

// splitMaterialPiece cuts text into non-blank pieces of at most about 1000
// runes, preferring a new Markdown heading, then a paragraph, then a line.
// It never splits a rune and never duplicates non-blank text.
func splitMaterialPiece(text string) []string {
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes); {
		end := len(runes)
		if end-start > materialChunkRunes {
			end = start + materialChunkRunes
			cut := 0
			// A heading starts the next passage.
			for i := start + materialChunkRunes/2; i < end-1; i++ {
				if runes[i] == '\n' && runes[i+1] == '#' {
					cut = i + 1
				}
			}
			if cut == 0 {
				for i := start + materialChunkRunes/2; i < end-1; i++ {
					if runes[i] == '\n' && runes[i+1] == '\n' {
						cut = i + 2
					}
				}
			}
			if cut == 0 {
				for i := start + materialChunkRunes/2; i < end; i++ {
					if runes[i] == '\n' {
						cut = i + 1
					}
				}
			}
			if cut > start {
				end = cut
			}
		}
		if piece := strings.TrimSpace(string(runes[start:end])); piece != "" {
			out = append(out, piece)
		}
		start = end
	}
	return out
}

// BackfillMaterialPassages rebuilds up to limit missing or stale bodies and
// file texts, each in its own short transaction. The document lock serialises
// with every synchronous writer; content is read again after it is taken.
func BackfillMaterialPassages(ctx context.Context, pool *pgxpool.Pool, limit int32, now time.Time) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	needed, err := dbq.New(pool).ListMaterialBackfill(ctx, limit)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, unit := range needed {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return written, err
		}
		q := dbq.New(tx)
		n, err := func() (int, error) {
			if err := q.LockDocument(ctx, unit.DocumentID); err != nil {
				return 0, err
			}
			doc, err := loadDocument(ctx, q, unit.CourseID, unit.DocumentID)
			if err != nil {
				return 0, err
			}
			if doc.Status != "active" || doc.PurgedAt != nil || !courseLevel(doc.Kind) {
				return 0, nil
			}
			if unit.FileID == nil {
				return rebuildMaterialBody(ctx, q, materialDoc(doc), unit.VersionID, now)
			}
			return rebuildMaterialFile(ctx, q, materialDoc(doc), unit.VersionID, *unit.FileID, now)
		}()
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		written += n
		if err != nil {
			return written, fmt.Errorf("material search backfill for document %s: %w", unit.DocumentID, err)
		}
	}
	return written, nil
}
