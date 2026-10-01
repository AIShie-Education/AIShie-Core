package tools_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// A login ID is a person's sign-in name beside their email: the student or
// staff number a school gives them. An administrator gives and corrects it;
// a person registering through a join link gives their own, recorded as
// unchecked; nobody changes their own. Whoever sees a person's seat sees it,
// whoever seats people finds them by it, and an administrator sees it with
// the rest of an account.

func (b *built) actorView(t *testing.T, actor uuid.UUID) tools.ActorView {
	t.Helper()
	return testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": actor}))
}

func TestAnAdministratorGivesAndCorrectsALoginID(t *testing.T) {
	b := build(t)

	// Given with a person, trimmed, and vouched for.
	wei := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register",
		m{"kind": "human", "display_name": "Wei", "login_id": " HNU2023001 "})).ActorID
	if v := b.actorView(t, wei); v.LoginID == nil || *v.LoginID != "HNU2023001" || !v.LoginIDVerified || v.Email != nil {
		t.Fatalf("Wei as registered: %+v", v)
	}
	// Nobody else's, in any case; a person's alone; and of its shape.
	out := b.MustCall(b.admin, "actor.register", m{"kind": "human", "display_name": "Other", "login_id": "hnu2023001"}, "taken")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict || reason(out) != "login_id_taken" {
		t.Fatalf("a login ID taken, in another case: %+v", out)
	}
	for _, bad := range []string{"", "  ", "2023 0001", "wei@hainanu.edu.cn", strings.Repeat("7", 65), "學號2023", "hnu/2023"} {
		b.try(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Bad", "login_id": bad}, apperr.InvalidArgument)
	}
	b.try(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "bot", "login_id": "bot-1"}, apperr.InvalidArgument)
	b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Longest", "login_id": "hnu.2023-00_" + strings.Repeat("7", 52)})

	// Given later, to Yuki, and corrected; nobody else's, and never an
	// agent's. Each change is an event saying which field changed, and not
	// what it says.
	yuki := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "20230007"}))
	if yuki.LoginID == nil || *yuki.LoginID != "20230007" || !yuki.LoginIDVerified || yuki.DisplayName != "Yuki" {
		t.Fatalf("Yuki given a login ID: %+v", yuki)
	}
	var payload []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM event WHERE type = 'actor.updated' AND subject_id = $1 ORDER BY seq DESC LIMIT 1`,
		b.yuki).Scan(&payload); err != nil || string(payload) != `{"fields": ["login_id"]}` {
		t.Fatalf("the event: %s (%v)", payload, err)
	}
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "20230008"})
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "20230008"}) // her own again
	if v := b.actorView(t, b.yuki); *v.LoginID != "20230008" {
		t.Fatalf("corrected: %+v", v)
	}
	out = b.MustCall(b.admin, "actor.update", m{"actor_id": b.ken, "login_id": "hnu2023001"}, "wei's")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict || reason(out) != "login_id_taken" {
		t.Fatalf("Wei's login ID given to Ken: %+v", out)
	}
	b.try(t, b.admin, "actor.update", m{"actor_id": b.grader, "login_id": "grader"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.update", m{"actor_id": b.ken, "login_id": " "}, apperr.InvalidArgument) // it is not taken away

	// An administrator finds them by a piece of it.
	list := testkit.Result[tools.ActorListOut](t, b.do(t, b.admin, "actor.list", m{"search": "hnu20230"}))
	if len(list.Actors) != 1 || list.Actors[0].ID != wei {
		t.Fatalf("searching by a piece of a login ID: %+v", list.Actors)
	}

	// A person sees their own, and whether anyone vouched for it.
	me := testkit.Result[tools.MeOut](t, b.do(t, b.yuki, "me.get", m{}))
	if me.LoginID == nil || *me.LoginID != "20230008" || me.LoginIDVerified == nil || !*me.LoginIDVerified || me.Email != nil {
		t.Fatalf("Yuki's me.get: %+v", me)
	}
	if me := testkit.Result[tools.MeOut](t, b.do(t, b.ken, "me.get", m{})); me.LoginID != nil || me.LoginIDVerified != nil {
		t.Fatalf("Ken, who has none: %+v", me)
	}
}

// Nobody changes their own login ID: only administrators' tools take one to
// set, and none of those is on the caller's own account.
func TestNobodyChangesTheirOwnLoginID(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	var takes []string
	for _, tl := range c.P.Registry().All() {
		if _, ok := tl.InputSchema.Properties["login_id"]; ok {
			takes = append(takes, tl.Name)
			if tl.Gate.Self {
				t.Errorf("%s takes a login_id on the caller's own account", tl.Name)
			}
		}
	}
	want := []string{"actor.lookup_by_email", "actor.register", "actor.update", "member.lookup_actor"}
	if !slices.Equal(takes, want) {
		t.Fatalf("the tools that take a login_id: %v, want %v", takes, want)
	}
	for _, name := range []string{"actor.register", "actor.update"} {
		if tl, _ := c.P.Registry().Get(name); len(tl.Gate.Platform) == 0 {
			t.Errorf("%s sets a login ID without being a platform administrator's", name)
		}
	}
}

func TestALoginIDIsShownWithTheSeatAndFindsThePerson(t *testing.T) {
	b := build(t)
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "login_id": "HNU20230007"})

	// Whoever reads the members sees it on a person's seat, and on no agent's.
	if v := b.memberView(t, b.yukiM); v.LoginID == nil || *v.LoginID != "HNU20230007" {
		t.Fatalf("member.get: %+v", v)
	}
	for _, v := range testkit.Result[tools.MemberListOut](t, b.do(t, b.sato, "member.list", m{"course_id": b.course})).Members {
		switch v.ID {
		case b.yukiM:
			if v.LoginID == nil || *v.LoginID != "HNU20230007" {
				t.Fatalf("member.list, Yuki: %+v", v)
			}
		default:
			if v.LoginID != nil {
				t.Fatalf("member.list, %s: %+v", v.DisplayName, v)
			}
		}
	}

	// Whoever seats members finds her by all of it, in any case, and by
	// nothing less.
	for _, id := range []string{"hnu20230007", " HNU20230007 "} {
		got := testkit.Result[tools.MemberLookupActorOut](t, b.do(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "login_id": id}))
		if got.ActorID != b.yuki || got.MemberID == nil || *got.MemberID != b.yukiM {
			t.Fatalf("member.lookup_actor by %q: %+v", id, got)
		}
	}
	for _, part := range []string{"HNU2023", "20230007"} {
		b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "login_id": part}, apperr.NotFound)
	}
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "login_id": "HNU20230007", "actor_id": b.yuki}, apperr.InvalidArgument)
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "login_id": " "}, apperr.InvalidArgument)
	if out := b.MustCall(b.yuki, "member.lookup_actor", m{"course_id": b.course, "login_id": "HNU20230007"}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student looking people up: %+v", out)
	}

	// So do administrators, and nobody is told the number back.
	got := testkit.Result[tools.ActorLookupOut](t, b.do(t, b.admin, "actor.lookup_by_email", m{"login_id": "hnu20230007"}))
	if got.ActorID != b.yuki {
		t.Fatalf("actor.lookup_by_email by login ID: %+v", got)
	}
	b.try(t, b.admin, "actor.lookup_by_email", m{"login_id": "hnu20230007", "email": "yuki@example.edu"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.lookup_by_email", m{}, apperr.InvalidArgument)
}

// An invitation is for a person with a login ID and no email as for one
// with an email: they take it up, and sign in with the login ID.
func TestAPersonWithALoginIDAloneIsInvitedAndSignsInWithIt(t *testing.T) {
	b := build(t)
	b.do(t, b.admin, "actor.update", m{"actor_id": b.ken, "login_id": "20230002"})
	inv := testkit.Result[tools.ActorInviteOut](t, b.do(t, b.admin, "actor.invite", m{"actor_id": b.ken}))
	if inv.Email != nil || inv.LoginID == nil || *inv.LoginID != "20230002" {
		t.Fatalf("the invitation: %+v", inv)
	}
	authn := auth.NewAuthenticator(b.Pool, 0)
	acc, err := authn.AcceptInvite(t.Context(), inv.Token, "kens own long password")
	if err != nil || acc.ActorID != b.ken || acc.Email != nil || acc.LoginID == nil || *acc.LoginID != "20230002" {
		t.Fatalf("taken up: %+v %v", acc, err)
	}
	if s, err := authn.Login(t.Context(), "20230002", "kens own long password"); err != nil || s.ActorID != b.ken || s.PasswordChangeRequired {
		t.Fatalf("signing in with the login ID: %+v %v", s, err)
	}
	// Someone with neither has nothing to sign in with.
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.yuki}, apperr.FailedPrecondition)
}

func TestRegisteringThroughAJoinLinkWithALoginIDAndNoEmail(t *testing.T) {
	b := build(t)
	link := b.joinLink(t, b.sato, m{})
	if p := b.preview(t, link.Token); p.EmailRequired || !p.Registration {
		t.Fatalf("a link to anyone: %+v", p)
	}

	out, wei, err := b.registerAs(t, link.Token, tools.JoinRegistration{DisplayName: "Wei", LoginID: "20230001"})
	if err != nil {
		t.Fatal(err)
	}
	joined(t, out)
	a, err := b.Q.GetActor(t.Context(), wei)
	if err != nil || a.Email != nil || !a.EmailVerified || a.LoginID == nil || *a.LoginID != "20230001" || a.LoginIDVerified {
		t.Fatalf("Wei as registered: %+v %v", a, err)
	}
	// An administrator sees it is unchecked, and vouches for it by setting it.
	if v := b.actorView(t, wei); v.LoginIDVerified || !v.EmailVerified {
		t.Fatalf("Wei to an administrator: %+v", v)
	}
	b.do(t, b.admin, "actor.update", m{"actor_id": wei, "login_id": "20230001"})
	if v := b.actorView(t, wei); !v.LoginIDVerified {
		t.Fatalf("vouched for: %+v", v)
	}

	// A login ID someone has is refused, in any case, before anything is
	// written, telling them to sign in; the account is not touched.
	_, _, err = b.registerAs(t, link.Token, tools.JoinRegistration{DisplayName: "Impostor", LoginID: "20230001", Email: "imp@example.edu"})
	if e, ok := apperr.As(err); !ok || e.Code != apperr.Conflict || e.Details["reason"] != "login_id_taken" {
		t.Fatalf("a login ID taken: %v", err)
	}
	// Two at once meet at the index, and the second is told the same.
	err = db.InTx(t.Context(), b.Pool, func(tx pgx.Tx) error {
		_, err := auth.RegisterPerson(t.Context(), dbq.New(tx), auth.NewPerson{DisplayName: "Racer", LoginID: "20230001",
			PasswordHash: "$argon2id$stand-in", CreatedBy: b.sato}, time.Now())
		return err
	})
	if e, ok := apperr.As(err); !ok || e.Details["reason"] != "login_id_taken" {
		t.Fatalf("a login ID taken meanwhile: %v", err)
	}

	// Both, and each is theirs to vouch for.
	out, lin, err := b.registerAs(t, link.Token, tools.JoinRegistration{DisplayName: "Lin", LoginID: "20230003", Email: "lin@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	joined(t, out)
	if a, _ := b.Q.GetActor(t.Context(), lin); a.EmailVerified || a.LoginIDVerified {
		t.Fatalf("Lin as registered: %+v", a)
	}

	// A link kept to domains asks for an email, and takes nobody without one.
	domains := b.joinLink(t, b.sato, m{"allowed_email_domains": []string{"hainanu.edu.cn"}})
	if p := b.preview(t, domains.Token); !p.EmailRequired {
		t.Fatalf("a link kept to domains: %+v", p)
	}
	_, _, err = b.registerAs(t, domains.Token, tools.JoinRegistration{DisplayName: "Fang", LoginID: "20230004"})
	if e, ok := apperr.As(err); !ok || e.Details["reason"] != "email_domain_not_allowed" {
		t.Fatalf("no email, through a link kept to domains: %v", err)
	}
	out, _, err = b.registerAs(t, domains.Token, tools.JoinRegistration{DisplayName: "Fang", LoginID: "20230004", Email: "fang@hainanu.edu.cn"})
	if err != nil {
		t.Fatal(err)
	}
	joined(t, out)
}

// What someone gives is held to its rules before anything is looked up.
func TestAJoinRegistrationGivesALoginIDOrAnEmail(t *testing.T) {
	ok := "a long enough password"
	for _, tc := range []struct {
		name string
		reg  tools.JoinRegistration
		want bool
	}{
		{"a login ID", tools.JoinRegistration{DisplayName: "Wei", LoginID: " 20230001 ", Password: ok}, true},
		{"an email", tools.JoinRegistration{DisplayName: "Wei", Email: "wei@example.edu", Password: ok}, true},
		{"both", tools.JoinRegistration{DisplayName: "Wei", LoginID: "20230001", Email: "wei@example.edu", Password: ok}, true},
		{"neither", tools.JoinRegistration{DisplayName: "Wei", Password: ok}, false},
		{"neither but spaces", tools.JoinRegistration{DisplayName: "Wei", LoginID: " ", Email: " ", Password: ok}, false},
		{"a login ID with an @", tools.JoinRegistration{DisplayName: "Wei", LoginID: "wei@example.edu", Password: ok}, false},
		{"a login ID with a space", tools.JoinRegistration{DisplayName: "Wei", LoginID: "2023 0001", Password: ok}, false},
		{"a login ID too long", tools.JoinRegistration{DisplayName: "Wei", LoginID: strings.Repeat("7", 65), Password: ok}, false},
		{"a good login ID and a bad email", tools.JoinRegistration{DisplayName: "Wei", LoginID: "20230001", Email: "wei", Password: ok}, false},
		{"a short password", tools.JoinRegistration{DisplayName: "Wei", LoginID: "20230001", Password: "short"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.reg
			err := r.Check()
			if (err == nil) != tc.want {
				t.Fatalf("%+v: %v", tc.reg, err)
			}
			if err != nil && !apperr.Is(err, apperr.InvalidArgument) {
				t.Fatalf("refused otherwise than as invalid: %v", err)
			}
			if err == nil && r.LoginID != strings.TrimSpace(tc.reg.LoginID) {
				t.Fatalf("not trimmed: %q", r.LoginID)
			}
		})
	}
	// Old clients send no login_id; a field nobody knows is refused.
	var r tools.JoinRegistration
	dec := json.NewDecoder(strings.NewReader(`{"display_name":"Aoi","email":"aoi@example.edu","password":"a long enough password"}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.Check() != nil {
		t.Fatalf("an old client's registration: %+v %v", r, err)
	}
}
