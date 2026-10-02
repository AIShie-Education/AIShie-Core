package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Exporting conversations for audit (docs/schema.md §2.8). An administrator
// takes away what was said in the site's conversations: root and the
// platform's administrators anywhere, a department's administrators in the
// courses of the departments they administer and beneath them, as they
// manage those courses from outside (the Admin gate), and nobody else. An
// agent never does, whatever role it was given: an export is a person's
// decision to take personal data away, and an agent is refused
// (people_only), reading kind to refuse as a password reset does.
//
// An export is an action, and its own record: who made it, when, over what
// (its payload, the filters) and what it held (its result, the counts and
// the files). It is never proposed: the Admin gate allows or denies, and a
// denial is on record as any is. What it holds is everything written in
// the conversations it chooses, whoever may read them now: a retracted
// message with its text, as the database keeps it, marked retracted; the
// files each message carries, described and never their bytes; and the
// answers and questions proposed in them and never posted, which are
// actions and no messages, with what they said. It holds no token, password
// or secret: a proposal's files are named, never the upload tokens it names
// them by, and nothing says where a file is kept.
//
// Its files are written to the file store as they are made, a page of
// conversations at a time, and never held whole: exports/<export>.jsonl,
// the conversations as JSON Lines, and exports/<export>.csv, the messages
// as CSV. A URL to download each lasts fifteen minutes, and is in no record
// (SecretOut): conversation.export_file gives another to whoever made the
// export, while its files are kept, ExportLimits.TTL, after which the sweep
// removes them (jobs).

const (
	// ExportPrefix begins the key of every file of an export:
	// exports/<export>.jsonl and exports/<export>.csv, where <export> is the
	// export's action. Nothing but an export writes there, and the sweep
	// removes what is there once it is ExportLimits.TTL old. The files are
	// side by side, in no directory of their own, so that removing them
	// leaves none behind on the server's disk.
	ExportPrefix = "exports/"

	// DefaultExportMaxMessages, DefaultExportMaxBytes and DefaultExportTTL
	// are an export's limits where nothing else is said: 100,000 messages,
	// answers and questions proposed and never posted counted with them;
	// 256 MiB of their text; and its files kept for a day.
	DefaultExportMaxMessages = 100000
	DefaultExportMaxBytes    = 256 << 20
	DefaultExportTTL         = 24 * time.Hour

	// ToolConversationExport is the export's action type.
	ToolConversationExport = "conversation.export"
	// EventConversationExported is its news, in no course's feed.
	EventConversationExported = "conversation.exported"

	// ExportJSONL and ExportCSV are an export's two files, by the format
	// conversation.export_file names them by.
	ExportJSONL = "jsonl"
	ExportCSV   = "csv"

	// exportHold is how long an export may take to write before its answer
	// is given: the connection it came on is held open that long.
	exportHold = 5 * time.Minute
	// A page: of conversations, and of their messages.
	exportConversationPage = 100
	exportMessagePage      = 1000
)

// ExportLimits bounds what one export holds, and says how long its files are
// kept.
type ExportLimits struct {
	// MaxMessages bounds the messages one export holds, answers and
	// questions proposed and never posted counted with them.
	MaxMessages int
	// MaxBytes bounds their text, in bytes: what is said, which is most of
	// what the files hold.
	MaxBytes int64
	// TTL is how long an export's files are kept, from when it is made.
	TTL time.Duration
}

func (l ExportLimits) withDefaults() ExportLimits {
	if l.MaxMessages <= 0 {
		l.MaxMessages = DefaultExportMaxMessages
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultExportMaxBytes
	}
	if l.TTL <= 0 {
		l.TTL = DefaultExportTTL
	}
	return l
}

// fileBytes bounds one file of an export as the store writes it: the text,
// escaped at worst sixfold (a control character in JSON), and what is said
// of each message beside it. It is a guard; the limits are held before.
func (l ExportLimits) fileBytes() int64 {
	return 6*l.MaxBytes + int64(l.MaxMessages)*8192 + 1<<20
}

// ExportKey is where an export's file of the given format is kept.
func ExportKey(export uuid.UUID, format string) string {
	return ExportPrefix + export.String() + "." + format
}

// exportFileKind is what each format's file is called when it is
// downloaded, and its type.
type exportFileKind struct {
	download, contentType string
}

var exportFiles = map[string]exportFileKind{
	ExportJSONL: {"conversations-%s.jsonl", "application/x-ndjson"},
	ExportCSV:   {"messages-%s.csv", "text/csv; charset=utf-8"},
}

// exportFormats are the formats in the order an export lists its files.
var exportFormats = []string{ExportJSONL, ExportCSV}

func exportDownloadName(export uuid.UUID, format string) string {
	return fmt.Sprintf(exportFiles[format].download, export)
}

// ExportFileShape reports whether name, what follows ExportPrefix in a key,
// is what an export writes there: <export>.<format>, the export spelt as the
// server spells an id and the format one of its two. Anything else under
// the prefix is someone else's, and the sweep leaves it alone.
func ExportFileShape(name string) bool {
	export, format, ok := strings.Cut(name, ".")
	id, err := uuid.Parse(export)
	_, known := exportFiles[format]
	return ok && known && err == nil && id.String() == export
}

var (
	errExportPeopleOnly = apperr.Forbid("conversations are exported by a person, who answers for taking them away; an agent exports none").
				With("reason", "people_only")
	// errNoExport is what an export that does not exist, and one that is
	// not the caller's, both answer: the same, so that nobody learns which.
	errNoExport      = apperr.Missing("no such export of yours")
	errExportExpired = apperr.Missing("the export's files have been removed, as every export's are once it is a while old; export again").
				With("reason", "export_expired")
)

