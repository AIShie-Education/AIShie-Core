#!/usr/bin/env bash
# End to end, from the outside: the real binary, a scratch database, and
# nothing but curl. It bootstraps an installation, root with a password and
# no token, and then builds the worked example from docs/schema.md §5
# entirely through the REST API — register the actors, each person invited,
# choosing a password and signing in with it, each agent given a token, and
# nobody a person a token; create and open the course, seat the instructor, set up grading,
# publish an assignment, hand in work, have an agent grade it, approve, post;
# have the instructor rename the course, halve the assignment's points with
# the grade rescaled, override and restore the student's total, rename and
# bring back the slides, and make the student a TA and a student again;
# and have the instructor's own tutor agent answer the student's question,
# once its runtime says it answers in the site, and be asked nothing more once
# he switches that off; and have another agent of his, given member_manage,
# seat a student with its own token, and be refused on his seat, and his own
# assistant propose an assignment he may make without anyone's confirmation,
# which he then approves himself. Then he shows a join link: a new student
# registers through it, a registered one joins, and once he revokes it, it
# seats nobody; his agents, without member_invite, make none. Then a student
# with no email registers through another link with her student number as
# her login ID, and signs in with it; forgets her password, and he gives her
# a temporary one; she signs in with that, is made to set her own before
# anything else, and works as before; and he cannot reset a TA's.
# Then a department's administrator, invited and appointed by root, makes a
# course beneath her appointment and seats its instructor, found by their
# email. Then Core vouches for the instructor to an agent runtime, and the key it
# publishes checks what it says, before and after a restart. Last, the sign-in
# page is told how a person signs in: by password alone, and then, restarted
# with single sign-on against a stand-in provider, by that too, under its name.
#
#   make e2e            (builds first)
#   scripts/e2e.sh      (expects bin/aishiterud)
#
# Uses the PG* environment for createdb/dropdb, like `make db-test-sql`.
set -euo pipefail

BIN=${BIN:-bin/aishiterud}
PORT=${PORT:-18099}
IDP_PORT=${IDP_PORT:-18098}
DB="aishiteru_e2e_$$"
BASE="http://127.0.0.1:$PORT"
WORK=$(mktemp -d)
SERVER_PID=""
IDP_PID=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null && wait "$SERVER_PID" 2>/dev/null || true
  [ -n "$IDP_PID" ] && kill "$IDP_PID" 2>/dev/null && wait "$IDP_PID" 2>/dev/null || true
  dropdb --if-exists "$DB" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; [ -f "$WORK/server.log" ] && tail -5 "$WORK/server.log" >&2; exit 1; }

# json FILE EXPR — evaluate a Python expression against the parsed body as d.
json() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1"; }

N=0
# call WANT_STATUS METHOD PATH TOKEN [JSON_BODY]; the body is left in $WORK/body.
call() {
  local want=$1 method=$2 path=$3 token=$4 body=${5:-}
  N=$((N + 1))
  local args=(-s -o "$WORK/body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $token")
  if [ "$method" = POST ]; then
    [ -n "$body" ] || body='{}'
    args+=(-H "Idempotency-Key: ${KEY:-e2e-$N}" -H 'Content-Type: application/json' -d "$body")
  fi
  local got
  got=$(curl "${args[@]}" "$BASE$path")
  [ "$got" = "$want" ] || fail "$method $path → $got, want $want: $(cat "$WORK/body")"
  # A join link's token is a credential of sorts: what is printed leaves it
  # out, as the server's request log does.
  printf '  %-4s %-62s %s\n' "$method" "$(printf '%s' "$path" | sed -E 's/aisjoin_[A-Za-z0-9_-]+/…/')" "$got"
}

# reason — the reason the last refusal gave, from $WORK/body.
reason() { json "$WORK/body" 'd["error"]["details"]["reason"]'; }

