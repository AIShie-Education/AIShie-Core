#!/usr/bin/env bash
# End to end, from the outside: the real binary, a scratch database, and
# nothing but curl. It bootstraps an installation, root with a password and
# no token, and then builds the worked example from docs/schema.md §5
# entirely through the REST API — register the actors, each person invited,
# choosing a password and signing in with it, each agent given a token, and
# nobody a person a token; create and open the course, seat the instructor, set up grading,
# publish an assignment, hand in work, have an agent grade it, ask it for
# changes, which it reads and makes, proposing again naming the grade it
# revises, approve, post;
# have the instructor rename the course, halve the assignment's points with
# the grade rescaled, override and restore the student's total, rename and
# bring back the slides, and make the student a TA and a student again;
# have the transcription service, given its credential by root, transcribe
# the slides for the student to read and the instructor to correct; have the
# instructor upload a lecture of three files and its text, which the student
# downloads file by file under their names, and the service transcribes file
# by file, and add a version of two files, and be refused past the limits on
# a version's files, saying why; and have the service be refused everything
# else, and everything once its credential is revoked;
# and have the operator give the site's agent runtime its credential on the
# command line; have the instructor register a tutor agent hosted by the
# runtime, for which he holds no token, and the runtime, checking he owns it,
# be issued its token by its id; have it answer the student's question, its
# inbox and her conversation each waiting to hear what comes next, the
# answer's draft reaching her while it is written; and answer her question
# again, her essay attached as a PDF, which it lists and downloads, and the
# grader cannot, and she withdraws, and which past the limits on files is
# refused, saying why; and have the runtime, with that credential, convert
# the instructor's Word handout and the slides the student sends the tutor
# to PDF, uploading a small PDF in their place, which the student opens,
# shown where it is opened, and nobody else does, and a failed one the
# instructor sends back; and be asked nothing more once the runtime stops
# hosting it; and have another agent of his, an mcp agent, given member_manage,
# seat a student with its own token, never be asked in the site, and be
# refused on his seat, and his own
# assistant be refused at once an assignment worth less than nothing,
# nothing of it recorded, and propose one he may make without anyone's
# confirmation, which he then approves himself, as he does its next version of the
# lecture, with its files. Then he shows a join link: a new student
# registers through it, a registered one joins, and once he revokes it, it
# seats nobody; his agents, without member_invite, make none. Then a student
# with no email registers through another link with her student number as
# her login ID, and signs in with it; forgets her password, and he gives her
# a temporary one; she signs in with that, is made to set her own before
# anything else, and works as before; and he cannot reset a TA's.
# Then a department's administrator, invited and appointed by root, makes a
# course beneath her appointment and seats its instructor, found by their
# email. Then root exports CS101's conversations for audit and downloads
# both files, the question the student withdrew in them, marked; the
# department's administrator exports only what is beneath her, and the
# instructor, the student and an agent are refused. Then Core vouches for the instructor to an agent runtime, and the key it
# publishes checks what it says, before and after a restart. Then the sign-in
# page is told how a person signs in: by password alone, and then, restarted
# with single sign-on against a stand-in provider, by that too, under its name.
# Last, root finds no provider of the site's can be added without
# SECRETS_KEY; restarted with one, finds the server reaches none on this
# machine, where the stand-in is, while the operator's is tested as before;
# restarted with SSO_ALLOW_PRIVATE_ISSUERS too, tests a stand-in provider
# that signs people in, sets it up, switches it on over the version read and
# is told its secret nowhere; the instructor, linked at it, signs in through
# it; restarted without the setting, the server reaches it no more and
# offers it no more, saying why in sso.list and its log; and root cannot
# remove it while he is linked, and then,
# forced, does.
#
#   make e2e            (builds first)
#   scripts/e2e.sh      (expects bin/aishie-core)
#
# Uses the PG* environment for createdb/dropdb, like `make db-test-sql`.
set -euo pipefail

BIN=${BIN:-bin/aishie-core}
PORT=${PORT:-18099}
IDP_PORT=${IDP_PORT:-18098}
DB="aishie_e2e_$$"
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
  # IF_MATCH='"3"' call ... — the version a write is made over.
  [ -z "${IF_MATCH:-}" ] || args+=(-H "If-Match: $IF_MATCH")
  # REVISES=<action id> call ... — the proposal sent back for changes a write revises.
  [ -z "${REVISES:-}" ] || args+=(-H "Revises: $REVISES")
  local got
  got=$(curl "${args[@]}" "$BASE$path")
  [ "$got" = "$want" ] || fail "$method $path → $got, want $want: $(cat "$WORK/body")"
  # A join link's token is a credential of sorts: what is printed leaves it
  # out, as the server's request log does.
  printf '  %-4s %-62s %s\n' "$method" "$(printf '%s' "$path" | sed -E 's/aisjoin_[A-Za-z0-9_-]+/…/')" "$got"
}

# wait_on PATH TOKEN — a GET that waits for news (wait_s), in the background,
# checked not to have answered half a second later; heard WANT — its answer,
# which must come within two seconds of what it waits for, which the caller
# has just done, left in $WORK/body.
wait_on() {
  N=$((N + 1))
  WAIT_PATH=$1
  curl -s -o "$WORK/waited" -w '%{http_code}' -H "Authorization: Bearer $2" "$BASE$1" >"$WORK/waited.status" &
  WAIT_PID=$!
  sleep 0.5
  kill -0 "$WAIT_PID" 2>/dev/null || fail "GET $1 answered with nothing yet to wait for: $(cat "$WORK/waited")"
}
heard() {
  local since=$(($(date +%s%N) / 1000000))
  wait "$WAIT_PID" || fail "GET $WAIT_PATH failed"
  local ms=$(($(date +%s%N) / 1000000 - since)) got
  got=$(cat "$WORK/waited.status")
  [ "$got" = "$1" ] || fail "GET $WAIT_PATH → $got, want $1: $(cat "$WORK/waited")"
  [ "$ms" -le 2000 ] || fail "GET $WAIT_PATH answered $ms ms after what it waited for"
  cp "$WORK/waited" "$WORK/body"
  printf '  %-4s %-62s %s, %d ms after
' GET "$WAIT_PATH" "$got" "$ms"
}

# reason — the reason the last refusal gave, from $WORK/body.
reason() { json "$WORK/body" 'd["error"]["details"]["reason"]'; }

# code — the code of the last refusal, from $WORK/body.
code() { json "$WORK/body" 'd["error"]["code"]'; }

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
# Set below, where the site's provider on this machine is to be reached.
unset SSO_ALLOW_PRIVATE_ISSUERS
# Small limits on the files a message carries, so that going past them
# costs nothing: 4 KiB a file, three to a message, 8 KiB in a conversation.
export ATTACHMENT_MAX_BYTES=4096 ATTACHMENT_MAX_PER_MESSAGE=3 ATTACHMENT_MAX_CONVERSATION_BYTES=8192
# And on the files a version of a document holds: three, 8 KiB in all.
export DOCUMENT_MAX_FILES_PER_VERSION=3 DOCUMENT_MAX_VERSION_BYTES=8192
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
agent() { # NAME → sets ACTOR_ID and TOKEN, the API token the admin gives it: an mcp agent's
  call 200 POST /v1/actors "$ADMIN" "{\"kind\":\"agent\",\"display_name\":\"$1\",\"hosting\":\"mcp\"}"
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
call 400 POST "$C/documents" "$SATO" "{\"kind\":\"material\",\"title\":\"Lecture 1\",\"upload_token\":\"$UPLOAD\"}" # files, even of one
[ "$(code)" = invalid_argument ] || fail "upload_token alone refused, but not as no field of the call: $(cat "$WORK/body")"
call 200 POST "$C/documents" "$SATO" "{\"kind\":\"material\",\"title\":\"Lecture 1\",\"files\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"slides.pdf\"}]}"
DOC=$(json "$WORK/body" 'd["result"]["document_id"]')
call 404 GET "$C/documents/$DOC" "$YUKI" # unpublished: to a student it does not exist yet
call 200 POST "$C/documents/$DOC/publish" "$SATO"
call 200 GET "$C/documents/$DOC" "$YUKI"
curl -sf -o "$WORK/got.pdf" "$(json "$WORK/body" 'd["result"]["version"]["files"][0]["download_url"]')" || fail "download"
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

step "Sato asks for changes, saying what; the agent finds it in its feed, reads what to change, and proposes again, naming the grade it revises"
call 400 POST "$C/actions/$ACTION/decide" "$SATO" '{"decision":"request_changes"}' # saying what, or not at all
[ "$(reason)" = note_required ] || fail "changes asked for saying nothing, refused, but not saying why: $(cat "$WORK/body")"
NOTE="Say what the second section is missing."
call 200 POST "$C/actions/$ACTION/decide" "$SATO" "{\"decision\":\"request_changes\",\"reason\":\"$NOTE\"}"
[ "$(json "$WORK/body" 'd["result"]["outcome"]')" = changes_requested ] || fail "the proposal was not sent back: $(cat "$WORK/body")"
call 200 GET "$C/events?since_seq=0" "$GRADER"
json "$WORK/body" '[e for e in d["result"]["events"] if e["type"] == "action.changes_requested" and e["action_id"] == "'"$ACTION"'"] or sys.exit("no action.changes_requested in the agent feed")' >/dev/null
call 200 GET "$C/actions/mine" "$GRADER"
[ "$(json "$WORK/body" '[(a["status"], a["result"]["decision"]["reason"]) for a in d["result"]["actions"] if a["id"] == "'"$ACTION"'"]')" = "[('changes_requested', '$NOTE')]" ] ||
  fail "the agent does not read what to change: $(cat "$WORK/body")"
KEY=yuki-hw3 call 409 POST "$C/grades" "$GRADER" "$GRADE" # the first call, retried: sent back, nothing done
[ "$(json "$WORK/body" 'd["status"]')" = changes_requested ] || fail "the first call, retried, does not say it was sent back"
call 200 GET "$C/grades" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["grades"])')" = 0 ] || fail "a grade sent back for changes was written"
FIRST=$ACTION
REVISED="{\"submission_id\":\"$SUB\",\"score\":85,\"feedback\":\"Clear thesis; the second section needs evidence for its claim.\"}"
REVISES=$HW3 KEY=yuki-hw3-bad call 400 POST "$C/grades" "$GRADER" "$REVISED" # no proposal of its own
[ "$(reason)" = not_revisable ] || fail "revising what is no proposal of its own, refused, but not saying why: $(cat "$WORK/body")"
REVISES=$FIRST KEY=yuki-hw3-r1 call 202 POST "$C/grades" "$GRADER" "$REVISED"
ACTION=$(json "$WORK/body" 'd["action_id"]')
call 200 GET "$C/actions/$FIRST" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["status"], d["result"]["result"]["decision"]["reason"]')" = "changes_requested $NOTE" ] ||
  fail "Sato does not read what he asked for: $(cat "$WORK/body")"