// ---------------------------------------------------------------------------
// conversation.export
// ---------------------------------------------------------------------------

// ConversationExportIn chooses the conversations an export holds. With no
// course and no department it is the whole site's, which only a platform
// administrator exports.
type ConversationExportIn struct {
	CourseID           *uuid.UUID `json:"course_id,omitempty" jsonschema:"only this course's conversations; give this or within_dept_id, or neither for the whole site's"`
	WithinDeptID       *uuid.UUID `json:"within_dept_id,omitempty" jsonschema:"only the conversations of the courses in this department and every department beneath it"`
	ParticipantActorID *uuid.UUID `json:"participant_actor_id,omitempty" jsonschema:"only the conversations this person or agent took part in, as the one who asked or as the agent asked"`
	From               *time.Time `json:"from,omitempty" jsonschema:"only what was written at or after this time: the conversations opened then, or with something written then, and in them what was written then"`
	Before             *time.Time `json:"before,omitempty" jsonschema:"only what was written before this time, as from"`
}

// check holds the filters to their shape: one place, and a span of time
// that is one.
func (in ConversationExportIn) check() error {
	if in.CourseID != nil && in.WithinDeptID != nil {
		return apperr.Invalid("give course_id or within_dept_id, not both")
	}
	if in.From != nil && in.Before != nil && !in.From.Before(*in.Before) {
		return apperr.Invalid("from must come before before")
	}
	return nil
}

// ExportFile is one file of an export, as its record keeps it.
type ExportFile struct {
	Format      string `json:"format" jsonschema:"jsonl: the conversations, one to a line, each with its messages; csv: the messages, one to a row"`
	Filename    string `json:"filename" jsonschema:"the name it downloads under"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum" jsonschema:"sha256:<hex> of the file's bytes"`
}

// ExportDownload is a URL for one file of an export. It is in no record.
type ExportDownload struct {
	Format      string    `json:"format"`
	DownloadURL string    `json:"download_url" jsonschema:"a short-lived URL that serves the file as a download, under its name: GET it as it is, with no Authorization header"`
	ExpiresAt   time.Time `json:"expires_at" jsonschema:"when download_url stops working, about 15 minutes from now; conversation.export_file gives another"`
}

type ConversationExportOut struct {
	ExportID uuid.UUID `json:"export_id" jsonschema:"the export's id, which is its action's: conversation.export_file takes it"`
	AsOf     time.Time `json:"as_of" jsonschema:"when it was made: nothing written after it is in it"`
	// The counts, which the action records.
	Conversations int `json:"conversations"`
	Messages      int `json:"messages" jsonschema:"the messages it holds, the retracted among them"`
	Retracted     int `json:"retracted" jsonschema:"of them, how many are retracted: held with their text, and marked"`
	Attachments   int `json:"attachments" jsonschema:"the files the messages carry, described; their bytes are not in it"`
	Proposals     int `json:"proposals" jsonschema:"the answers and questions proposed in its conversations and never posted: waiting for a decision, rejected or cancelled"`
	// TextBytes is what its limit (max_bytes) measures.
	TextBytes int64        `json:"text_bytes" jsonschema:"the bytes of what its messages and proposals say, which max_bytes bounds"`
	Files     []ExportFile `json:"files"`
	ExpiresAt time.Time    `json:"expires_at" jsonschema:"when its files are removed; conversation.export_file gives a URL for one until then"`
	// Downloads is the one thing the record leaves out: a URL to each file,
	// which whoever holds it may use until it expires.
	Downloads []ExportDownload `json:"downloads,omitempty" jsonschema:"a URL for each file, given once, to the caller alone; a call replayed with its idempotency key gives none, and conversation.export_file gives one again"`
}

// errExportTooLarge refuses an export past its limits, saying how much it
// would hold, so that its caller narrows it.
func errExportTooLarge(l ExportLimits, conversations, messages, bytes int64) *apperr.Error {
	return apperr.Precondition("this export would hold %d messages and %d bytes of text in %d conversations, past its limits of %d "+
		"messages and %d bytes; narrow it: one course, one department, one participant, or a shorter span of time",
		messages, bytes, conversations, l.MaxMessages, l.MaxBytes).
		With("reason", "export_too_large").With("conversations", conversations).With("messages", messages).
		With("text_bytes", bytes).With("max_messages", l.MaxMessages).With("max_bytes", l.MaxBytes)
}