# signin WANT BODY — POST /v1/auth/login as the sign-in page does; the session
# the cookie carries is left in $SESSION, the body in $WORK/body.
signin() {
  N=$((N + 1))
  local got
  got=$(curl -s -o "$WORK/body" -D "$WORK/headers" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "$BASE/v1/auth/login")
  [ "$got" = "$1" ] || fail "POST /v1/auth/login → $got, want $1: $(cat "$WORK/body")"
  SESSION=$(sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' "$WORK/headers" | tr -d '\r')
  printf '  %-4s %-62s %s\n' POST /v1/auth/login "$got"
}

# accept INVITATION PASSWORD — POST /v1/auth/invite as the front end's page
# that takes invitations does: the password goes in, a session comes back as
# a cookie, left in $SESSION.
accept() {
  N=$((N + 1))
  local got
  got=$(curl -s -o "$WORK/body" -D "$WORK/headers" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d "{\"token\":\"$1\",\"password\":\"$2\"}" "$BASE/v1/auth/invite")
  [ "$got" = 200 ] || fail "POST /v1/auth/invite → $got: $(cat "$WORK/body")"
  SESSION=$(sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' "$WORK/headers" | tr -d '\r')
  [ -n "$SESSION" ] || fail "taking up the invitation set no session: $(cat "$WORK/headers")"
  printf '  %-4s %-62s %s\n' POST /v1/auth/invite "$got"
}

# ---------------------------------------------------------------------------
step "A scratch database, migrated and seeded by the binary itself"
createdb "$DB"
if [ -n "${PGHOST:-}" ]; then
  export DATABASE_URL="postgres://${PGUSER:-postgres}:${PGPASSWORD:-}@${PGHOST}:${PGPORT:-5432}/$DB?sslmode=disable"
else
  export DATABASE_URL="postgres:///$DB"
fi
export HTTP_ADDR="127.0.0.1:$PORT"
export BLOB_FS_ROOT="$WORK/blobs" PUBLIC_URL="$BASE"
# The agent runtime Core vouches for people to; its key is derived from
# SIGNING_KEY, the same across the restart below.
RUNTIME="$BASE/runtime"
SIGNING_KEY=$(python3 -c 'import secrets; print(secrets.token_hex(32))')
export RUNTIME_AUDIENCES="$RUNTIME" SIGNING_KEY
"$BIN" migrate up
"$BIN" seed

step "bootstrap: the one actor created by nobody, with a password to sign in with, and no token"
# An empty password is refused before anything is made, and so is none at
# all: the bootstrap after them is still the first.
printf '\n' | "$BIN" bootstrap --name Root --email root@example.edu --password-stdin >/dev/null 2>&1 &&
  fail "bootstrap took an empty password"
"$BIN" bootstrap --name Root --email root@example.edu </dev/null >/dev/null 2>&1 && fail "bootstrap ran with no password"
OUT=$(printf '%s\n' "roots own password" | "$BIN" bootstrap --name Root --email root@example.edu --password-stdin 2>"$WORK/bootstrap.err") ||
  fail "bootstrap: $(cat "$WORK/bootstrap.err")"
[ -z "$OUT" ] || fail "bootstrap printed on standard output"
grep -q 'ais_' "$WORK/bootstrap.err" && fail "bootstrap printed a token"
grep -q 'Sign in at your site with root@example.edu and that password' "$WORK/bootstrap.err" || fail "bootstrap did not say how root signs in: $(cat "$WORK/bootstrap.err")"
printf '%s\n' "another long password" | "$BIN" bootstrap --name Usurper --email usurper@example.edu --password-stdin >/dev/null 2>&1 &&
  fail "bootstrap ran twice"
echo "  root made, with a password and no token; no password, an empty one and a second bootstrap are refused"

start() {
  "$BIN" serve 2>>"$WORK/server.log" &
  SERVER_PID=$!
  for _ in $(seq 1 50); do curl -sf "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
  curl -sf "$BASE/healthz" >/dev/null || fail "the server did not come up"
}
start

# ---------------------------------------------------------------------------
step "Root signs in; root makes an admin, who is invited and signs in; the admin registers everyone, inviting each person and giving each agent a token"
call 401 GET /v1/me "not-a-token"
signin 200 '{"login":"root@example.edu","password":"roots own password"}'
ROOT=$SESSION
call 200 GET /v1/me "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["platform_role"]')" = root ] || fail "root is not root: $(cat "$WORK/body")"
call 200 GET /v1/me/credentials "$ROOT"
[ "$(json "$WORK/body" 'sorted(set(c["kind"] for c in d["result"]["credentials"]))')" = "['password', 'session']" ] ||
  fail "root holds more than a password and a session: $(cat "$WORK/body")"
call 403 POST /v1/me/credentials/tokens "$ROOT" '{"label":"cli"}' # a person holds no API token, root included
[ "$(reason)" = api_tokens_are_for_agents ] || fail "refused, but not as a person's token: $(cat "$WORK/body")"

person() { # BY NAME [MORE_JSON] → sets ACTOR_ID and TOKEN, the session they signed in with
  local email password invite
  email="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')@example.edu"
  password="$2's own password"
  call 200 POST /v1/actors "$1" "{\"kind\":\"human\",\"display_name\":\"$2\",\"email\":\"$email\"${3:+,$3}}"
  ACTOR_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
  call 200 POST "/v1/actors/$ACTOR_ID/invite" "$1"
  invite=$(json "$WORK/body" 'd["result"]["token"]')
  accept "$invite" "$password"
  signin 200 "{\"login\":\"$email\",\"password\":\"$password\"}"
  TOKEN=$SESSION
}
agent() { # NAME → sets ACTOR_ID and TOKEN, the API token the admin gives it
  call 200 POST /v1/actors "$ADMIN" "{\"kind\":\"agent\",\"display_name\":\"$1\"}"
  ACTOR_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
  call 200 POST "/v1/actors/$ACTOR_ID/tokens" "$ADMIN" '{"label":"e2e"}'
  TOKEN=$(json "$WORK/body" 'd["result"]["token"]')
}
person "$ROOT" Admin '"platform_role":"admin"'; ADMIN_ID=$ACTOR_ID; ADMIN=$TOKEN
person "$ADMIN" Sato;  SATO_ID=$ACTOR_ID; SATO=$TOKEN
person "$ADMIN" Yuki;  YUKI_ID=$ACTOR_ID; YUKI=$TOKEN
person "$ADMIN" Ken;   KEN_ID=$ACTOR_ID
person "$ADMIN" Hana;  HANA=$TOKEN
person "$ADMIN" Ren;   REN=$TOKEN
agent grader-v2;       GRADER_ID=$ACTOR_ID; GRADER=$TOKEN
# Nobody gives a person a token, and an agent is invited to nothing.
call 403 POST "/v1/actors/$SATO_ID/tokens" "$ADMIN" '{"label":"e2e"}'
[ "$(reason)" = api_tokens_are_for_agents ] || fail "refused, but not as a person's token: $(cat "$WORK/body")"
call 403 POST "/v1/actors/$ADMIN_ID/tokens" "$ROOT" '{"label":"e2e"}'
call 403 POST /v1/me/credentials/tokens "$SATO" '{"label":"my script"}'
call 403 POST "/v1/actors/$GRADER_ID/invite" "$ADMIN"
[ "$(reason)" = agents_use_api_tokens ] || fail "refused, but not as an agent's invitation: $(cat "$WORK/body")"
call 403 POST /v1/me/password "$GRADER" '{"password":"the graders password"}'
[ "$(reason)" = agents_use_api_tokens ] || fail "refused, but not as an agent's password: $(cat "$WORK/body")"
call 200 GET /v1/me/credentials "$GRADER"
[ "$(json "$WORK/body" '[c["kind"] for c in d["result"]["credentials"]]')" = "['api_token']" ] || fail "the grader holds more than its token: $(cat "$WORK/body")"

step "The admin creates CS101, opens it, and seats Sato; from here it is Sato's course"
call 200 POST /v1/terms "$ADMIN" '{"name":"2026 Autumn","starts_on":"2026-09-01","ends_on":"2026-12-20"}'
TERM=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST /v1/departments "$ADMIN" '{"name":"Computing"}'
DEPT=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST /v1/courses "$ADMIN" "{\"dept_id\":\"$DEPT\",\"term_id\":\"$TERM\",\"code\":\"CS101\",\"section\":\"A\",\"title\":\"Introduction to Computing\"}"
COURSE=$(json "$WORK/body" 'd["result"]["course_id"]')
TOTAL=$(json "$WORK/body" 'd["result"]["root_component_id"]')
C="/v1/courses/$COURSE"
call 200 POST "$C/activate" "$ADMIN"
call 200 POST "$C/instructors" "$ADMIN" "{\"actor_id\":\"$SATO_ID\"}"
SATO_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 403 GET "$C/members" "$ADMIN" # an admin is nobody inside a course

step "Sato sets up grading, publishes HW3, and adds a student and a grading agent"
call 200 POST "$C/components" "$SATO" "{\"parent_id\":\"$TOTAL\",\"name\":\"Assignments\",\"weight\":40}"
BUCKET=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST "$C/assignments" "$SATO" "{\"title\":\"HW3\",\"points_possible\":100,\"component_id\":\"$BUCKET\"}"
HW3=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST "$C/assignments/$HW3/publish" "$SATO"
call 200 POST "$C/members" "$SATO" "{\"actor_id\":\"$YUKI_ID\",\"preset\":\"student\"}"
YUKI_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 200 POST "$C/members" "$SATO" "{\"actor_id\":\"$GRADER_ID\",\"preset\":\"grader\",\"listed_assignments\":[\"$HW3\"]}"

step "Sato uploads the lecture slides: a URL from a tool call, the bytes by plain PUT, then attach and publish"
call 200 GET "$C/upload-url?kind=material&content_type=application/pdf" "$SATO"
PUT_URL=$(json "$WORK/body" 'd["result"]["upload_url"]')
UPLOAD=$(json "$WORK/body" 'd["result"]["upload_token"]')
printf '%%PDF-1.7 lecture one' >"$WORK/slides.pdf"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/pdf' --data-binary "@$WORK/slides.pdf" "$PUT_URL")" = 200 ] || fail "PUT to the upload URL"
call 200 POST "$C/documents" "$SATO" "{\"kind\":\"material\",\"title\":\"Lecture 1\",\"upload_token\":\"$UPLOAD\"}"
DOC=$(json "$WORK/body" 'd["result"]["document_id"]')
call 404 GET "$C/documents/$DOC" "$YUKI" # unpublished: to a student it does not exist yet
call 200 POST "$C/documents/$DOC/publish" "$SATO"
call 200 GET "$C/documents/$DOC" "$YUKI"
curl -sf -o "$WORK/got.pdf" "$(json "$WORK/body" 'd["result"]["version"]["download_url"]')" || fail "download"
cmp -s "$WORK/slides.pdf" "$WORK/got.pdf" || fail "the student downloaded different bytes"
echo "  the student downloaded exactly what the instructor uploaded"

step "Yuki hands in her essay"
call 200 POST "$C/submissions" "$YUKI" "{\"assignment_id\":\"$HW3\",\"body\":\"My essay.\"}"
SUB=$(json "$WORK/body" 'd["result"]["submission_id"]')
call 200 POST "$C/submissions/$SUB/submit" "$YUKI"
call 409 POST "$C/submissions/$SUB" "$YUKI" '{"body":"second thoughts"}' # frozen once submitted

step "The agent grades it: confirm_required, so 202 and no grade yet"
GRADE="{\"submission_id\":\"$SUB\",\"score\":85,\"feedback\":\"Clear thesis.\"}"
KEY=yuki-hw3 call 202 POST "$C/grades" "$GRADER" "$GRADE"
ACTION=$(json "$WORK/body" 'd["action_id"]')
[ "$(json "$WORK/body" 'd["status"]')" = proposed ] || fail "not proposed"
KEY=yuki-hw3 call 202 POST "$C/grades" "$GRADER" "$GRADE" # a retry: same answer, nothing done twice
KEY=yuki-hw3 call 409 POST "$C/grades" "$GRADER" "{\"submission_id\":\"$SUB\",\"score\":60}" # same key, other content
call 403 POST "$C/actions/$ACTION/decide" "$GRADER" '{"decision":"approve"}' # not its own proposal
call 200 GET "$C/grades" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["grades"])')" = 0 ] || fail "Yuki sees a grade before anyone approved or posted it"