step "Sato approves the revision, then posts"
call 200 GET "$C/actions/proposed" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["actions"][0]["id"], d["result"]["actions"][0]["revises_action_id"]')" = "$ACTION $FIRST" ] ||
  fail "the revision, naming what it revises, is not in the approval queue: $(cat "$WORK/body")"
call 200 POST "$C/actions/$ACTION/decide" "$SATO" '{"decision":"approve"}'
[ "$(json "$WORK/body" 'd["result"]["outcome"]')" = executed ] || fail "the proposal did not execute"
call 200 POST "$C/grades/post" "$SATO" "{\"assignment_id\":\"$HW3\"}"
[ "$(json "$WORK/body" 'd["result"]["snapshots"]')" = 2 ] || fail "want two totals written down: the bucket and the course"

step "Yuki sees her grade and her total; the agent finds its approval in the feed"
call 200 GET "$C/gradebook/$YUKI_M" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["components"][0]["percent"]')" = 85 ] || fail "Yuki's total is not 85"
call 200 GET "$C/events?since_seq=0" "$GRADER"
json "$WORK/body" '"action.approved" in [e["type"] for e in d["result"]["events"]] or sys.exit("no action.approved in the agent feed")' >/dev/null
REVISES=$FIRST KEY=yuki-hw3-r1 call 200 POST "$C/grades" "$GRADER" "$REVISED" # the revision, replayed now, reports executed
KEY=yuki-hw3-r1 call 409 POST "$C/grades" "$GRADER" "$REVISED" # the same key revising nothing is another call
[ "$(code)" = idempotency_conflict ] || fail "the revision's key, revising nothing, is not a conflict: $(cat "$WORK/body")"

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

step "Root gives the transcription service its credential; it claims the slides, reads them and writes their text back; Yuki reads it; Sato corrects it"
call 403 POST /v1/services/document_text/credentials "$SATO" '{"label":"mine"}'
call 200 POST /v1/services/document_text/credentials "$ROOT" '{"label":"runtime"}'
SVC=$(json "$WORK/body" 'd["result"]["token"]')
SVC_CRED=$(json "$WORK/body" 'd["result"]["credential_id"]')
case "$SVC" in aissvc_*) ;; *) fail "the service's credential: $(cat "$WORK/body")" ;; esac
call 403 POST /v1/services/document_text/queue "$SATO" '{}' # the service's alone
call 200 POST /v1/services/document_text/queue "$SVC" '{"max":5}'
[ "$(json "$WORK/body" 'len(d["result"]["claimed"])')" = 1 ] || fail "the service claimed $(cat "$WORK/body")"
VERSION=$(json "$WORK/body" 'd["result"]["claimed"][0]["version_id"]')
FILE=$(json "$WORK/body" 'd["result"]["claimed"][0]["file_id"]')
LEASE=$(json "$WORK/body" 'd["result"]["claimed"][0]["lease_id"]')
curl -sf -o "$WORK/claimed.pdf" "$(json "$WORK/body" 'd["result"]["claimed"][0]["download_url"]')" || fail "the service's download"
cmp -s "$WORK/slides.pdf" "$WORK/claimed.pdf" || fail "the service downloaded different bytes"
call 400 GET "/v1/services/document_text/versions/$VERSION/file?lease_id=$LEASE" "$SVC" # by its file, always
call 200 GET "/v1/services/document_text/versions/$VERSION/file?lease_id=$LEASE&file_id=$FILE" "$SVC"
call 200 POST "/v1/services/document_text/versions/$VERSION/complete" "$SVC" \
  "{\"lease_id\":\"$LEASE\",\"file_id\":\"$FILE\",\"status\":\"done\",\"body\":\"## Page 1\\n\\nLoops.\",\"pages\":1,\"model\":\"A model\"}"
call 400 GET "$C/documents/$DOC/text" "$YUKI" # which file?
call 200 GET "$C/documents/$DOC/text?file_id=$FILE" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["text"]["source"], d["result"]["text"]["body"]')" = "ai ## Page 1

Loops." ] || fail "Yuki reads the text $(cat "$WORK/body")"
call 200 POST "$C/documents/$DOC/versions/$VERSION/text" "$SATO" "{\"file_id\":\"$FILE\",\"body\":\"## Page 1\\n\\nLoops, for and while.\"}"
call 200 GET "$C/documents/$DOC" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["version"]["files"][0]["text"]["source"], d["result"]["version"]["files"][0]["text"]["edited_by_name"]')" = "staff Sato" ] ||
  fail "Yuki reads $(cat "$WORK/body")"
call 422 POST "$C/documents/$DOC/versions/$VERSION/text/retranscribe" "$SATO" "{\"file_id\":\"$FILE\"}" # his edit goes only if he says so
[ "$(reason)" = staff_edit ] || fail "refused, but not for his edit: $(cat "$WORK/body")"
call 403 POST "$C/documents/$DOC/versions/$VERSION/text" "$YUKI" "{\"file_id\":\"$FILE\",\"body\":\"mine\"}"

step "Sato uploads a lecture of three files and its text; Yuki reads it and downloads each file under its name; the service transcribes each file on its own, for Yuki to read file by file"
# docfile CONTENT_TYPE FILE [FILENAME] — a URL for a file of Sato's material, named FILENAME if given, and
# FILE PUT to it; its token left in $UPLOAD.
docfile() {
  local q="kind=material&content_type=$1" put
  [ -z "${3:-}" ] || q="$q&filename=$3"
  call 200 GET "$C/upload-url?$q" "${4:-$SATO}"
  put=$(json "$WORK/body" 'd["result"]["upload_url"]')
  UPLOAD=$(json "$WORK/body" 'd["result"]["upload_token"]')
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Content-Type: $1" --data-binary "@$2" "$put")" = 200 ] || fail "PUT to a document's upload URL"
}
printf '%%PDF-1.7 week three' >"$WORK/week3-slides.pdf"
printf 'The handout for week three.' >"$WORK/handout.txt"
printf 'for i in range(3):\n    print(i)\n' >"$WORK/loops.py"
call 200 GET "$C/upload-url?kind=material&content_type=application/pdf" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["max_files"], d["result"]["max_version_bytes"]')" = "3 8192" ] ||
  fail "the limits the upload URL says: $(cat "$WORK/body")"
docfile application/pdf "$WORK/week3-slides.pdf"
T1=$UPLOAD
docfile text/plain "$WORK/handout.txt" handout.txt # named as it is uploaded
T2=$UPLOAD
docfile text/x-python "$WORK/loops.py"
T3=$UPLOAD
call 200 POST "$C/documents" "$SATO" "{\"kind\":\"material\",\"title\":\"Week 3\",\"body_md\":\"Slides first, then run the program.\",\"files\":[{\"upload_token\":\"$T1\",\"filename\":\"week3-slides.pdf\"},{\"upload_token\":\"$T2\"},{\"upload_token\":\"$T3\",\"filename\":\"loops.py\"}]}"
W3=$(json "$WORK/body" 'd["result"]["document_id"]')
W3_FILES=$(json "$WORK/body" '" ".join(d["result"]["file_ids"])')
call 200 POST "$C/documents/$W3/publish" "$SATO"
call 200 GET "$C/documents/$W3" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["version"]["body_md"], " ".join(f["filename"] for f in d["result"]["version"]["files"])')" = \
  "Slides first, then run the program. week3-slides.pdf handout.txt loops.py" ] || fail "Yuki reads $(cat "$WORK/body")"
[ "$(json "$WORK/body" '" ".join(f["id"] for f in d["result"]["version"]["files"])')" = "$W3_FILES" ] || fail "the files are not in order"
json "$WORK/body" '"\n".join(f["filename"] + " " + f["download_url"] for f in d["result"]["version"]["files"])' >"$WORK/w3-urls"
while read -r name url; do
  curl -sf -D "$WORK/headers" -o "$WORK/got" "$url" || fail "download of $name from the version"
  cmp -s "$WORK/$name" "$WORK/got" || fail "$name downloaded from the version is not what Sato uploaded"
  grep -qi "^content-disposition: attachment; filename=$name" "$WORK/headers" || fail "$name is not saved under its name: $(cat "$WORK/headers")"
done <"$WORK/w3-urls"
for f in $W3_FILES; do
  call 200 GET "$C/documents/$W3/files/$f" "$YUKI"
  name=$(json "$WORK/body" 'd["result"]["filename"]')
  curl -sf -o "$WORK/got" "$(json "$WORK/body" 'd["result"]["download_url"]')" || fail "download of $name by its id"
  cmp -s "$WORK/$name" "$WORK/got" || fail "$name downloaded by its id is not what Sato uploaded"
done
echo "  Yuki downloaded each of the three files exactly as Sato uploaded it, from the version and by its id, under its name"
call 200 POST /v1/services/document_text/queue "$SVC" '{"max":5}'
[ "$(json "$WORK/body" '" ".join(c["file_id"] for c in d["result"]["claimed"])')" = "$W3_FILES" ] || fail "the service claimed $(cat "$WORK/body")"
json "$WORK/body" '"\n".join(" ".join([c["version_id"], c["file_id"], c["lease_id"], c["filename"]]) for c in d["result"]["claimed"])' >"$WORK/w3-claims"
while read -r version file lease name; do
  call 200 GET "/v1/services/document_text/versions/$version/file?lease_id=$lease&file_id=$file" "$SVC"
  curl -sf -o "$WORK/got" "$(json "$WORK/body" 'd["result"]["download_url"]')" || fail "the service's download of $name"
  cmp -s "$WORK/$name" "$WORK/got" || fail "the service downloaded a $name that is not what Sato uploaded"
  call 200 POST "/v1/services/document_text/versions/$version/complete" "$SVC" \
    "{\"lease_id\":\"$lease\",\"file_id\":\"$file\",\"status\":\"done\",\"body\":\"## $name\",\"pages\":1,\"model\":\"A model\"}"
