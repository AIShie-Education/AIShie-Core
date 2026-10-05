// Package groupsplit is the random split of a course's students into groups
// (group.split, docs/schema.md §2.5a, Forming groups): pure, so that what it
// deals depends on the seed, the students and the groups alone, never on a
// library's shuffle, and a front end can work out the same deal to preview
// it, and a test pin it.
//
// The deal:
//
//  1. The students are put in a fixed order, their seat ids ascending (UUID
//     v7, so the order they were seated in).
//  2. They are shuffled by Fisher–Yates from the last index down: for each i
//     from n-1 to 1, an index j is drawn uniformly in [0, i] and the two
//     swapped. j is drawn by rejection sampling from successive Uint64s of a
//     ChaCha8 generator (C2SP chacha8rand) seeded with the SHA-256 of
//     "aishie.group.split.v1\x00" and the seed: x is taken if it is at most
//     2^64-1 - (2^64 mod (i+1)), and j is x mod (i+1).
//  3. Each student in turn goes to the eligible group with the fewest
//     members, ties broken by the group's created_at and then its id; a
//     group at its capacity, or at the limit (splitting by size), is not
//     eligible. A student with nowhere to go refuses the deal whole.
package groupsplit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"math"
	mrand "math/rand/v2"
	"slices"
	"time"

	"github.com/google/uuid"
)

// seedDomain is hashed in front of every seed: another deal would be another
// name, and give other results for the same seed.
const seedDomain = "aishie.group.split.v1\x00"

// MaxSeed is the longest seed, in characters.
const MaxSeed = 64

// ErrNoRoom is a deal in which a student would have nowhere to go: every
// eligible group at its capacity or at the limit.
var ErrNoRoom = errors.New("a student would have nowhere to go: every group that takes students is full")

// Group is a group students may be dealt into.
type Group struct {
	ID        uuid.UUID
	CreatedAt time.Time
	// Members it has before the deal: those it keeps.
	Members int
	// Capacity is the most members it takes; 0 is no limit.
	Capacity int
}

// Placement is a student and the group the deal gave them.
type Placement struct {
	Student uuid.UUID
	Group   uuid.UUID
}

// ValidSeed says whether s may be a seed: 1 to MaxSeed printable ASCII
// characters.
func ValidSeed(s string) bool {
	if len(s) == 0 || len(s) > MaxSeed {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// NewSeed makes a seed: twelve base32 characters, from the system's random
// source.
func NewSeed() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])[:12]
}

// source is the deal's generator for seed.
func source(seed string) *mrand.ChaCha8 {
	return mrand.NewChaCha8(sha256.Sum256([]byte(seedDomain + seed)))
}

// below draws uniformly in [0, n), by rejection: the 2^64 mod n values at
// the top of the range, which would favour the smallest results, are drawn
// again.
func below(src *mrand.ChaCha8, n uint64) uint64 {
	excess := (math.MaxUint64%n + 1) % n
	for {
		if x := src.Uint64(); x <= math.MaxUint64-excess {
			return x % n
		}
	}
}

// Order is the order the deal takes students in: by seat id, then shuffled
// by the seed.
func Order(students []uuid.UUID, seed string) []uuid.UUID {
	out := slices.Clone(students)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	src := source(seed)
	for i := len(out) - 1; i > 0; i-- {
		j := below(src, uint64(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Deal places students in groups, as the package says: in Order, each in
// the eligible group with the fewest members. limit, above zero, is the
// most members a group is dealt up to, splitting by size. Nothing is placed
// if anyone would have nowhere to go (ErrNoRoom).
func Deal(students []uuid.UUID, groups []Group, limit int, seed string) ([]Placement, error) {
	gs := slices.Clone(groups)
	slices.SortFunc(gs, func(a, b Group) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	out := make([]Placement, 0, len(students))
	for _, s := range Order(students, seed) {
		best := -1
		for i, g := range gs {
			if (g.Capacity > 0 && g.Members >= g.Capacity) || (limit > 0 && g.Members >= limit) {
				continue
			}
			if best < 0 || g.Members < gs[best].Members {
				best = i
			}
		}
		if best < 0 {
			return nil, ErrNoRoom
		}
		gs[best].Members++
		out = append(out, Placement{Student: s, Group: gs[best].ID})
	}
	return out, nil
}

// By is how a split is sized: groups of a size, or a number of groups.
type By string

const (
	BySize  By = "size"
	ByCount By = "count"
)

// NewGroups is how many groups a split makes, as well as those there are.
// By count, until the set has n groups not archived (those it keeps
// counting: notArchived). By size, until there are ceil((members + toDeal) /
// n) groups taking students, eligible of them there already, where members
// are those the eligible groups keep.
func NewGroups(by By, n, notArchived, eligible, members, toDeal int) int {
	if n <= 0 {
		return 0
	}
	var want int
	switch by {
	case ByCount:
		want = n - notArchived
	case BySize:
		want = (members+toDeal+n-1)/n - eligible
	}
	return max(want, 0)
}
