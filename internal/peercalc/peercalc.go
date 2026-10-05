// Package peercalc works out peer evaluation within a group (docs/schema.md
// §2.5b, Peer evaluation): what each member of a group's circle received
// from the others, as a factor against an even share, the score that factor
// gives a member's grade at the form's weight, and the flags a teacher reads
// beside them. It is pure, so that a page can work the same numbers out and
// a test pin them.
//
// The factor (WebPA's fair share). For a circle C of n members and the
// raters R ⊆ C with a sheet, x(r, m) is what r gave m: for a rating form the
// sum over the criteria of the criterion's weight times the rating, for a
// share form the share. A rater's entries about members of C (k_r of them:
// n − 1, or n with self-evaluation) are normalised to fractions,
// f(r, m) = x(r, m) / Σ x(r, ·); a rater whose entries sum to zero hands out
// nothing and drops out of R. Then
//
//	F(m) = Σ_{r ∈ R rating m} f(r, m)  /  Σ_{r ∈ R rating m} 1/k_r
//
// what m received over what an even share from the same raters would be,
// and 1 when nobody in R rated m. Each rater hands out exactly 1, so an even
// contributor's F is 1. It is worked out exactly, as a fraction, and given to
// ten decimal places, half away from zero (decimal.DivRound(…, 10)).
//
// The score from group score G at weight w (a percentage) is
//
//	S(m) = round2(G × (1 − w/100 + w/100 × F(m)))
//
// rounded once, at the end, to two places, half away from zero
// (decimal.Round(2)), floored at 0 and capped at the points possible unless
// the group grade allows extra.
package peercalc