done <"$WORK/w3-claims"
for f in $W3_FILES; do
  call 200 GET "$C/documents/$W3/text?file_id=$f" "$YUKI"
  [ "$(json "$WORK/body" 'd["result"]["text"]["body"] == "## " + d["result"]["filename"]')" = True ] || fail "Yuki reads the text $(cat "$WORK/body")"
done
W3_V1=$(json "$WORK/body" 'd["result"]["version_id"]')
call 400 POST "$C/documents/$W3/versions/$W3_V1/text" "$SATO" '{"body":"## Slides"}' # which file?
[ "$(code)" = invalid_argument ] || fail "an edit naming no file of three, refused, but not for want of one: $(cat "$WORK/body")"

step "Sato adds a version of two files, which Yuki does not see until it is published; past the limits on a version's files it is refused, saying why"
printf '%%PDF-1.7 week three, corrected' >"$WORK/week3-slides.pdf"
printf 'Bring a laptop.' >"$WORK/notes.txt"
docfile application/pdf "$WORK/week3-slides.pdf"
T1=$UPLOAD
docfile text/plain "$WORK/notes.txt" notes.txt
T2=$UPLOAD
call 200 POST "$C/documents/$W3/versions" "$SATO" "{\"files\":[{\"upload_token\":\"$T1\",\"filename\":\"week3-slides.pdf\"},{\"upload_token\":\"$T2\"}]}"
[ "$(json "$WORK/body" 'd["result"]["seq"], len(d["result"]["file_ids"])')" = "2 2" ] || fail "the second version: $(cat "$WORK/body")"
W3_V2_FILE=$(json "$WORK/body" 'd["result"]["file_ids"][1]')
call 404 GET "$C/documents/$W3/files/$W3_V2_FILE" "$YUKI" # a draft's
call 200 GET "$C/documents/$W3/files/$W3_V2_FILE" "$SATO"
call 200 GET "$C/documents/$W3/versions" "$SATO"
[ "$(json "$WORK/body" '[len(v["files"]) for v in d["result"]["versions"]]')" = "[3, 2]" ] || fail "the versions: $(cat "$WORK/body")"
for i in 1 2 3 4; do printf 'part %s' "$i" >"$WORK/part$i.txt"; done
FOUR=""
for i in 1 2 3 4; do
  docfile text/plain "$WORK/part$i.txt" "part$i.txt"
  FOUR="$FOUR${FOUR:+,}{\"upload_token\":\"$UPLOAD\"}"
done
call 400 POST "$C/documents/$W3/versions" "$SATO" "{\"files\":[$FOUR]}"
[ "$(reason)" = too_many_files ] || fail "four files refused, but not as too many: $(cat "$WORK/body")"
head -c 5000 /dev/zero >"$WORK/big1.bin"
head -c 5000 /dev/zero >"$WORK/big2.bin"
BIG=""
for i in 1 2; do
  docfile application/octet-stream "$WORK/big$i.bin" "big$i.bin"
  BIG="$BIG${BIG:+,}{\"upload_token\":\"$UPLOAD\"}"
done
call 422 POST "$C/documents/$W3/versions" "$SATO" "{\"files\":[$BIG]}"
[ "$(reason)" = version_too_large ] || fail "10000 bytes refused, but not as too much for a version: $(cat "$WORK/body")"

step "The service's credential opens nothing else, and once revoked, nothing at all"
call 403 GET /v1/me "$SVC"
[ "$(reason)" = not_for_services ] || fail "refused, but not as a service: $(cat "$WORK/body")"
call 403 GET "$C/documents/$DOC" "$SVC"
call 403 POST /v1/auth/logout "$SVC"
call 200 GET /v1/services/document_text/credentials "$ROOT"
[ "$(json "$WORK/body" '[c["live"] for c in d["result"]["credentials"]]')" = "[True]" ] || fail "the service's credentials: $(cat "$WORK/body")"
call 200 POST "/v1/services/document_text/credentials/$SVC_CRED/revoke" "$ROOT"
call 401 POST /v1/services/document_text/queue "$SVC" '{}'

step "Sato makes Yuki a TA and a student again: the roster follows, and nothing she may do changes"
call 200 POST "$C/members/$YUKI_M/role" "$SATO" '{"role":"ta"}'
[ "$(json "$WORK/body" 'd["result"]["previous"], d["result"]["changed"]')" = "student True" ] || fail "the role change: $(cat "$WORK/body")"
call 200 GET "$C/members?role=student" "$SATO"
[ "$(json "$WORK/body" 'len(d["result"]["members"])')" = 0 ] || fail "a TA is listed as a student"
call 200 GET "$C/grades" "$YUKI" # her records are hers
call 403 POST "$C/members/$SATO_M/role" "$SATO" '{"role":"observer"}' # not on one's own seat
call 200 POST "$C/members/$YUKI_M/role" "$SATO" '{"role":"student"}'

step "The operator gives the site's agent runtime its credential on the command line, printed once, as a set-up script takes it"
RT=$("$BIN" service issue agent_runtime --label e2e-runtime 2>"$WORK/service.err") || fail "service issue: $(cat "$WORK/service.err")"
case "$RT" in aissvc_*) ;; *) fail "the runtime's credential: $RT" ;; esac
grep -qF "$RT" "$WORK/service.err" && fail "service issue printed the credential on standard error"
"$BIN" service issue grading --label nope >/dev/null 2>&1 && fail "service issue took a scope there is no service for"
call 200 GET /v1/services/agent_runtime/credentials "$ROOT"
[ "$(json "$WORK/body" '[(c["label"], c["live"]) for c in d["result"]["credentials"]]')" = "[('e2e-runtime', True)]" ] ||
  fail "the runtime's credentials: $(cat "$WORK/body")"
echo "  the agent runtime holds its credential, issued at setup"

step "Sato registers a tutor agent hosted by the runtime: he holds no token for it, and until the runtime hosts it, it is asked nothing in the site"
call 400 POST /v1/me/agents "$SATO" '{"display_name":"Course tutor"}' # how it is hosted is his to choose, for good
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Course tutor","hosting":"runtime"}'
TUTOR_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 403 POST "/v1/me/agents/$TUTOR_ID/tokens" "$SATO" '{"label":"my laptop"}'
[ "$(reason)" = hosted_by_runtime ] || fail "refused, but not as hosted by the runtime: $(cat "$WORK/body")"
call 422 POST "/v1/me/agents/$TUTOR_ID" "$SATO" '{"hosting":"mcp"}'
[ "$(reason)" = hosting_fixed ] || fail "refused, but not as fixed: $(cat "$WORK/body")"
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$TUTOR_ID\",\"preset\":\"course_tutor\"}"
TUTOR_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 200 GET "$C/conversations/respondents" "$YUKI"
json "$WORK/body" '"'"$TUTOR_M"'" not in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("the tutor is offered to Yuki with nothing running it")' >/dev/null
call 422 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"What does HW3 ask for?\"}"
[ "$(reason)" = agent_not_hosted ] || fail "refused, but not as not hosted: $(cat "$WORK/body")"
call 200 GET "/v1/me/agents/$TUTOR_ID" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["hosting"], d["result"]["site_chat"]')" = "runtime False" ] || fail "Sato is told the tutor is asked in the site: $(cat "$WORK/body")"

step "Sato asks the runtime to host it; the runtime, checking he owns it, is issued its token by its id; Yuki asks it; its inbox, waiting, hears the question; Yuki, waiting, watches the answer's draft come, and reads the answer"
call 403 GET "/v1/services/agent_runtime/owners/$SATO_ID/agents/$TUTOR_ID" "$SATO" # the runtime's alone
[ "$(reason)" = service_only ] || fail "refused, but not as the runtime's: $(cat "$WORK/body")"
call 200 GET "/v1/services/agent_runtime/owners/$SATO_ID/agents/$TUTOR_ID" "$RT"
[ "$(json "$WORK/body" 'd["result"]["owns"], d["result"]["agent"]["hosting"], d["result"]["agent"]["hostable"]')" = "True runtime True" ] ||
  fail "the runtime's check of Sato's tutor: $(cat "$WORK/body")"
call 200 GET "/v1/services/agent_runtime/owners/$YUKI_ID/agents/$TUTOR_ID" "$RT"
[ "$(json "$WORK/body" 'd["result"]["owns"], d["result"].get("agent")')" = "False None" ] || fail "Yuki is told she owns the tutor: $(cat "$WORK/body")"
call 200 POST "/v1/services/agent_runtime/agents/$TUTOR_ID/token" "$RT" '{"label":"e2e runtime"}'
TUTOR=$(json "$WORK/body" 'd["result"]["token"]')
case "$TUTOR" in ais_*) ;; *) fail "the tutor's token: $(cat "$WORK/body")" ;; esac
# The runtime runs the agent with it: whose agent it is, and how it is hosted.
call 200 GET /v1/me "$TUTOR"
[ "$(json "$WORK/body" 'd["result"].get("owner_actor_id"), d["result"]["hosting"]')" = "$SATO_ID runtime" ] || fail "the tutor agent, as it is told: $(cat "$WORK/body")"
call 200 GET /v1/me "$GRADER"
[ "$(json "$WORK/body" 'd["result"].get("owner_actor_id"), d["result"]["hosting"]')" = "None mcp" ] || fail "the grader, as it is told: $(cat "$WORK/body")"
call 200 GET "/v1/me/agents/$TUTOR_ID/credentials" "$SATO"
[ "$(json "$WORK/body" '[(c.get("issued_to"), "revoked_at" in c) for c in d["result"]["credentials"]]')" = "[('agent_runtime', False)]" ] ||
  fail "the tutor's credentials, as Sato sees them: $(cat "$WORK/body")"
