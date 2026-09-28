package tools

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Join links (docs/schema.md §2.2). Whoever holds member_invite in a course
// makes a link, which students open — typically by scanning the QR code the
// front end shows of it in class — to be seated as students there and then:
// signed in, by course.join, which only the join endpoints call; with no
// account, by registering through the link, which makes the person and seats
// them in one transaction. Every link lives JoinLinkLifetime from when it is
// made. The seat is the course's student preset, held to its maker's own
// seat when the link is made and at every join, as member.add holds a seat
// to whoever adds it: the link lends its maker's authority, and lends it only
// while they have it.

func joinLinkTools() []tool.Tool {
	return []tool.Tool{joinLinkCreate(), joinLinkList(), joinLinkRevoke(), courseJoin()}
}

const (
	// ToolCourseJoin is the tool the join endpoints call: a person takes a
	// seat through a join link. It is Unlisted: neither adapter offers it.
	ToolCourseJoin = "course.join"

	EventJoinLinkCreated = "course.join_link_created"
	EventJoinLinkRevoked = "course.join_link_revoked"

	// JoinLinkLifetime is how long every join link works, from when it is
	// made: long enough for a room to scan it, too short for a photograph
	// of the screen passed around afterwards to be a way in. Nobody chooses
	// another; the database holds it (course_join_link_lives_ten_minutes).
	JoinLinkLifetime   = 10 * time.Minute
	MaxJoinLinkUses    = 10000
	MaxJoinLinkDomains = 20
)

// Why a join link seats nobody now: the reason a join, a registration or the
// page that opens a link is told.
const (
	JoinRevoked              = "revoked"
	JoinExpired              = "expired"
	JoinUsedUp               = "used_up"
	JoinCourseArchived       = "course_archived"
	JoinCreatorLostAuthority = "creator_lost_authority"
	// Why a person is not let in through a link that seats others.
	JoinPeopleOnly         = "people_only"
	JoinEmailDomainAllowed = "email_domain_not_allowed"
)

// invitesMembers gates the join link tools: making a course's links, and
// listing and revoking them.
var invitesMembers = tool.Gate{Perms: []domain.Perm{domain.PermMemberInvite}}

// maxJoinTokenLen bounds what is taken for a token before anything is looked
// up: one of ours is 64 characters.
const maxJoinTokenLen = 128

func noSuchJoinLink() *apperr.Error {
	return apperr.Missing("no such join link: check that the whole link was copied")
}

// findJoinLink finds the link a presented token is for. Every way a token
// can be wrong — not shaped like one, an unknown prefix, a secret that does
// not match — is the same not_found, so that none tells a guesser more than
// another. The secret is compared with the kept hash in constant time.
func findJoinLink(ctx context.Context, q dbq.Querier, token string) (dbq.GetJoinLinkByPrefixRow, error) {
	if len(token) > maxJoinTokenLen {
		return dbq.GetJoinLinkByPrefixRow{}, noSuchJoinLink()
	}
	prefix, ok := auth.JoinTokenPrefix(token)
	if !ok {
		return dbq.GetJoinLinkByPrefixRow{}, noSuchJoinLink()
	}
	l, err := q.GetJoinLinkByPrefix(ctx, prefix)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return l, noSuchJoinLink()
	case err != nil:
		return l, err
	case !auth.JoinTokenMatches(token, l.SecretHash):
		return dbq.GetJoinLinkByPrefixRow{}, noSuchJoinLink()
	}
	return l, nil
}

func linkOf(r dbq.GetJoinLinkByPrefixRow) dbq.CourseJoinLink {
	return dbq.CourseJoinLink{ID: r.ID, CourseID: r.CourseID, TokenPrefix: r.TokenPrefix, SecretHash: r.SecretHash, Role: r.Role,
		PresetID: r.PresetID, CreatedByMemberID: r.CreatedByMemberID, ExpiresAt: r.ExpiresAt, MaxUses: r.MaxUses, Uses: r.Uses,
		AllowedEmailDomains: r.AllowedEmailDomains, RevokedAt: r.RevokedAt, RevokedByMemberID: r.RevokedByMemberID,
		CreatedAt: r.CreatedAt}
}