func conversationExport(d Deps) tool.Tool {
	return tool.Define(tool.Spec[ConversationExportIn, ConversationExportOut]{
		Name: ToolConversationExport,
		Description: "Export conversations for audit: for root and platform administrators, anywhere; for a department's " +
			"administrators, the courses of the departments they administer and beneath them, and only by naming a course " +
			"(course_id) or a department (within_dept_id) of theirs. Nobody else, and never an agent (people_only). " +
			"Optionally only one participant's (participant_actor_id) and only what was written in a span of time (from, " +
			"before). It holds every message of the conversations it chooses, retracted ones with their text, marked " +
			"retracted; what files each carries, never their bytes; and the answers and questions proposed and never posted. " +
			"Two files: the conversations as JSON Lines, one to a line with their messages, and the messages as CSV, one to a " +
			"row, in UTF-8 with a byte order mark. Each is given with a URL that downloads it for 15 minutes; " +
			"conversation.export_file gives another until the files are removed, expires_at. The files hold personal data. " +
			"Past its limits (max messages, max bytes of text) it is refused, export_too_large, saying how much it would " +
			"hold: narrow it. Every export is recorded, filters and counts, as an action.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/conversation-exports"},
		// The URLs are the means to read what the export holds, and are
		// given to its caller alone, never kept.
		SecretOut: []string{"downloads"},
		// What the export is about is what a department administrator must
		// cover: the course's department, or the department named; with
		// neither, the whole site, a platform administrator's alone.
		Resolve: func(ctx context.Context, q dbq.Querier, in ConversationExportIn) (tool.Target, error) {
			if err := in.check(); err != nil {
				return tool.Target{}, err
			}
			target := tool.Target{Type: "conversation_export"}
			switch {
			case in.CourseID != nil:
				c, err := q.GetCourse(ctx, *in.CourseID)
				if errors.Is(err, pgx.ErrNoRows) {
					return tool.Target{}, apperr.Missing("no such course")
				} else if err != nil {
					return tool.Target{}, err
				}
				target.DeptID = &c.DeptID
			case in.WithinDeptID != nil:
				if _, err := findDepartment(ctx, q, *in.WithinDeptID, "no such department"); err != nil {
					return tool.Target{}, err
				}
				target.DeptID = in.WithinDeptID
			}
			if in.ParticipantActorID != nil {
				if _, err := q.GetActor(ctx, *in.ParticipantActorID); errors.Is(err, pgx.ErrNoRows) {
					return tool.Target{}, apperr.Missing("no such participant")
				} else if err != nil {
					return tool.Target{}, err
				}
			}
			return target, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ConversationExportIn) (ConversationExportOut, error) {
			// Taking personal data away is a person's to answer for. This
			// reads kind to refuse, as a password reset does; nothing that
			// grants reads it.
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return ConversationExportOut{}, err
			}
			if isAgent(me.Kind) {
				return ConversationExportOut{}, errExportPeopleOnly
			}
			if d.Blob == nil {
				return ConversationExportOut{}, errNoFileStorage
			}
			f := exportFilter{in: in, asOf: ec.Now}
			size, err := ec.Q.ExportSize(ctx, f.size())
			if err != nil {
				return ConversationExportOut{}, err
			}
			l := d.Exports
			if size.Messages+size.Proposals > int64(l.MaxMessages) || size.MessageBytes+size.ProposalBytes > l.MaxBytes {
				return ConversationExportOut{}, errExportTooLarge(l, size.Conversations, size.Messages+size.Proposals,
					size.MessageBytes+size.ProposalBytes)
			}
			// Written a page at a time, it may take a while: the answer
			// waits for it on a connection held open meanwhile.
			wake.Hold(ctx, time.Now().Add(exportHold))
			out, err := writeExport(ctx, d, ec.Q, ec.ActionID, f)
			if err != nil {
				return ConversationExportOut{}, err
			}
			// When its files go is worked out from when it was carried out,
			// as the record keeps that: to the microsecond.
			out.ExpiresAt = ec.Now.Truncate(time.Microsecond).Add(l.TTL)
			for _, file := range out.Files {
				url, err := d.Blob.PresignDownload(ctx, ExportKey(out.ExportID, file.Format), file.Filename, downloadTTL)
				if err != nil {
					return ConversationExportOut{}, err
				}
				out.Downloads = append(out.Downloads, ExportDownload{Format: file.Format, DownloadURL: url, ExpiresAt: ec.Now.Add(downloadTTL)})
			}
			ec.Emit(events.Event{Type: EventConversationExported, SubjectType: "conversation_export", SubjectID: &out.ExportID,
				Payload: map[string]any{"conversations": out.Conversations, "messages": out.Messages, "proposals": out.Proposals}})
			return out, nil
		},
	})
}

// exportFilter is an export's filters as its queries take them, and the
// moment it is made, as of which it holds what was written.
type exportFilter struct {
	in   ConversationExportIn
	asOf time.Time
}

func (f exportFilter) size() dbq.ExportSizeParams {
	return dbq.ExportSizeParams{AsOf: f.asOf, CourseID: f.in.CourseID, WithinDeptID: f.in.WithinDeptID,
		ParticipantActorID: f.in.ParticipantActorID, FromAt: f.in.From, BeforeAt: f.in.Before}
}

func (f exportFilter) conversations(after uuid.UUID) dbq.ListExportConversationsParams {
	return dbq.ListExportConversationsParams{After: after, AsOf: f.asOf, CourseID: f.in.CourseID, WithinDeptID: f.in.WithinDeptID,
		ParticipantActorID: f.in.ParticipantActorID, FromAt: f.in.From, BeforeAt: f.in.Before, MaxRows: exportConversationPage}
}

// ---------------------------------------------------------------------------
// conversation.export_file
// ---------------------------------------------------------------------------

type ConversationExportFileIn struct {
	ExportID uuid.UUID `json:"export_id" jsonschema:"an export of yours, from conversation.export"`
	Format   string    `json:"format" jsonschema:"jsonl, the conversations one to a line, or csv, the messages one to a row"`
}

type ConversationExportFileOut struct {
	ExportID uuid.UUID `json:"export_id"`
	ExportFile
	DownloadURL string    `json:"download_url" jsonschema:"a short-lived URL that serves the file as a download, under its name: GET it as it is, with no Authorization header"`
	ExpiresAt   time.Time `json:"expires_at" jsonschema:"when download_url stops working, about 15 minutes from now; ask again for another"`
	// ExportExpiresAt is when the file itself goes.
	ExportExpiresAt time.Time `json:"export_expires_at" jsonschema:"when the export's files are removed"`
}