# Nothing is declared, and there is nothing to declare it with since 0027.
call 404 POST /v1/me/site-chat "$TUTOR" '{"on":true}'
call 200 GET "/v1/me/agents/$TUTOR_ID" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["site_chat"]')" = True ] || fail "Sato is not told the tutor is asked in the site"
call 200 GET "$C/conversations/respondents" "$YUKI"
json "$WORK/body" '"'"$TUTOR_M"'" in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("the tutor is not offered to Yuki")' >/dev/null
call 200 GET "$C/conversations/inbox" "$TUTOR"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"])')" = 0 ] || fail "the tutor's inbox before anyone asked: $(cat "$WORK/body")"
wait_on "$C/conversations/inbox?wait_s=20" "$TUTOR"
call 200 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"What does HW3 ask for?\"}"
CONV=$(json "$WORK/body" 'd["result"]["conversation_id"]')
heard 200
QUESTION=$(json "$WORK/body" 'd["result"]["conversations"][0]["latest_opener_message_id"]')
# The runtime writes the answer's draft as the model writes it: recorded
# nowhere, and shown to Yuki at once, since the tutor answers without approval.
wait_on "$C/conversations/$CONV/messages?after_seq=1&wait_s=20&seen_state=awaiting_answer&seen_draft_version=0" "$YUKI"
call 200 POST "$C/conversations/$CONV/draft" "$TUTOR" '{"attempt":"a1","version":1,"text":"An essay","steps":[{"kind":"reading_assignment","target":"HW3","state":"done"},{"kind":"writing","state":"running"}]}'
[ "$(json "$WORK/body" 'd["result"]["stored"], d["result"]["version"], "action_id" in d')" = "True 1 False" ] || fail "the draft: $(cat "$WORK/body")"
heard 200
[ "$(json "$WORK/body" '*(lambda x: (x["version"], x["text"], x["steps"][0]["target"], x["steps"][1]["state"]))(d["result"]["draft"])')" = "1 An essay HW3 running" ] ||
  fail "Yuki does not see the draft: $(cat "$WORK/body")"
call 403 POST "$C/conversations/$CONV/draft" "$YUKI" '{"attempt":"a1","version":2,"text":"Mine"}'
[ "$(reason)" = conversations_are_with_agents ] || fail "Yuki writing the tutor's draft, refused, but not as a person: $(cat "$WORK/body")"
wait_on "$C/conversations/$CONV/messages?after_seq=1&wait_s=20&seen_state=awaiting_answer&seen_draft_version=1" "$YUKI"
KEY="answer:$CONV:$QUESTION:1" call 200 POST "$C/conversations/$CONV/answer" "$TUTOR" "{\"in_reply_to_message_id\":\"$QUESTION\",\"body\":\"An essay with a thesis.\"}"
heard 200
[ "$(json "$WORK/body" 'd["result"]["messages"][-1]["body"], d["result"]["conversation"]["state"], d["result"]["draft"]')" = "An essay with a thesis. answered None" ] ||
  fail "Yuki does not see the answer in its draft's place: $(cat "$WORK/body")"
call 409 POST "$C/conversations/$CONV/draft" "$TUTOR" '{"attempt":"a1","version":2}'
[ "$(reason)" = conversation_not_awaiting ] || fail "a draft after the answer, refused, but not as waiting for none: $(cat "$WORK/body")"
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
# Sato, who decides actions, lists the tutor's conversations on its page.
call 200 GET "$C/conversations?as=overseer&respondent_member_id=$TUTOR_M" "$SATO"
[ "$(json "$WORK/body" '[c["id"] for c in d["result"]["conversations"]] == ["'"$CONV"'"], "unread" in d["result"]["conversations"][0]')" = "True False" ] ||
  fail "the tutor's conversations, as Sato oversees them: $(cat "$WORK/body")"
call 200 GET "/v1/me/conversations?course_id=$COURSE&limit=1" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"]), d["result"].get("next") is not None')" = "1 True" ] || fail "a page of one: $(cat "$WORK/body")"
call 200 GET "/v1/me/conversations?limit=1&after=$(json "$WORK/body" 'd["result"]["next"]')" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["conversations"])')" = 0 ] || fail "the page after the last: $(cat "$WORK/body")"
call 400 GET "/v1/me/conversations?after=nonsense" "$YUKI"
call 200 GET /v1/me/conversations "$GRADER" # an agent that asked nothing
[ "$(json "$WORK/body" 'len(d["result"]["conversations"])')" = 0 ] || fail "the grader lists conversations: $(cat "$WORK/body")"

step "Yuki asks the tutor in the site again, her essay attached as a PDF: its runtime lists the file and downloads it under its name, the grader finds nothing, and the tutor answers with a file of its own"
# upload CONTENT_TYPE FILE TOKEN — a URL for a message's file, as TOKEN, and FILE PUT to it; its token left in $UPLOAD.
upload() {
  call 200 GET "$C/conversations/upload-url?content_type=$1" "$3"
  local put
  put=$(json "$WORK/body" 'd["result"]["upload_url"]')
  UPLOAD=$(json "$WORK/body" 'd["result"]["upload_token"]')
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Content-Type: $1" --data-binary "@$2" "$put")" = 200 ] || fail "PUT to a message's upload URL"
}
call 200 GET "$C/conversations/upload-url?content_type=application/pdf" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["max_bytes"], d["result"]["max_files"], d["result"]["max_conversation_bytes"]')" = "4096 3 8192" ] ||
  fail "the limits a front end is told: $(cat "$WORK/body")"
printf '%%PDF-1.7 my essay, second draft' >"$WORK/essay.pdf"
upload application/pdf "$WORK/essay.pdf" "$YUKI"
call 200 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"Is my essay on track?\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"essay draft 2.pdf\"}]}"
CONV2=$(json "$WORK/body" 'd["result"]["conversation_id"]')
ASKED=$(json "$WORK/body" 'd["result"]["message_id"]')
# The tutor's runtime finds the question in its inbox, reads it, and downloads what it carries.
call 200 GET "$C/conversations/inbox" "$TUTOR"
[ "$(json "$WORK/body" '[c["latest_opener_message_id"] for c in d["result"]["conversations"]]')" = "['$ASKED']" ] || fail "the tutor's inbox: $(cat "$WORK/body")"
call 200 GET "$C/conversations/$CONV2/messages" "$TUTOR"
[ "$(json "$WORK/body" '*(lambda a: (len(a), a[0]["filename"], a[0]["content_type"], a[0]["byte_size"], "download_url" in a[0]))(d["result"]["messages"][0]["attachments"])')" = \
  "1 essay draft 2.pdf application/pdf $(wc -c <"$WORK/essay.pdf" | tr -d ' ') False" ] || fail "the question's files, as the tutor reads them: $(cat "$WORK/body")"
ESSAY=$(json "$WORK/body" 'd["result"]["messages"][0]["attachments"][0]["id"]')
call 200 GET "$C/conversation-attachments/$ESSAY" "$TUTOR"
[ "$(json "$WORK/body" 'd["result"]["message_id"], d["result"]["conversation_id"]')" = "$ASKED $CONV2" ] || fail "conversation.attachment: $(cat "$WORK/body")"
[ "$(curl -s -D "$WORK/headers" -o "$WORK/got.pdf" -w '%{http_code}' "$(json "$WORK/body" 'd["result"]["download_url"]')")" = 200 ] || fail "the tutor's download"
cmp -s "$WORK/essay.pdf" "$WORK/got.pdf" || fail "the tutor downloaded different bytes"
{ grep -qi '^content-disposition: attachment; filename="essay draft 2.pdf"' "$WORK/headers" && grep -qi '^x-content-type-options: nosniff' "$WORK/headers"; } ||
  fail "the file is not served as a download under its name: $(cat "$WORK/headers")"
echo "  the tutor downloaded exactly what Yuki attached, as a download named as she named it"
# To anyone else the conversation and its file do not exist.
call 404 GET "$C/conversation-attachments/$ESSAY" "$GRADER"
call 404 GET "$C/conversations/$CONV2/messages" "$GRADER"
# The tutor answers with a file of its own, and Yuki downloads it.
printf 'Section 2: say what the evidence shows.' >"$WORK/notes.txt"
upload text/plain "$WORK/notes.txt" "$TUTOR"
KEY="answer:$CONV2:$ASKED:1" call 200 POST "$C/conversations/$CONV2/answer" "$TUTOR" "{\"in_reply_to_message_id\":\"$ASKED\",\"body\":\"On track; see my notes.\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"notes.txt\"}]}"
call 200 GET "$C/conversations/$CONV2/messages?after_seq=1" "$YUKI"
call 200 GET "$C/conversation-attachments/$(json "$WORK/body" 'd["result"]["messages"][0]["attachments"][0]["id"]')" "$YUKI"
curl -sf -o "$WORK/got.txt" "$(json "$WORK/body" 'd["result"]["download_url"]')" || fail "Yuki's download"
cmp -s "$WORK/notes.txt" "$WORK/got.txt" || fail "Yuki downloaded different bytes"

step "Past the limits on files a message is refused, saying why; withdrawn, Yuki's question keeps its file from its readers, as its text; her chat panel lists the conversation first"
head -c 5000 /dev/zero >"$WORK/big.bin"
upload application/octet-stream "$WORK/big.bin" "$YUKI"
call 422 POST "$C/conversations/$CONV2/ask" "$YUKI" "{\"body\":\"The appendix\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"appendix.bin\"}]}"
[ "$(reason)" = file_too_large ] || fail "a file too large refused, but not as one: $(cat "$WORK/body")"
head -c 3000 /dev/zero >"$WORK/part.bin"
PARTS=""
for i in 1 2 3 4; do
  upload application/octet-stream "$WORK/part.bin" "$YUKI"
  PARTS="$PARTS${PARTS:+,}{\"upload_token\":\"$UPLOAD\",\"filename\":\"part $i.bin\"}"
done
call 400 POST "$C/conversations/$CONV2/ask" "$YUKI" "{\"body\":\"Four parts\",\"attachments\":[$PARTS]}"
[ "$(reason)" = too_many_attachments ] || fail "four files refused, but not as too many: $(cat "$WORK/body")"
THREE=$(python3 -c "import sys; print(sys.argv[1].rsplit(',{', 1)[0])" "$PARTS")
call 422 POST "$C/conversations/$CONV2/ask" "$YUKI" "{\"body\":\"Three parts\",\"attachments\":[$THREE]}"
[ "$(reason)" = conversation_attachments_full ] || fail "9000 bytes more refused, but not as the conversation full: $(cat "$WORK/body")"
call 400 POST "$C/conversations/$CONV2/ask" "$YUKI" "{\"body\":\"A path\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"../../etc/passwd\"}]}"
[ "$(reason)" = bad_filename ] || fail "a path for a name refused, but not as a bad name: $(cat "$WORK/body")"
upload text/plain "$WORK/notes.txt" "$TUTOR"
call 403 POST "$C/conversations/$CONV2/ask" "$YUKI" "{\"body\":\"The tutor's\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"notes.txt\"}]}"
[ "$(reason)" = not_your_upload ] || fail "someone else's upload refused, but not as theirs: $(cat "$WORK/body")"
call 403 GET "$C/conversations/upload-url?content_type=text/plain" "$GRADER" # who neither asks nor answers uploads nothing for a message
call 200 POST "$C/conversation-messages/$ASKED/retract" "$YUKI" '{"reason":"wrong draft"}'
call 404 GET "$C/conversation-attachments/$ESSAY" "$TUTOR"
[ "$(reason)" = retracted ] || fail "a retracted message's file refused, but not as retracted: $(cat "$WORK/body")"
call 200 GET "$C/conversations/$CONV2/messages" "$TUTOR"
[ "$(json "$WORK/body" '*(lambda x: ("attachments" in x, "body" in x, x["retracted"]["reason"]))(d["result"]["messages"][0])')" = "False False wrong draft" ] ||
  fail "the retracted question, as the tutor reads it: $(cat "$WORK/body")"