import (
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The kinds of form.
const (
	// Rating: each member is rated on each criterion, an integer on the
	// form's scale.
	Rating = "rating"
	// Share: each rater splits exactly 100 points among those they evaluate.
	Share = "share"
)

// ShareTotal is what a rater splits among those they evaluate, on a share
// form.
const ShareTotal = 100

// FactorPlaces is how many decimal places a factor is given to.
const FactorPlaces = 10

// The flags a member, a rater or a group is given, and their thresholds.
const (
	// FlagLow: a factor below LowBelow.
	FlagLow = "low"
	// FlagHigh: a factor above HighAbove.
	FlagHigh = "high"
	// FlagSelfAbovePeers: with self-evaluation on, the share a member gave
	// themselves, against an even one, is SelfAbovePeersBy or more above
	// their factor from their peers alone.
	FlagSelfAbovePeers = "self_above_peers"
	// FlagUniform: a rater gave everyone they rated, two or more, the same.
	FlagUniform = "uniform"
	// FlagMissing: a member of the circle with no sheet about it.
	FlagMissing = "missing"
	// FlagPairWithoutSelf: a circle of two with self-evaluation off, in
	// which each rater's one entry is the whole of what they hand out, so
	// that every factor is 1: a pair is moderated only with
	// self-evaluation on.
	FlagPairWithoutSelf = "pair_without_self_evaluation"
)

var (
	// LowBelow, HighAbove and SelfAbovePeersBy are the flags' thresholds.
	LowBelow         = big.NewRat(4, 5)
	HighAbove        = big.NewRat(6, 5)
	SelfAbovePeersBy = big.NewRat(3, 10)
)

// Criterion is one criterion of a rating form: its key, and its weight in
// what a rater gives a member.
type Criterion struct {
	Key    string
	Weight decimal.Decimal
}

// Form is what the calculation needs of a peer form.
type Form struct {
	Kind           string
	Criteria       []Criterion
	SelfEvaluation bool
}

// Entry is what a rater gave one member: ratings by criterion key (a rating
// form), or a share (a share form).
type Entry struct {
	Ratings map[string]int
	Share   int
}

// Sheet is a rater's current sheet: their entries by the member rated.
type Sheet struct {
	Rater   uuid.UUID
	Entries map[uuid.UUID]Entry
}

// Member is what one member of the circle received.
type Member struct {
	// Factor is F, to ten places: 1 when nobody rated them.
	Factor decimal.Decimal
	// Raters is how many raters with a sheet rated them, themselves
	// included where they rated themselves; PeerRaters leaves them out.
	Raters, PeerRaters int
	// PeerFactor is their factor from their peers alone, nil when no peer
	// rated them; SelfFactor what they gave themselves against an even
	// share, nil when they did not.
	PeerFactor, SelfFactor *decimal.Decimal
	// Flags: low, high, self_above_peers.
	Flags []string

	factor, peer, self *big.Rat
}

// Rater is what one member of the circle did as a rater.
type Rater struct {
	// Counted: their sheet rates someone of the circle and hands out more
	// than nothing, so it counts.
	Counted bool
	// Rated is how many members of the circle their sheet rates.
	Rated int
	// Flags: uniform, missing.
	Flags []string
}

// Result is the calculation for one circle.
type Result struct {
	Members map[uuid.UUID]Member
	Raters  map[uuid.UUID]Rater
	// Flags of the circle as a whole: pair_without_self_evaluation.
	Flags []string
}

// given is x(r, m): what an entry gives a member, exactly.
func (f Form) given(e Entry) *big.Rat {
	if f.Kind == Share {
		return new(big.Rat).SetInt64(int64(e.Share))
	}
	sum := new(big.Rat)
	for _, c := range f.Criteria {
		w := c.Weight.Rat()
		sum.Add(sum, w.Mul(w, new(big.Rat).SetInt64(int64(e.Ratings[c.Key]))))
	}
	return sum
}

// counted are the entries of a sheet that count in a circle: those about its
// members, the rater's own only with self-evaluation on.
func (f Form) counted(s Sheet, in map[uuid.UUID]bool) map[uuid.UUID]*big.Rat {
	out := map[uuid.UUID]*big.Rat{}
	for m, e := range s.Entries {
		if !in[m] || (m == s.Rater && !f.SelfEvaluation) {
			continue
		}
		out[m] = f.given(e)
	}
	return out
}

// Compute works out what each member of circle received from the sheets of
// its members. Sheets of raters outside the circle, and entries about
// members outside it, are passed over: what counts is what a member of the
// circle now gave another member of it now.
func Compute(f Form, circle []uuid.UUID, sheets []Sheet) Result {
	in := make(map[uuid.UUID]bool, len(circle))
	for _, m := range circle {
		in[m] = true
	}
	num, den := map[uuid.UUID]*big.Rat{}, map[uuid.UUID]*big.Rat{}
	peerNum, peerDen := map[uuid.UUID]*big.Rat{}, map[uuid.UUID]*big.Rat{}
	self := map[uuid.UUID]*big.Rat{}
	raters, peerRaters := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	res := Result{Members: make(map[uuid.UUID]Member, len(circle)), Raters: make(map[uuid.UUID]Rater, len(circle))}
	add := func(m map[uuid.UUID]*big.Rat, k uuid.UUID, v *big.Rat) {
		if m[k] == nil {
			m[k] = new(big.Rat)
		}
		m[k].Add(m[k], v)
	}
	seen := map[uuid.UUID]bool{}
	for _, s := range sheets {
		if !in[s.Rater] || seen[s.Rater] {
			continue
		}
		seen[s.Rater] = true
		given := f.counted(s, in)
		r := Rater{Rated: len(given)}
		total := new(big.Rat)
		var first *big.Rat
		uniform := len(given) >= 2
		for _, x := range given {
			total.Add(total, x)
			if first == nil {
				first = x
			} else if first.Cmp(x) != 0 {
				uniform = false
			}
		}
		if uniform {
			r.Flags = append(r.Flags, FlagUniform)
		}
		if len(given) == 0 {
			r.Flags = append(r.Flags, FlagMissing)
		}
		if total.Sign() > 0 {
			r.Counted = true
			even := big.NewRat(1, int64(len(given)))
			for m, x := range given {
				frac := new(big.Rat).Quo(x, total)
				add(num, m, frac)
				add(den, m, even)
				raters[m]++
				if m == s.Rater {
					self[m] = new(big.Rat).Mul(frac, new(big.Rat).SetInt64(int64(len(given))))
					continue
				}
				add(peerNum, m, frac)
				add(peerDen, m, even)
				peerRaters[m]++
			}
		}
		res.Raters[s.Rater] = r
	}
	for _, m := range circle {
		if _, ok := res.Raters[m]; !ok {
			res.Raters[m] = Rater{Flags: []string{FlagMissing}}
		}
		mm := Member{Raters: raters[m], PeerRaters: peerRaters[m], factor: big.NewRat(1, 1)}
		if raters[m] > 0 {
			mm.factor = new(big.Rat).Quo(num[m], den[m])
		}
		mm.Factor = Decimal(mm.factor, FactorPlaces)
		if peerRaters[m] > 0 {
			mm.peer = new(big.Rat).Quo(peerNum[m], peerDen[m])
			p := Decimal(mm.peer, FactorPlaces)
			mm.PeerFactor = &p
		}
		if self[m] != nil {
			mm.self = self[m]
			s := Decimal(mm.self, FactorPlaces)
			mm.SelfFactor = &s
		}
		if raters[m] > 0 {
			if mm.factor.Cmp(LowBelow) < 0 {
				mm.Flags = append(mm.Flags, FlagLow)
			}
			if mm.factor.Cmp(HighAbove) > 0 {
				mm.Flags = append(mm.Flags, FlagHigh)
			}
		}
		if mm.self != nil && mm.peer != nil && new(big.Rat).Sub(mm.self, mm.peer).Cmp(SelfAbovePeersBy) >= 0 {
			mm.Flags = append(mm.Flags, FlagSelfAbovePeers)
		}
		res.Members[m] = mm
	}
	if len(circle) == 2 && !f.SelfEvaluation {
		res.Flags = append(res.Flags, FlagPairWithoutSelf)
	}
	return res
}

// Decimal is r to places decimal places, half away from zero.
func Decimal(r *big.Rat, places int32) decimal.Decimal {
	return decimal.NewFromBigInt(r.Num(), 0).DivRound(decimal.NewFromBigInt(r.Denom(), 0), places)
}

// Score is a member's score from group score g at weight (a percentage, 0
// to 100) and factor: g × (1 − w + w × factor), w the weight over 100,
// rounded once to two places, half away from zero; never below zero, and
// never above points unless the group grade allows extra.
func Score(g, points decimal.Decimal, weight int32, factor decimal.Decimal, allowExtra bool) decimal.Decimal {
	w := decimal.NewFromInt32(weight)
	s := g.Mul(decimal.NewFromInt(100).Sub(w).Add(w.Mul(factor))).Shift(-2).Round(2)
	if s.IsNegative() {
		s = decimal.Zero
	}
	if !allowExtra && s.GreaterThan(points) {
		s = points
	}
	return s
}

// Received is what peers gave a member, beside the factor: for a rating
// form the average from their peers on each criterion, to two places; for a
// share form their factor from their peers alone, as a percentage of an even
// share, to one place. Nil when no peer with a sheet that counts rated them.
type Received struct {
	Averages     map[string]decimal.Decimal
	SharePercent *decimal.Decimal
	PeerRaters   int
}

// ReceivedBy is what each member of circle received from their peers, as
// Compute counts them.
func ReceivedBy(f Form, circle []uuid.UUID, sheets []Sheet) map[uuid.UUID]Received {
	res := Compute(f, circle, sheets)
	in := make(map[uuid.UUID]bool, len(circle))
	for _, m := range circle {
		in[m] = true
	}
	out := make(map[uuid.UUID]Received, len(circle))
	sums := map[uuid.UUID]map[string]int{}
	seen := map[uuid.UUID]bool{}
	for _, s := range sheets {
		if !in[s.Rater] || !res.Raters[s.Rater].Counted || seen[s.Rater] {
			continue
		}
		seen[s.Rater] = true
		for m, e := range s.Entries {
			if !in[m] || m == s.Rater {
				continue
			}
			if sums[m] == nil {
				sums[m] = map[string]int{}
			}
			for _, c := range f.Criteria {
				sums[m][c.Key] += e.Ratings[c.Key]
			}
		}
	}
	for _, m := range circle {
		mm := res.Members[m]
		r := Received{PeerRaters: mm.PeerRaters}
		if mm.PeerRaters == 0 {
			out[m] = r
			continue
		}
		if f.Kind == Share {
			p := Decimal(new(big.Rat).Mul(mm.peer, big.NewRat(100, 1)), 1)
			r.SharePercent = &p
		} else {
			r.Averages = map[string]decimal.Decimal{}
			for _, c := range f.Criteria {
				r.Averages[c.Key] = Decimal(big.NewRat(int64(sums[m][c.Key]), int64(mm.PeerRaters)), 2)
			}
		}
		out[m] = r
	}
	return out
}

// SortedFlags is flags in a fixed order, for a reader to compare.
func SortedFlags(flags []string) []string {
	out := slices.Clone(flags)
	slices.SortFunc(out, strings.Compare)
	return out
}
