// Package jobs runs the background sweeps: proposals that have waited too
// long, memberships past their expiry, delegates' seats whose principal has
// gone, assignments whose due date has passed, sessions long dead, answers'
// drafts nobody writes any more, files queued to leave the store with what
// was deleted, uploaded files that nothing came to point at, and exports of
// conversations kept as long as they are kept.
//
// Nothing here is what makes the system correct. authorize() ignores an
// expired member from the instant of expiry, and approving a stale proposal
// cancels it on the spot. The sweeps make those facts visible — a row in the
// action log, an event in the feed, a queue without dead entries — sooner
// than the next person to trip over them would.
//
// There is no scheduler and no queue, only Postgres:
//
//   - one instance sweeps at a time, by a session advisory lock taken with
//     pg_try_advisory_lock on a connection held for the sweep. An instance
//     that does not get it skips the tick. If the holder dies, its connection
//     closes and the lock goes with it;
//   - every write is an action by the system actor through
//     pipeline.InvokeSystem, under a deterministic idempotency key naming the
//     thing swept — so even without the lock, sweeping twice acts once;
//   - each internal tool re-checks its condition under a row lock, so a sweep
//     that loses a race with a person does nothing.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// lockKey is the advisory lock all instances contend for. Arbitrary, fixed.
const lockKey = 0x4149534A4F4253 // "AISJOBS"

const (
	DefaultInterval  = time.Minute
	defaultBatch     = 200
	sessionRetention = 7 * 24 * time.Hour
	// blobSweepEvery: listing every file the server keeps is not something to
	// do every minute.
	blobSweepEvery = time.Hour
	// queuedFileRetryMost is the longest a file the store refused to delete
	// waits before it is asked again.
	queuedFileRetryMost = 24 * time.Hour
)

type Config struct {
	// Interval is how often a sweep is attempted.
	Interval time.Duration
	// Batch bounds how much one sweep takes on of each kind; the rest waits
	// for the next tick.
	Batch int32
	// Blob is the file store to clear of orphans. Nil means none is cleared.
	Blob blob.Store
	// ExportTTL is how long an export of conversations is kept, after which
	// its files are removed from Blob; zero means tools.DefaultExportTTL.
	ExportTTL time.Duration
}

type Runner struct {
	pool   *pgxpool.Pool
	pl     *pipeline.Pipeline
	system uuid.UUID
	cfg    Config
	log    *slog.Logger
	// blobsSwept is when this instance last went through the file store.
	blobsSwept time.Time
	// blobsAt is how far it has got through the store since: the last key
	// it took on, or empty when the next pass starts from the beginning.
	blobsAt string
	// exportsSwept is when this instance last went through the exports'
	// files to the end.
	exportsSwept time.Time
}