call 200 GET /v1/me/conversations "$YUKI"
[ "$(json "$WORK/body" '[c["conversation_id"] for c in d["result"]["conversations"]]')" = "['$CONV2', '$CONV']" ] || fail "Yuki's panel: $(cat "$WORK/body")"

step "Office files are previewed as PDFs: Sato's Word handout and the slides Yuki sends the tutor are queued as they are recorded; the runtime claims each, reads it, uploads the PDF it made and says it is done; Yuki opens each PDF where she reads its file, and nobody else does"
# A PDF of one page, as the runtime's converter makes one.
python3 - "$WORK/rendition.pdf" <<'PY'
import sys
objs = [b"<< /Type /Catalog /Pages 2 0 R >>", b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] >>"]
out, offsets = bytearray(b"%PDF-1.4\n"), []
for i, o in enumerate(objs, 1):
    offsets.append(len(out))
    out += b"%d 0 obj\n%s\nendobj\n" % (i, o)
xref = len(out)
out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1) + b"".join(b"%010d 00000 n \n" % x for x in offsets)
out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
open(sys.argv[1], "wb").write(out)
PY
# convert ID LEASE — what the runtime does once it has converted a claimed file: a URL for the PDF, the PDF PUT
# there, and the rendition said to be done.
convert() {
  call 200 GET "/v1/services/agent_runtime/renditions/$1/upload-url?lease_id=$2" "$RT"
  [ "$(json "$WORK/body" 'd["result"]["headers"]["Content-Type"]')" = application/pdf ] || fail "the PDF's upload URL: $(cat "$WORK/body")"
  local put token
  put=$(json "$WORK/body" 'd["result"]["upload_url"]')
  token=$(json "$WORK/body" 'd["result"]["upload_token"]')
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/pdf' --data-binary "@$WORK/rendition.pdf" "$put")" = 200 ] ||
    fail "PUT of the PDF"
  call 200 POST "/v1/services/agent_runtime/renditions/$1/complete" "$RT" "{\"lease_id\":\"$2\",\"status\":\"done\",\"upload_token\":\"$token\",\"page_count\":1}"
  [ "$(json "$WORK/body" 'd["result"]["state"], d["result"]["byte_size"]')" = "done $(wc -c <"$WORK/rendition.pdf" | tr -d ' ')" ] ||
    fail "the rendition done: $(cat "$WORK/body")"
}
# opens URL NAME — the PDF behind URL, opened as a browser opens it: a PDF, shown where it is opened, named NAME.
opens() {
  [ "$(curl -s -D "$WORK/headers" -o "$WORK/got.pdf" -w '%{http_code}' "$1")" = 200 ] || fail "opening the PDF of $2"
  cmp -s "$WORK/rendition.pdf" "$WORK/got.pdf" || fail "the PDF of $2 is not what the runtime uploaded"
  { grep -qi '^content-type: application/pdf' "$WORK/headers" && grep -qi "^content-disposition: inline; filename=$2" "$WORK/headers" &&
    grep -qi '^x-content-type-options: nosniff' "$WORK/headers"; } || fail "the PDF of $2 is not shown as a PDF named $2: $(cat "$WORK/headers")"
}
DOCX=application/vnd.openxmlformats-officedocument.wordprocessingml.document
printf 'PK\003\004 the handout for week four' >"$WORK/handout.docx"
docfile "$DOCX" "$WORK/handout.docx" handout.docx
call 200 POST "$C/documents" "$SATO" "{\"kind\":\"material\",\"title\":\"Week 4\",\"files\":[{\"upload_token\":\"$UPLOAD\"}]}"
W4=$(json "$WORK/body" 'd["result"]["document_id"]')
W4_FILE=$(json "$WORK/body" 'd["result"]["file_ids"][0]')
call 200 POST "$C/documents/$W4/publish" "$SATO"
call 200 GET "$C/documents/$W4" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["version"]["files"][0]["rendition"]["state"]')" = queued ] || fail "the handout's rendition: $(cat "$WORK/body")"
call 403 POST /v1/services/agent_runtime/renditions/claim "$SATO" '{}' # the runtime's alone
[ "$(reason)" = service_only ] || fail "refused, but not as the runtime's: $(cat "$WORK/body")"
call 200 POST /v1/services/agent_runtime/renditions/claim "$RT" '{"max":5,"lease_s":300}'
[ "$(json "$WORK/body" '[(c["source"], c["file_id"], c["filename"]) for c in d["result"]["claimed"]]')" = "[('document_file', '$W4_FILE', 'handout.docx')]" ] ||
  fail "the runtime claimed $(cat "$WORK/body")"
REND=$(json "$WORK/body" 'd["result"]["claimed"][0]["rendition_id"]')
LEASE=$(json "$WORK/body" 'd["result"]["claimed"][0]["lease_id"]')
curl -sf -o "$WORK/got" "$(json "$WORK/body" 'd["result"]["claimed"][0]["download_url"]')" || fail "the runtime's download of the handout"
cmp -s "$WORK/handout.docx" "$WORK/got" || fail "the runtime downloaded a handout that is not what Sato uploaded"
call 200 GET "/v1/services/agent_runtime/renditions/$REND/file?lease_id=$LEASE" "$RT"
call 200 POST "/v1/services/agent_runtime/renditions/$REND/renew" "$RT" "{\"lease_id\":\"$LEASE\",\"lease_s\":600}"
# What is not a PDF is refused, and the claim holds.
call 200 GET "/v1/services/agent_runtime/renditions/$REND/upload-url?lease_id=$LEASE" "$RT"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/pdf' --data-binary "@$WORK/handout.docx" "$(json "$WORK/body" 'd["result"]["upload_url"]')")" = 200 ] ||
  fail "PUT of what is not a PDF"
call 422 POST "/v1/services/agent_runtime/renditions/$REND/complete" "$RT" "{\"lease_id\":\"$LEASE\",\"status\":\"done\",\"upload_token\":\"$(json "$WORK/body" 'd["result"]["upload_token"]')\",\"page_count\":1}"
[ "$(reason)" = not_a_pdf ] || fail "what is not a PDF refused, but not as one: $(cat "$WORK/body")"
convert "$REND" "$LEASE"
call 409 POST "/v1/services/agent_runtime/renditions/$REND/complete" "$RT" "{\"lease_id\":\"$LEASE\",\"status\":\"failed\",\"reason\":\"timeout\"}"
[ "$(reason)" = lease_lost ] || fail "done twice refused, but not as the claim gone: $(cat "$WORK/body")"
call 200 GET "$C/documents/$W4" "$YUKI"
[ "$(json "$WORK/body" '*(lambda r: (r["state"], r["page_count"], "download_url" in r))(d["result"]["version"]["files"][0]["rendition"])')" = "done 1 True" ] ||
  fail "the handout's rendition, as Yuki reads it: $(cat "$WORK/body")"
opens "$(json "$WORK/body" 'd["result"]["version"]["files"][0]["rendition"]["download_url"]')" handout.pdf
call 200 GET "$C/documents/$W4/files/$W4_FILE" "$YUKI"
opens "$(json "$WORK/body" 'd["result"]["rendition"]["download_url"]')" handout.pdf
echo "  Yuki opened the handout's PDF, exactly as the runtime uploaded it, shown where it is opened and named as the handout"
# A draft's file, and its PDF, are not hers; what failed staff send back.
printf 'PK\003\004 the handout, corrected' >"$WORK/handout.docx"
docfile "$DOCX" "$WORK/handout.docx" handout.docx
call 200 POST "$C/documents/$W4/versions" "$SATO" "{\"files\":[{\"upload_token\":\"$UPLOAD\"}]}"
W4_V2_FILE=$(json "$WORK/body" 'd["result"]["file_ids"][0]')
call 404 GET "$C/documents/$W4/files/$W4_V2_FILE" "$YUKI"
call 200 POST /v1/services/agent_runtime/renditions/claim "$RT" '{}'
REND=$(json "$WORK/body" 'd["result"]["claimed"][0]["rendition_id"]')
call 200 POST "/v1/services/agent_runtime/renditions/$REND/complete" "$RT" "{\"lease_id\":\"$(json "$WORK/body" 'd["result"]["claimed"][0]["lease_id"]')\",\"status\":\"failed\",\"reason\":\"password_protected\"}"
call 200 GET "$C/documents/$W4/files/$W4_V2_FILE" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["rendition"]["state"], d["result"]["rendition"]["reason"]')" = "failed password_protected" ] ||
  fail "the corrected handout's rendition, as Sato reads it: $(cat "$WORK/body")"
call 403 POST "$C/documents/$W4/files/$W4_V2_FILE/rendition/retry" "$YUKI"
call 200 POST "$C/documents/$W4/files/$W4_V2_FILE/rendition/retry" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["changed"], d["result"]["state"]')" = "True queued" ] || fail "sent back: $(cat "$WORK/body")"
call 200 POST /v1/services/agent_runtime/renditions/claim "$RT" '{}'
convert "$(json "$WORK/body" 'd["result"]["claimed"][0]["rendition_id"]')" "$(json "$WORK/body" 'd["result"]["claimed"][0]["lease_id"]')"
# Yuki's slides, sent to the tutor.
PPTX=application/vnd.openxmlformats-officedocument.presentationml.presentation
printf 'PK\003\004 my slides' >"$WORK/talk.pptx"
upload "$PPTX" "$WORK/talk.pptx" "$YUKI"
call 200 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"Are my slides clear?\",\"attachments\":[{\"upload_token\":\"$UPLOAD\",\"filename\":\"talk.pptx\"}]}"
CONV3=$(json "$WORK/body" 'd["result"]["conversation_id"]')
call 200 GET "$C/conversations/$CONV3/messages" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["messages"][0]["attachments"][0]["rendition"]["state"]')" = queued ] || fail "the slides' rendition: $(cat "$WORK/body")"
TALK=$(json "$WORK/body" 'd["result"]["messages"][0]["attachments"][0]["id"]')
call 200 POST /v1/services/agent_runtime/renditions/claim "$RT" '{}'
[ "$(json "$WORK/body" '[(c["source"], c["attachment_id"], c["filename"]) for c in d["result"]["claimed"]]')" = "[('attachment', '$TALK', 'talk.pptx')]" ] ||
  fail "the runtime claimed $(cat "$WORK/body")"