step "Sato approves, then posts"
call 200 GET "$C/actions/proposed" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["actions"][0]["id"]')" = "$ACTION" ] || fail "the proposal is not in the approval queue"
call 200 POST "$C/actions/$ACTION/decide" "$SATO" '{"decision":"approve"}'
[ "$(json "$WORK/body" 'd["result"]["outcome"]')" = executed ] || fail "the proposal did not execute"
call 200 POST "$C/grades/post" "$SATO" "{\"assignment_id\":\"$HW3\"}"
[ "$(json "$WORK/body" 'd["result"]["snapshots"]')" = 2 ] || fail "want two totals written down: the bucket and the course"

step "Yuki sees her grade and her total; the agent finds its approval in the feed"
call 200 GET "$C/gradebook/$YUKI_M" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["components"][0]["percent"]')" = 85 ] || fail "Yuki's total is not 85"
call 200 GET "$C/events?since_seq=0" "$GRADER"
json "$WORK/body" '"action.approved" in [e["type"] for e in d["result"]["events"]] or sys.exit("no action.approved in the agent feed")' >/dev/null
KEY=yuki-hw3 call 200 POST "$C/grades" "$GRADER" "$GRADE" # the original call, replayed now, reports executed

step "Sato renames the course from his seat; its code and the rest stay the administrators'"
call 200 POST "$C/details" "$SATO" '{"title":"Computing for Everyone","description":"No experience needed."}'
[ "$(json "$WORK/body" 'd["result"]["changed"]')" = True ] || fail "the rename changed nothing"
call 200 GET "$C" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["title"], d["result"]["code"]')" = "Computing for Everyone CS101" ] || fail "the course reads $(cat "$WORK/body")"
call 403 POST "$C/details" "$YUKI" '{"title":"Mine"}'
call 400 POST "$C/details" "$SATO" '{"code":"CS999"}' # no such field here: the code is the administrators'

step "Sato halves what HW3 is worth, saying Yuki's grade is rescaled: 85 of 100 is 42.5 of 50, and her total stays 85"
call 422 POST "$C/assignments/$HW3" "$SATO" '{"points_possible":50}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = existing_grades_required ] || fail "refused, but not for want of existing_grades: $(cat "$WORK/body")"
call 200 POST "$C/assignments/$HW3" "$SATO" '{"points_possible":50,"existing_grades":"rescale"}'
[ "$(json "$WORK/body" 'd["result"]["rescaled"]')" = 1 ] || fail "want Yuki's one grade rescaled: $(cat "$WORK/body")"
call 200 GET "$C/grades" "$YUKI"
[ "$(json "$WORK/body" '[g["score"] for g in d["result"]["grades"] if g["origin"] == "entered"]')" = "[42.5]" ] || fail "Yuki's grade: $(cat "$WORK/body")"
call 200 GET "$C/gradebook/$YUKI_M" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["components"][0]["percent"]')" = 85 ] || fail "Yuki's total moved: $(cat "$WORK/body")"