// conversationExportFile gives a file of an export again, to whoever made
// it, while they may still export what it is about and until its files are
// removed: a URL lasts fifteen minutes, and a call replayed gives none. It
// is no new export and records nothing: the export is on record already,
// and its maker had its files.
func conversationExportFile(d Deps) tool.Tool {
	return tool.Define(tool.Spec[ConversationExportFileIn, ConversationExportFileOut]{
		Name: "conversation.export_file",
		Description: "A file of an export you made (conversation.export), again: its name, type, size and checksum, and a " +
			"short-lived URL that downloads it. format is jsonl, the conversations, or csv, the messages. Only for whoever " +
			"made the export, while they administer what it is about, and until its files are removed (export_expired).",
		Kind: tool.Read, Gate: administrators,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/conversation-exports/{export_id}/{format}"},
		// Any administrator gets as far as the export, which is theirs or
		// is nothing to them; theirs, it is held to their reach below.
		Resolve: func(_ context.Context, _ dbq.Querier, in ConversationExportFileIn) (tool.Target, error) {
			if _, ok := exportFiles[in.Format]; !ok {
				return tool.Target{}, apperr.Invalid("format is jsonl or csv")
			}
			return tool.Target{Type: "conversation_export", ID: &in.ExportID, AnyDept: true}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ConversationExportFileIn) (ConversationExportFileOut, error) {
			x, err := rc.Q.GetExport(ctx, in.ExportID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (x.ActorID != rc.Actor.ID || x.Status != string(domain.StatusExecuted) ||
				x.ExecutedAt == nil)) {
				return ConversationExportFileOut{}, errNoExport
			}
			if err != nil {
				return ConversationExportFileOut{}, err
			}
			var filters ConversationExportIn
			var made ConversationExportOut
			if err := json.Unmarshal(x.Payload, &filters); err != nil {
				return ConversationExportFileOut{}, fmt.Errorf("export %s: its filters: %w", x.ID, err)
			}
			if err := json.Unmarshal(x.Result, &made); err != nil {
				return ConversationExportFileOut{}, fmt.Errorf("export %s: its record: %w", x.ID, err)
			}
			if err := stillReaches(ctx, rc, filters); err != nil {
				return ConversationExportFileOut{}, err
			}
			expires := x.ExecutedAt.Add(d.Exports.TTL)
			if !rc.Now.Before(expires) {
				return ConversationExportFileOut{}, errExportExpired
			}
			var file *ExportFile
			for i := range made.Files {
				if made.Files[i].Format == in.Format {
					file = &made.Files[i]
				}
			}
			if file == nil {
				return ConversationExportFileOut{}, errNoExport
			}
			if d.Blob == nil {
				return ConversationExportFileOut{}, errNoFileStorage
			}
			key := ExportKey(x.ID, in.Format)
			if _, err := d.Blob.Stat(ctx, key); errors.Is(err, blob.ErrNotFound) {
				return ConversationExportFileOut{}, errExportExpired
			} else if err != nil {
				return ConversationExportFileOut{}, err
			}
			url, err := d.Blob.PresignDownload(ctx, key, file.Filename, downloadTTL)
			if err != nil {
				return ConversationExportFileOut{}, err
			}
			return ConversationExportFileOut{ExportID: x.ID, ExportFile: *file, DownloadURL: url,
				ExpiresAt: rc.Now.Add(downloadTTL), ExportExpiresAt: expires.UTC()}, nil
		},
	})
}