// New returns a runner that acts as the given system actor.
func New(pool *pgxpool.Pool, pl *pipeline.Pipeline, systemActor uuid.UUID, cfg Config, log *slog.Logger) *Runner {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Batch <= 0 {
		cfg.Batch = defaultBatch
	}
	if cfg.ExportTTL <= 0 {
		cfg.ExportTTL = tools.DefaultExportTTL
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{pool: pool, pl: pl, system: systemActor, cfg: cfg, log: log}
}

// Run sweeps on every tick until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if rep, err := r.Sweep(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Error("sweep failed", "err", err)
		} else if rep.Ran && rep.total() > 0 {
			r.log.Info("swept", "proposals_expired", rep.ProposalsExpired, "members_expired", rep.MembersExpired, "orphans_removed", rep.OrphansRemoved,
				"assignments_closed", rep.AssignmentsClosed, "submissions_missing", rep.SubmissionsMissing, "sessions_deleted", rep.SessionsDeleted,
				"drafts_deleted", rep.DraftsDeleted, "queued_files_deleted", rep.QueuedFilesDeleted,
				"orphan_files_removed", rep.OrphanFilesRemoved, "export_files_removed", rep.ExportFilesRemoved)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Report says what one sweep did.
type Report struct {
	// Ran is false when another instance held the lock and this one stood by.
	Ran                bool
	ProposalsExpired   int
	MembersExpired     int
	OrphansRemoved     int
	AssignmentsClosed  int
	SubmissionsMissing int
	SessionsDeleted    int64
	// DraftsDeleted counts answers' drafts nobody has written for
	// tools.DraftTTL, which reads leave out already.
	DraftsDeleted int64
	// QueuedFilesDeleted counts the files deleted from the store that a
	// deletion queued (blob_deletion): an assignment's, deleted for good.
	QueuedFilesDeleted int
	OrphanFilesRemoved int
	// ExportFilesRemoved counts the files of exports of conversations kept
	// as long as they are kept.
	ExportFilesRemoved int
}

func (r Report) total() int64 {
	return int64(r.ProposalsExpired+r.MembersExpired+r.OrphansRemoved+r.AssignmentsClosed+r.SubmissionsMissing+r.QueuedFilesDeleted+
		r.OrphanFilesRemoved+r.ExportFilesRemoved) + r.SessionsDeleted + r.DraftsDeleted
}

// Sweep does one round of everything, if no other instance is doing so.
func (r *Runner) Sweep(ctx context.Context) (Report, error) {
	// The lock belongs to a session, so it needs a connection of its own for
	// as long as it is held.
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return Report{}, err
	}
	defer conn.Release()
	lock := dbq.New(conn)
	got, err := lock.TryJobLock(ctx, lockKey)
	if err != nil || !got {
		return Report{}, err
	}
	defer func() {
		// Not ctx: the lock must be given back even when we are shutting down.
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := lock.ReleaseJobLock(release, lockKey); err != nil {
			// Closing the connection releases the lock all the same.
			_ = conn.Conn().Close(release)
		}
	}()

	rep := Report{Ran: true}
	q := dbq.New(r.pool)
	now := r.pl.Clock()

	if ttl := r.pl.Config().ProposalTTL; ttl > 0 {
		cutoff := now.Add(-ttl)
		stale, err := q.ListStaleProposals(ctx, dbq.ListStaleProposalsParams{CreatedBefore: cutoff, MaxRows: r.cfg.Batch})
		if err != nil {
			return rep, fmt.Errorf("stale proposals: %w", err)
		}
		for _, p := range stale {
			out, err := r.pl.InvokeSystem(ctx, r.system, tools.ToolActionExpire,
				tools.ActionExpireIn{ActionID: p.ID, CreatedBefore: cutoff}, "job:action.expire:"+p.ID.String())
			if r.did(out, err, "action.expire", p.ID) {
				rep.ProposalsExpired++
			}
		}
	}

	expired, err := q.ListExpiredMembers(ctx, dbq.ListExpiredMembersParams{Now: &now, MaxRows: r.cfg.Batch})
	if err != nil {
		return rep, fmt.Errorf("expired members: %w", err)
	}
	for _, m := range expired {
		// The key carries the expiry, so a membership that is extended and
		// then expires again is swept again.
		key := "job:member.expire:" + m.ID.String() + ":" + strconv.FormatInt(m.ExpiresAt.Unix(), 10)
		out, err := r.pl.InvokeSystem(ctx, r.system, tools.ToolMemberExpire, tools.MemberExpireIn{CourseID: m.CourseID, MemberID: m.ID}, key)
		if r.did(out, err, "member.expire", m.ID) {
			rep.MembersExpired++
		}
	}

	// After the expiry sweep, which takes delegates with the principals it
	// removes: what is left is what the previous release removed without
	// them, and seats that no longer match their actor's owner.
	orphans, err := q.ListOrphanedSeats(ctx, dbq.ListOrphanedSeatsParams{Now: &now, MaxRows: r.cfg.Batch})
	if err != nil {
		return rep, fmt.Errorf("orphaned seats: %w", err)
	}
	for _, m := range orphans {
		// Once orphaned, a seat stays so, and once removed, removed: the
		// seat alone names the sweep.
		out, err := r.pl.InvokeSystem(ctx, r.system, tools.ToolMemberRemoveOrphan,
			tools.MemberRemoveOrphanIn{CourseID: m.CourseID, MemberID: m.ID}, "job:member.remove_orphan:"+m.ID.String())
		if r.did(out, err, "member.remove_orphan", m.ID) {
			rep.OrphansRemoved++
		}
	}

	due, err := q.ListAssignmentsNewlyPastDue(ctx, dbq.ListAssignmentsNewlyPastDueParams{Now: &now, SystemActorID: r.system, MaxRows: r.cfg.Batch})
	if err != nil {
		return rep, fmt.Errorf("assignments past due: %w", err)
	}
	for _, a := range due {
		// The same key the query looks for; see ListAssignmentsNewlyPastDue.
		// A group assignment's ends ":groups".
		key := "job:submission.mark_missing:" + a.ID.String() + ":" + strconv.FormatInt(a.DueAt.Unix(), 10)
		groups := a.GroupSetID != nil
		if groups {
			key += ":groups"
		}
		out, err := r.pl.InvokeSystem(ctx, r.system, tools.ToolSubmissionMarkMissing,
			tools.MarkMissingIn{CourseID: a.CourseID, AssignmentID: a.ID, DueAt: *a.DueAt, Groups: groups}, key)
		if r.did(out, err, "submission.mark_missing", a.ID) {
			rep.AssignmentsClosed++
			var res tools.MarkMissingOut
			if json.Unmarshal(out.Result, &res) == nil {
				rep.SubmissionsMissing += res.Missing
			}
		}
	}

	stale := now.Add(-sessionRetention)
	if rep.SessionsDeleted, err = q.DeleteStaleSessions(ctx, &stale); err != nil {
		return rep, fmt.Errorf("stale sessions: %w", err)
	}
	// A draft is no record, and its deletion no action, as a session's is
	// not: one nobody wrote for a while is gone already to whoever reads it.
	if rep.DraftsDeleted, err = q.DeleteStaleDrafts(ctx, now.Add(-tools.DraftTTL)); err != nil {
		return rep, fmt.Errorf("stale drafts: %w", err)
	}
	if rep.QueuedFilesDeleted, err = r.deleteQueuedFiles(ctx, now); err != nil {
		return rep, fmt.Errorf("queued files: %w", err)
	}
	if rep.OrphanFilesRemoved, err = r.sweepBlobs(ctx, now); err != nil {
		return rep, fmt.Errorf("orphan files: %w", err)
	}
	if rep.ExportFilesRemoved, err = r.sweepExports(ctx, now); err != nil {
		return rep, fmt.Errorf("export files: %w", err)
	}
	return rep, nil
}