step "Sato overrides Yuki's course total, with a reason; the number worked out stays beside it, and the override comes off again"
call 403 POST "$C/gradebook/$YUKI_M/totals/$TOTAL/override" "$GRADER" '{"score":90,"reason":"Generous."}'
call 200 POST "$C/gradebook/$YUKI_M/totals/$TOTAL/override" "$SATO" '{"score":88,"reason":"Participation in every lab."}'
call 200 GET "$C/gradebook/$YUKI_M" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["components"][0]["percent"], d["result"]["components"][0]["override_percent"]')" = "85 88" ] ||
  fail "Yuki's total and its override: $(cat "$WORK/body")"
call 200 POST "$C/gradebook/$YUKI_M/totals/$TOTAL/clear-override" "$SATO"
call 200 GET "$C/gradebook/$YUKI_M" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["components"][0].get("override_percent")')" = None ] || fail "the override is still there: $(cat "$WORK/body")"

step "Sato renames the slides, archives them and brings them back"
call 200 POST "$C/documents/$DOC" "$SATO" '{"title":"Lecture 1: Loops"}'
call 200 POST "$C/documents/$DOC/archive" "$SATO"
call 404 GET "$C/documents/$DOC" "$YUKI" # withdrawn
call 200 POST "$C/documents/$DOC/unarchive" "$SATO"
call 200 GET "$C/documents/$DOC" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["title"]')" = "Lecture 1: Loops" ] || fail "Yuki reads $(cat "$WORK/body")"

step "Sato makes Yuki a TA and a student again: the roster follows, and nothing she may do changes"
call 200 POST "$C/members/$YUKI_M/role" "$SATO" '{"role":"ta"}'
[ "$(json "$WORK/body" 'd["result"]["previous"], d["result"]["changed"]')" = "student True" ] || fail "the role change: $(cat "$WORK/body")"
call 200 GET "$C/members?role=student" "$SATO"
[ "$(json "$WORK/body" 'len(d["result"]["members"])')" = 0 ] || fail "a TA is listed as a student"
call 200 GET "$C/grades" "$YUKI" # her records are hers
call 403 POST "$C/members/$SATO_M/role" "$SATO" '{"role":"observer"}' # not on one's own seat
call 200 POST "$C/members/$YUKI_M/role" "$SATO" '{"role":"student"}'

step "Sato brings in a tutor agent of his own; until something runs it that answers, it is asked nothing in the site"
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Course tutor"}'
TUTOR_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/me/agents/$TUTOR_ID/tokens" "$SATO" '{"label":"runtime"}'
TUTOR=$(json "$WORK/body" 'd["result"]["token"]')
# What a service hosting it checks before it takes the token: whose agent it is.
call 200 GET /v1/me "$TUTOR"
[ "$(json "$WORK/body" 'd["result"].get("owner_actor_id")')" = "$SATO_ID" ] || fail "the tutor agent does not name Sato as its owner"
call 200 GET /v1/me "$GRADER"
[ "$(json "$WORK/body" 'd["result"].get("owner_actor_id")')" = None ] || fail "an agent nobody owns names an owner"
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$TUTOR_ID\",\"preset\":\"course_tutor\"}"
TUTOR_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 200 GET "$C/conversations/respondents" "$YUKI"
json "$WORK/body" '"'"$TUTOR_M"'" not in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("the tutor is offered to Yuki with nothing running it")' >/dev/null
call 422 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"What does HW3 ask for?\"}"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = agent_answers_elsewhere ] || fail "refused, but not as answering elsewhere: $(cat "$WORK/body")"
call 200 GET "/v1/me/agents/$TUTOR_ID" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["site_chat"]')" = False ] || fail "Sato is told the tutor takes conversations in the site"

step "Its runtime says, with its token, that it answers in the site; Yuki asks it; it finds the question in its inbox and answers"
call 200 POST /v1/me/site-chat "$TUTOR" '{"on":true}'
[ "$(json "$WORK/body" 'd["result"]["site_chat"]')" = True ] || fail "the runtime's declaration did not hold: $(cat "$WORK/body")"
call 200 GET "/v1/me/agents/$TUTOR_ID" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["site_chat"]')" = True ] || fail "Sato is not told the tutor takes conversations in the site"
call 200 GET "$C/conversations/respondents" "$YUKI"
json "$WORK/body" '"'"$TUTOR_M"'" in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("the tutor is not offered to Yuki")' >/dev/null
call 200 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"What does HW3 ask for?\"}"
CONV=$(json "$WORK/body" 'd["result"]["conversation_id"]')
call 200 GET "$C/conversations/inbox" "$TUTOR"
QUESTION=$(json "$WORK/body" 'd["result"]["conversations"][0]["latest_opener_message_id"]')
KEY="answer:$CONV:$QUESTION:1" call 200 POST "$C/conversations/$CONV/answer" "$TUTOR" "{\"in_reply_to_message_id\":\"$QUESTION\",\"body\":\"An essay with a thesis.\"}"
call 200 GET "$C/conversations/$CONV/messages" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["messages"][-1]["body"]')" = "An essay with a thesis." ] || fail "Yuki does not see the answer"
call 404 GET "$C/conversations/$CONV" "$GRADER" # nobody else's to read

step "Conversations are with agents: Sato is offered to nobody, asked nothing and answers nothing, and his seat says why"
call 200 GET "$C/conversations/respondents" "$YUKI"
json "$WORK/body" '"'"$SATO_M"'" not in [r["member_id"] for r in d["result"]["respondents"]] and all(r["kind"] == "agent" for r in d["result"]["respondents"]) or sys.exit("a person is offered as a respondent")' >/dev/null
call 403 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$SATO_M\",\"body\":\"When is the exam?\"}"
[ "$(reason)" = conversations_are_with_agents ] || fail "refused, but not as a person: $(cat "$WORK/body")"
call 403 POST "$C/conversations/$CONV/answer" "$SATO" "{\"in_reply_to_message_id\":\"$QUESTION\",\"body\":\"On Friday.\"}"
[ "$(reason)" = conversations_are_with_agents ] || fail "a person's answer refused, but not as a person's: $(cat "$WORK/body")"
call 200 GET "$C/members/$SATO_M" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["perms"]["conversation_answer"], d["result"]["perm_ceilings"]["conversation_answer"], d["result"]["perm_ceiling_reasons"]["conversation_answer"]')" = "denied denied conversations_are_with_agents" ] ||
  fail "Sato's seat says he answers: $(cat "$WORK/body")"

