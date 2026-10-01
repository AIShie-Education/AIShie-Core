package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/members"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Join links: whoever holds member_invite makes a link that seats a student
// at once, with their authority, for as long as they have it and for ten
// minutes at most; a person signed in joins through it, and someone with no
// account registers through it and joins in one transaction.

// joinLink makes a link to the course as maker.
func (b *built) joinLink(t *testing.T, maker uuid.UUID, args m) tools.JoinLinkCreateOut {
	t.Helper()
	args["course_id"] = b.course
	return testkit.Result[tools.JoinLinkCreateOut](t, b.do(t, maker, "course.join_link_create", args))
}

// join is what POST /v1/join/{token} does for a person signed in.
func (b *built) join(t *testing.T, actor uuid.UUID, token string) pipeline.Outcome {
	t.Helper()
	out, err := b.joinWith(actor, token, "join-"+uuid.NewString())
	if err != nil {
		t.Fatalf("course.join: %v", err)
	}
	return out
}

func (b *built) joinWith(actor uuid.UUID, token, key string) (pipeline.Outcome, error) {
	raw, _ := json.Marshal(m{"token": token})
	return b.P.InvokeUnlisted(b.T.Context(), pipeline.Caller{ActorID: actor}, tools.ToolCourseJoin, raw, key)
}

// preview is what GET /v1/join/{token} says, as the pipeline's clock has it.
func (b *built) preview(t *testing.T, token string) tools.JoinPreview {
	t.Helper()
	p, err := tools.PreviewJoinLink(t.Context(), dbq.New(b.Pool), token, b.P.Clock())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return p
}