convert "$(json "$WORK/body" 'd["result"]["claimed"][0]["rendition_id"]')" "$(json "$WORK/body" 'd["result"]["claimed"][0]["lease_id"]')"
for who in "$YUKI" "$TUTOR"; do
  call 200 GET "$C/conversation-attachments/$TALK" "$who"
  [ "$(json "$WORK/body" 'd["result"]["rendition"]["state"]')" = "done" ] || fail "the slides' rendition: $(cat "$WORK/body")"
  opens "$(json "$WORK/body" 'd["result"]["rendition"]["download_url"]')" talk.pdf
done
call 404 GET "$C/conversation-attachments/$TALK" "$GRADER"
call 200 POST /v1/services/agent_runtime/renditions/claim "$RT" '{}'
[ "$(json "$WORK/body" 'len(d["result"]["claimed"])')" = 0 ] || fail "something was left to convert: $(cat "$WORK/body")"
echo "  Yuki and the tutor opened the slides' PDF; the grader found nothing"

step "The runtime stops hosting the tutor, revoking its token by its id: Yuki asks it nothing more and still reads what it said; Sato says nothing of it in Core"
call 400 POST "/v1/me/agents/$TUTOR_ID" "$SATO" '{"site_chat":false}' # no field of agent.update since 0027
[ "$(code)" = invalid_argument ] || fail "site_chat refused, but not as no field of the call: $(cat "$WORK/body")"
call 200 POST "/v1/services/agent_runtime/agents/$TUTOR_ID/token/revoke" "$RT"
[ "$(json "$WORK/body" 'len(d["result"]["revoked"])')" = 1 ] || fail "revoking the tutor's token: $(cat "$WORK/body")"
call 401 GET /v1/me "$TUTOR"
call 422 POST "$C/conversations/$CONV/ask" "$YUKI" '{"body":"And how long should it be?"}'
[ "$(reason)" = agent_not_hosted ] || fail "refused, but not as not hosted: $(cat "$WORK/body")"
call 200 GET "$C/conversations/$CONV/messages" "$YUKI"
[ "$(json "$WORK/body" 'len(d["result"]["messages"])')" = 2 ] || fail "Yuki no longer reads the conversation"

step "Sato gives an mcp agent of his own member_manage: with his token for it, from his own script, it seats a student, and is refused on Sato's seat; the runtime does not host it"
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Enrolment helper","hosting":"mcp"}'
HELPER_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/me/agents/$HELPER_ID/tokens" "$SATO" '{"label":"my script"}'
HELPER=$(json "$WORK/body" 'd["result"]["token"]')
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$HELPER_ID\",\"preset\":\"instructor\",\"perms\":{\"member_manage\":\"autonomous\"}}"
HELPER_M=$(json "$WORK/body" 'd["result"]["member_id"]')
call 422 POST "/v1/services/agent_runtime/agents/$HELPER_ID/token" "$RT"
[ "$(reason)" = not_runtime_hosted ] || fail "the runtime hosting an mcp agent refused, but not as not runtime hosted: $(cat "$WORK/body")"
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

step "Sato's own assistant, an mcp agent nobody asks in the site, proposes HW4, its assignments waiting for a confirmation, one worth less than nothing refused at once and recorded nowhere; Sato, who makes assignments without one, approves it himself and it is made"
call 200 POST /v1/me/agents "$SATO" '{"display_name":"Assistant","hosting":"mcp"}'
ASSIST_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/me/agents/$ASSIST_ID/tokens" "$SATO" '{"label":"e2e"}'
ASSIST=$(json "$WORK/body" 'd["result"]["token"]')
call 200 POST "$C/delegates" "$SATO" "{\"actor_id\":\"$ASSIST_ID\",\"perms\":{\"assignment_write\":\"confirm_required\",\"document_write\":\"confirm_required\"}}"
ASSIST_M=$(json "$WORK/body" 'd["result"]["member_id"]')
# It answers Sato, its principal, and is still never asked in the site: an mcp agent, his tools'.
call 200 GET "$C/conversations/respondents" "$SATO"
json "$WORK/body" '"'"$ASSIST_M"'" not in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("an mcp agent is offered in the site")' >/dev/null
call 422 POST "$C/conversations" "$SATO" "{\"respondent_member_id\":\"$ASSIST_M\",\"body\":\"What is due this week?\"}"
[ "$(reason)" = mcp_agent ] || fail "refused, but not as an mcp agent: $(cat "$WORK/body")"
call 400 POST "$C/assignments" "$ASSIST" '{"title":"HW4","points_possible":-100}'
[ "$(json "$WORK/body" '"action_id" in d, d["error"]["message"]')" = "False points_possible cannot be negative" ] ||
  fail "a proposal that could never be carried out: $(cat "$WORK/body")"
call 202 POST "$C/assignments" "$ASSIST" '{"title":"HW4","points_possible":100}'
HW4_ASK=$(json "$WORK/body" 'd["action_id"]')
call 200 GET "$C/actions/proposed" "$SATO"
[ "$(json "$WORK/body" '[a["yours_to_decide"] for a in d["result"]["actions"] if a["id"] == "'"$HW4_ASK"'"]')" = "[True]" ] ||
  fail "Sato is not told his assistant's proposal is his to decide: $(cat "$WORK/body")"
call 200 POST "$C/actions/$HW4_ASK/decide" "$SATO" '{"decision":"approve"}'
[ "$(json "$WORK/body" 'd["result"]["outcome"], d["result"]["by_owner"]')" = "executed True" ] || fail "the owner's approval: $(cat "$WORK/body")"
call 200 GET "$C/assignments" "$SATO"
json "$WORK/body" '"HW4" in [a["title"] for a in d["result"]["assignments"]] or sys.exit("HW4 was not made")' >/dev/null

step "Sato's assistant proposes the next version of Week 3 with its files, kept by their tokens until Sato approves it"
printf '%%PDF-1.7 week three, third time' >"$WORK/week3-slides.pdf"
docfile application/pdf "$WORK/week3-slides.pdf" "" "$ASSIST"
T1=$UPLOAD
docfile text/x-python "$WORK/loops.py" loops.py "$ASSIST"
T2=$UPLOAD
call 202 POST "$C/documents/$W3/versions" "$ASSIST" "{\"body_md\":\"Slides, then the program.\",\"files\":[{\"upload_token\":\"$T1\",\"filename\":\"week3-slides.pdf\"},{\"upload_token\":\"$T2\"}]}"
W3_ASK=$(json "$WORK/body" 'd["action_id"]')
call 200 GET "$C/documents/$W3/versions" "$SATO"
[ "$(json "$WORK/body" 'len(d["result"]["versions"])')" = 2 ] || fail "a proposed version was written before anyone decided: $(cat "$WORK/body")"
call 200 POST "$C/actions/$W3_ASK/decide" "$SATO" '{"decision":"approve"}'
[ "$(json "$WORK/body" 'd["result"]["outcome"]')" = executed ] || fail "the approval: $(cat "$WORK/body")"
call 200 GET "$C/documents/$W3" "$SATO"
[ "$(json "$WORK/body" 'd["result"]["version"]["seq"], " ".join(f["filename"] for f in d["result"]["version"]["files"])')" = "3 week3-slides.pdf loops.py" ] ||
  fail "the approved version: $(cat "$WORK/body")"
curl -sf -o "$WORK/got" "$(json "$WORK/body" 'd["result"]["version"]["files"][0]["download_url"]')" || fail "download of the approved slides"
cmp -s "$WORK/week3-slides.pdf" "$WORK/got" || fail "the approved slides are not what the assistant uploaded"

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
json "$WORK/body" '{"conversation_upload_url", "conversation_attachment"} <= {t["name"] for t in d["result"]["tools"]} or sys.exit("a message'"'"'s files are not reached over MCP")' >/dev/null
echo "  tools/list offers $(json "$WORK/body" 'len(d["result"]["tools"])') tools, grade_submit among them"
# The revision REST made earlier, replayed over MCP with the same key and
# what it revises, as an argument there: one action, two doors.
CALL="{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"grade_submit\",\"arguments\":{\"course_id\":\"$COURSE\",\"submission_id\":\"$SUB\",\"score\":85,\"feedback\":\"Clear thesis; the second section needs evidence for its claim.\",\"idempotency_key\":\"yuki-hw3-r1\",\"revises\":\"$FIRST\"}}}"
[ "$(mcp "$GRADER" "$CALL")" = 200 ] || fail "tools/call: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["structuredContent"]["action_id"]')" = "$ACTION" ] || fail "MCP and REST did not reach the same action: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["structuredContent"]["replayed"]')" = True ] || fail "not a replay"
echo "  grade_submit over MCP with REST's idempotency key and revision replays REST's action: one tool layer"
# Sato's mcp agent, which nobody asks in the site, works over MCP with his
# token for it: told it is an mcp agent, and reading its seat.
[ "$(mcp "$HELPER" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"me_get","arguments":{}}}')" = 200 ] || fail "me_get: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["structuredContent"]["result"]["hosting"]')" = mcp ] || fail "the mcp agent over MCP: $(cat "$WORK/body")"
[ "$(mcp "$HELPER" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"me_memberships","arguments":{}}}')" = 200 ] || fail "me_memberships: $(cat "$WORK/body")"
[ "$(json "$WORK/body" '[s["member_id"] for s in d["result"]["structuredContent"]["result"]["memberships"]]')" = "['$HELPER_M']" ] ||
  fail "the mcp agent's seats over MCP: $(cat "$WORK/body")"
[ "$(mcp "$HELPER" '{"jsonrpc":"2.0","id":5,"method":"tools/list"}')" = 200 ] || fail "tools/list: $(cat "$WORK/body")"
json "$WORK/body" 'not any(t["name"].startswith("agent_runtime_") for t in d["result"]["tools"]) or sys.exit("the runtime'"'"'s tools are offered over MCP")' >/dev/null
echo "  Sato's mcp agent works over MCP, told it is one; the runtime's tools are not offered there"
N=$((N + 6))

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