step "Yuki's chat panel lists her conversations with agents in every course, the newest first, saying what she has not read"
call 200 GET /v1/me/conversations "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"]), d["result"]["conversations"][0]["conversation_id"] == "'"$CONV"'"')" = "1 True" ] ||
  fail "Yuki's conversations: $(cat "$WORK/body")"
[ "$(json "$WORK/body" '*(lambda c: (c["course"]["code"], c["respondent"]["member_id"] == "'"$TUTOR_M"'", c["respondent"]["actor_id"] == "'"$TUTOR_ID"'", c["respondent"]["kind"], c["state"], c["unread"], c["may_ask"]))(d["result"]["conversations"][0])')" = "CS101 True True agent answered True True" ] ||
  fail "Yuki's conversation with the tutor, as her panel shows it: $(cat "$WORK/body")"
# She reads the answer: it is unread no more, wherever she looks.
call 200 POST "$C/conversations/$CONV/read" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["read_up_to_seq"], d["result"]["unread"]')" = "2 False" ] || fail "marking it read: $(cat "$WORK/body")"
call 200 GET /v1/me/conversations "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["conversations"][0]["unread"]')" = False ] || fail "still unread in her panel: $(cat "$WORK/body")"
call 200 GET "$C/conversations/$CONV" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["unread"]')" = False ] || fail "still unread in the conversation: $(cat "$WORK/body")"
call 404 POST "$C/conversations/$CONV/read" "$GRADER" # nobody else's to read, nor to mark read
call 200 GET "/v1/me/conversations?course_id=$COURSE&limit=1" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"]), d["result"].get("next") is not None')" = "1 True" ] || fail "a page of one: $(cat "$WORK/body")"
call 200 GET "/v1/me/conversations?limit=1&after=$(json "$WORK/body" 'd["result"]["next"]')" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"])')" = 0 ] || fail "the page after the last: $(cat "$WORK/body")"
call 400 GET "/v1/me/conversations?after=nonsense" "$YUKI"
call 200 GET /v1/me/conversations "$GRADER" # an agent that asked nothing
[ "$(json "$WORK/body" 'len(d["result"]["conversations"])')" = 0 ] || fail "the grader lists conversations: $(cat "$WORK/body")"

step "Sato switches the tutor's site chat off: Yuki asks it nothing more and still reads what it said; only its runtime switches it on"
call 200 POST "/v1/me/agents/$TUTOR_ID" "$SATO" '{"site_chat":false}'
call 422 POST "$C/conversations/$CONV/ask" "$YUKI" '{"body":"And how long should it be?"}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = agent_answers_elsewhere ] || fail "refused, but not as answering elsewhere: $(cat "$WORK/body")"
call 200 GET "$C/conversations/$CONV/messages" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["messages"])')" = 2 ] || fail "Yuki no longer reads the conversation"
call 400 POST "/v1/me/agents/$TUTOR_ID" "$SATO" '{"site_chat":true}'

step "Sato gives an agent of his own member_manage: with its own token it seats a student, and is refused on Sato's seat"
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Enrolment helper"}'
HELPER_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/me/agents/$HELPER_ID/tokens" "$SATO" '{"label":"runtime"}'
HELPER=$(json "$WORK/body" 'd["result"]["token"]')
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$HELPER_ID\",\"preset\":\"instructor\",\"perms\":{\"member_manage\":\"autonomous\"}}"
HELPER_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 200 GET "$C/members/$HELPER_M" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["perms"]["member_manage"], d["result"]["perms"]["agent_delegate"]')" = "autonomous denied" ] ||
  fail "the helper's seat: $(cat "$WORK/body")"
# Seated with the instructor preset, it still decides only by proposal, and its seat says why.
[ "$(json "$WORK/body" 'd["result"]["perms"]["action_decide"], d["result"]["perm_ceilings"]["action_decide"], d["result"]["perm_ceiling_reasons"]["action_decide"]')" = "confirm_required confirm_required agent_decides_by_proposal" ] ||
  fail "the helper's ceilings: $(cat "$WORK/body")"
call 200 POST "$C/members" "$HELPER" "{\"actor_id\":\"$KEN_ID\",\"preset\":\"student\"}"
KEN_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 200 GET "$C/members/$KEN_M" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["role"]')" = student ] || fail "the helper seated Ken otherwise than as a student: $(cat "$WORK/body")"
call 403 POST "$C/members/$SATO_M/pause" "$HELPER"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = not_your_principal ] || fail "refused, but not as its principal's seat: $(cat "$WORK/body")"
call 403 POST "$C/members/$TUTOR_M/remove" "$HELPER"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = not_your_principal ] || fail "refused, but not as its principal's other agent: $(cat "$WORK/body")"

step "Sato shows a join link in class: a new student registers through it, a registered one joins; revoked, it seats nobody"
call 200 POST "$C/join-links" "$SATO" '{"max_uses":40}'
JOIN=$(json "$WORK/body" 'd["result"]["token"]')
JOIN_LINK=$(json "$WORK/body" 'd["result"]["link_id"]')
[[ $JOIN == aisjoin_* ]] || fail "no join token: $(cat "$WORK/body")"
json "$WORK/body" '0 < (__import__("datetime").datetime.fromisoformat(d["result"]["expires_at"].replace("Z", "+00:00")) - __import__("datetime").datetime.now(__import__("datetime").timezone.utc)).total_seconds() <= 600 or sys.exit("the link does not end within ten minutes")' >/dev/null
call 403 POST "$C/join-links" "$HELPER" # manages members, but was given no member_invite
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = permission_denied ] || fail "the helper refused, but not for want of member_invite: $(cat "$WORK/body")"
call 403 POST "$C/join-links" "$GRADER"
call 200 GET "/v1/join/$JOIN" ""
[ "$(json "$WORK/body" 'd["joinable"], d["registration"], d["course"]["code"], d["expires_at"] != ""')" = "True True CS101 True" ] ||
  fail "the page that opens the link: $(cat "$WORK/body")"
call 404 GET "/v1/join/aisjoin_abcdefghijkl_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" ""
# Aoi, with no account, registers as the join page does: a session comes back as a cookie.
N=$((N + 1))
[ "$(curl -s -o "$WORK/body" -D "$WORK/headers" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"display_name":"Aoi","email":"aoi@example.edu","password":"aois own password"}' "$BASE/v1/join/$JOIN/register")" = 200 ] ||
  fail "Aoi could not register through the link: $(cat "$WORK/body")"
