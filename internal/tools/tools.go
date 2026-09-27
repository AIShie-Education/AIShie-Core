// Package tools is the catalogue: every tool the system offers, one file per
// noun. A tool here is a declaration — who may call it, what it acts on, what
// the domain's rules are, what it does — and nothing about transport,
// idempotency, the action log or events' delivery, which the pipeline owns.
package tools

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Deps is what tools need beyond their execution context.
type Deps struct {
	// Pipeline runs a proposal on behalf of action.decide, and says how long
	// a proposal may wait for it.
	Pipeline *pipeline.Pipeline
	// Blob is where files live. Nil means the installation has no file
	// storage: documents can still hold text, and uploads are refused.
	Blob blob.Store
	// Uploads signs and checks upload tokens.
	Uploads *blob.Signer
	// MaxUploadBytes bounds one file.
	MaxUploadBytes int64
	// DisableAgentSelfService stops people registering agents of their own
	// (agent.create): administrators still do, with actor.register. What is
	// already registered is left as it is. The zero value, self-service on,
	// is the default.
	DisableAgentSelfService bool
	// MaxAgentsPerOwner bounds the agents one person may have that are not
	// suspended; zero means DefaultMaxAgentsPerOwner.
	MaxAgentsPerOwner int
}

// DefaultMaxUploadBytes is 50 MiB: a scanned exam script, not a video.
const DefaultMaxUploadBytes = 50 << 20

// RegisterAll fills the registry.
func RegisterAll(reg *tool.Registry, d Deps) {
	if d.MaxUploadBytes <= 0 {
		d.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if d.MaxAgentsPerOwner <= 0 {
		d.MaxAgentsPerOwner = DefaultMaxAgentsPerOwner
	}
	if d.Uploads == nil {
		// A random key: fine for one process, until it restarts.
		d.Uploads, _ = blob.NewSigner("")
	}
	reg.Register(meTools()...)
	reg.Register(agentTools(d)...)
	reg.Register(platformTools()...)
	reg.Register(courseTools()...)
	reg.Register(memberTools()...)
	reg.Register(componentTools()...)
	reg.Register(assignmentTools()...)
	reg.Register(submissionTools()...)
	reg.Register(documentTools(d)...)
	reg.Register(gradeTools(d)...)
	reg.Register(gradeReadTools()...)
	reg.Register(actionTools(d)...)
	reg.Register(conversationTools()...)
	reg.Register(eventTools()...)
	reg.Register(systemTools()...)
}

var one = decimal.NewFromInt(1)

// Page is the paging part of a list tool's input. Lists page by id: ids are
// UUID v7, so id order is creation order and the last id seen is the cursor.
type Page struct {
	After *uuid.UUID `json:"after,omitempty" jsonschema:"the id of the last item already seen"`
	Limit int        `json:"limit,omitempty" jsonschema:"at most this many items; default 50, maximum 200"`
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

func (p Page) after() uuid.UUID {
	if p.After == nil {
		return uuid.Nil
	}
	return *p.After
}

func (p Page) limit() int32 {
	switch {
	case p.Limit <= 0:
		return defaultPageSize
	case p.Limit > maxPageSize:
		return maxPageSize
	}
	return int32(p.Limit)
}