// listed is one link as course.join_link_list shows it to Sato.
func (b *built) listed(t *testing.T, id uuid.UUID) tools.JoinLinkView {
	t.Helper()
	for _, l := range testkit.Result[tools.JoinLinkListOut](t, b.do(t, b.sato, "course.join_link_list", m{"course_id": b.course})).Links {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("link %v is not listed", id)
	return tools.JoinLinkView{}
}

// joined insists a join seated someone, and says where.
func joined(t *testing.T, out pipeline.Outcome) tools.CourseJoinOut {
	t.Helper()
	if out.Status != domain.StatusExecuted {
		t.Fatalf("course.join: %+v", out)
	}
	return testkit.Result[tools.CourseJoinOut](t, out)
}

// refused insists a join was attempted and refused, for reason.
func refused(t *testing.T, out pipeline.Outcome, status domain.ActionStatus, why string) {
	t.Helper()
	if out.Status != status || reason(out) != why {
		t.Fatalf("want %s (%s), got %+v", status, why, out)
	}
}

// register is what POST /v1/join/{token}/register does, but for the
// password, which is hashed there and not here.
func (b *built) register(t *testing.T, token, name, email string) (pipeline.Outcome, uuid.UUID, error) {
	t.Helper()
	return b.registerAs(t, token, tools.JoinRegistration{DisplayName: name, Email: email})
}

// registerAs is register with whatever the person gives: a login ID, an
// email, or both.
func (b *built) registerAs(t *testing.T, token string, reg tools.JoinRegistration) (pipeline.Outcome, uuid.UUID, error) {
	t.Helper()
	ctx, q := t.Context(), dbq.New(b.Pool)
	p, err := tools.PreviewJoinLink(ctx, q, token, b.P.Clock())
	if err != nil {
		return pipeline.Outcome{}, uuid.Nil, err
	}
	if err := tools.JoinRegistrationRefusal(ctx, q, p, reg); err != nil {
		return pipeline.Outcome{}, uuid.Nil, err
	}
	var made uuid.UUID
	raw, _ := json.Marshal(m{"token": token})
	out, err := b.P.InvokeAsNew(ctx, tools.ToolCourseJoin, raw, "register:"+p.LinkID().String(), pipeline.NewActor{
		Make: func(ctx context.Context, q *dbq.Queries, now time.Time) (uuid.UUID, error) {
			id, err := auth.RegisterPerson(ctx, q, p.RegisteringPerson(reg, "$argon2id$stand-in"), now)
			made = id
			return id, err
		},
	})
	return out, made, err
}

// at sets the pipeline's clock to a moment, until the test ends.
func (b *built) at(t *testing.T, when time.Time) {
	t.Helper()
	b.P.SetClock(func() time.Time { return when })
	t.Cleanup(func() { b.P.SetClock(time.Now) })
}

func apperrReason(err error) any {
	if e, ok := apperr.As(err); ok {
		return e.Details["reason"]
	}
	return nil
}

func sha(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestAJoinLinkIsMadeByWhoeverHoldsMemberInviteAndItsTokenIsShownOnce(t *testing.T) {
	b := build(t)
	made := time.Now().Add(time.Hour).Truncate(time.Second)
	b.at(t, made)
	args := m{"course_id": b.course}
	out := b.MustCall(b.sato, "course.join_link_create", args, "link-1")
	link := testkit.Result[tools.JoinLinkCreateOut](t, out)
	if !strings.HasPrefix(link.Token, "aisjoin_") || link.LinkID == uuid.Nil {
		t.Fatalf("made %+v", link)
	}
	// Ten minutes from when it is made, whoever makes it: nobody chooses.
	if !link.ExpiresAt.Equal(made.Add(tools.JoinLinkLifetime)) || tools.JoinLinkLifetime != 10*time.Minute {
		t.Fatalf("a link made at %v expires at %v", made, link.ExpiresAt)
	}
	var created, expires time.Time
	if err := b.Pool.QueryRow(t.Context(), `SELECT created_at, expires_at FROM course_join_link WHERE id = $1`, link.LinkID).
		Scan(&created, &expires); err != nil || !created.Equal(made) || expires.Sub(created) != 10*time.Minute {
		t.Fatalf("kept as made %v, expiring %v (%v)", created, expires, err)
	}
	if _, err := b.Call(b.sato, "course.join_link_create", m{"course_id": b.course, "expires_at": made.Add(24 * time.Hour)}, "longer"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("a link asked to live longer: %v", err)
	}

	// Only the hash is kept, beside the prefix that finds it; the token is in
	// no action, no result and no event.
	var prefix, hash string
	if err := b.Pool.QueryRow(t.Context(), `SELECT token_prefix, secret_hash FROM course_join_link WHERE id = $1`, link.LinkID).
		Scan(&prefix, &hash); err != nil {
		t.Fatal(err)
	}
	if hash != sha(link.Token) || !strings.HasPrefix(link.Token, "aisjoin_"+prefix+"_") {
		t.Fatalf("kept %s and %s for %s", prefix, hash, link.Token)
	}
	secret := link.Token[len("aisjoin_")+len(prefix)+1:]
	if n := b.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Fatalf("the token's secret is in %d actions", n)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE payload::text LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Fatalf("the token's secret is in %d events", n)
	}
	// A replay says what was made, and not the token: it was shown once.
	again := b.MustCall(b.sato, "course.join_link_create", args, "link-1")
	if !again.Replayed || strings.Contains(string(again.Result), secret) ||
		testkit.Result[tools.JoinLinkCreateOut](t, again).LinkID != link.LinkID {
		t.Fatalf("replayed %+v", again)
	}

	// Nobody without member_invite makes one: not a student, and not someone
	// who manages the members but was not given it.
	refused(t, b.MustCall(b.yuki, "course.join_link_create", m{"course_id": b.course}, "link-yuki"), domain.StatusDenied, "permission_denied")
	tanaka := b.person(t, "Tanaka", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor", "perms": m{"member_invite": "denied"}})
	refused(t, b.MustCall(tanaka, "course.join_link_create", m{"course_id": b.course}, "link-tanaka"), domain.StatusDenied, "permission_denied")
	refused(t, b.MustCall(tanaka, "course.join_link_list", m{"course_id": b.course}, ""), domain.StatusDenied, "permission_denied")

	// Its limits.
	for name, bad := range map[string]m{
		"no uses":       {"max_uses": 0},
		"too many uses": {"max_uses": tools.MaxJoinLinkUses + 1},
		"no domains":    {"allowed_email_domains": []string{}},
		"not a domain":  {"allowed_email_domains": []string{"example"}},
		"an address":    {"allowed_email_domains": []string{"yuki@example.edu"}},
		"too many names": {"allowed_email_domains": func() []string {
			var d []string
			for i := range tools.MaxJoinLinkDomains + 1 {
				d = append(d, "d"+string(rune('a'+i))+".example.edu")
			}
			return d
		}()},
	} {
		bad["course_id"] = b.course
		out := b.MustCall(b.sato, "course.join_link_create", bad, "bad-"+name)
		if out.Status != domain.StatusFailed || out.Error.Code != apperr.InvalidArgument {
			t.Errorf("%s: %+v", name, out)
		}
	}
	// What is kept of a list of domains: lower-case, each once, no @.
	listed := b.joinLink(t, b.sato, m{"allowed_email_domains": []string{"@Example.EDU", "connect.example.edu", "example.edu"},
		"max_uses": 5})
	if strings.Join(listed.AllowedEmailDomains, ",") != "connect.example.edu,example.edu" || *listed.MaxUses != 5 {
		t.Fatalf("made %+v", listed)
	}

	// An archived course takes no link, as it takes no write.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	refused(t, b.MustCall(b.sato, "course.join_link_create", m{"course_id": b.course}, "link-archived"), domain.StatusDenied, "course_archived")
}

// Every link works for ten minutes from when it is made, and then never
// again, by the pipeline's clock.
func TestAJoinLinkLivesTenMinutes(t *testing.T) {
	b := build(t)
	made := time.Now().Truncate(time.Second)
	b.at(t, made)
	link := b.joinLink(t, b.sato, m{})

	b.at(t, made.Add(tools.JoinLinkLifetime-time.Second))
	if p := b.preview(t, link.Token); !p.Joinable || !p.ExpiresAt.Equal(made.Add(tools.JoinLinkLifetime)) {
		t.Fatalf("a second before its end: %+v", p)
	}
	joined(t, b.join(t, b.person(t, "Mei", ""), link.Token))
	if l := b.listed(t, link.LinkID); l.Status != "live" || !l.Joinable {
		t.Fatalf("listed a second before its end: %+v", l)
	}

	b.at(t, made.Add(tools.JoinLinkLifetime))
	if p := b.preview(t, link.Token); p.Joinable || p.Registration || p.Reason != tools.JoinExpired {
		t.Fatalf("at its end: %+v", p)
	}
	refused(t, b.join(t, b.person(t, "Aoi", ""), link.Token), domain.StatusFailed, tools.JoinExpired)
	if _, _, err := b.register(t, link.Token, "Ren", "ren@example.edu"); apperrReason(err) != tools.JoinExpired {
		t.Fatalf("registering at its end: %v", err)
	}
	if l := b.listed(t, link.LinkID); l.Status != "expired" || l.Joinable || l.Reason != tools.JoinExpired || l.Uses != 1 {
		t.Fatalf("listed at its end: %+v", l)
	}
	// Someone seated through it keeps the seat; the link is over, not them.
	if n := b.Count(`SELECT count(*) FROM course_member WHERE join_link_id = $1 AND status = 'active'`, link.LinkID); n != 1 {
		t.Fatalf("%d seats through the expired link", n)
	}
}

// The seat a link hands out is its maker's to give: within what they hold,
// and never by proposal, which would show the token to whoever approved it.
// An agent follows its seat's member_invite like anyone; a delegate holds it
// only when named, and never above its principal.
func TestAJoinLinkIsMadeOnlyForASeatItsMakerCouldGive(t *testing.T) {
	b := build(t)
	// A TA who invites only with approval.
	ta := b.person(t, "Tanaka", "")
	taM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": ta, "preset": "ta", "perms": m{"member_invite": "confirm_required"}})).MemberID
	refused(t, b.MustCall(ta, "course.join_link_create", m{"course_id": b.course}, "ta-proposes"), domain.StatusFailed, "not_by_proposal")
	if n := b.Count(`SELECT count(*) FROM action WHERE status = 'proposed'`); n != 0 {
		t.Fatalf("%d proposals queued", n)
	}

	// Inviting without approval, but holding less than a student's seat
	// gives: a TA hands in no work.
	b.Exec(`UPDATE course_member SET perm_member_invite = 'autonomous' WHERE id = $1`, taM)
	out := b.MustCall(ta, "course.join_link_create", m{"course_id": b.course}, "ta-short")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "submission_write" {
		t.Fatalf("an inviter holding less than a student's seat: %+v", out)
	}
	// Holding it, but reaching only a list of students: no new student is
	// on it.
	b.Exec(`UPDATE course_member SET perm_submission_write = 'autonomous', student_scope = 'listed' WHERE id = $1`, taM)
	out = b.MustCall(ta, "course.join_link_create", m{"course_id": b.course}, "ta-listed")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || !strings.Contains(out.Error.Message, "list") {
		t.Fatalf("a listed inviter's link: %+v", out)
	}
	b.Exec(`UPDATE course_member SET student_scope = 'all' WHERE id = $1`, taM)
	b.joinLink(t, ta, m{})

	// An agent nobody owns follows its seat: given member_invite, it makes
	// links, as a roster agent might.
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "enrolment bot"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "instructor"})
	b.joinLink(t, bot, m{})

	// A person's own agent holds none unless it is named for it.
	plain := b.agent(t, b.sato, "Sato's assistant")
	b.delegate(t, b.sato, plain, m{"preset": "instructor"})
	refused(t, b.MustCall(plain, "course.join_link_create", m{"course_id": b.course}, "plain-link"), domain.StatusDenied, "permission_denied")

	// Named, it makes a link, with its principal's authority as well as its
	// own: the seat ends when its principal's does, and the link stops
	// working once its principal no longer holds member_invite.
	mori := b.mori(t)
	ends := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	b.do(t, mori, "member.rescope", m{"course_id": b.course, "member_id": b.satoM, "expires_at": ends})
	helper := b.agent(t, b.sato, "Sato's enrolment helper")
	b.delegate(t, b.sato, helper, m{"preset": "instructor", "perms": m{"member_invite": "autonomous"}})
	link := b.joinLink(t, helper, m{})
	seat := joined(t, b.join(t, b.person(t, "Hana", ""), link.Token)).MemberID
	var addedBy uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT added_by_actor_id FROM course_member WHERE id = $1`, seat).Scan(&addedBy); err != nil || addedBy != helper {
		t.Fatalf("a seat through the helper's link added by %v (%v)", addedBy, err)
	}
	if v := b.memberView(t, seat); v.ExpiresAt == nil || !v.ExpiresAt.Equal(ends) {
		t.Fatalf("a seat through a delegate's link ends %v; its principal's ends %v", v.ExpiresAt, ends)
	}
	b.do(t, mori, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": m{"member_invite": "confirm_required"}})
	if p := b.preview(t, link.Token); p.Joinable || p.Reason != tools.JoinCreatorLostAuthority {
		t.Fatalf("a delegate's link once its principal invites only with approval: %+v", p)
	}
	refused(t, b.join(t, b.person(t, "Ren", ""), link.Token), domain.StatusFailed, tools.JoinCreatorLostAuthority)
	refused(t, b.MustCall(helper, "course.join_link_create", m{"course_id": b.course}, "helper-capped"), domain.StatusFailed, "not_by_proposal")

	// A student's agent is given none: she holds none.
	yukis := b.agent(t, b.yuki, "Yuki's helper")
	out = b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": yukis, "perms": m{"member_invite": "autonomous"}}, "yuki-names")
	if out.Status != domain.StatusFailed || out.Error.Details["permission"] != "member_invite" {
		t.Fatalf("a student naming member_invite for her agent: %+v", out)
	}
}

func TestAPersonJoinsThroughALinkOnceAndAsAStudent(t *testing.T) {
	b := build(t)
	link := b.joinLink(t, b.sato, m{})
	mei := b.person(t, "Mei", "mei@example.edu")

	first := joined(t, b.join(t, mei, link.Token))
	if first.AlreadyMember || first.CourseID != b.course || first.JoinLinkID != link.LinkID || first.Status != domain.MemberActive {
		t.Fatalf("joined %+v", first)
	}
	// A student's seat, the preset's, naming the link, added by its maker;
	// listed for herself.
	var role, preset string
	var addedBy uuid.UUID
	var viaLink *uuid.UUID
	var expires *time.Time
	if err := b.Pool.QueryRow(t.Context(), `SELECT m.role, p.name, m.added_by_actor_id, m.join_link_id, m.expires_at
	        FROM course_member m JOIN permission_preset p ON p.id = m.preset_id WHERE m.id = $1`, first.MemberID).
		Scan(&role, &preset, &addedBy, &viaLink, &expires); err != nil {
		t.Fatal(err)
	}
	if role != "student" || preset != "student" || addedBy != b.sato || viaLink == nil || *viaLink != link.LinkID || expires != nil {
		t.Fatalf("seat: %s %s added by %v through %v, ends %v", role, preset, addedBy, viaLink, expires)
	}
	view := b.memberView(t, first.MemberID)
	if len(view.ListedStudents) != 1 || view.ListedStudents[0] != first.MemberID || view.JoinLinkID == nil || *view.JoinLinkID != link.LinkID {
		t.Fatalf("member.get: %+v", view)
	}
	if got := b.membership(t, mei); got.MemberID != first.MemberID {
		t.Fatalf("me.memberships: %+v", got)
	}
	// Who joined through which link: member.list says, and the feed.
	through := testkit.Result[tools.MemberListOut](t, b.do(t, b.sato, "member.list", m{"course_id": b.course, "join_link_id": link.LinkID}))
	if len(through.Members) != 1 || through.Members[0].ID != first.MemberID {
		t.Fatalf("member.list through the link: %+v", through.Members)
	}
	var payload map[string]any
	var actionID uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload, action_id FROM event WHERE type = $1 AND subject_id = $2`,
		members.EventAdded, first.MemberID).Scan(&payload, &actionID); err != nil {
		t.Fatal(err)
	}
	if payload["via"] != "join_link" || payload["join_link_id"] != link.LinkID.String() || payload["role"] != "student" {
		t.Fatalf("member.added: %v", payload)
	}
	var who uuid.UUID
	var typ, target string
	var body []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT actor_id, action_type, target_type, payload FROM action WHERE id = $1`, actionID).
		Scan(&who, &typ, &target, &body); err != nil {
		t.Fatal(err)
	}
	if who != mei || typ != tools.ToolCourseJoin || target != "course_join_link" || strings.Contains(string(body), "aisjoin") {
		t.Fatalf("the action: %v %s %s %s", who, typ, target, body)
	}
	seen := false
	for _, e := range feed(t, b, b.sato) {
		seen = seen || (e.Type == members.EventAdded && e.SubjectID != nil && *e.SubjectID == first.MemberID)
	}
	if !seen {
		t.Fatal("the instructor's feed does not say who joined")
	}

	// Again: the seat she has, and no use counted.
	again := joined(t, b.join(t, mei, link.Token))
	if !again.AlreadyMember || again.MemberID != first.MemberID {
		t.Fatalf("joined again: %+v", again)
	}
	// Someone seated otherwise is told the same, and counts nothing either.
	yuki := joined(t, b.join(t, b.yuki, link.Token))
	if !yuki.AlreadyMember || yuki.MemberID != b.yukiM {
		t.Fatalf("Yuki joined: %+v", yuki)
	}
	if n := b.Count(`SELECT uses FROM course_join_link WHERE id = $1`, link.LinkID); n != 1 {
		t.Fatalf("%d uses counted, want 1", n)
	}
	// A retry with its key is the same call.
	key := "retry-" + uuid.NewString()
	ken := b.person(t, "Ken2", "")
	one, _ := b.joinWith(ken, link.Token, key)
	two, _ := b.joinWith(ken, link.Token, key)
	if joined(t, one).MemberID != testkit.Result[tools.CourseJoinOut](t, two).MemberID || !two.Replayed {
		t.Fatalf("retried: %+v then %+v", one, two)
	}

	// A paused seat is hers as it is: a link resumes nothing.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": first.MemberID})
	paused := joined(t, b.join(t, mei, link.Token))
	if !paused.AlreadyMember || paused.Status != domain.MemberPaused || paused.MemberID != first.MemberID {
		t.Fatalf("joined while paused: %+v", paused)
	}
	// Removed, she may join afresh, as seating her again would: a new seat.
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": first.MemberID})
	fresh := joined(t, b.join(t, mei, link.Token))
	if fresh.AlreadyMember || fresh.MemberID == first.MemberID {
		t.Fatalf("joined after removal: %+v", fresh)
	}
	// A seat past its expiry is as good as removed.
	b.Exec(`UPDATE course_member SET expires_at = now() - interval '1 minute' WHERE id = $1`, fresh.MemberID)
	renewed := joined(t, b.join(t, mei, link.Token))
	if renewed.AlreadyMember || renewed.MemberID == fresh.MemberID {
		t.Fatalf("joined after expiry: %+v", renewed)
	}
}

func TestAJoinLinkSeatsPeopleWhoAreActiveAndWhoseEmailItTakes(t *testing.T) {
	b := build(t)
	link := b.joinLink(t, b.sato, m{"allowed_email_domains": []string{"example.edu"}})

	// An agent is seated by whoever manages the course, not by a link.
	refused(t, b.join(t, b.grader, link.Token), domain.StatusFailed, tools.JoinPeopleOnly)
	mine := b.agent(t, b.yuki, "Yuki's assistant")
	refused(t, b.join(t, mine, link.Token), domain.StatusFailed, tools.JoinPeopleOnly)

	// Someone suspended is refused as every call of theirs is.
	shun := b.person(t, "Shun", "shun@example.edu")
	b.do(t, b.admin, "actor.suspend", m{"actor_id": shun})
	refused(t, b.join(t, shun, link.Token), domain.StatusDenied, "actor_not_active")

	// The domain, exactly, in any case; not another, not a subdomain, and
	// not someone with no email at all.
	for _, email := range []string{"aoi@other.edu", "aoi@mail.example.edu", "aoi@example.edu.evil.com", ""} {
		who := b.person(t, "Aoi "+email, email)
		out := b.join(t, who, link.Token)
		refused(t, out, domain.StatusFailed, tools.JoinEmailDomainAllowed)
		if got := out.Error.Details["allowed_email_domains"]; got == nil {
			t.Fatalf("the refusal does not say which domains: %+v", out.Error)
		}
	}
	joined(t, b.join(t, b.person(t, "Rin", "Rin@EXAMPLE.edu"), link.Token))
	if n := b.Count(`SELECT uses FROM course_join_link WHERE id = $1`, link.LinkID); n != 1 {
		t.Fatalf("%d uses counted for one join", n)
	}
	// Refusals count no use.
	if l := b.listed(t, link.LinkID); l.Uses != 1 {
		t.Fatalf("listed: %+v", l)
	}
}

// A token that finds no link is refused before anything is attempted, and
// the same way however it is wrong: nothing tells a guesser which part was.
func TestUnknownJoinTokensAreOneNotFound(t *testing.T) {
	b := build(t)
	link := b.joinLink(t, b.sato, m{})
	prefix := strings.SplitN(link.Token, "_", 3)[1]
	var messages []string
	for _, token := range []string{
		"", "not-a-token", "aisjoin_", link.Token + "x", flipLast(link.Token),
		"aisjoin_" + prefix + "_" + strings.Repeat("A", 43), // its prefix, another secret
		"aisjoin_abcdefghijkl_" + strings.Repeat("A", 43),   // a prefix nobody has
		"aisinv_" + prefix + "_" + strings.Repeat("A", 43),  // another scheme
		"aisjoin_" + strings.ToUpper(prefix) + "_" + link.Token[len("aisjoin_")+13:],
		strings.Repeat("a", 5000),
	} {
		before := b.Count(`SELECT count(*) FROM action`)
		_, err := b.joinWith(b.person(t, "Guesser", ""), token, "guess-"+uuid.NewString())
		if !apperr.Is(err, apperr.NotFound) {
			t.Fatalf("%q: %v", token, err)
		}
		messages = append(messages, err.Error())
		if _, err := tools.PreviewJoinLink(t.Context(), dbq.New(b.Pool), token, time.Now()); !apperr.Is(err, apperr.NotFound) || err.Error() != messages[0] {
			t.Fatalf("preview of %q: %v", token, err)
		}
		if after := b.Count(`SELECT count(*) FROM action`); after != before+1 { // the guesser's registration, and no join
			t.Fatalf("%q recorded %d actions", token, after-before-1)
		}
	}
	for _, msg := range messages {
		if msg != messages[0] {
			t.Fatalf("two refusals differ: %q and %q", messages[0], msg)
		}
	}
}

// What a link says of itself, to whoever opens it and to whoever may list
// the course's links: revoked, expired, used up, its course archived, or its
// maker no longer able to make it; and joining is refused for the same
// reason.
func TestAJoinLinkSaysWhyItSeatsNobody(t *testing.T) {
	b := build(t)
	live := b.joinLink(t, b.sato, m{"allowed_email_domains": []string{"example.edu"}})
	p := b.preview(t, live.Token)
	if !p.Joinable || !p.Registration || p.Reason != "" || p.Course.Code != "CS101" || p.Course.Section != "A" ||
		p.Course.Title != "Introduction to Computing" || len(p.AllowedEmailDomains) != 1 || !p.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("a live link: %+v", p)
	}
	// What it shows, and nothing else.
	raw, _ := json.Marshal(p)
	var shown map[string]any
	_ = json.Unmarshal(raw, &shown)
	keys := []string{}
	for k := range shown {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "allowed_email_domains,course,email_required,expires_at,joinable,registration" ||
		len(shown["course"].(map[string]any)) != 3 || shown["email_required"] != true {
		t.Fatalf("the preview shows %s", raw)
	}
	if l := b.listed(t, live.LinkID); l.Status != "live" || !l.Joinable || l.CreatedByMemberID != b.satoM || l.CreatedByName != "Sato" ||
		!l.ExpiresAt.Equal(live.ExpiresAt) || len(l.AllowedEmailDomains) != 1 || l.MaxUses != nil {
		t.Fatalf("listed: %+v", l)
	}

	// Used up.
	once := b.joinLink(t, b.sato, m{"max_uses": 1})
	joined(t, b.join(t, b.person(t, "Mei", ""), once.Token))
	if p := b.preview(t, once.Token); p.Joinable || p.Registration || p.Reason != tools.JoinUsedUp {
		t.Fatalf("used up: %+v", p)
	}
	refused(t, b.join(t, b.person(t, "Aoi", ""), once.Token), domain.StatusFailed, tools.JoinUsedUp)
	if l := b.listed(t, once.LinkID); l.Status != "used_up" || l.Joinable || l.Reason != tools.JoinUsedUp || l.Uses != 1 || *l.MaxUses != 1 {
		t.Fatalf("listed used up: %+v", l)
	}

	// Revoked, by anyone who holds member_invite; once.
	gone := b.joinLink(t, b.sato, m{})
	refused(t, b.MustCall(b.yuki, "course.join_link_revoke", m{"course_id": b.course, "link_id": gone.LinkID}, "yuki-revokes"),
		domain.StatusDenied, "permission_denied")
	mori := b.mori(t)
	b.do(t, mori, "course.join_link_revoke", m{"course_id": b.course, "link_id": gone.LinkID})
	out := b.MustCall(b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": gone.LinkID}, "revoke-twice")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict {
		t.Fatalf("revoked twice: %+v", out)
	}
	if _, err := b.Call(b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": uuid.New()}, "revoke-nothing"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("revoking no link: %v", err)
	}
	if p := b.preview(t, gone.Token); p.Joinable || p.Reason != tools.JoinRevoked {
		t.Fatalf("revoked: %+v", p)
	}
	refused(t, b.join(t, b.person(t, "Aoi3", ""), gone.Token), domain.StatusFailed, tools.JoinRevoked)
	if l := b.listed(t, gone.LinkID); l.Status != "revoked" || l.RevokedByMemberID == nil || l.RevokedByName == nil || *l.RevokedByName != "Mori" {
		t.Fatalf("listed revoked: %+v", l)
	}
	// Whoever joined through a revoked link keeps the seat.
	kept := b.joinLink(t, b.sato, m{})
	seat := joined(t, b.join(t, b.person(t, "Hana", ""), kept.Token)).MemberID
	b.do(t, b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": kept.LinkID})
	if v := b.memberView(t, seat); v.Status != domain.MemberActive {
		t.Fatalf("a seat taken through a revoked link: %+v", v)
	}

	// The list: newest first, and never a token, nor its hash.
	listing := b.do(t, b.sato, "course.join_link_list", m{"course_id": b.course})
	if s := string(listing.Result); strings.Contains(s, "aisjoin_") || strings.Contains(s, "sha256:") ||
		strings.Contains(s, strings.SplitN(live.Token, "_", 3)[1]) {
		t.Fatalf("the list shows a token: %s", s)
	}
	links := testkit.Result[tools.JoinLinkListOut](t, listing).Links
	if len(links) != 4 || links[0].ID != kept.LinkID || links[3].ID != live.LinkID {
		t.Fatalf("the list, newest first: %+v", links)
	}
	page := testkit.Result[tools.JoinLinkListOut](t, b.do(t, b.sato, "course.join_link_list", m{"course_id": b.course, "limit": 2}))
	if len(page.Links) != 2 || page.Next == nil || *page.Next != links[1].ID {
		t.Fatalf("the first page: %+v", page)
	}
	rest := testkit.Result[tools.JoinLinkListOut](t, b.do(t, b.sato, "course.join_link_list", m{"course_id": b.course, "after": page.Next}))
	if len(rest.Links) != 2 || rest.Links[0].ID != links[2].ID {
		t.Fatalf("the next page: %+v", rest)
	}
	refused(t, b.MustCall(b.yuki, "course.join_link_list", m{"course_id": b.course}, ""), domain.StatusDenied, "permission_denied")

	// Archived: every link of the course seats nobody, and says why.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if p := b.preview(t, live.Token); p.Joinable || p.Reason != tools.JoinCourseArchived {
		t.Fatalf("archived: %+v", p)
	}
	refused(t, b.join(t, b.person(t, "Aoi4", "aoi4@example.edu"), live.Token), domain.StatusDenied, "course_archived")
	if l := b.listed(t, live.LinkID); l.Status != "live" || l.Joinable || l.Reason != tools.JoinCourseArchived {
		t.Fatalf("listed in an archived course: %+v", l)
	}
}

// A link lends its maker's authority, and only while they have it: asked
// again at every join, as it was when the link was made.
func TestAJoinLinkStopsWhenItsMakerCouldNoLongerSeatAStudent(t *testing.T) {
	b := build(t)
	// Mori, a second instructor, whose seat is changed under the link he makes.
	mori := b.person(t, "Mori", "")
	moriM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.admin, "course.seat_instructor", m{"course_id": b.course, "actor_id": mori})).MemberID
	link := b.joinLink(t, mori, m{})
	lost := func(why string) {
		t.Helper()
		if p := b.preview(t, link.Token); p.Joinable || p.Registration || p.Reason != tools.JoinCreatorLostAuthority {
			t.Fatalf("%s: preview %+v", why, p)
		}
		out := b.join(t, b.person(t, "Joiner", ""), link.Token)
		refused(t, out, domain.StatusFailed, tools.JoinCreatorLostAuthority)
		if !strings.Contains(out.Error.Message, "new link") {
			t.Fatalf("%s: the refusal does not say what to do: %s", why, out.Error.Message)
		}
		if l := b.listed(t, link.LinkID); l.Joinable || l.Reason != tools.JoinCreatorLostAuthority {
			t.Fatalf("%s: listed %+v", why, l)
		}
	}
	works := func(why string) {
		t.Helper()
		joined(t, b.join(t, b.person(t, "Joiner", ""), link.Token))
	}
	works("as made")

	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": moriM})
	lost("paused")
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": moriM})
	works("resumed")

	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"member_invite": "denied"}})
	lost("without member_invite")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"member_invite": "confirm_required"}})
	lost("with member_invite only by approval")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"member_invite": "pending_review"}})
	works("with member_invite under review")
	// member_manage is not what it asks.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"member_manage": "denied"}})
	works("without member_manage")

	// Holding less than a student's seat gives.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"submission_write": "denied"}})
	lost("holding less than the preset")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": moriM, "perms": m{"submission_write": "autonomous"}})
	works("holding it again")

	// Reaching only a list of students, which no new student is on.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": moriM, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	lost("listed")
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": moriM, "student_scope": "all"})
	works("the class again")

	// The seat a link gives ends when its maker's does.
	ends := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": moriM, "expires_at": ends})
	seat := joined(t, b.join(t, b.person(t, "Joiner", ""), link.Token)).MemberID
	if v := b.memberView(t, seat); v.ExpiresAt == nil || !v.ExpiresAt.Equal(ends) {
		t.Fatalf("a seat through the link of a maker whose seat ends %v ends %v", ends, v.ExpiresAt)
	}

	b.do(t, b.admin, "actor.suspend", m{"actor_id": mori})
	lost("suspended")
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": mori})
	works("reactivated")

	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": moriM})
	lost("removed")
	// Seated again, he is a new seat: the link was the old one's.
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": b.course, "actor_id": mori})
	lost("seated again")
}

// Joins through one link take their turns: however many come at once, no
// more are seated than it allows, and the count says so.
func TestConcurrentJoinsNeverOvershootALinksUses(t *testing.T) {
	b := build(t)
	const limit, joiners = 3, 12
	link := b.joinLink(t, b.sato, m{"max_uses": limit})
	people := make([]uuid.UUID, joiners)
	for i := range people {
		people[i] = b.person(t, "Joiner", "")
	}
	outs := make([]pipeline.Outcome, joiners)
	errs := make([]error, joiners)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, who := range people {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outs[i], errs[i] = b.joinWith(who, link.Token, "rush-"+uuid.NewString())
		}()
	}
	close(start)
	wg.Wait()
	seated := 0
	for i, out := range outs {
		switch {
		case errs[i] != nil:
			t.Fatalf("join %d: %v", i, errs[i])
		case out.Status == domain.StatusExecuted:
			seated++
		case reason(out) != tools.JoinUsedUp:
			t.Fatalf("join %d refused otherwise than used up: %+v", i, out)
		}
	}
	if seated != limit {
		t.Fatalf("%d seated through a link for %d", seated, limit)
	}
	if n := b.Count(`SELECT uses FROM course_join_link WHERE id = $1`, link.LinkID); n != limit {
		t.Fatalf("%d uses counted", n)
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE join_link_id = $1`, link.LinkID); n != limit {
		t.Fatalf("%d seats name the link", n)
	}
}