AOI=$(sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' "$WORK/headers" | tr -d '\r')
[ -n "$AOI" ] || fail "registering set no session: $(cat "$WORK/headers")"
AOI_M=$(json "$WORK/body" 'd["member_id"]')
printf '  %-4s %-62s %s\n' POST "/v1/join/…/register" 200
call 200 GET /v1/me "$AOI"
[ "$(json "$WORK/body" 'd["result"]["email"], d["result"]["email_verified"]')" = "aoi@example.edu False" ] || fail "Aoi as registered: $(cat "$WORK/body")"
call 200 GET "$C/members/$AOI_M" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["role"], d["result"]["join_link_id"]')" = "student $JOIN_LINK" ] || fail "Aoi's seat: $(cat "$WORK/body")"
call 200 GET "$C/documents/$DOC" "$AOI" # she reads the course's material at once
call 409 POST "/v1/join/$JOIN/register" "" '{"display_name":"Impostor","email":"AOI@example.edu","password":"another long password"}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = email_taken ] || fail "an email registered already: $(cat "$WORK/body")"
# Hana, registered already, joins signed in; opening it again, she is told she is in.
KEY=hana-joins call 200 POST "/v1/join/$JOIN" "$HANA"
[ "$(json "$WORK/body" 'd["status"], d["result"]["already_member"]')" = "executed False" ] || fail "Hana joining: $(cat "$WORK/body")"
KEY=hana-again call 200 POST "/v1/join/$JOIN" "$HANA"
[ "$(json "$WORK/body" 'd["result"]["already_member"]')" = True ] || fail "Hana joining again: $(cat "$WORK/body")"
call 200 GET "$C/join-links" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["links"][0]["status"], d["result"]["links"][0]["uses"], d["result"]["links"][0]["max_uses"]')" = "live 2 40" ] ||
  fail "the course's links: $(cat "$WORK/body")"
grep -q aisjoin_ "$WORK/body" && fail "the list shows a token"
call 200 POST "$C/join-links/$JOIN_LINK/revoke" "$SATO"
call 200 GET "/v1/join/$JOIN" ""
[ "$(json "$WORK/body" 'd["joinable"], d["reason"], d["registration"]')" = "False revoked False" ] || fail "a revoked link's page: $(cat "$WORK/body")"
call 422 POST "/v1/join/$JOIN/register" "" '{"display_name":"Rin","email":"rin@example.edu","password":"rins own password"}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = revoked ] || fail "registering through a revoked link: $(cat "$WORK/body")"
KEY=ren-late call 422 POST "/v1/join/$JOIN" "$REN"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = revoked ] || fail "joining through a revoked link: $(cat "$WORK/body")"

step "Sato's own assistant proposes HW4, its assignments waiting for a confirmation; Sato, who makes assignments without one, approves it himself and it is made"
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Assistant"}'
ASSIST_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/me/agents/$ASSIST_ID/tokens" "$SATO" '{"label":"e2e"}'
ASSIST=$(json "$WORK/body" 'd["result"]["token"]')
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$ASSIST_ID\",\"perms\":{\"assignment_write\":\"confirm_required\"}}"
call 202 POST "$C/assignments" "$ASSIST" '{"title":"HW4","points_possible":100}'
HW4_ASK=$(json "$WORK/body" 'd["action_id"]')
call 200 GET "$C/actions/proposed" "$SATO"
[ "$(json "$WORK/body" '[a["yours_to_decide"] for a in d["result"]["actions"] if a["id"] == "'"$HW4_ASK"'"]')" = "[True]" ] ||
  fail "Sato is not told his assistant's proposal is his to decide: $(cat "$WORK/body")"
call 200 POST "$C/actions/$HW4_ASK/decide" "$SATO" '{"decision":"approve"}'
[ "$(json "$WORK/body" 'd["result"]["outcome"], d["result"]["by_owner"]')" = "executed True" ] || fail "the owner's approval: $(cat "$WORK/body")"
call 200 GET "$C/assignments" "$SATO"
json "$WORK/body" '"HW4" in [a["title"] for a in d["result"]["assignments"]] or sys.exit("HW4 was not made")' >/dev/null

step "Wei, who has no email, registers through a new link with her student number as her login ID, and signs in with it"
call 200 POST "$C/join-links" "$SATO" '{}'
JOIN2=$(json "$WORK/body" 'd["result"]["token"]')
call 200 GET "/v1/join/$JOIN2" ""
[ "$(json "$WORK/body" 'd["joinable"], d["email_required"]')" = "True False" ] || fail "the page that opens the link: $(cat "$WORK/body")"
N=$((N + 1))
[ "$(curl -s -o "$WORK/body" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"display_name":"Wei","login_id":"20230001","password":"weis own password"}' "$BASE/v1/join/$JOIN2/register")" = 200 ] ||
  fail "Wei could not register with a login ID and no email: $(cat "$WORK/body")"
WEI_ID=$(json "$WORK/body" 'd["actor_id"]')
WEI_M=$(json "$WORK/body" 'd["member_id"]')
printf '  %-4s %-62s %s\n' POST "/v1/join/…/register" 200
call 409 POST "/v1/join/$JOIN2/register" "" '{"display_name":"Impostor","login_id":"20230001","password":"another long password"}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = login_id_taken ] || fail "a login ID registered already: $(cat "$WORK/body")"
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" 'd["password_accepts"]')" = "['login_id', 'email']" ] || fail "the sign-in page is not told a login ID signs in: $(cat "$WORK/body")"
signin 200 '{"login":"20230001","password":"weis own password"}'
[ "$(json "$WORK/body" 'd["actor_id"], d["password_change_required"]')" = "$WEI_ID False" ] || fail "Wei signing in: $(cat "$WORK/body")"
WEI=$SESSION
call 200 GET /v1/me "$WEI"
[ "$(json "$WORK/body" 'd["result"]["login_id"], d["result"]["login_id_verified"], d["result"].get("email")')" = "20230001 False None" ] ||
  fail "Wei as registered: $(cat "$WORK/body")"
call 200 GET "$C/members/$WEI_M" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["login_id"], d["result"]["role"]')" = "20230001 student" ] || fail "Wei's seat, to Sato: $(cat "$WORK/body")"
signin 401 '{"login":"20230001","password":"not her password"}'

step "Wei forgets her password: Sato gives her a temporary one; she signs in with it and sets her own before anything else"
call 200 POST "$C/members/$WEI_M/reset-password" "$SATO"
TEMPORARY=$(json "$WORK/body" 'd["result"]["temporary_password"]')
# Two sessions end: the one she registered with, and the one she signed in with.
[ "$(json "$WORK/body" 'd["result"]["login_id"], len(d["result"]["temporary_password"]), d["result"]["sessions_ended"]')" = "20230001 19 2" ] ||
  fail "the reset: $(cat "$WORK/body")"