// stillReaches holds a department administrator to what an export of
// theirs is about, as the export itself was held: the course's department
// as it is now, or the department it named, which an appointment of theirs
// must cover. One that named neither was a platform administrator's, and
// is theirs alone. A platform administrator reaches everything.
func stillReaches(ctx context.Context, rc *tool.ReadCtx, filters ConversationExportIn) error {
	if rc.Admin.Platform {
		return nil
	}
	var dept *uuid.UUID
	switch {
	case filters.CourseID != nil:
		c, err := rc.Q.GetCourse(ctx, *filters.CourseID)
		if err != nil {
			return err
		}
		dept = &c.DeptID
	case filters.WithinDeptID != nil:
		dept = filters.WithinDeptID
	default:
		return apperr.Forbid("not permitted").With("reason", "platform_role_required")
	}
	_, ok, err := rc.Admin.Covers(ctx, rc.Q, *dept)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.Forbid("what the export is about is no longer in the departments you administer").
			With("reason", "department_out_of_scope")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Writing an export
// ---------------------------------------------------------------------------

// exportPart is one file of an export while it is written: streamed to the
// store as it is written, through a pipe, and hashed on the way.
type exportPart struct {
	format string
	pw     *io.PipeWriter
	w      *bufio.Writer
	hash   hash.Hash
	n      int64
	done   chan exportPut
}

type exportPut struct {
	info blob.Info
	err  error
}

func openExportPart(ctx context.Context, d Deps, export uuid.UUID, format string) *exportPart {
	pr, pw := io.Pipe()
	p := &exportPart{format: format, pw: pw, hash: sha256.New(), done: make(chan exportPut, 1)}
	p.w = bufio.NewWriterSize(p, 64<<10)
	go func() {
		info, err := d.Blob.Put(ctx, ExportKey(export, format), exportFiles[format].contentType, pr, d.Exports.fileBytes())
		// A store that stopped reading, done or failed, stops the writer.
		_ = pr.CloseWithError(err)
		p.done <- exportPut{info, err}
	}()
	return p
}

// Write takes what the buffer hands on, to the pipe and the hash.
func (p *exportPart) Write(b []byte) (int, error) {
	n, err := p.pw.Write(b)
	p.hash.Write(b[:n])
	p.n += int64(n)
	return n, err
}

// finish ends the file, and says what the store kept.
func (p *exportPart) finish() (ExportFile, error) {
	err := p.w.Flush()
	if err != nil {
		_ = p.pw.CloseWithError(err)
	} else {
		_ = p.pw.Close()
	}
	put := <-p.done
	if err == nil {
		err = put.err
	}
	if errors.Is(err, blob.ErrTooLarge) {
		return ExportFile{}, apperr.Precondition("the export's %s file grew past what one may hold; narrow it", p.format).
			With("reason", "export_too_large")
	}
	return ExportFile{Format: p.format, ContentType: exportFiles[p.format].contentType, ByteSize: p.n,
		Checksum: "sha256:" + hex.EncodeToString(p.hash.Sum(nil))}, err
}

// abandon ends the file unwritten: the store keeps nothing of it.
func (p *exportPart) abandon(why error) {
	_ = p.pw.CloseWithError(why)
	<-p.done
}

// exportCSVHeader is the CSV file's first row: its columns, which the
// contract names (docs/schema.md §2.8, Exporting conversations for audit).
var exportCSVHeader = []string{
	"conversation_id", "course_id", "course_code", "course_section", "course_title", "conversation_title",
	"status", "message_id", "seq", "action_id", "created_at",
	"author_member_id", "author_actor_id", "author_name", "author_kind", "author_role",
	"in_reply_to_message_id", "body", "retracted_at", "retracted_by_name", "reason", "decided_at", "decided_by_name",
	"attachment_ids", "attachment_filenames",
}

// The status of a row of the CSV file: a message posted, or posted and
// retracted; an answer or question proposed and never posted.
const (
	exportPosted    = "posted"
	exportRetracted = "retracted"
)

// utf8BOM begins the CSV file, so that a spreadsheet takes it as UTF-8 and
// shows Chinese as it is written.
const utf8BOM = "\ufeff"

// writeExport writes an export's two files as it reads what they hold, a
// page of conversations at a time, and says what they hold. Past the limits
// as it goes — something written since they were measured — it stops, and
// the store keeps nothing.
func writeExport(ctx context.Context, d Deps, q *dbq.Queries, export uuid.UUID, f exportFilter) (out ConversationExportOut, err error) {
	jsonl := openExportPart(ctx, d, export, ExportJSONL)
	csvPart := openExportPart(ctx, d, export, ExportCSV)
	finished := false
	defer func() {
		if !finished {
			if err == nil {
				err = errors.New("export: not finished")
			}
			jsonl.abandon(err)
			csvPart.abandon(err)
		}
	}()
	w := &exportWriter{d: d, q: q, f: f, jsonl: jsonl.w, csv: csv.NewWriter(csvPart.w), out: &out}
	w.csv.UseCRLF = true
	if _, err := csvPart.w.WriteString(utf8BOM); err != nil {
		return out, err
	}
	if err := w.csv.Write(exportCSVHeader); err != nil {
		return out, err
	}
	for after := uuid.Nil; ; {
		page, err := q.ListExportConversations(ctx, f.conversations(after))
		if err != nil {
			return out, err
		}
		if err := w.page(ctx, page); err != nil {
			return out, err
		}
		if len(page) < exportConversationPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if w.csv.Flush(); w.csv.Error() != nil {
		return out, w.csv.Error()
	}
	finished = true
	files := make([]ExportFile, 0, 2)
	var failed error
	for _, p := range []*exportPart{jsonl, csvPart} {
		file, err := p.finish()
		if err != nil && failed == nil {
			failed = err
		}
		file.Filename = exportDownloadName(export, p.format)
		files = append(files, file)
	}
	if failed != nil {
		// One file written and the other not is no export: the one is
		// removed, best effort, and the sweep removes it otherwise.
		for _, format := range exportFormats {
			_ = d.Blob.Delete(context.WithoutCancel(ctx), ExportKey(export, format))
		}
		return out, failed
	}
	out.ExportID, out.AsOf, out.Files = export, f.asOf.Truncate(time.Microsecond).UTC(), files
	return out, nil
}

// exportWriter writes what an export holds into its two files.
type exportWriter struct {
	d     Deps
	q     *dbq.Queries
	f     exportFilter
	jsonl *bufio.Writer
	csv   *csv.Writer
	out   *ConversationExportOut
}

// held counts a message or a proposal into what the export holds, and
// stops it past its limits.
func (w *exportWriter) held(text string) error {
	w.out.TextBytes += int64(len(text))
	if w.out.Messages+w.out.Proposals > w.d.Exports.MaxMessages || w.out.TextBytes > w.d.Exports.MaxBytes {
		return errExportTooLarge(w.d.Exports, int64(w.out.Conversations), int64(w.out.Messages+w.out.Proposals), w.out.TextBytes)
	}
	return nil
}

// page writes a page of conversations: each a line of the JSON Lines file,
// its messages and proposals in it, and each of those a row of the CSV
// file. The messages are read a page at a time across the conversations,
// in the order the conversations came.
func (w *exportWriter) page(ctx context.Context, convs []dbq.ListExportConversationsRow) error {
	if len(convs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(convs))
	for i, c := range convs {
		ids[i] = c.ID
	}
	proposals, err := w.q.ListExportProposals(ctx, dbq.ListExportProposalsParams{ConversationIds: ids,
		FromAt: w.f.in.From, BeforeAt: w.f.in.Before, AsOf: w.f.asOf})
	if err != nil {
		return err
	}
	proposed := map[uuid.UUID][]dbq.ListExportProposalsRow{}
	for _, p := range proposals {
		if p.ConversationID != nil {
			proposed[*p.ConversationID] = append(proposed[*p.ConversationID], p)
		}
	}
	msgs := &exportMessages{w: w, ids: ids}
	for _, c := range convs {
		head := exportConversation(c)
		line, err := exportJSON(head)
		if err != nil {
			return err
		}
		// The line is the conversation's object with its messages and
		// proposals written into it as they are read: no conversation is
		// held whole, however long.
		if _, err := w.jsonl.Write(line[:len(line)-1]); err != nil {
			return err
		}
		if _, err := w.jsonl.WriteString(`,"messages":[`); err != nil {
			return err
		}
		for i := 0; ; i++ {
			m, ok, err := msgs.next(ctx, c.ID)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if err := w.message(c, m, i); err != nil {
				return err
			}
		}
		if _, err := w.jsonl.WriteString(`],"proposals":[`); err != nil {
			return err
		}
		for i, p := range proposed[c.ID] {
			if err := w.proposal(c, p, i); err != nil {
				return err
			}
		}
		if _, err := w.jsonl.WriteString("]}\n"); err != nil {
			return err
		}
		w.out.Conversations++
	}
	return nil
}

// exportMessages reads the messages of a page of conversations a page at a
// time, and hands them out conversation by conversation.
type exportMessages struct {
	w         *exportWriter
	ids       []uuid.UUID
	rows      []dbq.ListExportMessagesRow
	files     map[uuid.UUID][]AttachmentView
	sources   map[uuid.UUID][]exportSource
	at        int
	afterConv uuid.UUID
	afterSeq  int32
	done      bool
}

// message is a message with the files it carries, and an answer with
// what it relied on.
type exportedMessage struct {
	dbq.ListExportMessagesRow
	files   []AttachmentView
	sources []exportSource
}

// next is the next message of conv, if it has another.
func (e *exportMessages) next(ctx context.Context, conv uuid.UUID) (exportedMessage, bool, error) {
	if e.at == len(e.rows) && !e.done {
		rows, err := e.w.q.ListExportMessages(ctx, dbq.ListExportMessagesParams{AsOf: e.w.f.asOf, ConversationIds: e.ids,
			AfterConversationID: e.afterConv, AfterSeq: e.afterSeq, FromAt: e.w.f.in.From, BeforeAt: e.w.f.in.Before,
			MaxRows: exportMessagePage})
		if err != nil {
			return exportedMessage{}, false, err
		}
		shown := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			shown[i] = r.ID
		}
		// A retracted message's files are listed as its text is: for the
		// record, which is what an export is.
		if e.files, err = attachmentsOf(ctx, e.w.q, shown); err != nil {
			return exportedMessage{}, false, err
		}
		// An answer's sources, whoever may read them now: for the
		// record too, with their documents' titles as they are now.
		if e.sources, err = exportSourcesOf(ctx, e.w.q, shown); err != nil {
			return exportedMessage{}, false, err
		}
		e.rows, e.at, e.done = rows, 0, len(rows) < exportMessagePage
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			e.afterConv, e.afterSeq = last.ConversationID, last.Seq
		}
	}
	if e.at == len(e.rows) || e.rows[e.at].ConversationID != conv {
		return exportedMessage{}, false, nil
	}
	r := e.rows[e.at]
	e.at++
	return exportedMessage{ListExportMessagesRow: r, files: e.files[r.ID], sources: e.sources[r.ID]}, true, nil
}

