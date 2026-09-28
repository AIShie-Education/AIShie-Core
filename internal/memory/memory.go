// Package memory holds what agents' memory is, apart from who may reach it:
// how much an agent may keep and how fast it may write (Config), what an
// entry may say (CheckText, CheckTags), and how entries are found
// (SearchText, QueryTerms). docs/schema.md §2.9.
//
// Memory is kept in Core and belongs to the agent, whatever runs it. Who may
// read and write which of it is the tools' (internal/tools, memoryAccess),
// from the agent's seats as authorization reads them; nothing here looks at
// a seat.
package memory

import (
	"crypto/sha256"
	"hash/fnv"

	"github.com/google/uuid"
)

// The three scopes an entry is kept in.
const (
	// ScopeOwner is about the agent's owner, across courses.
	ScopeOwner = "owner"
	// ScopeAsker is about one person who asks the agent, in one course: used
	// only when answering that person.
	ScopeAsker = "asker"
	// ScopeCourse is a course's shared memory: what any student of the
	// course may be told, reviewed by someone who manages the course.
	ScopeCourse = "course"
)

// What state an entry is in.
const (
	StatusActive   = "active"
	StatusProposed = "proposed"
	StatusRejected = "rejected"
)

// Who wrote an entry, or last changed it.
const (
	SourceAgent = "agent"
	SourceOwner = "owner"
	SourceStaff = "staff"
)

// The limits no setting changes.
const (
	// MaxChars and MaxBytes bound an entry's text, once trimmed.
	MaxChars = 1000
	MaxBytes = 4000
	// MaxTags bounds an entry's tags.
	MaxTags = 5
	// MaxPinned bounds the pinned entries of one bucket.
	MaxPinned = 20
	// MaxQueryChars and MaxQueryTerms bound what memory.search looks for.
	MaxQueryChars = 500
	MaxQueryTerms = 24
)

// The defaults of the limits MEMORY_MAX_* and MEMORY_WRITES_* set.
const (
	DefaultMaxOwner      = 200
	DefaultMaxAsker      = 50
	DefaultMaxShared     = 200
	DefaultMaxProposed   = 50
	DefaultMaxPerAgent   = 5000
	DefaultWritesPerHour = 60
	DefaultWritesPerDay  = 300
)

// Config is whether this installation keeps agents' memory at all (MEMORY),
// how much an agent may keep, and how fast it may write. The zero value is
// memory off; a limit left at zero is its default (WithDefaults).
type Config struct {
	// Enabled is MEMORY=on. Off, every memory tool refuses
	// (memory_unavailable); what is kept stays kept.
	Enabled bool
	// MaxOwner, MaxAsker and MaxShared bound the entries in force in one
	// bucket of each scope; MaxProposed the proposals waiting in one
	// course's shared bucket; MaxPerAgent everything one agent holds.
	MaxOwner, MaxAsker, MaxShared, MaxProposed, MaxPerAgent int
	// WritesPerHour and WritesPerDay bound how many entries an agent writes
	// or corrects (memory.write, memory.update). Forgetting is never
	// limited.
	WritesPerHour, WritesPerDay int
}

// WithDefaults is c with every limit left at zero set to its default.
func (c Config) WithDefaults() Config {
	for _, f := range []struct {
		v   *int
		def int
	}{
		{&c.MaxOwner, DefaultMaxOwner}, {&c.MaxAsker, DefaultMaxAsker}, {&c.MaxShared, DefaultMaxShared},
		{&c.MaxProposed, DefaultMaxProposed}, {&c.MaxPerAgent, DefaultMaxPerAgent},
		{&c.WritesPerHour, DefaultWritesPerHour}, {&c.WritesPerDay, DefaultWritesPerDay},
	} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	return c
}

// MaxActive is how many entries in force one bucket of scope may hold.
func (c Config) MaxActive(scope string) int {
	switch scope {
	case ScopeOwner:
		return c.MaxOwner
	case ScopeAsker:
		return c.MaxAsker
	}
	return c.MaxShared
}

// Bucket is what an entry's limits and the uniqueness of its text are
// counted in, as the database works it out (memory_entry.bucket): one for
// the owner, one per asker's seat, one per the agent's seat for a course's
// shared memory. seat is the asker's for ScopeAsker and the agent's own for
// ScopeCourse, and is not looked at for ScopeOwner.
func Bucket(scope string, seat uuid.UUID) string {
	switch scope {
	case ScopeOwner:
		return ScopeOwner
	case ScopeAsker:
		return "asker:" + seat.String()
	}
	return "course:" + seat.String()
}

// LockNamespace is the first key of the advisory lock a write to a bucket
// takes before it counts ("MEMO").
const LockNamespace int32 = 0x4d454d4f

// LockKey is the second: an FNV-1a hash of the holder and the bucket. Two
// buckets that hash alike are only counted one after the other.
func LockKey(holder uuid.UUID, bucket string) int32 {
	h := fnv.New32a()
	_, _ = h.Write(holder[:])
	_, _ = h.Write([]byte(bucket))
	return int32(h.Sum32()) //nolint:gosec // a lock key: any 32 bits will do
}

// Hash is what memory_entry.text_hash holds: the SHA-256 of the text as
// CheckText leaves it. The same text twice in one bucket is one entry.
func Hash(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}