call 401 GET /v1/me "$WEI" # signed out everywhere
signin 401 '{"login":"20230001","password":"weis own password"}'
signin 200 "{\"login\":\"20230001\",\"password\":\"$TEMPORARY\"}"
[ "$(json "$WORK/body" 'd["password_change_required"]')" = True ] || fail "Wei is not told to change the temporary password: $(cat "$WORK/body")"
WEI=$SESSION
call 403 GET /v1/me "$WEI"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = password_change_required ] || fail "refused, but not for the password: $(cat "$WORK/body")"
call 403 GET "$C/documents/$DOC" "$WEI"
call 400 POST /v1/me/password "$WEI" "{\"password\":\"$TEMPORARY\"}"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = password_unchanged ] || fail "the temporary password taken as her own: $(cat "$WORK/body")"
call 200 POST /v1/me/password "$WEI" '{"password":"weis new password"}'
call 200 GET /v1/me "$WEI"
call 200 GET "$C/documents/$DOC" "$WEI" # she reads the course's material again
signin 200 '{"login":"20230001","password":"weis new password"}'
[ "$(json "$WORK/body" 'd["password_change_required"]')" = False ] || fail "Wei signing in with her own: $(cat "$WORK/body")"
signin 401 "{\"login\":\"20230001\",\"password\":\"$TEMPORARY\"}"
unset TEMPORARY

step "A TA's password is not Sato's to reset"
person "$ADMIN" Tanaka; TANAKA_ID=$ACTOR_ID
call 200 POST "$C/members" "$SATO" "{\"actor_id\":\"$TANAKA_ID\",\"preset\":\"ta\"}"
TANAKA_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 403 POST "$C/members/$TANAKA_M/reset-password" "$SATO"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = not_a_student ] || fail "refused, but not as a TA's: $(cat "$WORK/body")"
call 403 POST "$C/members/$WEI_M/reset-password" "$HELPER" # his agent, which manages members, is never handed a password
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = people_only ] || fail "refused, but not as an agent: $(cat "$WORK/body")"

step "The same server over MCP: an agent's own door, with the same token"
mcp() { # JSON-RPC body → $WORK/body
  curl -s -o "$WORK/body" -w '%{http_code}' -X POST "$BASE/mcp" -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d "$2"
}
[ "$(mcp not-a-token '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')" = 401 ] || fail "MCP accepted a bad token"
[ "$(mcp "$GRADER" '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')" = 200 ] || fail "tools/list: $(cat "$WORK/body")"
json "$WORK/body" '"grade_submit" in [t["name"] for t in d["result"]["tools"]] or sys.exit("grade_submit is not offered over MCP")' >/dev/null
echo "  tools/list offers $(json "$WORK/body" 'len(d["result"]["tools"])') tools, grade_submit among them"
# The call REST made earlier, replayed over MCP with the same key: one action, two doors.
CALL="{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"grade_submit\",\"arguments\":{\"course_id\":\"$COURSE\",\"submission_id\":\"$SUB\",\"score\":85,\"feedback\":\"Clear thesis.\",\"idempotency_key\":\"yuki-hw3\"}}}"
[ "$(mcp "$GRADER" "$CALL")" = 200 ] || fail "tools/call: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["structuredContent"]["action_id"]')" = "$ACTION" ] || fail "MCP and REST did not reach the same action: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["structuredContent"]["replayed"]')" = True ] || fail "not a replay"
echo "  grade_submit over MCP with REST's idempotency key replays REST's action: one tool layer"
N=$((N + 3))

step "Root makes Engineering with Software beneath it, and invites Ada, new, who takes it up; root appoints her at Engineering"
call 200 POST /v1/departments "$ROOT" '{"name":"Engineering"}'
ENG=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST /v1/departments "$ROOT" "{\"name\":\"Software\",\"parent_id\":\"$ENG\"}"
call 200 POST /v1/actor-invitations "$ROOT" '{"display_name":"Ada","email":"ada@example.edu"}'
ADA_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
INVITE=$(json "$WORK/body" 'd["result"]["token"]')
# Taken up as the front end's page takes it: the password goes in, a session comes back as a cookie.
accept "$INVITE" "adas own password"
ADA=$SESSION
call 200 POST "/v1/departments/$ENG/admins" "$ROOT" "{\"actor_id\":\"$ADA_ID\"}"

step "Ada makes Design beneath Engineering and a course in it"
call 200 POST /v1/departments "$ADA" "{\"name\":\"Design\",\"parent_id\":\"$ENG\"}"
DESIGN=$(json "$WORK/body" 'd["result"]["id"]')
call 200 POST /v1/courses "$ADA" "{\"dept_id\":\"$DESIGN\",\"term_id\":\"$TERM\",\"code\":\"DES101\",\"title\":\"Drawing\"}"
DES101=$(json "$WORK/body" 'd["result"]["course_id"]')
[ "$(json "$WORK/body" 'd["status"]')" = executed ] || fail "the course was not made"

step "Ada finds its instructor by their whole email and seats them; the directory is not hers"
call 200 POST /v1/actors "$ADMIN" '{"kind":"human","display_name":"Mori","email":"mori@example.edu"}'
MORI_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 404 GET "/v1/actor-lookup?email=mori" "$ADA"
call 200 GET "/v1/actor-lookup?email=MORI%40example.edu" "$ADA"
[ "$(json "$WORK/body" 'd["result"]["actor_id"]')" = "$MORI_ID" ] || fail "the lookup found someone else: $(cat "$WORK/body")"
grep -q 'example.edu' "$WORK/body" && fail "the lookup shows an email: $(cat "$WORK/body")"
call 200 POST "/v1/courses/$DES101/instructors" "$ADA" "{\"actor_id\":\"$MORI_ID\"}"
call 403 GET /v1/actors "$ADA"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = platform_role_required ] || fail "refused, but not for want of a platform role: $(cat "$WORK/body")"
call 403 POST "$C" "$ADA" '{"title":"Not hers"}' # CS101 is in Computing, outside her appointment
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"]')" = department_out_of_scope ] || fail "CS101 refused, but not as out of her reach: $(cat "$WORK/body")"

