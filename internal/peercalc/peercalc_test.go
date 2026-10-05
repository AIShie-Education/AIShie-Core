package peercalc

import (
	"math/big"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	ana, ben, cai, dev = id(0xa), id(0xb), id(0xc), id(0xd)
	stranger           = id(0xe)
)

func id(n byte) uuid.UUID { return uuid.UUID{15: n} }

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func shares(rater uuid.UUID, given map[uuid.UUID]int) Sheet {
	s := Sheet{Rater: rater, Entries: map[uuid.UUID]Entry{}}
	for m, n := range given {
		s.Entries[m] = Entry{Share: n}
	}
	return s
}

func ratings(rater uuid.UUID, given map[uuid.UUID]map[string]int) Sheet {
	s := Sheet{Rater: rater, Entries: map[uuid.UUID]Entry{}}
	for m, r := range given {
		s.Entries[m] = Entry{Ratings: r}
	}
	return s
}

func wantFactor(t *testing.T, r Result, m uuid.UUID, want string) {
	t.Helper()
	if got := r.Members[m].Factor; !got.Equal(dec(want)) {
		t.Fatalf("factor of %s: %s, want %s", m, got, want)
	}
}

func wantFlags(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(SortedFlags(got), SortedFlags(want)) {
		t.Fatalf("flags %v, want %v", got, want)
	}
}

// The design's example (docs/schema.md §2.5b): a group of four, a share
// form, self-evaluation off, a weight of 20 % and a group score of 80 of 100.
func TestTheWorkedExample(t *testing.T) {
	form := Form{Kind: Share}
	circle := []uuid.UUID{ana, ben, cai, dev}
	sheets := []Sheet{
		shares(ana, map[uuid.UUID]int{ben: 40, cai: 40, dev: 20}),
		shares(ben, map[uuid.UUID]int{ana: 40, cai: 40, dev: 20}),
		shares(cai, map[uuid.UUID]int{ana: 40, ben: 40, dev: 20}),
		shares(dev, map[uuid.UUID]int{ana: 40, ben: 30, cai: 30}),
	}
	r := Compute(form, circle, sheets)
	g, p := dec("80"), dec("100")
	sum, adjusted := decimal.Zero, decimal.Zero
	for _, c := range []struct {
		m             uuid.UUID
		factor, score string
	}{{ana, "1.2", "83.2"}, {ben, "1.1", "81.6"}, {cai, "1.1", "81.6"}, {dev, "0.6", "73.6"}} {
		wantFactor(t, r, c.m, c.factor)
		s := Score(g, p, 20, r.Members[c.m].Factor, false)
		if !s.Equal(dec(c.score)) {
			t.Fatalf("score of %s: %s, want %s", c.m, s, c.score)
		}
		if r.Members[c.m].Raters != 3 || r.Members[c.m].PeerRaters != 3 || r.Members[c.m].SelfFactor != nil {
			t.Fatalf("who rated %s: %+v", c.m, r.Members[c.m])
		}
		sum, adjusted = sum.Add(r.Members[c.m].Factor), adjusted.Add(s.Sub(g))
	}
	if !sum.Equal(dec("4")) || !adjusted.IsZero() {
		t.Fatalf("the factors add up to %s and the adjustments to %s, want 4 and 0", sum, adjusted)
	}
	wantFlags(t, r.Members[dev].Flags, FlagLow)
	wantFlags(t, r.Members[ana].Flags) // 1.2 is not above 1.2
	for _, m := range circle {
		if rr := r.Raters[m]; !rr.Counted || rr.Rated != 3 || len(rr.Flags) != 0 {
			t.Fatalf("rater %s: %+v", m, rr)
		}
	}
	if len(r.Flags) != 0 {
		t.Fatalf("the circle's flags: %v", r.Flags)
	}
	received := ReceivedBy(form, circle, sheets)
	if p := received[ana].SharePercent; p == nil || !p.Equal(dec("120")) || received[dev].SharePercent.String() != "60" {
		t.Fatalf("what peers gave: %+v", received)
	}

	// Had Dev sent no sheet: Ana, Ben and Cai each rated by two (an even
	// share 2/3), each given 0.80, so 1.2; Dev, by all three, 0.6; weighted
	// by what each could receive, they add up to three, one for each rater.
	r = Compute(form, circle, sheets[:3])
	for _, m := range []uuid.UUID{ana, ben, cai} {
		wantFactor(t, r, m, "1.2")
	}
	wantFactor(t, r, dev, "0.6")
	wantFlags(t, r.Raters[dev].Flags, FlagMissing)
	if r.Raters[dev].Counted {
		t.Fatal("Dev sent no sheet and counts")
	}
}