// deleteQueuedFiles deletes from the store the files a deletion queued once
// their rows had gone (blob_deletion): an assignment's, deleted for good, its
// submitted and feedback files, the files of the instructions and rubric it
// purged, and their renditions' PDFs. The deletion queues them rather than
// deleting them itself, since it may still roll back and the store may be
// down; the row is the durable record that the file is to go.
//
// Every tick takes up to a batch of what is due, the longest due first. A
// file deleted, or gone already (the filesystem store ignores what is not
// there, and S3 answers a removal of nothing as done), leaves the queue; one
// the store refuses stays, asked again after 2^attempts minutes, a day at
// most, its error kept and logged. Without a store nothing is deleted, and
// the queue waits for one: nothing queues a file on an installation without
// a store (no_file_storage).
func (r *Runner) deleteQueuedFiles(ctx context.Context, now time.Time) (int, error) {
	if r.cfg.Blob == nil {
		return 0, nil
	}
	q := dbq.New(r.pool)
	due, err := q.ListDueBlobDeletions(ctx, dbq.ListDueBlobDeletionsParams{Now: now, MaxRows: r.cfg.Batch})
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, f := range due {
		if err := r.cfg.Blob.Delete(ctx, f.StorageKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
			r.log.Error("sweep step failed", "step", "queued file", "key", f.StorageKey, "attempts", f.Attempts+1, "err", err)
			attempts := f.Attempts + 1
			why := lastError(err)
			if err := q.PostponeBlobDeletion(ctx, dbq.PostponeBlobDeletionParams{StorageKey: f.StorageKey, Attempts: attempts,
				NextTryAt: now.Add(retryAfter(attempts)), LastError: &why}); err != nil {
				return deleted, err
			}
			continue
		}
		if err := q.DeleteBlobDeletion(ctx, f.StorageKey); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// retryAfter is how long a file the store refused attempts times waits
// before it is asked again: 2^attempts minutes, a day at most.
func retryAfter(attempts int32) time.Duration {
	if attempts >= 11 { // 2^11 minutes is more than a day
		return queuedFileRetryMost
	}
	return min(time.Duration(1<<attempts)*time.Minute, queuedFileRetryMost)
}

// lastError is what the queue keeps of an error: 1 to 500 characters.
func lastError(err error) string {
	why := []rune(strings.TrimSpace(err.Error()))
	if len(why) == 0 {
		return "the file store refused, saying nothing"
	}
	if len(why) > 500 {
		why = why[:500]
	}
	return string(why)
}

// sweepExports removes the files of exports of conversations once they are
// ExportTTL old, by when the store says they were written: an export is
// personal data, made to be taken away and not kept here. Its record, the
// action, stays; conversation.export_file refuses its files once they are
// as old (export_expired), whether or not the sweep has come yet.
//
// A pass goes through exports/ blobSweepEvery, so a file is gone within an
// hour of its time, and takes on a batch at a time: a full batch is taken
// up again at the next tick, from the start, since what it removed is no
// longer listed. Only what an export writes there is looked at
// (tools.ExportFileShape), and its age is all that is asked: an export's
// files are kept for no record, and nothing points at them. Removing one
// records no action, as removing an orphan does not.
func (r *Runner) sweepExports(ctx context.Context, now time.Time) (int, error) {
	if r.cfg.Blob == nil || now.Sub(r.exportsSwept) < blobSweepEvery {
		return 0, nil
	}
	cutoff := now.Add(-r.cfg.ExportTTL)
	batch := int(r.cfg.Batch)
	var old []string
	err := r.cfg.Blob.List(ctx, tools.ExportPrefix, "", func(key string, modified time.Time) error {
		if !tools.ExportFileShape(strings.TrimPrefix(key, tools.ExportPrefix)) || !modified.Before(cutoff) {
			return nil
		}
		if old = append(old, key); len(old) >= batch {
			return blob.ErrStopList
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range old {
		if err := r.cfg.Blob.Delete(ctx, key); err != nil {
			r.log.Error("sweep step failed", "step", "export file", "key", key, "err", err)
			continue
		}
		removed++
	}
	if len(old) < batch {
		r.exportsSwept = now
	}
	return removed, nil
}

// sweepBlobs removes files that no document version, as one of its files or
// in its own columns, no message of a conversation and no rendition points
// at, and none will. A rendition's PDF the agent runtime uploaded and never
// named, or one whose file was purged by a release that does not know of
// renditions, is such a file.
//
// A file is uploaded first and attached afterwards, so there are always some
// that are not attached yet, and some never are: the tab was closed, the
// proposal carrying it was rejected, the attaching transaction rolled back
// after the object had been moved. Which of the unattached ones are still
// wanted cannot be read from the database — a proposal names the files it
// would attach by upload token, inside its payload — so age decides it. A
// proposal is decided or cancelled within the TTL, and cannot be made naming
// an upload already more than tools.OrphanGrace old, going by when the store
// says it was written, as the sweep does (see checkUploadAge in package
// tools); so past TTL + OrphanGrace nothing can still be waiting on the file.
// Without a TTL a proposal may wait for ever, and then nothing is removed at
// all, nor is any upload too old to be proposed.
//
// An upload token does not expire for attaching (see blob.UploadClaim), so
// this is also what bounds it: a file not attached within TTL + OrphanGrace
// is gone at the next pass through the store, and attaching it then fails
// as though it had never been uploaded. A pass starts blobSweepEvery after
// the last one ended and takes a tick for every batch of orphans it removes;
// the files it keeps, attached or another deployment's, are put to the
// database a page at a time and take no place in the batch. So an orphan is
// gone about an hour after TTL + OrphanGrace however many files are kept,
// later only when orphans are made faster than a batch a tick. The price is
// that one tick may list the whole store, which is why passes are an hour
// apart. How far a pass has got is kept in memory: a restart starts it over.
//
// Only the server's own files are looked at. The bucket or directory may be
// shared with other things — a backup, another program's objects — and their
// age says nothing about whether anyone still wants them. Nor are uploads
// under a course this database does not have: they are another deployment's,
// kept in the same place, and only its database knows which it has attached.
// A deployment whose database was copied from this one has the same courses,
// and its files cannot be told from ours; the README says not to share.
func (r *Runner) sweepBlobs(ctx context.Context, now time.Time) (int, error) {
	ttl := r.pl.Config().ProposalTTL
	if r.cfg.Blob == nil || ttl <= 0 || now.Sub(r.blobsSwept) < blobSweepEvery {
		return 0, nil
	}
	cutoff := now.Add(-ttl - tools.OrphanGrace)
	batch := int(r.cfg.Batch)
	// A tick takes on a batch of orphans, and the next goes on from the last
	// one it took. What is listed is put to the database a page of old files
	// at a time, one query to a page, and only what that finds to be orphans
	// counts: most old files are attached, and are kept, and they must not
	// fill the batch, or a pass would take a tick for every batch of them.
	// The prefixes before the one a tick stopped in are done with.
	prefixes := r.blobPrefixes()
	from := 0
	for i, prefix := range prefixes {
		if strings.HasPrefix(r.blobsAt, prefix) {
			from = i
		}
	}
	var page, orphans []upload
	check := func() error {
		if len(page) == 0 {
			return nil
		}
		found, err := orphansAmong(ctx, dbq.New(r.pool), page)
		orphans, page = append(orphans, found...), page[:0]
		return err
	}
	for i, prefix := range prefixes[from:] {
		after := ""
		if i == 0 {
			after = r.blobsAt
		}
		err := r.cfg.Blob.List(ctx, prefix, after, func(key string, modified time.Time) error {
			course, ok := ownKey(strings.TrimPrefix(key, prefix))
			if !ok || !modified.Before(cutoff) {
				return nil
			}
			if page = append(page, upload{key, course}); len(page) < batch {
				return nil
			}
			if err := check(); err != nil {
				return err
			}
			if len(orphans) >= batch {
				return blob.ErrStopList
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		if len(orphans) >= batch {
			break
		}
	}
	if err := check(); err != nil {
		return 0, err
	}
	// A full batch means there may be more; the next tick goes on after the
	// last orphan taken, and lists again what came after it in its page.
	// Anything short of one is the end of the store.
	done := len(orphans) < batch
	if !done {
		orphans = orphans[:batch]
	}
	removed := 0
	for _, u := range orphans {
		gone, err := r.removeIfOrphan(ctx, u)
		if err != nil {
			r.log.Error("sweep step failed", "step", "orphan file", "key", u.key, "err", err)
			continue
		}
		if gone {
			removed++
		}
	}
	if done {
		r.blobsSwept, r.blobsAt = now, ""
	} else {
		r.blobsAt = orphans[batch-1].key
	}
	return removed, nil
}

// blobPrefixes are where the server's files are kept: uploads for documents,
// under documents/ and, from before a version held several files, courses/,
// for messages of conversations, and renditions' PDFs, and where attaching
// moves them, which for a store that moves nothing is the same place.
func (r *Runner) blobPrefixes() []string {
	var prefixes []string
	for _, prefix := range []string{tools.UploadPrefix, tools.DocumentPrefix, tools.AttachmentPrefix, tools.RenditionPrefix} {
		prefixes = append(prefixes, prefix)
		if final := r.cfg.Blob.FinalKey(prefix); final != prefix {
			prefixes = append(prefixes, final)
		}
	}
	return prefixes
}

// upload is an old file the sweep has listed under one of its prefixes, and
// the course its key names.
type upload struct {
	key    string
	course uuid.UUID
}

// ownKey reports whether name, what follows the prefix a key was listed
// under, is what document.upload_url, conversation.upload_url or
// agent_runtime.rendition_upload_url puts there:
// <course>/<upload>, two UUIDs spelt as the server spells them, and if so
// which course it names. Anything else under the prefix was put there by
// someone else, and is left alone.
func ownKey(name string) (uuid.UUID, bool) {
	course, upload, _ := strings.Cut(name, "/")
	id, isCourse := canonicalUUID(course)
	_, isUpload := canonicalUUID(upload)
	return id, isCourse && isUpload
}

func canonicalUUID(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	return u, err == nil && u.String() == s
}

// orphansAmong returns those of the uploads that are orphans, in the order
// they were given: under a course this database has, and attached to
// nothing. See ListOrphanUploads.
func orphansAmong(ctx context.Context, q *dbq.Queries, uploads []upload) ([]upload, error) {
	arg := dbq.ListOrphanUploadsParams{StorageKeys: make([]string, len(uploads)), CourseIds: make([]uuid.UUID, len(uploads))}
	for i, u := range uploads {
		arg.StorageKeys[i], arg.CourseIds[i] = u.key, u.course
	}
	rows, err := q.ListOrphanUploads(ctx, arg)
	found := make([]upload, len(rows))
	for i, row := range rows {
		found[i] = upload{row.StorageKey, row.CourseID}
	}
	return found, err
}

// removeIfOrphan deletes one object unless a version, a message or a
// rendition points at it, or it is another deployment's. The page it was listed in said it was neither, but
// an attach may have committed since: it asks again, holding the lock that
// attaching takes on the same key, so that "is it attached?" and the
// deletion are one step. An attach in flight either commits first, and the
// file is kept, or comes after, and finds nothing uploaded.
func (r *Runner) removeIfOrphan(ctx context.Context, u upload) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := dbq.New(tx)
	if err := q.LockStorageKey(ctx, u.key); err != nil {
		return false, err
	}
	if found, err := orphansAmong(ctx, q, []upload{u}); err != nil || len(found) == 0 {
		return false, err
	}
	if err := r.cfg.Blob.Delete(ctx, u.key); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// did reports whether a system call actually did something just now, and logs
// the ones that went wrong. One failure does not stop the sweep: the others
// are independent of it.
func (r *Runner) did(out pipeline.Outcome, err error, what string, id uuid.UUID) bool {
	switch {
	case errors.Is(err, tools.ErrSweepMoot):
		return false // unpublished, or its due date moved, meanwhile; swept once it is due again
	case errors.Is(err, tools.ErrNotOrphaned):
		return false // removed meanwhile, by someone or by the expiry sweep's cascade
	case err != nil:
		r.log.Error("sweep step failed", "step", what, "id", id, "err", err)
		return false
	case out.Status == domain.StatusFailed:
		r.log.Warn("sweep step was refused", "step", what, "id", id, "error", out.Error)
		return false
	case out.Replayed:
		return false // already done, by an earlier sweep or another instance
	}
	var res tools.SweepOut
	return json.Unmarshal(out.Result, &res) == nil && res.Done
}