step "Core vouches for Sato to the agent runtime; the key it publishes checks what it says"
# jwt PART EXPR — evaluate EXPR against the decoded header (0) or claims (1) of $ASSERTION as d.
jwt() { python3 -c "import base64,json,sys; p=sys.argv[1].split('.')[int(sys.argv[2])]; d=json.loads(base64.urlsafe_b64decode(p+'='*(-len(p)%4))); print($2)" "$ASSERTION" "$1"; }
call 200 GET /v1/auth/keys ""
cp "$WORK/body" "$WORK/keys.json"
[ "$(json "$WORK/keys.json" 'len(d["keys"]), d["keys"][0]["kty"], d["keys"][0]["crv"], d["keys"][0]["alg"]')" = "1 OKP Ed25519 EdDSA" ] ||
  fail "the key set: $(cat "$WORK/keys.json")"
call 200 POST /v1/auth/assertion "$SATO" "{\"audience\":\"$RUNTIME\"}"
ASSERTION=$(json "$WORK/body" 'd["assertion"]')
# Compact JWS: three parts of base64url with no padding, the last a 64-byte signature.
[[ "$ASSERTION" =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$ ]] || fail "the assertion is not a compact JWS in unpadded base64url"
[ "$(jwt 0 'd["alg"], d["kid"]')" = "$(json "$WORK/keys.json" '"EdDSA", d["keys"][0]["kid"]')" ] || fail "the header does not name the published key"
[ "$(jwt 1 'd["iss"], d["aud"], d["sub"], d["kind"], d["name"], d["exp"] - d["iat"]')" = "$BASE $RUNTIME $SATO_ID human Sato 300" ] ||
  fail "the claims: $(jwt 1 d)"
# Checked by OpenSSL, where it is version 3 (LibreSSL cannot); the Go tests
# check it with go-jose and go-oidc either way.
if openssl version 2>/dev/null | grep -q '^OpenSSL [3-9]'; then
  python3 - "$ASSERTION" "$WORK" <<'PY'
import base64, json, sys
tok, work = sys.argv[1:]
b = lambda s: base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))
head, claims, sig = tok.split(".")
x = b(json.load(open(work + "/keys.json"))["keys"][0]["x"])
open(work + "/key.der", "wb").write(bytes.fromhex("302a300506032b6570032100") + x)  # SubjectPublicKeyInfo, Ed25519
open(work + "/signed", "wb").write((head + "." + claims).encode())
open(work + "/sig", "wb").write(b(sig))
PY
  openssl pkeyutl -verify -pubin -inkey "$WORK/key.der" -keyform DER -rawin -in "$WORK/signed" -sigfile "$WORK/sig" >/dev/null ||
    fail "OpenSSL finds the signature bad"
  echo "  OpenSSL checks the signature against the published key"
else
  echo "  (the signature is not checked here: that needs OpenSSL 3)"
fi
call 400 POST /v1/auth/assertion "$SATO" '{"audience":"https://evil.example/runtime"}' # not one of RUNTIME_AUDIENCES
call 403 POST /v1/auth/assertion "$GRADER" "{\"audience\":\"$RUNTIME\"}"          # an agent is vouched for to nobody
call 401 GET /v1/me "$ASSERTION"                                                    # and it is no credential here
# With audiences and no key to sign for them, the server does not start.
SIGNING_KEY='' "$BIN" serve 2>"$WORK/refused.log" && fail "a server with RUNTIME_AUDIENCES and no key started"
grep -q 'RUNTIME_AUDIENCES needs ASSERTION_KEY or SIGNING_KEY' "$WORK/refused.log" || fail "refused, but not for want of a key: $(cat "$WORK/refused.log")"
# Restarted with the same SIGNING_KEY, the server publishes the same key.
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
start
call 200 GET /v1/auth/keys ""
cmp -s "$WORK/body" "$WORK/keys.json" || fail "the key changed across a restart: $(cat "$WORK/body")"
echo "  a server restarted with the same SIGNING_KEY publishes the same key"

step "The sign-in page is told how a person signs in here: by password, and no single sign-on"
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" 'd == {"password": True, "password_accepts": ["login_id", "email"], "sso": None}')" = True ] ||
  fail "the sign-in methods: $(cat "$WORK/body")"
curl -s -o /dev/null -D "$WORK/headers" "$BASE/v1/auth/methods"
grep -qi '^cache-control: public, max-age=60' "$WORK/headers" || fail "the sign-in methods may not be kept for a minute: $(cat "$WORK/headers")"

step "Restarted with single sign-on against a stand-in provider, the sign-in page is told to offer it too, by name"
# The stand-in provider is its discovery document, which is all the server
# reads of a provider before anyone signs in.
ISSUER="http://127.0.0.1:$IDP_PORT/adfs"
mkdir -p "$WORK/idp/adfs/.well-known"
printf '{"issuer":"%s","authorization_endpoint":"%s/oauth2/authorize","token_endpoint":"%s/oauth2/token","jwks_uri":"%s/discovery/keys"}\n' \
  "$ISSUER" "$ISSUER" "$ISSUER" "$ISSUER" >"$WORK/idp/adfs/.well-known/openid-configuration"
python3 -m http.server "$IDP_PORT" --bind 127.0.0.1 --directory "$WORK/idp" >"$WORK/idp.log" 2>&1 &
IDP_PID=$!
for _ in $(seq 1 50); do curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null || fail "the stand-in provider did not come up: $(cat "$WORK/idp.log")"
export OIDC_ISSUER="$ISSUER" OIDC_CLIENT_ID=aishiteru-e2e OIDC_DISPLAY_NAME="PolyU NetID"
# A name the button cannot show as it is, and the server does not start.
OIDC_DISPLAY_NAME=$'PolyU\tNetID' "$BIN" serve 2>"$WORK/refused.log" && fail "a server with a tab in OIDC_DISPLAY_NAME started"
grep -q 'OIDC_DISPLAY_NAME' "$WORK/refused.log" || fail "refused, but not for the name: $(cat "$WORK/refused.log")"
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
start
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" 'd == {"password": True, "password_accepts": ["login_id", "email"], "sso": {"label": "PolyU NetID", "start": "/v1/auth/sso/start"}}')" = True ] ||
  fail "the sign-in methods with single sign-on: $(cat "$WORK/body")"
[[ "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/v1/auth/sso/start?return_to=/courses")" == "302 $ISSUER/oauth2/authorize?"* ]] ||
  fail "where the answer says to start does not send the browser to the provider"
echo "  the answer names the button, says where to start, and says nothing else of the provider"

printf '\n\033[32mPASS\033[0m %d requests\n' "$N"
