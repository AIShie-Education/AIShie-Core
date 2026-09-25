package gradecalc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }
func dp(s string) *decimal.Decimal {
	v := d(s)
	return &v
}

// id gives stable, readable ids: id(1), id(2), ...
func id(n byte) uuid.UUID { return uuid.UUID{15: n} }

// The course from the constraint tests: Total = Assignments (40) + Midterm (30)
// + Final (30). Assignments holds HW1..HW3 and drops the lowest.
func course() *Component {
	return &Component{ID: id(1), Weight: d("1"), Children: []*Component{
		{ID: id(2), Weight: d("40"), DropLowest: 1, Assignments: []Assignment{
			{ID: id(11), PointsPossible: d("10")},
			{ID: id(12), PointsPossible: d("10")},
			{ID: id(13), PointsPossible: d("100")},
		}},
		{ID: id(3), Weight: d("30"), PointsPossible: dp("100")},
		{ID: id(4), Weight: d("30"), PointsPossible: dp("200")},
	}}
}

func wantFraction(t *testing.T, r Result, want string) {
	t.Helper()
	if r.Fraction == nil {
		t.Fatalf("fraction = nil, want %s", want)
	}
	if !r.Fraction.Round(6).Equal(d(want)) {
		t.Fatalf("fraction = %s, want %s", r.Fraction.Round(6), want)
	}
}

func TestFullyGraded(t *testing.T) {
	s := Scores{
		Assignment: map[uuid.UUID]decimal.Decimal{id(11): d("5"), id(12): d("9"), id(13): d("80")},
		Component:  map[uuid.UUID]decimal.Decimal{id(3): d("70"), id(4): d("180")},
	}
	got := Compute(course(), s, Policy{})

	// HW1 (0.5) is the lowest and is dropped: (9 + 80) / (10 + 100).
	wantFraction(t, got[id(2)], "0.809091")
	wantFraction(t, got[id(3)], "0.7")
	wantFraction(t, got[id(4)], "0.9")
	// (40×0.809091 + 30×0.7 + 30×0.9) / 100
	wantFraction(t, got[id(1)], "0.803636")
	for cid, r := range got {
		if !r.Complete {
			t.Errorf("component %s is not complete", cid)
		}
	}
	dropped := 0
	for _, it := range got[id(2)].Items {
		if it.Dropped {
			dropped++
			if it.ID != id(11) {
				t.Errorf("dropped %s, want HW1", it.ID)
			}
		}
	}
	if dropped != 1 {
		t.Errorf("dropped %d items, want 1", dropped)
	}
	if got := Percent(*got[id(1)].Fraction); !got.Equal(d("80.36")) {
		t.Errorf("percent = %s, want 80.36", got)
	}
}

func TestGradeSoFarLeavesOutWhatIsUngraded(t *testing.T) {
	s := Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(13): d("80")}}
	got := Compute(course(), s, Policy{})

	// One graded item: drop_lowest never drops everything.
	wantFraction(t, got[id(2)], "0.8")
	if got[id(2)].Complete {
		t.Error("bucket with ungraded work reported complete")
	}
	// Midterm and final have no grade: the total is the bucket alone.
	if got[id(3)].Fraction != nil || got[id(3)].Complete {
		t.Errorf("ungraded exam: %+v", got[id(3)])
	}
	wantFraction(t, got[id(1)], "0.8")
	if got[id(1)].Complete {
		t.Error("total reported complete with two exams ungraded")
	}
}

func TestUngradedAsZero(t *testing.T) {
	s := Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(13): d("80")}}
	got := Compute(course(), s, Policy{UngradedAsZero: true})

	// HW1 and HW2 are zeros; one zero is dropped (the tie goes to either
	// 10-pointer): (0 + 80) / (10 + 100).
	wantFraction(t, got[id(2)], "0.727273")
	wantFraction(t, got[id(3)], "0")
	// (40×0.727273 + 0 + 0) / 100
	wantFraction(t, got[id(1)], "0.290909")
	if !got[id(1)].Complete {
		t.Error("with ungraded counted as zero, the total is complete")
	}
}

func TestNothingGraded(t *testing.T) {
	got := Compute(course(), Scores{}, Policy{})
	for cid, r := range got {
		if r.Fraction != nil {
			t.Errorf("component %s has fraction %s with no grades at all", cid, r.Fraction)
		}
	}
	if got[id(1)].Complete {
		t.Error("an ungraded course is not complete")
	}
}

func TestDropNeverDropsEverything(t *testing.T) {
	c := &Component{ID: id(1), DropLowest: 5, Assignments: []Assignment{
		{ID: id(11), PointsPossible: d("10")}, {ID: id(12), PointsPossible: d("10")},
	}}
	got := Compute(c, Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(11): d("4"), id(12): d("6")}}, Policy{})
	wantFraction(t, got[id(1)], "0.6") // the better one survives
}

