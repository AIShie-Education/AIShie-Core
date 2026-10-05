package groupsplit

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seat is a seat id whose order is n's, as UUID v7 seats are in the order
// they were seated.
func seat(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8000-%012d", n)) }

func seats(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = seat(i + 1)
	}
	return out
}

func short(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strings.TrimLeft(id.String()[24:], "0")
	}
	return strings.Join(parts, " ")
}

// The generator is C2SP chacha8rand, seeded with the SHA-256 of the domain
// and the seed: its first two words, pinned, for a front end's own to be
// checked against.
func TestTheGeneratorIsPinned(t *testing.T) {
	src := source("lab-2026")
	if a, b := src.Uint64(), src.Uint64(); a != 7146087731461242251 || b != 16347766031737719105 {
		t.Fatalf("the first two words for seed lab-2026: %d %d", a, b)
	}
}

// Golden orders: the deal depends on the seed and the seats alone, whatever
// order they are given in.
func TestTheOrderDependsOnTheSeedAlone(t *testing.T) {
	for seed, want := range map[string]string{
		"lab-2026":     "7 1 5 2 6 4 3",
		"A":            "3 5 1 7 6 4 2",
		"QK3M7ZP2VX9D": "1 7 5 4 3 2 6",
	} {
		in := seats(7)
		if got := short(Order(in, seed)); got != want {
			t.Errorf("seed %q: %s, want %s", seed, got, want)
		}
		slices.Reverse(in)
		if got := short(Order(in, seed)); got != want {
			t.Errorf("seed %q, the seats given the other way round: %s, want %s", seed, got, want)
		}
		if got := short(in); got != "7 6 5 4 3 2 1" {
			t.Errorf("Order changed what it was given: %s", got)
		}
	}
	if got := Order(nil, "x"); len(got) != 0 {
		t.Errorf("nobody's order: %v", got)
	}
}

// The draw is uniform: rejection sampling takes no value of the top of the
// range that would favour the smallest results, and every index is drawn.
func TestTheDrawIsUniform(t *testing.T) {
	src := source("uniform")
	counts := make([]int, 7)
	for range 70000 {
		counts[below(src, 7)]++
	}
	for i, c := range counts {
		if c < 9500 || c > 10500 {
			t.Fatalf("index %d drawn %d times of 70000: %v", i, c, counts)
		}
	}
	if below(src, 1) != 0 {
		t.Fatal("the one index of one")
	}
}

// Each student goes to the group with the fewest members, the oldest first
// among equals; a group at its capacity, or at the limit, takes nobody.
func TestTheDealFillsTheSmallestGroupFirst(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	a, b, c := uuid.MustParse("00000000-0000-7000-8000-0000000000a1"), uuid.MustParse("00000000-0000-7000-8000-0000000000b1"),
		uuid.MustParse("00000000-0000-7000-8000-0000000000c1")
	groups := []Group{
		{ID: c, CreatedAt: t0.Add(time.Minute)}, // made after a and b, at the same moment as each other
		{ID: b, CreatedAt: t0, Members: 1},
		{ID: a, CreatedAt: t0},
	}
	got, err := Deal(seats(5), groups, 0, "lab-2026")
	if err != nil {
		t.Fatal(err)
	}
	// In the order 1 4 3 5 2 (of five seats, lab-2026): a, the older of
	// the two empty groups; c, the other; then a again, oldest of the
	// three with one; then b, with one against a's two; then c.
	order := Order(seats(5), "lab-2026")
	if got := short(order); got != "1 4 3 5 2" {
		t.Fatalf("the order of five seats: %s", got)
	}
	wantGroups := []uuid.UUID{a, c, a, b, c}
	for i, p := range got {
		if p.Student != order[i] || p.Group != wantGroups[i] {
			t.Fatalf("the deal of %s: %v", short(order), got)
		}
	}

	// Capacity and limit.
	capped := []Group{{ID: a, CreatedAt: t0, Capacity: 1}, {ID: b, CreatedAt: t0, Members: 2}}
	got, err = Deal(seats(2), capped, 3, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Group != a || got[1].Group != b {
		t.Fatalf("a takes one, b one more up to the limit: %v", got)
	}
	if _, err := Deal(seats(3), capped, 3, "x"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a third student has nowhere to go: %v", err)
	}
	if _, err := Deal(seats(1), nil, 0, "x"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("no group at all: %v", err)
	}
	if got, err := Deal(nil, nil, 0, "x"); err != nil || len(got) != 0 {
		t.Fatalf("nobody to deal: %v %v", got, err)
	}
}

// The same seed deals the same way; another seed, another way.
func TestTheSameSeedDealsTheSame(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	groups := []Group{{ID: seat(101), CreatedAt: t0}, {ID: seat(102), CreatedAt: t0}, {ID: seat(103), CreatedAt: t0}}
	one, _ := Deal(seats(30), groups, 0, "same")
	two, _ := Deal(seats(30), groups, 0, "same")
	other, _ := Deal(seats(30), groups, 0, "other")
	if !slices.Equal(one, two) {
		t.Fatal("the same seed dealt two ways")
	}
	if slices.Equal(one, other) {
		t.Fatal("two seeds dealt the same way")
	}
	sizes := map[uuid.UUID]int{}
	for _, p := range one {
		sizes[p.Group]++
	}
	for g, n := range sizes {
		if n != 10 {
			t.Fatalf("group %s has %d of 30 in three groups", g, n)
		}
	}
}

func TestSeeds(t *testing.T) {
	for _, s := range []string{"a", "lab 2026", strings.Repeat("x", 64), "~!@#"} {
		if !ValidSeed(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", strings.Repeat("x", 65), "tab\there", "é", "line\n"} {
		if ValidSeed(s) {
			t.Errorf("%q taken", s)
		}
	}
	a, b := NewSeed(), NewSeed()
	if len(a) != 12 || !ValidSeed(a) || a == b {
		t.Fatalf("new seeds %q %q", a, b)
	}
}

// How many groups a split makes.
func TestHowManyGroupsASplitMakes(t *testing.T) {
	for _, c := range []struct {
		by                                      By
		n, notArchived, eligible, members, deal int
		want                                    int
	}{
		{ByCount, 5, 0, 0, 0, 23, 5},
		{ByCount, 5, 2, 1, 3, 10, 3}, // two there, one of them kept
		{ByCount, 2, 4, 4, 0, 10, 0}, // more than asked: none made
		{BySize, 4, 0, 0, 0, 23, 6},  // ceil(23/4)
		{BySize, 4, 3, 3, 5, 10, 1},  // ceil(15/4) = 4, three there
		{BySize, 4, 9, 9, 0, 8, 0},   // enough already
		{BySize, 0, 0, 0, 0, 8, 0},   // refused before it comes here
		{BySize, 500, 0, 0, 0, 1, 1}, // one group of up to 500
		{ByCount, 1, 0, 0, 0, 0, 1},  // nobody to deal, one group asked for
		{BySize, 3, 0, 0, 0, 0, 0},   // nobody to deal, nobody to size groups for
	} {
		if got := NewGroups(c.by, c.n, c.notArchived, c.eligible, c.members, c.deal); got != c.want {
			t.Errorf("%+v: %d", c, got)
		}
	}
}