// exportSourcesOf are the sources of the given messages, by message, as an
// export holds them: every one, named by its ids, with its document's
// title and kind as they are now and whether its version was purged; a
// purged version's file is gone, and named no more.
func exportSourcesOf(ctx context.Context, q dbq.Querier, messages []uuid.UUID) (map[uuid.UUID][]exportSource, error) {
	out := map[uuid.UUID][]exportSource{}
	if len(messages) == 0 {
		return out, nil
	}
	rows, err := q.ListMessageSources(ctx, messages)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		title, kind, seq := r.DocumentTitle, r.DocumentKind, r.VersionSeq
		out[r.MessageID] = append(out[r.MessageID], exportSource{DocumentID: r.DocumentID, VersionID: r.VersionID,
			FileID: r.FileID, Page: r.Page, Slide: r.Slide, Part: r.Part, Title: &title, Kind: &kind, VersionSeq: &seq,
			Filename: r.Filename, Purged: r.VersionPurgedAt != nil || r.DocumentPurgedAt != nil})
	}
	return out, nil
}

// exportProposedSources are the sources a proposed answer named, as it
// named them: ids alone.
func exportProposedSources(payload []byte) ([]exportSource, error) {
	var named []SourceIn
	if err := json.Unmarshal(payload, &named); err != nil {
		return nil, err
	}
	out := make([]exportSource, len(named))
	for i, s := range named {
		out[i] = exportSource{DocumentID: s.DocumentID, VersionID: s.VersionID, FileID: s.FileID, Page: s.Page, Slide: s.Slide,
			Part: s.Part}
	}
	return out, nil
}

// The JSON Lines file's records. Every field is always there, null where
// there is nothing, so that a program reading it finds one shape.

type exportCourse struct {
	ID      uuid.UUID `json:"id"`
	Code    string    `json:"code"`
	Section string    `json:"section"`
	Title   string    `json:"title"`
	DeptID  uuid.UUID `json:"dept_id"`
	TermID  uuid.UUID `json:"term_id"`
}