// Someone with no account registers through a link: the person, their seat
// and the action are made together, and a join that is refused leaves no
// account behind.
func TestRegisteringThroughALinkMakesAPersonWhoseEmailNobodyHasChecked(t *testing.T) {
	b := build(t)
	link := b.joinLink(t, b.sato, m{"allowed_email_domains": []string{"example.edu"}})

	out, aoi, err := b.register(t, link.Token, "Aoi", "aoi@example.edu")
	if err != nil {
		t.Fatal(err)
	}
	seat := joined(t, out)
	var kind string
	var verified bool
	var createdBy uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT kind, email_verified, created_by_actor_id FROM actor WHERE id = $1`, aoi).
		Scan(&kind, &verified, &createdBy); err != nil {
		t.Fatal(err)
	}
	if kind != "human" || verified || createdBy != b.sato {
		t.Fatalf("registered: %s, email verified %v, created by %v", kind, verified, createdBy)
	}
	if n := b.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL`, aoi); n != 1 {
		t.Fatalf("%d passwords", n)
	}
	if got := b.memberView(t, seat.MemberID); got.ActorID != aoi || got.Role != "student" || got.JoinLinkID == nil || *got.JoinLinkID != link.LinkID {
		t.Fatalf("seated: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND action_type = $2 AND status = 'executed'`, aoi, tools.ToolCourseJoin); n != 1 {
		t.Fatalf("%d joins recorded as hers", n)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = $1 AND subject_id = $2 AND payload->>'via' = 'join_link'`, members.EventAdded, seat.MemberID); n != 1 {
		t.Fatalf("%d member.added events through the link", n)
	}
	// What an administrator and she are shown; an email an administrator
	// sets is one they vouch for.
	if v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": aoi})); v.EmailVerified {
		t.Fatalf("actor.get: %+v", v)
	}
	if me := testkit.Result[tools.MeOut](t, b.do(t, aoi, "me.get", m{})); me.EmailVerified == nil || *me.EmailVerified {
		t.Fatalf("me.get: %+v", me)
	}
	if me := testkit.Result[tools.MeOut](t, b.do(t, b.sato, "me.get", m{})); me.EmailVerified != nil {
		t.Fatalf("me.get with no email: %+v", me)
	}
	if v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": b.person(t, "Vouched", "v@example.edu")})); !v.EmailVerified {
		t.Fatalf("an email an administrator gave: %+v", v)
	}
	b.do(t, b.admin, "actor.update", m{"actor_id": aoi, "display_name": "Aoi Tanaka"})
	if v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": aoi})); v.EmailVerified {
		t.Fatalf("a new name vouched for the email: %+v", v)
	}
	b.do(t, b.admin, "actor.update", m{"actor_id": aoi, "email": "aoi@example.edu"})
	if v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": aoi})); !v.EmailVerified {
		t.Fatalf("an administrator set the email: %+v", v)
	}

	// Registered already, in any case: refused, and told to sign in.
	_, _, err = b.register(t, link.Token, "Impostor", "AOI@example.edu")
	if apperrReason(err) != "email_taken" || !strings.Contains(err.Error(), "sign in") {
		t.Fatalf("an email registered already: %v", err)
	}
	// At a domain the link does not take: refused before the email is
	// looked at, so that it tells nobody which are registered.
	b.person(t, "Somebody", "someone@other.edu")
	if _, _, err := b.register(t, link.Token, "Other", "someone@other.edu"); apperrReason(err) != tools.JoinEmailDomainAllowed {
		t.Fatalf("another domain: %v", err)
	}

	// Refused under the link's lock — revoked after the checks before it —
	// nothing is left: no person, no password, no action.
	revoked := b.joinLink(t, b.sato, m{})
	p := b.preview(t, revoked.Token)
	if !p.Joinable {
		t.Fatalf("preview: %+v", p)
	}
	b.do(t, b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": revoked.LinkID})
	raw, _ := json.Marshal(m{"token": revoked.Token})
	actions := b.Count(`SELECT count(*) FROM action`)
	_, err = b.P.InvokeAsNew(t.Context(), tools.ToolCourseJoin, raw, "register:late", pipeline.NewActor{
		Make: func(ctx context.Context, q *dbq.Queries, now time.Time) (uuid.UUID, error) {
			return auth.RegisterPerson(ctx, q, p.RegisteringPerson(tools.JoinRegistration{DisplayName: "Late", Email: "late@example.edu"},
				"$argon2id$stand-in"), now)
		},
	})
	if apperrReason(err) != tools.JoinRevoked {
		t.Fatalf("registering through a link revoked meanwhile: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM actor WHERE email = 'late@example.edu'`); n != 0 {
		t.Fatal("a refused registration left its person behind")
	}
	if n := b.Count(`SELECT count(*) FROM action`); n != actions {
		t.Fatalf("a refused registration recorded %d actions", n-actions)
	}
	// Nor through a link that is no longer one at all.
	if _, _, err := b.register(t, "aisjoin_abcdefghijkl_"+strings.Repeat("A", 43), "Nobody", "nobody@example.edu"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("an unknown link: %v", err)
	}
}

