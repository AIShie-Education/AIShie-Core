// Package jobs runs the background sweeps: proposals that have waited too
// long, memberships past their expiry, assignments whose due date has passed,
// sessions long dead.
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
)

type Config struct {
	// Interval is how often a sweep is attempted.
	Interval time.Duration
	// Batch bounds how much one sweep takes on of each kind; the rest waits
	// for the next tick.
	Batch int32
}

type Runner struct {
	pool   *pgxpool.Pool
	pl     *pipeline.Pipeline
	system uuid.UUID
	cfg    Config
	log    *slog.Logger
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
				"assignments_closed", rep.AssignmentsClosed, "submissions_missing", rep.SubmissionsMissing, "sessions_deleted", rep.SessionsDeleted)
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
}

func (r Report) total() int64 {
	return int64(r.ProposalsExpired+r.MembersExpired+r.AssignmentsClosed+r.SubmissionsMissing) + r.SessionsDeleted
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
	return rep, nil
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
