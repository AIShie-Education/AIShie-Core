// Package ids makes primary keys. They are UUID v7, generated here and not by
// the database: time-ordered, so they index well and double as the keyset
// pagination cursor.
package ids

import "github.com/google/uuid"

// New returns a fresh UUID v7. It panics only if the system's random source
// fails, in which case nothing else would work either.
func New() uuid.UUID { return uuid.Must(uuid.NewV7()) }