// linkSeat is the seat a link hands out, from its preset as it stands: its
// levels and scope, for a student, whose list is themselves.
type linkSeat struct {
	perms                         permSet
	studentScope, assignmentScope string
}

func seatOfPreset(p dbq.PermissionPreset) linkSeat {
	return linkSeat{perms: presetPerms(p), studentScope: p.StudentScope, assignmentScope: p.AssignmentScope}
}

// listsItself: a student's seat lists itself, unless the preset reaches the
// whole class.
func (s linkSeat) listsItself() bool { return s.studentScope == domain.ScopeListed }

// withinMaker is withinGranter for a seat a link hands out, measured against
// the seat of whoever is lending their authority to it. It says why not in
// its own words when it is not.
func (s linkSeat) withinMaker(ctx context.Context, q dbq.Querier, maker *domain.Member) error {
	return withinGranter(ctx, q, maker, s.perms, s.listsItself(), s.studentScope, nil, s.assignmentScope, nil)
}

// linkGrant is what a link seats with now, as its maker's seat stands.
type linkGrant struct {
	maker  *domain.Member
	preset dbq.PermissionPreset
	seat   linkSeat
}

func joinRefused(reason, format string, args ...any) *apperr.Error {
	return apperr.Precondition(format, args...).With("reason", reason)
}

func makerLost(why string) *apperr.Error {
	return joinRefused(JoinCreatorLostAuthority, "whoever made the join link can no longer invite students to the course (%s); "+
		"ask for a new link", why)
}

// joinStanding says whether a link seats anyone now and, when it does, with
// what: when it does not, the refusal, with its reason. It is asked of every
// join, under the link's lock and with its maker's seat held (hold), and of
// the page that opens a link, locking nothing. In order: revoked, expired,
// used up, its course archived, and its maker no longer able to make it —
// suspended, their seat paused, removed or over (or, for a delegate, its
// principal's), their member_invite denied or needing approval (a proposal
// would have put the seat before someone to decide, and a link seats at
// once), or the preset grown beyond what they hold, or their reach narrowed
// to a list, which no new student is on (withinGranter). An error is a fault
// of ours.
func joinStanding(ctx context.Context, q dbq.Querier, l dbq.CourseJoinLink, courseStatus string, now time.Time, hold bool) (linkGrant, *apperr.Error, error) {
	switch {
	case l.RevokedAt != nil:
		return linkGrant{}, joinRefused(JoinRevoked, "the join link has been revoked; ask for a new one"), nil
	case !l.ExpiresAt.After(now):
		return linkGrant{}, joinRefused(JoinExpired, "the join link expired at %s; ask for a new one",
			l.ExpiresAt.UTC().Format(time.RFC3339)), nil
	case l.MaxUses != nil && l.Uses >= *l.MaxUses:
		return linkGrant{}, joinRefused(JoinUsedUp, "the join link has been used as many times as it may be; ask for a new one"), nil
	case courseStatus == domain.CourseArchived:
		return linkGrant{}, joinRefused(JoinCourseArchived, "the course is archived: nobody joins it"), nil
	}
	if hold {
		// KEY SHARE, as a write holds its caller's seat: a change to the
		// maker's seat, which locks it FOR UPDATE, waits for this join, or
		// this join waits for it and reads what it did.
		if err := q.ShareSeats(ctx, []uuid.UUID{l.CreatedByMemberID}); err != nil {
			return linkGrant{}, nil, err
		}
	}
	maker, err := authz.LoadMember(ctx, q, l.CreatedByMemberID)
	if err != nil {
		return linkGrant{}, nil, err
	}
	actor, err := authz.LoadActor(ctx, q, maker.ActorID)
	if err != nil {
		return linkGrant{}, nil, err
	}
	switch {
	case !actor.Active():
		return linkGrant{}, makerLost("they are suspended"), nil
	case !maker.Live(now) || !maker.PrincipalLive(now):
		return linkGrant{}, makerLost("their seat is paused, removed or over"), nil
	case maker.Perm(domain.PermMemberInvite) < domain.PendingReview:
		return linkGrant{}, makerLost("they no longer hold member_invite without approval"), nil
	}
	preset, err := q.GetPreset(ctx, l.PresetID)
	if err != nil {
		return linkGrant{}, nil, err
	}
	g := linkGrant{maker: maker, preset: preset, seat: seatOfPreset(preset)}
	if err := g.seat.withinMaker(ctx, q, maker); err != nil {
		if e, ok := apperr.As(err); ok {
			return linkGrant{}, makerLost(e.Message), nil
		}
		return linkGrant{}, nil, err
	}
	return g, nil, nil
}