func TestDropTieIsDeterministic(t *testing.T) {
	mk := func(order ...byte) *Component {
		c := &Component{ID: id(1), DropLowest: 1}
		pts := map[byte]string{11: "10", 12: "50", 13: "10"}
		for _, n := range order {
			c.Assignments = append(c.Assignments, Assignment{ID: id(n), PointsPossible: d(pts[n])})
		}
		return c
	}
	// All three are 50%. Dropping the 50-pointer is the rule; input order
	// must not matter.
	s := Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(11): d("5"), id(12): d("25"), id(13): d("5")}}
	for _, order := range [][]byte{{11, 12, 13}, {13, 12, 11}, {12, 11, 13}} {
		got := Compute(mk(order...), s, Policy{})
		for _, it := range got[id(1)].Items {
			if it.Dropped != (it.ID == id(12)) {
				t.Errorf("order %v: item %s dropped=%v", order, it.ID, it.Dropped)
			}
		}
	}
}

func TestZeroPointAndZeroWeightCountForNothing(t *testing.T) {
	c := &Component{ID: id(1), Weight: d("1"), Children: []*Component{
		{ID: id(2), Weight: d("1"), Assignments: []Assignment{
			{ID: id(11), PointsPossible: d("10")},
			{ID: id(12), PointsPossible: d("0")}, // a survey
		}},
		{ID: id(3), Weight: d("0"), PointsPossible: dp("100")}, // practice exam, ungraded
	}}
	got := Compute(c, Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(11): d("7")}}, Policy{})
	wantFraction(t, got[id(2)], "0.7")
	wantFraction(t, got[id(1)], "0.7")
	if !got[id(1)].Complete {
		t.Error("an ungraded zero-weight child must not make the total incomplete")
	}
}

func TestExtraCreditCanExceedOne(t *testing.T) {
	c := &Component{ID: id(1), PointsPossible: dp("100")}
	got := Compute(c, Scores{Component: map[uuid.UUID]decimal.Decimal{id(1): d("105")}}, Policy{})
	wantFraction(t, got[id(1)], "1.05")
}

// Same is everything a stored snapshot shows: a result read back from its
// JSON is the same, and a change to any line of the working is not, even
// where the number stays put.
func TestSameIsEverythingASnapshotShows(t *testing.T) {
	s := Scores{Assignment: map[uuid.UUID]decimal.Decimal{id(13): d("80")}}
	bucket := Compute(course(), s, Policy{})[id(2)] // HW1, HW2 ungraded; HW3 0.8
	stored := func() Result {
		t.Helper()
		raw, err := json.Marshal(bucket)
		if err != nil {
			t.Fatal(err)
		}
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if !bucket.Same(stored()) {
		t.Fatal("a result is not the same as itself read back")
	}
	same := stored()
	same.Items[0].Weight = d("10.00")
	if !bucket.Same(same) {
		t.Error("10 and 10.00 are not the same weight")
	}
	for what, change := range map[string]func(r *Result){
		"complete":        func(r *Result) { r.Complete = true },
		"the fraction":    func(r *Result) { r.Fraction = dp("0.5") },
		"no fraction":     func(r *Result) { r.Fraction = nil },
		"a gap filled":    func(r *Result) { r.Items[1].Fraction = dp("0.8") },
		"a line's score":  func(r *Result) { r.Items[2].Fraction = dp("0.9") },
		"a line's weight": func(r *Result) { r.Items[0].Weight = d("20") },
		"a line dropped":  func(r *Result) { r.Items[2].Dropped = true },
		"a line's kind":   func(r *Result) { r.Items[2].Kind = KindComponent },
		"another line":    func(r *Result) { r.Items[2].ID = id(14) },
		"a line fewer":    func(r *Result) { r.Items = r.Items[:2] },
	} {
		r := stored()
		change(&r)
		if bucket.Same(r) || r.Same(bucket) {
			t.Errorf("%s changed, and the result is still the same", what)
		}
	}
}

func TestAncestors(t *testing.T) {
	root := course()
	eq := func(got []uuid.UUID, want ...uuid.UUID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	// An assignment's grade changes its bucket and everything above it.
	if got := Ancestors(root, id(13)); !eq(got, id(2), id(1)) {
		t.Errorf("assignment: %v", got)
	}
	// A directly graded component's own grade is entered, not computed: only
	// what is above it is recomputed.
	if got := Ancestors(root, id(3)); !eq(got, id(1)) {
		t.Errorf("exam: %v", got)
	}
	if got := Ancestors(root, id(2)); !eq(got, id(2), id(1)) {
		t.Errorf("bucket: %v", got)
	}
	if got := Ancestors(root, id(99)); got != nil {
		t.Errorf("unknown id: %v", got)
	}
}
