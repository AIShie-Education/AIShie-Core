#!/usr/bin/env bash
# End to end, from the outside: the real binary, a scratch database, and
# nothing but curl. It bootstraps an installation and then builds the worked
# example from docs/schema.md §5 entirely through the REST API — register the
# actors, create and open the course, seat the instructor, set up grading,
# publish an assignment, hand in work, have an agent grade it, approve, post.
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
"$BIN" migrate up
"$BIN" seed

step "bootstrap: the one actor created by nobody"
ROOT=$("$BIN" bootstrap --name Root 2>/dev/null)
[[ $ROOT == ais_* ]] || fail "bootstrap printed no token"
"$BIN" bootstrap --name Usurper >/dev/null 2>&1 && fail "bootstrap ran twice"
echo "  root token ${ROOT:0:16}…; a second bootstrap is refused"

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

printf '\n\033[32mPASS\033[0m %d requests\n' "$N"