// What a person gives to register is held to its rules before anything is
// looked up.
func TestARegistrationsFieldsAreChecked(t *testing.T) {
	for name, r := range map[string]tools.JoinRegistration{
		"no name":           {DisplayName: "  ", Email: "a@example.edu", Password: "long enough password"},
		"a control":         {DisplayName: "A\x07", Email: "a@example.edu", Password: "long enough password"},
		"a long name":       {DisplayName: strings.Repeat("a", 201), Email: "a@example.edu", Password: "long enough password"},
		"no email":          {DisplayName: "A", Email: "", Password: "long enough password"},
		"not an email":      {DisplayName: "A", Email: "a.example.edu", Password: "long enough password"},
		"with a name":       {DisplayName: "A", Email: "Aoi <a@example.edu>", Password: "long enough password"},
		"a short password":  {DisplayName: "A", Email: "a@example.edu", Password: "short"},
		"too long password": {DisplayName: "A", Email: "a@example.edu", Password: strings.Repeat("p", auth.MaxPasswordLen+1)},
	} {
		if err := r.Check(); !apperr.Is(err, apperr.InvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := tools.JoinRegistration{DisplayName: "  Aoi ", Email: " aoi@example.edu ", Password: "long enough password"}
	if err := r.Check(); err != nil || r.DisplayName != "Aoi" || r.Email != "aoi@example.edu" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Whoever may list the course's links, or manages its members, is told when
// a link is made or revoked; students are not. Who joined is member.added,
// for the roster's readers.
func TestNewsOfJoinLinksIsForWhoeverInvitesOrManagesMembers(t *testing.T) {
	b := build(t)
	// Rin hands out links and manages no member.
	rin := b.person(t, "Rin", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": rin, "preset": "observer", "perms": m{"member_invite": "autonomous"}})
	link := b.joinLink(t, b.sato, m{})
	joined(t, b.join(t, b.person(t, "Mei", ""), link.Token))
	b.do(t, b.sato, "course.join_link_revoke", m{"course_id": b.course, "link_id": link.LinkID})
	seen := func(who uuid.UUID) map[string]int {
		n := map[string]int{}
		for _, e := range feed(t, b, who) {
			n[e.Type]++
		}
		return n
	}
	sato, yuki, rins := seen(b.sato), seen(b.yuki), seen(rin)
	if sato[tools.EventJoinLinkCreated] != 1 || sato[tools.EventJoinLinkRevoked] != 1 || sato[members.EventAdded] == 0 {
		t.Fatalf("Sato's feed: %v", sato)
	}
	if rins[tools.EventJoinLinkCreated] != 1 || rins[tools.EventJoinLinkRevoked] != 1 {
		t.Fatalf("Rin's feed: %v", rins)
	}
	if yuki[tools.EventJoinLinkCreated] != 0 || yuki[tools.EventJoinLinkRevoked] != 0 {
		t.Fatalf("Yuki's feed: %v", yuki)
	}
	known := map[string]bool{}
	for _, k := range tools.KnownEventTypes() {
		known[k] = true
	}
	if !known[tools.EventJoinLinkCreated] || !known[tools.EventJoinLinkRevoked] {
		t.Fatal("a join link's events have no visibility rule")
	}
}

// flipLast is a token with its last character changed.
func flipLast(s string) string {
	last := "A"
	if strings.HasSuffix(s, "A") {
		last = "B"
	}
	return s[:len(s)-1] + last
}
