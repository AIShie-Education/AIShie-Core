#!/usr/bin/env bash
# End to end, from the outside: the real binary, a scratch database, and
# nothing but curl. It bootstraps an installation and then builds the worked
# example from docs/schema.md §5 entirely through the REST API — register the
# actors, create and open the course, seat the instructor, set up grading,
# publish an assignment, hand in work, have an agent grade it, approve, post;
# and have the instructor's own tutor agent answer the student's question.
#
#   make e2e            (builds first)
#   scripts/e2e.sh      (expects bin/aishiterud)
#
# Uses the PG* environment for createdb/dropdb, like `make db-test-sql`.
set -euo pipefail

BIN=${BIN:-bin/aishiterud}
PORT=${PORT:-18099}
DB="aishiteru_e2e_$$"
BASE="http://127.0.0.1:$PORT"
WORK=$(mktemp -d)
SERVER_PID=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null && wait "$SERVER_PID" 2>/dev/null || true
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
  printf '  %-4s %-62s %s\n' "$method" "$path" "$got"
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
"$BIN" migrate up
"$BIN" seed

step "bootstrap: the one actor created by nobody"
# An empty password is refused before anything is made: the bootstrap
# after it is still the first.
printf '\n' | "$BIN" bootstrap --name Root --email root@example.edu --password-stdin >/dev/null 2>&1 &&
  fail "bootstrap took an empty password"
ROOT=$("$BIN" bootstrap --name Root 2>/dev/null)
[[ $ROOT == ais_* ]] || fail "bootstrap printed no token"
"$BIN" bootstrap --name Usurper >/dev/null 2>&1 && fail "bootstrap ran twice"
echo "  root token ${ROOT:0:16}…; an empty password and a second bootstrap are refused"

"$BIN" serve 2>"$WORK/server.log" &
SERVER_PID=$!
for _ in $(seq 1 50); do curl -sf "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$BASE/healthz" >/dev/null || fail "the server did not come up"

# ---------------------------------------------------------------------------
step "Root makes an admin; the admin registers everyone and gives each a token"
call 401 GET /v1/me "not-a-token"
call 200 POST /v1/actors "$ROOT" '{"kind":"human","display_name":"Admin","platform_role":"admin"}'
ADMIN_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
call 200 POST "/v1/actors/$ADMIN_ID/tokens" "$ROOT" '{"label":"e2e"}'
ADMIN=$(json "$WORK/body" 'd["result"]["token"]')

register() { # KIND NAME → sets ACTOR_ID and TOKEN
  call 200 POST /v1/actors "$ADMIN" "{\"kind\":\"$1\",\"display_name\":\"$2\"}"
  ACTOR_ID=$(json "$WORK/body" 'd["result"]["actor_id"]')
  call 200 POST "/v1/actors/$ACTOR_ID/tokens" "$ADMIN" '{"label":"e2e"}'
  TOKEN=$(json "$WORK/body" 'd["result"]["token"]')
}
register human Sato;      SATO_ID=$ACTOR_ID;   SATO=$TOKEN
register human Yuki;      YUKI_ID=$ACTOR_ID;   YUKI=$TOKEN
register agent grader-v2; GRADER_ID=$ACTOR_ID; GRADER=$TOKEN

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

step "Sato brings in a tutor agent of his own; Yuki asks it; it finds the question in its inbox and answers"
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
json "$WORK/body" '"'"$TUTOR_M"'" in [r["member_id"] for r in d["result"]["respondents"]] or sys.exit("the tutor is not offered to Yuki")' >/dev/null
call 200 POST "$C/conversations" "$YUKI" "{\"respondent_member_id\":\"$TUTOR_M\",\"body\":\"What does HW3 ask for?\"}"
CONV=$(json "$WORK/body" 'd["result"]["conversation_id"]')
call 200 GET "$C/conversations/inbox" "$TUTOR"
QUESTION=$(json "$WORK/body" 'd["result"]["conversations"][0]["latest_opener_message_id"]')
KEY="answer:$CONV:$QUESTION:1" call 200 POST "$C/conversations/$CONV/answer" "$TUTOR" "{\"in_reply_to_message_id\":\"$QUESTION\",\"body\":\"An essay with a thesis.\"}"
call 200 GET "$C/conversations/$CONV/messages" "$YUKI"
[ "$(json "$WORK/body" 'd["result"]["messages"][-1]["body"]')" = "An essay with a thesis." ] || fail "Yuki does not see the answer"
call 404 GET "$C/conversations/$CONV" "$GRADER" # nobody else's to read

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

printf '\n\033[32mPASS\033[0m %d requests\n' "$N"
