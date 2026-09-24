// Package jobs runs the background sweeps: proposals that have waited too
// long, memberships past their expiry, assignments whose due date has passed,
// sessions long dead, uploaded files that nothing came to point at.
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
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// lockKey is the advisory lock all instances contend for. Arbitrary, fixed.
const lockKey = 0x4149534A4F4253 // "AISJOBS"

const (
	DefaultInterval  = time.Minute
	defaultBatch     = 200
	sessionRetention = 7 * 24 * time.Hour
	// OrphanGrace is how long past the proposal TTL an unattached upload is
	// kept. See sweepBlobs.
	OrphanGrace = 48 * time.Hour
	// blobSweepEvery: listing every file the server keeps is not something to
	// do every minute.
	blobSweepEvery = time.Hour
)

type Config struct {
	// Interval is how often a sweep is attempted.
	Interval time.Duration
	// Batch bounds how much one sweep takes on of each kind; the rest waits
	// for the next tick.
	Batch int32
	// Blob is the file store to clear of orphans. Nil means none is cleared.
	Blob blob.Store
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
}

// New returns a runner that acts as the given system actor.
func New(pool *pgxpool.Pool, pl *pipeline.Pipeline, systemActor uuid.UUID, cfg Config, log *slog.Logger) *Runner {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Batch <= 0 {
		cfg.Batch = defaultBatch
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
			r.log.Info("swept", "proposals_expired", rep.ProposalsExpired, "members_expired", rep.MembersExpired,
				"assignments_closed", rep.AssignmentsClosed, "submissions_missing", rep.SubmissionsMissing, "sessions_deleted", rep.SessionsDeleted,
				"orphan_files_removed", rep.OrphanFilesRemoved)
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
	AssignmentsClosed  int
	SubmissionsMissing int
	SessionsDeleted    int64
	OrphanFilesRemoved int
}

func (r Report) total() int64 {
	return int64(r.ProposalsExpired+r.MembersExpired+r.AssignmentsClosed+r.SubmissionsMissing+r.OrphanFilesRemoved) + r.SessionsDeleted
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

	due, err := q.ListAssignmentsNewlyPastDue(ctx, dbq.ListAssignmentsNewlyPastDueParams{Now: &now, SystemActorID: r.system, MaxRows: r.cfg.Batch})
	if err != nil {
		return rep, fmt.Errorf("assignments past due: %w", err)
	}
	for _, a := range due {
		// The same key the query looks for; see ListAssignmentsNewlyPastDue.
		key := "job:submission.mark_missing:" + a.ID.String() + ":" + strconv.FormatInt(a.DueAt.Unix(), 10)
		out, err := r.pl.InvokeSystem(ctx, r.system, tools.ToolSubmissionMarkMissing,
			tools.MarkMissingIn{CourseID: a.CourseID, AssignmentID: a.ID, DueAt: *a.DueAt}, key)
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
	if rep.OrphanFilesRemoved, err = r.sweepBlobs(ctx, now); err != nil {
		return rep, fmt.Errorf("orphan files: %w", err)
	}
	return rep, nil
}

// sweepBlobs removes files that no document version points at and none will.
//
// A file is uploaded first and attached afterwards, so there are always some
// that are not attached yet, and some never are: the tab was closed, the
// proposal carrying it was rejected, the attaching transaction rolled back
// after the object had been moved. Which of the unattached ones are still
// wanted cannot be read from the database — a proposal names its feedback
// files by upload token, inside its payload — so age decides it. An upload is
// made before the proposal that names it, and a proposal is decided or
// cancelled within the TTL; past TTL + OrphanGrace nothing can still be
// waiting on the file. Without a TTL a proposal may wait for ever, and then
// nothing is removed at all.
//
// An upload token does not expire for attaching (see blob.UploadClaim), so
// this is also what bounds it: a file not attached within TTL + OrphanGrace
// is gone, and attaching it then fails as though it had never been uploaded.
//
// Only the server's own files are looked at. The bucket or directory may be
// shared with other things — a backup, another program's objects — and their
// age says nothing about whether anyone still wants them.
func (r *Runner) sweepBlobs(ctx context.Context, now time.Time) (int, error) {
	ttl := r.pl.Config().ProposalTTL
	if r.cfg.Blob == nil || ttl <= 0 || now.Sub(r.blobsSwept) < blobSweepEvery {
		return 0, nil
	}
	cutoff := now.Add(-ttl - OrphanGrace)
	// A pass through the store takes a batch of old files a tick, and each
	// tick goes on from where the last one stopped. Most old files are
	// attached, and kept; a pass that started over every tick would take on
	// the same attached files each time and never reach what lies behind
	// them. The prefixes before the one it stopped in are done with.
	prefixes := r.blobPrefixes()
	from := 0
	for i, prefix := range prefixes {
		if strings.HasPrefix(r.blobsAt, prefix) {
			from = i
		}
	}
	var old []string
	for i, prefix := range prefixes[from:] {
		after := ""
		if i == 0 {
			after = r.blobsAt
		}
		err := r.cfg.Blob.List(ctx, prefix, after, func(key string, modified time.Time) error {
			if !ownKey(strings.TrimPrefix(key, prefix)) || !modified.Before(cutoff) {
				return nil
			}
			if old = append(old, key); len(old) >= int(r.cfg.Batch) {
				return blob.ErrStopList
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		if len(old) >= int(r.cfg.Batch) {
			break
		}
	}
	removed := 0
	for _, key := range old {
		gone, err := r.removeIfOrphan(ctx, key)
		if err != nil {
			r.log.Error("sweep step failed", "step", "orphan file", "key", key, "err", err)
			continue
		}
		if gone {
			removed++
		}
	}
	// A full batch means there may be more; the next tick goes on after the
	// last key taken. Anything short of one is the end of the store.
	if len(old) < int(r.cfg.Batch) {
		r.blobsSwept, r.blobsAt = now, ""
	} else {
		r.blobsAt = old[len(old)-1]
	}
	return removed, nil
}

// blobPrefixes are where the server's files are kept: uploads, and where
// attaching moves them, which for a store that moves nothing is the same
// place.
func (r *Runner) blobPrefixes() []string {
	prefixes := []string{tools.UploadPrefix}
	if final := r.cfg.Blob.FinalKey(tools.UploadPrefix); final != tools.UploadPrefix {
		prefixes = append(prefixes, final)
	}
	return prefixes
}

// ownKey reports whether name, what follows the prefix a key was listed
// under, is what document.upload_url puts there: <course>/<upload>, two UUIDs
// spelt as the server spells them. Anything else under the prefix was put
// there by someone else, and is left alone.
func ownKey(name string) bool {
	course, upload, ok := strings.Cut(name, "/")
	return ok && canonicalUUID(course) && canonicalUUID(upload)
}

func canonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// removeIfOrphan deletes one object unless a version points at it. It holds
// the lock that attaching takes on the same key, so that "is it attached?"
// and the deletion are one step: an attach in flight either commits first,
// and the file is kept, or comes after, and finds nothing uploaded.
func (r *Runner) removeIfOrphan(ctx context.Context, key string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := dbq.New(tx)
	if err := q.LockStorageKey(ctx, key); err != nil {
		return false, err
	}
	if used, err := q.StorageKeyInUse(ctx, &key); err != nil || used {
		return false, err
	}
	if err := r.cfg.Blob.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// did reports whether a system call actually did something just now, and logs
// the ones that went wrong. One failure does not stop the sweep: the others
// are independent of it.
func (r *Runner) did(out pipeline.Outcome, err error, what string, id uuid.UUID) bool {
	switch {
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