step "Root exports CS101's conversations for audit: two files, each downloaded with nothing but its URL, the question Yuki withdrew in them with its text, marked; Ada exports what is beneath her and nothing else; Sato, Yuki and the grader are refused"
KEY=export-cs101 call 200 POST /v1/conversation-exports "$ROOT" "{\"course_id\":\"$COURSE\"}"
cp "$WORK/body" "$WORK/export.json"
EXPORT=$(json "$WORK/export.json" 'd["result"]["export_id"]')
[ "$(json "$WORK/export.json" 'd["action_id"] == d["result"]["export_id"], d["result"]["retracted"] >= 1, sorted(x["format"] for x in d["result"]["downloads"])')" = "True True ['csv', 'jsonl']" ] ||
  fail "the export: $(cat "$WORK/export.json")"
for f in jsonl csv; do
  URL=$(json "$WORK/export.json" '[x["download_url"] for x in d["result"]["downloads"] if x["format"] == "'"$f"'"][0]')
  got=$(curl -s -D "$WORK/headers" -o "$WORK/export.$f" -w '%{http_code}' "$URL")
  [ "$got" = 200 ] || fail "downloading the export's $f file → $got"
  name=$([ "$f" = jsonl ] && echo "conversations-$EXPORT.jsonl" || echo "messages-$EXPORT.csv")
  { grep -qi "^content-disposition: attachment; filename=$name" "$WORK/headers" && grep -qi '^x-content-type-options: nosniff' "$WORK/headers"; } ||
    fail "the export's $f file is not a download under its name: $(cat "$WORK/headers")"
  printf '  %-4s %-62s %s\n' GET "(the export's $f file, by its URL alone)" "$got"
done
python3 - "$WORK/export.jsonl" "$CONV" "$CONV2" "$ASKED" <<'PY' || fail "the conversations file: $(head -c 3000 "$WORK/export.jsonl")"
import json, sys
lines = [json.loads(line) for line in open(sys.argv[1], encoding="utf-8")]
ids = [c["id"] for c in lines]
assert sys.argv[2] in ids and sys.argv[3] in ids, ids
conv = next(c for c in lines if c["id"] == sys.argv[3])
asked = next(x for x in conv["messages"] if x["id"] == sys.argv[4])
assert asked["body"] == "Is my essay on track?" and asked["retracted"]["reason"] == "wrong draft", asked
assert [a["filename"] for a in asked["attachments"]] == ["essay draft 2.pdf"], asked
assert conv["respondent"]["kind"] == "agent" and conv["opener"]["name"] == "Yuki", conv
PY
python3 - "$WORK/export.csv" "$ASKED" <<'PY' || fail "the messages file: $(head -c 3000 "$WORK/export.csv")"
import csv, io, sys
raw = open(sys.argv[1], "rb").read()
assert raw.startswith(b"\xef\xbb\xbf"), raw[:16]
rows = list(csv.DictReader(io.StringIO(raw.decode("utf-8-sig"), newline="")))
asked = [r for r in rows if r["message_id"] == sys.argv[2]]
assert len(asked) == 1 and asked[0]["status"] == "retracted" and asked[0]["body"] == "Is my essay on track?" and asked[0]["reason"] == "wrong draft", asked
PY
echo "  the question Yuki withdrew is in both files, with its text, marked retracted, and its file described"
# Replayed, it is the same export and gives no URL; its maker is given a file again.
KEY=export-cs101 call 200 POST /v1/conversation-exports "$ROOT" "{\"course_id\":\"$COURSE\"}"
[ "$(json "$WORK/body" 'd["result"]["export_id"] == "'"$EXPORT"'", "downloads" in d["result"]')" = "True False" ] ||
  fail "the export replayed: $(cat "$WORK/body")"
call 200 GET "/v1/conversation-exports/$EXPORT/csv" "$ROOT"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(json "$WORK/body" 'd["result"]["download_url"]')")" = 200 ] || fail "the export's file, given again, does not download"
call 404 GET "/v1/conversation-exports/$EXPORT/csv" "$ADMIN" # an export is its maker's
# Ada exports what is beneath her appointment, and nothing else.
call 200 POST /v1/conversation-exports "$ADA" "{\"within_dept_id\":\"$ENG\"}"
[ "$(json "$WORK/body" 'd["result"]["conversations"]')" = 0 ] || fail "Ada's export of Engineering: $(cat "$WORK/body")"
call 403 POST /v1/conversation-exports "$ADA" "{\"course_id\":\"$COURSE\"}"
[ "$(reason)" = department_out_of_scope ] || fail "CS101's export refused Ada, but not as out of her reach: $(cat "$WORK/body")"
call 403 POST /v1/conversation-exports "$ADA" '{}'
[ "$(reason)" = platform_role_required ] || fail "the site's export refused Ada, but not for want of a platform role: $(cat "$WORK/body")"
for who in "$SATO" "$YUKI" "$GRADER"; do
  call 403 POST /v1/conversation-exports "$who" "{\"course_id\":\"$COURSE\"}"
  [ "$(reason)" = platform_role_required ] || fail "an export refused, but not for want of a platform role: $(cat "$WORK/body")"
done

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
[ "$(json "$WORK/body" 'd == {"password": True, "password_accepts": ["login_id", "email"], "sso": None, "sso_providers": []}')" = True ] ||
  fail "the sign-in methods: $(cat "$WORK/body")"
curl -s -o /dev/null -D "$WORK/headers" "$BASE/v1/auth/methods"
grep -qi '^cache-control: public, max-age=60' "$WORK/headers" || fail "the sign-in methods may not be kept for a minute: $(cat "$WORK/headers")"

step "Restarted with single sign-on against a stand-in provider, the sign-in page is told to offer it too, by name"
# The stand-in provider (scripts/e2e-idp.py) serves the operator's issuer,
# /adfs, as its discovery document, which is all the server reads of a
# provider before anyone signs in; and, at /site, a provider of the site's
# that signs in Mori, whose identity there is mori@campus.example, with the
# client and the secret made here, and sends the browser back only to the
# redirect URI the server says to register.
ISSUER="http://127.0.0.1:$IDP_PORT/adfs"
SITE_ISSUER="http://127.0.0.1:$IDP_PORT/site"
SITE_SECRET=$(python3 -c 'import secrets; print("e2e-" + secrets.token_urlsafe(24))')
IDP_CLIENT_ID=aishie-site IDP_CLIENT_SECRET=$SITE_SECRET IDP_REDIRECT_URI="$BASE/v1/auth/sso/callback" \
  IDP_SUBJECT=mori@campus.example IDP_EMAIL=mori@example.edu \
  python3 "$(dirname "$0")/e2e-idp.py" "$IDP_PORT" >"$WORK/idp.log" 2>&1 &
IDP_PID=$!
# It makes its RSA key in Python as it starts: a second or two, and on a busy
# machine half a minute.
for _ in $(seq 1 600); do
  curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null 2>&1 && break
  kill -0 "$IDP_PID" 2>/dev/null || break
  sleep 0.1
done
curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null || fail "the stand-in provider did not come up: $(cat "$WORK/idp.log")"
export OIDC_ISSUER="$ISSUER" OIDC_CLIENT_ID=aishie-e2e OIDC_PROVIDER_NAME=school-adfs OIDC_DISPLAY_NAME="School NetID"
# A name the button cannot show as it is, and the server does not start.
OIDC_DISPLAY_NAME=$'School\tNetID' "$BIN" serve 2>"$WORK/refused.log" && fail "a server with a tab in OIDC_DISPLAY_NAME started"
grep -q 'OIDC_DISPLAY_NAME' "$WORK/refused.log" || fail "refused, but not for the name: $(cat "$WORK/refused.log")"
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
start
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" 'd == {"password": True, "password_accepts": ["login_id", "email"], "sso": {"label": "School NetID", "start": "/v1/auth/sso/start"}, "sso_providers": [{"id": "school-adfs", "label": "School NetID", "start": "/v1/auth/sso/start/school-adfs"}]}')" = True ] ||
  fail "the sign-in methods with single sign-on: $(cat "$WORK/body")"
[[ "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/v1/auth/sso/start?return_to=/courses")" == "302 $ISSUER/oauth2/authorize?"* ]] ||
  fail "where the answer says to start does not send the browser to the provider"
echo "  the answer names the button, says where to start, and says nothing else of the provider"

step "Without SECRETS_KEY, no provider of the site's is added; the operator's is listed, read-only"
call 200 GET /v1/sso/providers "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["can_add"], d["result"]["cannot_add_reason"], [(p["id"], p["source"], p["read_only"], p["status"]) for p in d["result"]["providers"]]')" = \
  "False secrets_key_missing [('school-adfs', 'operator', True, 'offered')]" ] || fail "the providers: $(cat "$WORK/body")"
[ "$(json "$WORK/body" 'd["result"]["redirect_uri"]')" = "$BASE/v1/auth/sso/callback" ] || fail "the redirect URI: $(cat "$WORK/body")"
SITE="{\"id\":\"campus\",\"display_name\":\"Campus ID\",\"issuer\":\"$SITE_ISSUER\",\"client_id\":\"aishie-site\",\"client_secret\":\"$SITE_SECRET\"}"
# The operator's provider, by the id the server gives it: OIDC_PROVIDER_NAME,
# or its default.
OPERATOR=$(json "$WORK/body" '[p["id"] for p in d["result"]["providers"] if p["source"] == "operator"][0]')
# At an issuer elsewhere than this machine: one here, where the stand-in is,
# would be refused for that first (below).
call 422 POST /v1/sso/providers "$ROOT" "${SITE/"$SITE_ISSUER"/https://idp.example.edu/site}"
[ "$(reason)" = secrets_key_missing ] || fail "refused, but not for want of a key: $(cat "$WORK/body")"

# urlencoded VALUE — VALUE escaped for a query string.
urlencoded() { python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }

step "Restarted with SECRETS_KEY, root finds the server reaches no provider of the site's on this machine; the operator's, there too, is tested as before"
SECRETS_KEY=$(python3 -c 'import base64, os; print(base64.b64encode(os.urandom(32)).decode())')
export SECRETS_KEY
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
start
for at in "$SITE_ISSUER" "http://localhost:$IDP_PORT/site" "https://169.254.169.254/latest"; do
  call 200 GET "/v1/sso/test?issuer=$(urlencoded "$at")" "$ROOT"
  [ "$(json "$WORK/body" 'd["result"]["ok"], "issuer_address_not_allowed" in d["result"]["problems"][0], d["result"]["discovery_url"]')" = "False True " ] ||
    fail "the test of an issuer on this machine: $(cat "$WORK/body")"