// exportParty is a seat and whose it is: a person's or an agent's (kind),
// and the roster role it was seated with.
type exportParty struct {
	MemberID uuid.UUID `json:"member_id"`
	ActorID  uuid.UUID `json:"actor_id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Role     string    `json:"role"`
}

type exportOwner struct {
	ActorID uuid.UUID `json:"actor_id"`
	Name    string    `json:"name"`
}

type exportRespondent struct {
	exportParty
	PrincipalMemberID *uuid.UUID   `json:"principal_member_id"`
	AnswersCourse     bool         `json:"answers_course"`
	Owner             *exportOwner `json:"owner"`
}

type exportConversationHead struct {
	ID            uuid.UUID        `json:"id"`
	Course        exportCourse     `json:"course"`
	Title         *string          `json:"title"`
	Status        string           `json:"status"`
	ClosedReason  *string          `json:"closed_reason"`
	CreatedAt     time.Time        `json:"created_at"`
	ClosedAt      *time.Time       `json:"closed_at"`
	LastMessageAt *time.Time       `json:"last_message_at"`
	Opener        exportParty      `json:"opener"`
	Respondent    exportRespondent `json:"respondent"`
}

type exportRetraction struct {
	At       time.Time    `json:"at"`
	By       *exportParty `json:"by"`
	Reason   *string      `json:"reason"`
	ActionID *uuid.UUID   `json:"action_id"`
}

type exportAttachment struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    *string   `json:"checksum"`
	CreatedAt   time.Time `json:"created_at"`
}

// exportSource is a source of an answer, as an export holds it. A posted
// answer's says what its document is called now, and whether its version
// was purged; a proposed answer's is as it was named, its title, kind,
// seq and file name null.
type exportSource struct {
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  uuid.UUID  `json:"version_id"`
	FileID     *uuid.UUID `json:"file_id"`
	Page       *int32     `json:"page"`
	Slide      *int32     `json:"slide"`
	Part       *int32     `json:"part"`
	Title      *string    `json:"title"`
	Kind       *string    `json:"kind"`
	VersionSeq *int32     `json:"version_seq"`
	Filename   *string    `json:"filename"`
	Purged     bool       `json:"purged"`
}

type exportMessage struct {
	ID                 uuid.UUID          `json:"id"`
	Seq                int32              `json:"seq"`
	Author             exportParty        `json:"author"`
	CreatedAt          time.Time          `json:"created_at"`
	InReplyToMessageID *uuid.UUID         `json:"in_reply_to_message_id"`
	Body               string             `json:"body"`
	ActionID           uuid.UUID          `json:"action_id"`
	Retracted          *exportRetraction  `json:"retracted"`
	Attachments        []exportAttachment `json:"attachments"`
	Sources            []exportSource     `json:"sources"`
}

type exportProposal struct {
	ActionID            uuid.UUID      `json:"action_id"`
	Type                string         `json:"type"`
	Status              string         `json:"status"`
	ProposedBy          *exportParty   `json:"proposed_by"`
	CreatedAt           time.Time      `json:"created_at"`
	InReplyToMessageID  *uuid.UUID     `json:"in_reply_to_message_id"`
	Body                string         `json:"body"`
	AttachmentFilenames []string       `json:"attachment_filenames"`
	Sources             []exportSource `json:"sources"`
	DecidedAt           *time.Time     `json:"decided_at"`
	DecidedBy           *exportParty   `json:"decided_by"`
	Reason              *string        `json:"reason"`
}

func exportConversation(c dbq.ListExportConversationsRow) exportConversationHead {
	v := exportConversationHead{ID: c.ID, Title: c.Title, Status: c.Status, ClosedReason: c.ClosedReason,
		CreatedAt: c.CreatedAt.UTC(), ClosedAt: utc(c.ClosedAt), LastMessageAt: utc(c.LastMessageAt),
		Course: exportCourse{ID: c.CourseID, Code: c.CourseCode, Section: c.CourseSection, Title: c.CourseTitle,
			DeptID: c.CourseDeptID, TermID: c.CourseTermID},
		Opener: exportParty{MemberID: c.OpenerMemberID, ActorID: c.OpenerActorID, Name: c.OpenerName, Kind: c.OpenerKind,
			Role: c.OpenerRole},
		Respondent: exportRespondent{exportParty: exportParty{MemberID: c.RespondentMemberID, ActorID: c.RespondentActorID,
			Name: c.RespondentName, Kind: c.RespondentKind, Role: c.RespondentRole},
			PrincipalMemberID: c.RespondentPrincipalMemberID, AnswersCourse: c.RespondentAnswersCourse},
	}
	if c.RespondentOwnerActorID != nil {
		v.Respondent.Owner = &exportOwner{ActorID: *c.RespondentOwnerActorID, Name: deref(c.RespondentOwnerName)}
	}
	return v
}

// party is a seat that may be missing, as the joins that find it say.
func party(member, actor *uuid.UUID, name, kind, role *string) *exportParty {
	if member == nil || actor == nil {
		return nil
	}
	return &exportParty{MemberID: *member, ActorID: *actor, Name: deref(name), Kind: deref(kind), Role: deref(role)}
}

// utc is a moment that may be missing, in UTC, as an export writes every
// moment.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// message writes one message: into its conversation's line, the i-th, and
// as a row of the CSV file.
func (w *exportWriter) message(c dbq.ListExportConversationsRow, m exportedMessage, i int) error {
	w.out.Messages++
	w.out.Attachments += len(m.files)
	if err := w.held(m.Body); err != nil {
		return err
	}
	v := exportMessage{ID: m.ID, Seq: m.Seq, CreatedAt: m.CreatedAt.UTC(), InReplyToMessageID: m.InReplyToMessageID, Body: m.Body,
		ActionID: m.CreatedByActionID, Attachments: make([]exportAttachment, 0, len(m.files)), Sources: m.sources,
		Author: exportParty{MemberID: m.AuthorMemberID, ActorID: m.AuthorActorID, Name: m.AuthorName, Kind: m.AuthorKind,
			Role: m.AuthorRole}}
	if v.Sources == nil {
		v.Sources = []exportSource{}
	}
	ids, names := make([]string, len(m.files)), make([]string, len(m.files))
	for j, a := range m.files {
		v.Attachments = append(v.Attachments, exportAttachment{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType,
			ByteSize: a.ByteSize, Checksum: a.Checksum, CreatedAt: a.CreatedAt.UTC()})
		ids[j], names[j] = a.ID.String(), a.Filename
	}
	status, reason, retractedAt, retractedBy := exportPosted, "", "", ""
	if m.RetractedAt != nil {
		w.out.Retracted++
		v.Retracted = &exportRetraction{At: m.RetractedAt.UTC(), Reason: m.RetractionReason, ActionID: m.RetractionActionID,
			By: party(m.RetractedByMemberID, m.RetractedByActorID, m.RetractedByName, m.RetractedByKind, m.RetractedByRole)}
		status, reason, retractedAt, retractedBy = exportRetracted, deref(m.RetractionReason), stamp(m.RetractedAt), deref(m.RetractedByName)
	}
	if err := w.element(v, i); err != nil {
		return err
	}
	return w.row(c, []string{status, m.ID.String(), strconv.Itoa(int(m.Seq)), m.CreatedByActionID.String(), stamp(&m.CreatedAt),
		m.AuthorMemberID.String(), m.AuthorActorID.String(), m.AuthorName, m.AuthorKind, m.AuthorRole,
		idText(m.InReplyToMessageID), m.Body, retractedAt, retractedBy, reason, "", "",
		strings.Join(ids, "\n"), strings.Join(names, "\n")})
}

// proposal writes one answer or question proposed and never posted: into
// its conversation's line, the i-th of its proposals, and as a row of the
// CSV file, whose status says what became of it.
func (w *exportWriter) proposal(c dbq.ListExportConversationsRow, p dbq.ListExportProposalsRow, i int) error {
	w.out.Proposals++
	if err := w.held(p.Body); err != nil {
		return err
	}
	v := exportProposal{ActionID: p.ID, Type: p.ActionType, Status: p.Status, CreatedAt: p.CreatedAt.UTC(), Body: p.Body,
		AttachmentFilenames: p.AttachmentFilenames, DecidedAt: utc(p.DecidedAt),
		ProposedBy: party(p.ProposerMemberID, p.ProposerActorID, p.ProposerName, p.ProposerKind, p.ProposerRole),
		DecidedBy:  party(p.DecidedByMemberID, p.DecidedByActorID, p.DecidedByName, p.DecidedByKind, p.DecidedByRole)}
	if v.AttachmentFilenames == nil {
		v.AttachmentFilenames = []string{}
	}
	sources, err := exportProposedSources(p.Sources)
	if err != nil {
		return err
	}
	v.Sources = sources
	if id, err := uuid.Parse(p.InReplyToMessageID); err == nil {
		v.InReplyToMessageID = &id
	}
	if p.Reason != "" {
		v.Reason = &p.Reason
	}
	if err := w.element(v, i); err != nil {
		return err
	}
	by := v.ProposedBy
	if by == nil {
		by = &exportParty{}
	}
	return w.row(c, []string{p.Status, "", "", p.ID.String(), stamp(&p.CreatedAt),
		idText(p.ProposerMemberID), idText(p.ProposerActorID), by.Name, by.Kind, by.Role,
		idText(v.InReplyToMessageID), p.Body, "", "", p.Reason, stamp(p.DecidedAt), deref(p.DecidedByName),
		"", strings.Join(p.AttachmentFilenames, "\n")})
}

// element writes v into the JSON array being written, after a comma unless
// it is the first.
func (w *exportWriter) element(v any, i int) error {
	b, err := exportJSON(v)
	if err != nil {
		return err
	}
	if i > 0 {
		if err := w.jsonl.WriteByte(','); err != nil {
			return err
		}
	}
	_, err = w.jsonl.Write(b)
	return err
}

// row writes a row of the CSV file: the conversation's columns, then the
// record's. A cell that would begin as a spreadsheet's formula does begins
// with an apostrophe, so that it is shown as the text it is and never run;
// the JSON Lines file has every text exactly.
func (w *exportWriter) row(c dbq.ListExportConversationsRow, record []string) error {
	cells := append([]string{c.ID.String(), c.CourseID.String(), c.CourseCode, c.CourseSection, c.CourseTitle, deref(c.Title)}, record...)
	for i, s := range cells {
		cells[i] = inert(s)
	}
	return w.csv.Write(cells)
}

// inert is a cell's text as a spreadsheet may show it without running it:
// one beginning with =, +, -, @, a tab or a carriage return, which a
// spreadsheet takes for a formula, is given an apostrophe before it.
func inert(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// exportJSON is v as JSON on one line, its text as it is: nothing escaped
// that JSON does not need escaped.
func exportJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// stamp is a moment as the CSV file writes it, as the JSON Lines file does:
// RFC 3339, in UTC. Nothing, for none.
func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
