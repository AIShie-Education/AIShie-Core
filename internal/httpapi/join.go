package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// The join endpoints take a course's join link (course.join_link_create),
// for the front end's page that opens one. They are REST only: joining a
// course by a link is a person's, in a browser, and no agent's tool.
//
//	GET  /v1/join/{token}           what the page may show, to anyone
//	POST /v1/join/{token}           the person signed in joins, however they
//	                                signed in: password, single sign-on, token
//	POST /v1/join/{token}/register  someone with no account registers and joins,
//	                                unless registration is off
//
// The token is in the path, where the link carries it; the request log
// never writes it (safePath).
const JoinPath = "/v1/join/"

// joinPreview answers the page that opens a link: the course's code,
// section and title, whether the link seats anyone now and why not, until
// when, and whether it takes a registration, for which domains, and whether
// that asks for an email (email_required) or takes a login ID alone. No
// credential is asked for, and nothing else about the course or its members
// is said. A token that finds no link is one 404, however it is wrong.
func (s *server) joinPreview(w http.ResponseWriter, r *http.Request) {
	p, err := tools.PreviewJoinLink(r.Context(), dbq.New(s.Pool), r.PathValue("token"), s.Pipeline.Clock())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.NoJoinRegistration {
		p.Registration = false
	}
	// It says what holds now, and its path is a credential of sorts: no
	// cache keeps it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, p)
}

// joinWithLink seats the person signed in, as a student, through the link:
// course.join, which neither adapter offers, made through the pipeline like
// any call, so that it is authorized, recorded and replayed by its
// Idempotency-Key as every write is. Its answer is a tool call's.
func (s *server) joinWithLink(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values(HeaderIdempotencyKey)) > 1 {
		s.writeError(w, r, apperr.Invalid("%s is given more than once", HeaderIdempotencyKey))
		return
	}
	args, err := json.Marshal(tools.CourseJoinIn{Token: r.PathValue("token")})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p := r.Context().Value(callerKey{}).(auth.Principal)
	out, err := s.Pipeline.InvokeUnlisted(r.Context(), pipeline.Caller{ActorID: p.ActorID, CredentialID: p.CredentialID},
		tools.ToolCourseJoin, args, r.Header.Get(HeaderIdempotencyKey))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeOutcome(w, out)
}

type joinRegisteredOut struct {
	ActorID   string    `json:"actor_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CourseID  string    `json:"course_id"`
	MemberID  string    `json:"member_id"`
	ActionID  string    `json:"action_id"`
}

// joinRegister registers someone with no account through a link, seats them
// and signs them in, as a sign-in does: the session cookie, and who they
// are. It is the one way a person registers on their own. The name, the
// login ID or email or both, and the password are held to their rules
// first, looking nothing up; then, as
// at sign-in, the address's allowance is taken, which a registration that
// succeeds has back; then the link is found, and the link's own allowance
// taken, which nothing gives back, so that a link that leaks makes accounts
// no faster than it allows. What would be refused is refused before the
// password is hashed: the link seating nobody, an email at a domain it does
// not take or none for a link kept to domains, and then an email or a login
// ID registered already (email_taken, login_id_taken: sign in, and open the
// link again). The person is made, their password set, the join made as
// theirs and the session started in one transaction, or none of it is: the
// join asks everything again under the link's lock, and a refusal there
// leaves no account behind, and nothing recorded.
//
// With registration off (JOIN_LINK_REGISTRATION=off) it refuses everything
// (registration_disabled), before it reads anything, the body and the link
// included: people sign in, by single sign-on say, and then join.
func (s *server) joinRegister(w http.ResponseWriter, r *http.Request) {
	if s.NoJoinRegistration {
		s.writeError(w, r, errRegistrationDisabled)
		return
	}
	var in tools.JoinRegistration
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.writeError(w, r, apperr.Invalid("the body must be JSON with display_name, login_id or email or both, and password"))
		return
	}
	if err := in.Check(); err != nil {
		s.writeError(w, r, err)
		return
	}
	addr, known := s.clientAddr(r)
	if known {
		if ok, wait := s.SignIns.Allow(addrKey(addr)); !ok {
			s.tooMany(w, r, wait)
			return
		}
	}
	ctx, q, token := r.Context(), dbq.New(s.Pool), r.PathValue("token")
	preview, err := tools.PreviewJoinLink(ctx, q, token, s.Pipeline.Clock())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if ok, wait := s.Registrations.Allow(linkKey(preview.LinkID())); !ok {
		s.tooMany(w, r, wait)
		return
	}
	if err := tools.JoinRegistrationRefusal(ctx, q, preview, in); err != nil {
		s.writeError(w, r, err)
		return
	}
	hash, err := auth.HashNewPassword(in.Password)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	args, err := json.Marshal(tools.CourseJoinIn{Token: token})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var sess auth.Session
	// The person's first action, and their only one yet: no key of theirs
	// can be taken already. A retry finds the email or the login ID
	// registered, and is told to sign in; joining again once signed in is the
	// seat they have.
	out, err := s.Pipeline.InvokeAsNew(ctx, tools.ToolCourseJoin, args, "register:"+preview.LinkID().String(), pipeline.NewActor{
		Make: func(ctx context.Context, q *dbq.Queries, now time.Time) (uuid.UUID, error) {
			return auth.RegisterPerson(ctx, q, preview.RegisteringPerson(in, hash), now)
		},
		Then: func(ctx context.Context, q *dbq.Queries, actor uuid.UUID, _ pipeline.Outcome) error {
			var err error
			sess, err = s.Auth.StartSessionIn(ctx, q, actor, "registered through a join link")
			return err
		},
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var joined tools.CourseJoinOut
	if err := json.Unmarshal(out.Result, &joined); err != nil {
		s.writeError(w, r, err)
		return
	}
	// As a sign-in that succeeds, this was no guess, and its address has
	// its allowance back: a lecture hall behind one address registers one
	// after another.
	if known {
		s.SignIns.Refund(addrKey(addr))
	}
	http.SetCookie(w, s.sessionCookie(sess.Token, sess.ExpiresAt))
	writeJSON(w, http.StatusOK, joinRegisteredOut{ActorID: sess.ActorID.String(), ExpiresAt: sess.ExpiresAt,
		CourseID: joined.CourseID.String(), MemberID: joined.MemberID.String(), ActionID: out.ActionID.String()})
}

var errRegistrationDisabled = apperr.Precondition("nobody registers through a join link here: sign in, then open the link "+
	"again to join").With("reason", "registration_disabled")

// linkKey is the registration limit's key for a join link.
func linkKey(id uuid.UUID) string { return "join-link:" + id.String() }

// joinSafePath is a join endpoint's path with its token cut out, for the
// request log.
func joinSafePath(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, JoinPath)
	if !ok {
		return "", false
	}
	if strings.HasSuffix(rest, "/register") {
		return JoinPath + "…/register", true
	}
	return JoinPath + "…", true
}
