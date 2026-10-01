package memory_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/memory"
	"github.com/AIShie-Education/AIShie-Core/internal/testdb"
)

// Random text from every kind of script, and from what to_tsquery reads as
// syntax, through QueryTerms into to_tsquery: the database takes every one.
func TestQueryTermsInTheDatabase(t *testing.T) {
	pool := testdb.New(t)
	ranges := [][2]rune{
		{0x20, 0x7e},     // ASCII, & | ! ( ) : * ' \ < > among it
		{0xa0, 0x24f},    // Latin-1 and Latin Extended
		{0x300, 0x36f},   // combining marks
		{0x370, 0x3ff},   // Greek
		{0x400, 0x4ff},   // Cyrillic
		{0x590, 0x6ff},   // Hebrew, Arabic
		{0x900, 0x97f},   // Devanagari
		{0xe00, 0xe7f},   // Thai
		{0x2000, 0x206f}, // punctuation, zero-width and direction marks
		{0x3000, 0x30ff}, // CJK punctuation, Hiragana, Katakana
		{0x4e00, 0x9fff}, // Han
		{0xac00, 0xd7a3}, // Hangul
		{0xff00, 0xffef}, // full- and half-width forms
		{0x1f300, 0x1faff},
	}
	rng := rand.New(rand.NewPCG(9, 2026))
	var inputs []string
	for range 4000 {
		var b strings.Builder
		for range rng.IntN(60) {
			r := ranges[rng.IntN(len(ranges))]
			b.WriteRune(r[0] + rune(rng.IntN(int(r[1]-r[0]+1))))
		}
		inputs = append(inputs, b.String())
	}
	queries := make([]string, len(inputs))
	for i, in := range inputs {
		queries[i] = memory.QueryTerms(in)
	}
	ctx := context.Background()
	for start := 0; start < len(queries); start += 500 {
		batch := queries[start:min(start+500, len(queries))]
		if _, err := pool.Exec(ctx, `SELECT to_tsquery('simple', q) FROM unnest($1::text[]) q`, batch); err != nil {
			for _, q := range batch { // find the one
				if _, err := pool.Exec(ctx, `SELECT to_tsquery('simple', $1)`, q); err != nil {
					t.Fatalf("to_tsquery(%q): %v", q, err)
				}
			}
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT to_tsvector('simple', memory_search_text) FROM unnest($1::text[]) memory_search_text`,
		searchTexts(inputs)); err != nil {
		t.Fatal(err)
	}
}

func searchTexts(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = memory.SearchText(s)
	}
	return out
}

// The same as a fuzz target: go test -fuzz FuzzQueryTermsInTheDatabase
// ./internal/memory/ looks further than the random text above.
func FuzzQueryTermsInTheDatabase(f *testing.F) {
	pool := testdb.New(f)
	for _, s := range []string{"", "Recursion help", "递归函数", `a & b | !(c) <-> d:* 'e' \f`, "\xff\xfe", "ｶﾞｰ", "́́", "x:*:*"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if _, err := pool.Exec(context.Background(), `SELECT to_tsquery('simple', $1)`, memory.QueryTerms(s)); err != nil {
			t.Fatalf("QueryTerms(%q) = %q: %v", s, memory.QueryTerms(s), err)
		}
	})
}

// searchWorld is one agent with its owner, and the action its entries name.
type searchWorld struct {
	pool          *pgxpool.Pool
	agent, action uuid.UUID
	owner         uuid.UUID
}

func newSearchWorld(t *testing.T) *searchWorld {
	t.Helper()
	w := &searchWorld{pool: testdb.New(t), agent: ids.New(), action: ids.New(), owner: ids.New()}
	w.exec(t, `INSERT INTO actor (id, kind, display_name) VALUES ($1, 'human', 'Yuki')`, w.owner)
	w.exec(t, `INSERT INTO actor (id, kind, display_name, owner_actor_id, hosting) VALUES ($1, 'agent', 'helper', $2, 'mcp')`, w.agent, w.owner)
	w.exec(t, `INSERT INTO action (id, actor_id, action_type, target_type, payload_hash, idempotency_key, authz_result, status, executed_at)
	           VALUES ($1, $2, 'memory.write', 'memory', repeat('0', 64), 'k', 'autonomous', 'executed', now())`, w.action, w.agent)
	return w
}

func (w *searchWorld) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

// write keeps text about the owner, as last changed at, and returns its id.
func (w *searchWorld) write(t *testing.T, text string, at time.Time, pinned bool) uuid.UUID {
	t.Helper()
	body, e := memory.CheckText(text)
	if e != nil {
		t.Fatal(e)
	}
	id, err := dbq.New(w.pool).InsertMemory(context.Background(), dbq.InsertMemoryParams{
		ID: ids.New(), HolderActorID: w.agent, Scope: memory.ScopeOwner, SubjectActorID: &w.owner, Status: memory.StatusActive,
		Body: &body, SearchText: memory.SearchText(body), TextHash: memory.Hash(body), Tags: []string{}, Pinned: pinned,
		Source: memory.SourceAgent, ActorID: w.agent, ActionID: w.action, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *searchWorld) search(t *testing.T, query string, onlyMatches bool, now time.Time) []uuid.UUID {
	t.Helper()
	rows, err := dbq.New(w.pool).SearchMemory(context.Background(), dbq.SearchMemoryParams{Query: memory.QueryTerms(query),
		HolderActorID: w.agent, Buckets: []string{memory.ScopeOwner}, OnlyMatches: onlyMatches, Now: now, MaxRows: 50})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// What is written is found by its words, in English, Chinese, Japanese and
// Korean alike; the pinned come first, then the most relevant, then the
// newest; and the same text twice in a bucket is one entry.
func TestSearchFindsWhatWasWritten(t *testing.T) {
	w := newSearchWorld(t)
	now := time.Now()
	day := 24 * time.Hour
	python := w.write(t, "Prefers worked examples in Python before theory.", now.Add(-40*day), false)
	recursion := w.write(t, "Finds recursion hard; base cases first helps.", now.Add(-2*day), false)
	chinese := w.write(t, "他觉得递归函数很难。", now.Add(-3*day), false)
	japanese := w.write(t, "再帰関数が苦手です。", now.Add(-4*day), false)
	korean := w.write(t, "재귀 함수를 어려워함", now.Add(-5*day), false)
	final := w.write(t, "Working towards the CS101 final on 12 December.", now.Add(-day), false)
	pinned := w.write(t, "Call me Yuki, not Ms Tanaka.", now.Add(-90*day), true)

	// The parser sees letters in what is not ASCII only in a UTF-8
	// database, which is what Core is deployed on; a server made with
	// SQL_ASCII finds nothing of Chinese, Japanese or Korean.
	var encoding string
	if err := w.pool.QueryRow(context.Background(), `SHOW server_encoding`).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  []uuid.UUID // in order, pinned first
		cjk   bool
	}{
		{"python", []uuid.UUID{pinned, python}, false},
		{"PYTHON examples", []uuid.UUID{pinned, python}, false},
		{"recurs", []uuid.UUID{pinned, recursion}, false},
		{"递归", []uuid.UUID{pinned, chinese}, true},
		{"递归函数难", []uuid.UUID{pinned, chinese}, true},
		{"再帰関数", []uuid.UUID{pinned, japanese}, true},
		{"再帰", []uuid.UUID{pinned, japanese}, true},
		{"함수", []uuid.UUID{pinned, korean}, true},
		{"ＣＳ１０１", []uuid.UUID{pinned, final}, false},
		{"nothing here", []uuid.UUID{pinned}, false},
	} {
		if tc.cjk && encoding != "UTF8" {
			t.Logf("search %q: not in a %s database", tc.query, encoding)
			continue
		}
		if got := w.search(t, tc.query, true, now); !equal(got, tc.want) {
			t.Errorf("search %q: %v, want %v", tc.query, got, tc.want)
		}
	}

	// Without a query, or ranked without dropping what does not match: the
	// pinned, then the best match, then the newest.
	if got := w.search(t, "", false, now); !equal(got, []uuid.UUID{pinned, final, recursion, chinese, japanese, korean, python}) {
		t.Errorf("by recency: %v", got)
	}
	if got := w.search(t, "python theory", false, now); len(got) != 7 || got[0] != pinned || got[1] != python {
		t.Errorf("ranked by the question: %v", got)
	}

	// The same text again in the bucket is the entry already there.
	body, _ := memory.CheckText("  Finds recursion hard; base cases first helps.\r\n")
	if _, err := dbq.New(w.pool).InsertMemory(context.Background(), dbq.InsertMemoryParams{
		ID: ids.New(), HolderActorID: w.agent, Scope: memory.ScopeOwner, SubjectActorID: &w.owner, Status: memory.StatusActive,
		Body: &body, SearchText: memory.SearchText(body), TextHash: memory.Hash(body), Tags: []string{}, Source: memory.SourceAgent,
		ActorID: w.agent, ActionID: w.action, At: now}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the same text again: %v", err)
	}
	there, err := dbq.New(w.pool).GetMemoryByHash(context.Background(), dbq.GetMemoryByHashParams{HolderActorID: w.agent,
		Bucket: memory.ScopeOwner, TextHash: memory.Hash(body)})
	if err != nil || there.ID != recursion {
		t.Fatalf("the entry there: %v %v", there.ID, err)
	}
}

func equal(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