done
call 400 POST /v1/sso/providers "$ROOT" "$SITE"
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"], d["error"]["details"]["field"]')" = "issuer_address_not_allowed issuer" ] ||
  fail "a provider on this machine set up: $(cat "$WORK/body")"
# The stand-in serves the operator's discovery document and no key set: what
# matters here is that the document is read.
call 200 GET "/v1/sso/test?provider_id=$OPERATOR" "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["token_endpoint"], any("issuer_address_not_allowed" in p for p in d["result"]["problems"])')" = \
  "$ISSUER/oauth2/token False" ] || fail "the test of the operator's provider: $(cat "$WORK/body")"
ALLOWED='administrators may have this server reach identity providers on this machine'
grep -q "$ALLOWED" "$WORK/server.log" && fail "the server says private issuers are allowed, unasked"
echo "  refused for its address as it is tested and set up, saying nothing of it; the operator's provider is not held to it"

step "Restarted with SSO_ALLOW_PRIVATE_ISSUERS too, root tests the site's provider, sets it up and switches it on over the version read; its secret is said nowhere"
export SSO_ALLOW_PRIVATE_ISSUERS=1
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
start
grep -q "$ALLOWED" "$WORK/server.log" || fail "the server does not say private issuers are allowed"
call 200 GET "/v1/sso/test?issuer=$(urlencoded "$SITE_ISSUER")" "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["ok"], len(d["result"]["signing_keys"]), d["result"]["token_endpoint"]')" = "True 1 $SITE_ISSUER/token" ] ||
  fail "the test of the site's provider: $(cat "$WORK/body")"
call 200 GET "/v1/sso/test?issuer=http://127.0.0.1:$IDP_PORT/nothing" "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["ok"]')" = False ] || fail "an issuer with no provider passed its test: $(cat "$WORK/body")"
call 403 POST /v1/sso/providers "$ADA" "$SITE" # a department's administrator sets up none
[ "$(reason)" = platform_role_required ] || fail "refused, but not for want of a platform role: $(cat "$WORK/body")"
call 200 POST /v1/sso/providers "$ROOT" "$SITE"
[ "$(json "$WORK/body" 'd["result"]["status"], d["result"]["version"], d["result"]["client_secret_hint"][-4:]')" = \
  "disabled 1 ${SITE_SECRET: -4}" ] || fail "the provider set up: $(cat "$WORK/body")"
call 422 POST /v1/sso/providers/school-adfs "$ROOT" '{"version":1,"display_name":"Mine now"}'
[ "$(reason)" = set_by_operator ] || fail "the operator's provider was not refused as the operator's: $(cat "$WORK/body")"
IF_MATCH='"1"' call 200 POST /v1/sso/providers/campus/enabled "$ROOT" '{"enabled":true}'
[ "$(json "$WORK/body" 'd["result"]["status"], d["result"]["version"]')" = "offered 2" ] || fail "switched on: $(cat "$WORK/body")"
IF_MATCH='"1"' call 409 POST /v1/sso/providers/campus "$ROOT" '{"display_name":"Stale"}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"], d["error"]["details"]["current_version"]')" = "version_mismatch 2" ] ||
  fail "a write over an old version: $(cat "$WORK/body")"
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" '[(p["id"], p["label"], p["start"]) for p in d["sso_providers"]], d["sso"]["start"]')" = \
  "[('school-adfs', 'School NetID', '/v1/auth/sso/start/school-adfs'), ('campus', 'Campus ID', '/v1/auth/sso/start/campus')] /v1/auth/sso/start/school-adfs" ] ||
  fail "the sign-in methods with two providers: $(cat "$WORK/body")"
call 400 GET "/v1/auth/sso/start?return_to=/courses" "" # two providers: which?
[ "$(reason)" = provider_required ] || fail "a bare start with two providers: $(cat "$WORK/body")"
"$BIN" secrets rewrap >"$WORK/rewrap.out" 2>&1 || fail "secrets rewrap: $(cat "$WORK/rewrap.out")"
grep -q ': 0 sealed again, 1 already' "$WORK/rewrap.out" || fail "secrets rewrap: $(cat "$WORK/rewrap.out")"
for f in "$WORK/server.log" "$WORK/rewrap.out" "$WORK/idp.log"; do
  grep -qF "$SITE_SECRET" "$f" && fail "$f says the client secret"
done
call 200 GET /v1/sso/providers "$ROOT"
grep -qF "$SITE_SECRET" "$WORK/body" && fail "the providers' list says the client secret"
# The secret goes to psql on its input, not its command line.
[ "$({
  printf '\\set secret %s\n' "'$SITE_SECRET'"
  echo "SELECT count(*) FROM action WHERE strpos(payload::text, :'secret') > 0 OR strpos(coalesce(result::text, ''), :'secret') > 0"
  echo "  OR (action_type LIKE 'sso.%' AND payload ? 'client_secret');"
} | psql -X -At -d "$DB")" = 0 ] || fail "the action log records the client secret"
[ "$(psql -X -At -d "$DB" -c "SELECT count(*) FROM sso_provider WHERE client_secret_sealed LIKE 'v1.%' AND strpos(row_to_json(sso_provider)::text, 'e2e-') = 0")" = 1 ] ||
  fail "the provider's row keeps its secret otherwise than sealed"
echo "  set up, tested and switched on; its secret is in no answer, action, log line or column, but sealed"

step "Mori, linked at the site's provider, signs in through it as a browser does, and his session is his"
call 200 POST "/v1/actors/$MORI_ID/sso" "$ROOT" '{"provider":"campus","subject":"mori@campus.example"}'
# header FILE NAME — a header's value; cookie FILE NAME — the value of a cookie it sets.
header() { grep -i "^$2:" "$1" | head -1 | cut -d' ' -f2- | tr -d '\r'; }
cookie() { grep -i "^set-cookie: $2=" "$1" | head -1 | sed 's/^[^=]*=\([^;]*\).*/\1/' | tr -d '\r'; }
curl -s -o /dev/null -D "$WORK/h.start" "$BASE/v1/auth/sso/start/campus?return_to=/courses"
TO_IDP=$(header "$WORK/h.start" location)
[[ "$TO_IDP" == "$SITE_ISSUER/authorize?"* ]] || fail "the start sent the browser to $TO_IDP"
STATE=$(cookie "$WORK/h.start" ais_sso)
[ -n "$STATE" ] || fail "the start set no state cookie"
curl -s -o /dev/null -D "$WORK/h.idp" "$TO_IDP"
BACK=$(header "$WORK/h.idp" location)
[[ "$BACK" == "$BASE/v1/auth/sso/callback?"* ]] || fail "the provider sent the browser back to $BACK: $(cat "$WORK/h.idp")"
[ "$(curl -s -o "$WORK/body" -D "$WORK/h.back" -w '%{http_code}' -H "Cookie: ais_sso=$STATE" "$BACK")" = 302 ] || fail "the callback: $(cat "$WORK/body")"
[ "$(header "$WORK/h.back" location)" = /courses ] || fail "the callback sent the browser to $(header "$WORK/h.back" location)"
MORI=$(cookie "$WORK/h.back" ais_session)
[ -n "$MORI" ] || fail "the callback set no session"
N=$((N + 3))
printf '  %-4s %-62s %s\n' GET "/v1/auth/sso/start/campus → the provider → /v1/auth/sso/callback" 302
call 200 GET /v1/me "$MORI"
[ "$(json "$WORK/body" 'd["result"]["display_name"]')" = Mori ] || fail "signed in as someone else: $(cat "$WORK/body")"
# The same answer from the provider, again, even with the state cookie kept, signs in nobody: the provider redeems a code once.
[ "$(curl -s -o "$WORK/body" -D "$WORK/h.again" -w '%{http_code}' -H "Cookie: ais_sso=$STATE" "$BACK")" = 401 ] ||
  fail "a replayed callback: $(cat "$WORK/body")"
[ -z "$(cookie "$WORK/h.again" ais_session)" ] || fail "a replayed callback set a session"

step "Restarted without SSO_ALLOW_PRIVATE_ISSUERS, the server reaches the site's provider on this machine no more, nor offers it, and says why in sso.list and its log; the operator's still starts"
unset SSO_ALLOW_PRIVATE_ISSUERS
kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
LOGGED=$(wc -l <"$WORK/server.log")
start
call 422 GET "/v1/auth/sso/start/campus?return_to=/courses" ""
[ "$(reason)" = sso_provider_unavailable ] || fail "a sign-in through the provider on this machine: $(cat "$WORK/body")"
tail -n +$((LOGGED + 1)) "$WORK/server.log" | grep -q 'issuer_address_not_allowed' || fail "the log does not say why: $(tail -5 "$WORK/server.log")"
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" '[p["id"] for p in d["sso_providers"]]')" = "['school-adfs']" ] ||
  fail "the sign-in page offers the provider on this machine: $(cat "$WORK/body")"
call 200 GET /v1/sso/providers/campus "$ROOT"
[ "$(json "$WORK/body" 'd["result"]["status"], d["result"]["enabled"]')" = "issuer_address_not_allowed True" ] ||
  fail "the provider on this machine, as administrators read it: $(cat "$WORK/body")"
[[ "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/v1/auth/sso/start/$OPERATOR?return_to=/courses")" == "302 $ISSUER/oauth2/authorize?"* ]] ||
  fail "the operator's provider does not start a sign-in"
N=$((N + 1))

step "Root cannot remove the provider while Mori is linked at it; forced, it goes, and his identity is unlinked"
call 409 POST /v1/sso/providers/campus/delete "$ROOT" '{}'
[ "$(json "$WORK/body" 'd["error"]["details"]["reason"], d["error"]["details"]["linked_accounts"]')" = "provider_in_use 1" ] ||
  fail "a provider with linked accounts: $(cat "$WORK/body")"
call 200 POST /v1/sso/providers/campus/delete "$ROOT" '{"force":true}'
[ "$(json "$WORK/body" 'd["result"]["unlinked_accounts"]')" = 1 ] || fail "removed: $(cat "$WORK/body")"
call 404 GET "/v1/auth/sso/start/campus?return_to=/courses" ""
call 200 GET /v1/auth/methods ""
[ "$(json "$WORK/body" '[p["id"] for p in d["sso_providers"]]')" = "['school-adfs']" ] || fail "the sign-in methods after: $(cat "$WORK/body")"

printf '\n\033[32mPASS\033[0m %d requests\n' "$N"