// A rating form: each criterion's weight in what a rater gives; worked out
// exactly and given to ten places, half away from zero; the factors still
// add up to the circle's size.
func TestARatingFormWeighsItsCriteria(t *testing.T) {
	form := Form{Kind: Rating, Criteria: []Criterion{{Key: "quality", Weight: dec("2")}, {Key: "timeliness", Weight: dec("1")}}}
	circle := []uuid.UUID{ana, ben, cai}
	q := func(quality, timeliness int) map[string]int {
		return map[string]int{"quality": quality, "timeliness": timeliness}
	}
	sheets := []Sheet{
		ratings(ana, map[uuid.UUID]map[string]int{ben: q(5, 4), cai: q(3, 3)}), // 14 and 9 of 23
		ratings(ben, map[uuid.UUID]map[string]int{ana: q(4, 4), cai: q(2, 5)}), // 12 and 9 of 21
		ratings(cai, map[uuid.UUID]map[string]int{ana: q(5, 5), ben: q(5, 5)}), // the same to both
	}
	r := Compute(form, circle, sheets)
	wantFactor(t, r, ana, "1.0714285714") // 15/14
	wantFactor(t, r, ben, "1.1086956522") // 51/46
	wantFactor(t, r, cai, "0.8198757764") // 132/161
	total := new(big.Rat)
	for _, m := range circle {
		total.Add(total, r.Members[m].factor)
	}
	if total.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("the exact factors add up to %s", total)
	}
	wantFlags(t, r.Raters[cai].Flags, FlagUniform)
	wantFlags(t, r.Raters[ana].Flags)
	// 72.5 × (75 + 25 × 1.0714285714) / 100 = 73.7946…
	if s := Score(dec("72.5"), dec("100"), 25, r.Members[ana].Factor, false); !s.Equal(dec("73.79")) {
		t.Fatalf("Ana's score: %s", s)
	}
	received := ReceivedBy(form, circle, sheets)
	want := map[uuid.UUID][2]string{ana: {"4.5", "4.5"}, ben: {"5", "4.5"}, cai: {"2.5", "4"}}
	for m, w := range want {
		a := received[m].Averages
		if !a["quality"].Equal(dec(w[0])) || !a["timeliness"].Equal(dec(w[1])) || received[m].PeerRaters != 2 || received[m].SharePercent != nil {
			t.Fatalf("what %s received: %+v", m, received[m])
		}
	}
}

// A pair with self-evaluation off: each rater's one entry is the whole of
// what they hand out, so both factors are 1, and the circle says so. With
// self-evaluation on, a pair is moderated, and a member who gave themselves
// much more than their peer did is flagged.
func TestAPairIsModeratedOnlyWithSelfEvaluation(t *testing.T) {
	pair := []uuid.UUID{ana, ben}
	r := Compute(Form{Kind: Share}, pair, []Sheet{shares(ana, map[uuid.UUID]int{ben: 100}), shares(ben, map[uuid.UUID]int{ana: 100})})
	wantFactor(t, r, ana, "1")
	wantFactor(t, r, ben, "1")
	wantFlags(t, r.Flags, FlagPairWithoutSelf)

	self := Form{Kind: Share, SelfEvaluation: true}
	r = Compute(self, pair, []Sheet{shares(ana, map[uuid.UUID]int{ana: 60, ben: 40}), shares(ben, map[uuid.UUID]int{ana: 55, ben: 45})})
	wantFactor(t, r, ana, "1.15")
	wantFactor(t, r, ben, "0.85")
	if len(r.Flags) != 0 || r.Members[ana].Raters != 2 || r.Members[ana].PeerRaters != 1 ||
		!r.Members[ana].SelfFactor.Equal(dec("1.2")) || !r.Members[ana].PeerFactor.Equal(dec("1.1")) {
		t.Fatalf("Ana with self-evaluation: %+v %v", r.Members[ana], r.Flags)
	}
	wantFlags(t, r.Members[ana].Flags)

	r = Compute(self, pair, []Sheet{shares(ana, map[uuid.UUID]int{ana: 90, ben: 10}), shares(ben, map[uuid.UUID]int{ana: 50, ben: 50})})
	wantFactor(t, r, ana, "1.4")
	wantFactor(t, r, ben, "0.6")
	wantFlags(t, r.Members[ana].Flags, FlagHigh, FlagSelfAbovePeers) // gave herself 1.8 against 1.0 from Ben
	// Ben gave himself an even share, 1.0 against the 0.2 Ana gave him.
	wantFlags(t, r.Members[ben].Flags, FlagLow, FlagSelfAbovePeers)
}