// emailDomain is the domain of an email address, lower-cased: what follows
// its last @.
func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

// domainAllowed reports whether a link that lets in only some domains lets
// in this email: exactly one of them, and never a subdomain of one, which
// may be anyone's. A link with no list lets in anyone's; an actor with no
// email is let in by no list.
func domainAllowed(domains []string, email *string) bool {
	if domains == nil {
		return true
	}
	return email != nil && slices.Contains(domains, emailDomain(*email))
}

func domainRefused(domains []string) *apperr.Error {
	return joinRefused(JoinEmailDomainAllowed, "the join link is for people whose email is at %s", strings.Join(domains, ", ")).
		With("allowed_email_domains", domains)
}

// hostname is a domain as an email names it, in ASCII: an internationalised
// one in its xn-- form.
var hostname = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// joinDomains is the list a maker gives, as it is kept: lower-case, with no
// @ before it, each once, in order. Left out, it is null: every domain.
func joinDomains(in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	if len(in) == 0 {
		return nil, apperr.Invalid("allowed_email_domains is empty: leave it out to let in an email at any domain")
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")
		if len(d) > 253 || !hostname.MatchString(d) {
			return nil, apperr.Invalid("%q is not a domain such as example.edu", d)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) > MaxJoinLinkDomains {
		return nil, apperr.Invalid("allowed_email_domains names %d domains; at most %d", len(out), MaxJoinLinkDomains)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// course.join_link_create
// ---------------------------------------------------------------------------

type JoinLinkCreateIn struct {
	tool.InCourse
	MaxUses             *int     `json:"max_uses,omitempty" jsonschema:"how many people may join through it, from 1 to 10000; as many as like while it lives unless given"`
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty" jsonschema:"only people whose email is at one of these domains, exactly: example.edu does not take mail.example.edu; at most 20. Anyone's unless given. Core cannot check an email, so this keeps out whoever gives another, not whoever claims one of these"`
}

type JoinLinkCreateOut struct {
	LinkID              uuid.UUID `json:"link_id"`
	Token               string    `json:"token" jsonschema:"what the link carries, for the front end's join page (its path /join/<token>, say), which may show it as a QR code too; shown once, and a replay of this call comes back without it"`
	ExpiresAt           time.Time `json:"expires_at" jsonschema:"ten minutes after it was made, always"`
	MaxUses             *int      `json:"max_uses,omitempty"`
	AllowedEmailDomains []string  `json:"allowed_email_domains,omitempty"`
}

func joinLinkCreate() tool.Tool {
	return tool.Define(tool.Spec[JoinLinkCreateIn, JoinLinkCreateOut]{
		Name: "course.join_link_create",
		Description: "Make a link that seats whoever opens it as a student of the course, at once and with no approval — " +
			"typically shown as a QR code in class: a person signed in joins, and someone with no account registers through " +
			"it (name, email, password) and joins. Nobody registers any other way. Every link works for ten minutes from " +
			"now, and then never again; make another for the next class. It seats max_uses people at most if given, and " +
			"only emails at allowed_email_domains if given. The seat is the course's student preset, as member.add gives " +
			"it, and is yours to give: it must be within what you hold, now and at every join, so the link stops working " +
			"(creator_lost_authority) once you are paused or removed, lose member_invite, or no longer hold what the preset " +
			"gives; a seat taken through it ends when yours does. The token is returned once and only its hash is kept. " +
			"Revoke it with course.join_link_revoke. Not by proposal: with member_invite at confirm_required, ask someone " +
			"who holds it without approval.",
		Kind: tool.Write, Gate: invitesMembers,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/join-links"},
		SecretOut: []string{"token"},
		Resolve: func(_ context.Context, _ dbq.Querier, in JoinLinkCreateIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_join_link"}, nil
		},
		// The token is shown once, to whoever the call returns to. Carried
		// out on approval, that would be whoever approved it, and never
		// whoever asked for it; and it would live ten minutes from then.
		Pin: func(context.Context, dbq.Querier, *domain.Member, time.Time, JoinLinkCreateIn) (JoinLinkCreateIn, error) {
			return JoinLinkCreateIn{}, apperr.Precondition("a join link is not made by proposal: its token is shown once, to whoever "+
				"makes it, and approving it would show it to whoever approved it; ask someone who holds member_invite "+
				"without approval").With("reason", "not_by_proposal")
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in JoinLinkCreateIn) (JoinLinkCreateOut, error) {
			if in.MaxUses != nil && (*in.MaxUses < 1 || *in.MaxUses > MaxJoinLinkUses) {
				return JoinLinkCreateOut{}, apperr.Invalid("max_uses must be from 1 to %d", MaxJoinLinkUses)
			}
			domains, err := joinDomains(in.AllowedEmailDomains)
			if err != nil {
				return JoinLinkCreateOut{}, err
			}
			student := "student"
			preset, err := findPreset(ctx, ec.Q, in.CourseID, &student, nil)
			if err != nil {
				return JoinLinkCreateOut{}, err
			}
			if preset.Role != "student" {
				return JoinLinkCreateOut{}, apperr.Precondition("the course's student preset seats its members as %s, and a join link "+
					"seats students", preset.Role)
			}
			if err := seatOfPreset(preset).withinMaker(ctx, ec.Q, ec.Member); err != nil {
				return JoinLinkCreateOut{}, err
			}
			tok, err := auth.NewJoinToken()
			if err != nil {
				return JoinLinkCreateOut{}, err
			}
			// Both as the database keeps them, to the microsecond, so that
			// the one is the other and ten minutes exactly.
			made := ec.Now.Truncate(time.Microsecond)
			expires := made.Add(JoinLinkLifetime)
			id := ids.New()
			var maxUses *int32
			if in.MaxUses != nil {
				n := int32(*in.MaxUses) //nolint:gosec // bounded above
				maxUses = &n
			}
			if err := ec.Q.InsertJoinLink(ctx, dbq.InsertJoinLinkParams{
				ID: id, CourseID: in.CourseID, TokenPrefix: tok.Prefix, SecretHash: tok.Hash, Role: "student", PresetID: preset.ID,
				CreatedByMemberID: ec.Member.ID, ExpiresAt: expires, CreatedAt: made, MaxUses: maxUses, AllowedEmailDomains: domains,
			}); err != nil {
				return JoinLinkCreateOut{}, err
			}
			payload := map[string]any{"expires_at": expires}
			if in.MaxUses != nil {
				payload["max_uses"] = *in.MaxUses
			}
			if domains != nil {
				payload["allowed_email_domains"] = domains
			}
			ec.Emit(events.Event{Type: EventJoinLinkCreated, CourseID: &in.CourseID, SubjectType: "course_join_link", SubjectID: &id,
				Payload: payload})
			return JoinLinkCreateOut{LinkID: id, Token: tok.Full, ExpiresAt: expires, MaxUses: in.MaxUses, AllowedEmailDomains: domains}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// course.join_link_list
// ---------------------------------------------------------------------------

type JoinLinkListIn struct {
	tool.InCourse
	Page
}

// JoinLinkView is a link as whoever may list the course's links sees it:
// never its token, of which only a hash is kept.
type JoinLinkView struct {
	ID                  uuid.UUID  `json:"id"`
	Status              string     `json:"status" jsonschema:"live, expired, used_up or revoked"`
	Joinable            bool       `json:"joinable" jsonschema:"whether it seats anyone now"`
	Reason              string     `json:"reason,omitempty" jsonschema:"why it seats nobody now: revoked, expired, used_up, course_archived or creator_lost_authority"`
	Role                string     `json:"role" jsonschema:"what it seats: student"`
	PresetID            uuid.UUID  `json:"preset_id" jsonschema:"the preset whose levels and scope it gives, as they stand at each join"`
	CreatedByMemberID   uuid.UUID  `json:"created_by_member_id" jsonschema:"whose authority it seats with, and whose seat a seat taken through it ends with"`
	CreatedByName       string     `json:"created_by_name"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	MaxUses             *int32     `json:"max_uses,omitempty"`
	Uses                int32      `json:"uses" jsonschema:"how many people have joined through it; member.list with join_link_id says who"`
	AllowedEmailDomains []string   `json:"allowed_email_domains,omitempty"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	RevokedByMemberID   *uuid.UUID `json:"revoked_by_member_id,omitempty"`
	RevokedByName       *string    `json:"revoked_by_name,omitempty"`
}

type JoinLinkListOut struct {
	Links []JoinLinkView `json:"links"`
	Next  *uuid.UUID     `json:"next,omitempty"`
}

// joinLinkStatus is what a link says of itself, whatever its course and maker.
func joinLinkStatus(l dbq.CourseJoinLink, now time.Time) string {
	switch {
	case l.RevokedAt != nil:
		return JoinRevoked
	case !l.ExpiresAt.After(now):
		return JoinExpired
	case l.MaxUses != nil && l.Uses >= *l.MaxUses:
		return JoinUsedUp
	}
	return "live"
}

func joinLinkList() tool.Tool {
	return tool.Define(tool.Spec[JoinLinkListIn, JoinLinkListOut]{
		Name: "course.join_link_list",
		Description: "The course's join links, newest first: each one's status (live, expired, used_up or revoked), whether it " +
			"seats anyone now and why not (course_archived, or creator_lost_authority when whoever made it no longer could), " +
			"when it expires, who made and who revoked it, which email domains it takes, and how many have joined through " +
			"it; member.list with join_link_id says who. Never a token: only its hash is kept.",
		Kind: tool.Read, Gate: invitesMembers,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/join-links"},
		Resolve: func(_ context.Context, _ dbq.Querier, in JoinLinkListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_join_link"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in JoinLinkListIn) (JoinLinkListOut, error) {
			course, err := rc.Q.GetCourseForAuthz(ctx, in.CourseID)
			if err != nil {
				return JoinLinkListOut{}, err
			}
			rows, err := rc.Q.ListJoinLinks(ctx, dbq.ListJoinLinksParams{CourseID: in.CourseID, Before: in.After, MaxRows: in.limit()})
			if err != nil {
				return JoinLinkListOut{}, err
			}
			out := JoinLinkListOut{Links: make([]JoinLinkView, 0, len(rows))}
			for _, r := range rows {
				l := dbq.CourseJoinLink{ID: r.ID, CourseID: r.CourseID, Role: r.Role, PresetID: r.PresetID,
					CreatedByMemberID: r.CreatedByMemberID, ExpiresAt: r.ExpiresAt, MaxUses: r.MaxUses, Uses: r.Uses,
					AllowedEmailDomains: r.AllowedEmailDomains, RevokedAt: r.RevokedAt, RevokedByMemberID: r.RevokedByMemberID,
					CreatedAt: r.CreatedAt}
				v := JoinLinkView{ID: r.ID, Status: joinLinkStatus(l, rc.Now), Role: r.Role, PresetID: r.PresetID,
					CreatedByMemberID: r.CreatedByMemberID, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt,
					ExpiresAt: r.ExpiresAt, MaxUses: r.MaxUses, Uses: r.Uses, AllowedEmailDomains: r.AllowedEmailDomains,
					RevokedAt: r.RevokedAt, RevokedByMemberID: r.RevokedByMemberID, RevokedByName: r.RevokedByName}
				_, refusal, err := joinStanding(ctx, rc.Q, l, course.Status, rc.Now, false)
				if err != nil {
					return JoinLinkListOut{}, err
				}
				if refusal == nil {
					v.Joinable = true
				} else {
					v.Reason, _ = refusal.Details["reason"].(string)
				}
				out.Links = append(out.Links, v)
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// course.join_link_revoke
// ---------------------------------------------------------------------------

type JoinLinkRevokeIn struct {
	tool.InCourse
	LinkID uuid.UUID `json:"link_id"`
}

func joinLinkRevoke() tool.Tool {
	return tool.Define(tool.Spec[JoinLinkRevokeIn, OK]{
		Name: "course.join_link_revoke",
		Description: "Revoke a join link before its ten minutes are up: from now on it seats nobody, and whoever opens it is " +
			"told it was revoked. Anyone who holds member_invite may revoke any of the course's links, whoever made it. " +
			"Those who joined through it keep their seats; remove them with member.remove.",
		Kind: tool.Write, Gate: invitesMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/join-links/{link_id}/revoke"},
		Resolve: func(ctx context.Context, q dbq.Querier, in JoinLinkRevokeIn) (tool.Target, error) {
			if _, err := q.GetJoinLinkInCourse(ctx, dbq.GetJoinLinkInCourseParams{ID: in.LinkID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such join link in this course")
			} else if err != nil {
				return tool.Target{}, err
			}
			return tool.Target{CourseID: in.CourseID, Type: "course_join_link", ID: &in.LinkID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in JoinLinkRevokeIn) (OK, error) {
			// Held as a join holds it: a join in flight finishes first, and
			// one after this finds the link revoked.
			if _, err := ec.Q.LockJoinLink(ctx, in.LinkID); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.RevokeJoinLink(ctx, dbq.RevokeJoinLinkParams{ID: in.LinkID, RevokedAt: &ec.Now, RevokedByMemberID: &ec.Member.ID})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the join link is revoked already")
			}
			ec.Emit(events.Event{Type: EventJoinLinkRevoked, CourseID: &in.CourseID, SubjectType: "course_join_link", SubjectID: &in.LinkID})
			return OK{OK: true}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// course.join: taking a seat through a link (the join endpoints only)
// ---------------------------------------------------------------------------

type CourseJoinIn struct {
	Token string `json:"token" jsonschema:"the join link's token"`
}

type CourseJoinOut struct {
	CourseID      uuid.UUID `json:"course_id"`
	MemberID      uuid.UUID `json:"member_id"`
	Status        string    `json:"status" jsonschema:"the seat's status: active, or paused for a seat you had already that is paused"`
	AlreadyMember bool      `json:"already_member" jsonschema:"you had a seat in the course already: that seat, as it is; nothing was joined, and no use of the link counted"`
	JoinLinkID    uuid.UUID `json:"join_link_id"`
}

func courseJoin() tool.Tool {
	return tool.Define(tool.Spec[CourseJoinIn, CourseJoinOut]{
		Name: ToolCourseJoin,
		Description: "Take a seat, as a student, in the course a join link is for. For people, through the join endpoints " +
			"(POST /v1/join/{token}); no adapter offers it.",
		Kind: tool.Write, Gate: self, Unlisted: true,
		SecretIn: []string{"token"},
		// The link names its course, and so the action's: an archived one
		// refuses it as it refuses every write (course_archived).
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseJoinIn) (tool.Target, error) {
			l, err := findJoinLink(ctx, q, in.Token)
			if err != nil {
				return tool.Target{}, err
			}
			return tool.Target{CourseID: l.CourseID, Type: "course_join_link", ID: &l.ID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseJoinIn) (CourseJoinOut, error) {
			found, err := findJoinLink(ctx, ec.Q, in.Token)
			if err != nil {
				return CourseJoinOut{}, err
			}
			// The link first, and everything about it read under it: joins
			// through one link take their turns, each counting its use
			// against what the one before left, and a revocation waits for
			// them or they for it.
			link, err := ec.Q.LockJoinLink(ctx, found.ID)
			if err != nil {
				return CourseJoinOut{}, err
			}
			out := CourseJoinOut{CourseID: link.CourseID, JoinLinkID: link.ID}
			joiner, err := ec.Q.GetActorForShare(ctx, ec.Actor.ID)
			if err != nil {
				return CourseJoinOut{}, err
			}
			// A link seats people. An agent is seated by whoever manages the
			// course, or brought in by its owner as their delegate. This
			// reads kind to refuse, as the refusals of ownership do; nothing
			// that grants reads it.
			if joiner.Kind != "human" {
				return CourseJoinOut{}, joinRefused(JoinPeopleOnly, "a join link seats people; an agent is seated by whoever "+
					"manages the course (member.add), or by its owner as their delegate (member.add_delegate)")
			}
			// A seat already there is the answer, as it is: joining again
			// changes nothing, counts no use, and resumes nothing a manager
			// paused. One past its expiry is as good as removed, and seat()
			// removes it and makes a fresh one.
			switch live, err := ec.Q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: link.CourseID, ActorID: ec.Actor.ID}); {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return CourseJoinOut{}, err
			case live.ExpiresAt == nil || live.ExpiresAt.After(ec.Now):
				out.MemberID, out.Status, out.AlreadyMember = live.ID, live.Status, true
				return out, nil
			}
			course, err := ec.Q.GetCourseForAuthz(ctx, link.CourseID)
			if err != nil {
				return CourseJoinOut{}, err
			}
			grant, refusal, err := joinStanding(ctx, ec.Q, link, course.Status, ec.Now, true)
			if err != nil {
				return CourseJoinOut{}, err
			}
			if refusal != nil {
				return CourseJoinOut{}, refusal
			}
			if !domainAllowed(link.AllowedEmailDomains, joiner.Email) {
				return CourseJoinOut{}, domainRefused(link.AllowedEmailDomains)
			}
			// Added by whoever made the link, whose authority it is, which
			// keeps the chain of who seated whom; the seat names the link, and
			// ends when its maker's does.
			id, err := seat(ctx, ec, seating{
				courseID: link.CourseID, actorID: ec.Actor.ID, preset: &grant.preset, perms: grant.seat.perms, role: link.Role,
				studentScope: grant.seat.studentScope, assignmentScope: grant.seat.assignmentScope, expiresAt: seatEnds(grant.maker),
				addedBy: &grant.maker.ActorID, joinLinkID: &link.ID,
			})
			if err != nil {
				return CourseJoinOut{}, err
			}
			// Under the link's lock nothing can have taken the last use
			// meanwhile; the condition, and the CHECK beneath it, hold that
			// whatever else goes wrong.
			if n, err := ec.Q.CountJoinLinkUse(ctx, link.ID); err != nil {
				return CourseJoinOut{}, err
			} else if n == 0 {
				return CourseJoinOut{}, joinRefused(JoinUsedUp, "the join link has been used as many times as it may be; ask for a new one")
			}
			out.MemberID, out.Status = id, domain.MemberActive
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// What the join endpoints ask without a caller: the page that opens a link,
// and the checks before someone registers through one.
// ---------------------------------------------------------------------------

// JoinPreview is what the page that opens a join link may show, to anyone
// who holds the link, signed in or not: which course it is, whether it seats
// anyone now — signed in, or registering through it — and why not, until
// when, and for which domains. Nothing else about the course or its members.
type JoinPreview struct {
	Course   JoinPreviewCourse `json:"course"`
	Joinable bool              `json:"joinable"`
	// Reason is why not: revoked, expired, used_up, course_archived or
	// creator_lost_authority.
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// Registration says someone with no account may register through it
	// now: while it seats anyone, unless this server takes no
	// registrations through links (the join endpoints' switch), and people
	// sign in — by single sign-on, say — and then join.
	Registration        bool     `json:"registration"`
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty"`

	// For the endpoint that registers through it; never shown.
	linkID, makerActorID uuid.UUID
}

type JoinPreviewCourse struct {
	Code    string `json:"code"`
	Section string `json:"section"`
	Title   string `json:"title"`
}

// LinkID is the link's id, for keying a limit on it: the token finds it, and
// only the token.
func (p JoinPreview) LinkID() uuid.UUID { return p.linkID }

// PreviewJoinLink is what the page that opens a join link is told. A token
// that finds no link is not_found, the same for every way it can be wrong.
func PreviewJoinLink(ctx context.Context, q dbq.Querier, token string, now time.Time) (JoinPreview, error) {
	r, err := findJoinLink(ctx, q, token)
	if err != nil {
		return JoinPreview{}, err
	}
	p := JoinPreview{Course: JoinPreviewCourse{Code: r.Code, Section: r.Section, Title: r.Title}, ExpiresAt: r.ExpiresAt,
		AllowedEmailDomains: r.AllowedEmailDomains, linkID: r.ID}
	grant, refusal, err := joinStanding(ctx, q, linkOf(r), r.CourseStatus, now, false)
	if err != nil {
		return JoinPreview{}, err
	}
	if refusal != nil {
		p.Reason, _ = refusal.Details["reason"].(string)
		return p, nil
	}
	p.Joinable, p.Registration, p.makerActorID = true, true, grant.maker.ActorID
	return p, nil
}

// JoinRegistration is what someone gives to register through a join link.
type JoinRegistration struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

const maxDisplayNameLen = 200

// Check holds the fields to their rules, trimming the name and the email,
// and looks nothing up: a name of 1 to 200 characters with no control
// character, a plain email address, and a password within the rules for one
// (auth.CheckNewPassword).
func (r *JoinRegistration) Check() error {
	r.DisplayName, r.Email = strings.TrimSpace(r.DisplayName), strings.TrimSpace(r.Email)
	switch n := utf8.RuneCountInString(r.DisplayName); {
	case !utf8.ValidString(r.DisplayName) || n == 0 || n > maxDisplayNameLen ||
		strings.IndexFunc(r.DisplayName, unicode.IsControl) >= 0:
		return apperr.Invalid("display_name must be 1 to %d characters, none of them a control character", maxDisplayNameLen)
	case !plainEmail(r.Email):
		return apperr.Invalid("email must be an email address, such as yuki@example.edu")
	}
	return auth.CheckNewPassword(r.Password)
}

// plainEmail: an address and nothing else — no name before it, no angle
// brackets — that the database can hold.
func plainEmail(s string) bool {
	if len(s) > 254 || !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == "" && emailDomain(s) != ""
}

// JoinRegistrationRefusal is what registering through a link, as its preview
// found it, is refused for, found before anything is hashed or written: the
// link seats nobody now (its reason), the email is at no domain the link
// takes, or someone has it already, who is told to sign in and nothing else
// (auth.EmailTaken). The domain is asked before the email, so that a link
// for one domain tells nobody which addresses at another are registered.
// Nil means it may go ahead as far as can be told without the link's lock:
// the join itself asks everything again under it.
func JoinRegistrationRefusal(ctx context.Context, q dbq.Querier, p JoinPreview, email string) error {
	if !p.Joinable {
		standing := map[string]string{JoinRevoked: "the join link has been revoked", JoinExpired: "the join link has expired",
			JoinUsedUp: "the join link has been used as many times as it may be", JoinCourseArchived: "the course is archived",
			JoinCreatorLostAuthority: "whoever made the join link can no longer invite students to the course"}
		return joinRefused(p.Reason, "%s; ask for a new one", standing[p.Reason])
	}
	if !domainAllowed(p.AllowedEmailDomains, &email) {
		return domainRefused(p.AllowedEmailDomains)
	}
	switch _, err := q.GetActorByEmail(ctx, email); {
	case err == nil:
		return auth.EmailTaken()
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	return nil
}

// RegisteringPerson is the person a registration through a link makes, for
// auth.RegisterPerson: created by whoever made the link, whose authority lets
// them in.
func (p JoinPreview) RegisteringPerson(r JoinRegistration, passwordHash string) auth.NewPerson {
	return auth.NewPerson{DisplayName: r.DisplayName, Email: r.Email, PasswordHash: passwordHash, CreatedBy: p.makerActorID}
}