// Only what a member of the circle gave another counts: a sheet of someone
// outside it, an entry about someone outside it, a rater's own entry with
// self-evaluation off; a rater who hands out nothing drops out; a member
// nobody rated is given 1.
func TestOnlyTheCircleCounts(t *testing.T) {
	form := Form{Kind: Rating, Criteria: []Criterion{{Key: "effort", Weight: dec("1")}}}
	circle := []uuid.UUID{ana, ben, cai}
	e := func(n int) map[string]int { return map[string]int{"effort": n} }
	r := Compute(form, circle, []Sheet{
		ratings(stranger, map[uuid.UUID]map[string]int{ana: e(0), ben: e(10)}),           // left the group
		ratings(ana, map[uuid.UUID]map[string]int{ana: e(10), ben: e(3), cai: e(1)}),     // herself, off
		ratings(ana, map[uuid.UUID]map[string]int{ben: e(9)}),                            // a second sheet of hers: passed over
		ratings(ben, map[uuid.UUID]map[string]int{ana: e(0), cai: e(0)}),                 // nothing to hand out
		ratings(cai, map[uuid.UUID]map[string]int{stranger: e(5), ana: e(2), ben: e(2)}), // the stranger passed over
	})
	// Ana: from Cai 1/2 of an even 1/2. Ben: from Ana 3/4 and from Cai 1/2,
	// of an even 1/2 + 1/2. Cai: from Ana 1/4, of an even 1/2.
	wantFactor(t, r, ana, "1")
	wantFactor(t, r, ben, "1.25")
	wantFactor(t, r, cai, "0.5")
	if r.Raters[ben].Counted || r.Raters[ana].Rated != 2 {
		t.Fatalf("the raters: %+v", r.Raters)
	}
	wantFlags(t, r.Raters[ben].Flags, FlagUniform)
	if _, there := r.Raters[stranger]; there {
		t.Fatal("a rater outside the circle is a rater of it")
	}
	if r.Members[ana].Raters != 1 {
		t.Fatalf("Ana rated by %d", r.Members[ana].Raters)
	}

	// Nobody rated: 1, no flag.
	r = Compute(Form{Kind: Share}, circle, nil)
	for _, m := range circle {
		wantFactor(t, r, m, "1")
		wantFlags(t, r.Members[m].Flags)
		wantFlags(t, r.Raters[m].Flags, FlagMissing)
	}
	if got := ReceivedBy(Form{Kind: Share}, circle, nil); got[ana].SharePercent != nil || got[ana].PeerRaters != 0 {
		t.Fatalf("received from nobody: %+v", got[ana])
	}
}

// The score is rounded once, to two places, half away from zero; never
// below zero; never above the points possible unless the group grade allows
// extra; and a weight of 0 leaves the group's score as it is.
func TestTheScore(t *testing.T) {
	for _, c := range []struct {
		g, p   string
		weight int32
		factor string
		extra  bool
		want   string
	}{
		{"1", "10", 10, "1.05", false, "1.01"},    // 1.005, half away from zero
		{"80", "100", 0, "0.2", false, "80"},      // reference only
		{"80", "100", 100, "0", false, "0"},       // all of it, nothing received
		{"95", "100", 50, "1.4", false, "100"},    // capped
		{"95", "100", 50, "1.4", true, "114"},     // extra allowed
		{"120", "100", 10, "1", true, "120"},      // already extra
		{"33.333", "50", 30, "1", false, "33.33"}, // rounded even where nothing moves it
		{"72.5", "100", 25, "1.0714285714", false, "73.79"},
	} {
		if got := Score(dec(c.g), dec(c.p), c.weight, dec(c.factor), c.extra); !got.Equal(dec(c.want)) {
			t.Errorf("%+v: %s", c, got)
		}
	}
	if got := Decimal(big.NewRat(-1, 8), 2); !got.Equal(dec("-0.13")) {
		t.Fatalf("a negative half: %s", got)
	}
}
