// Package tools is the catalogue: every tool the system offers, one file per
// noun. A tool here is a declaration — who may call it, what it acts on, what
// the domain's rules are, what it does — and nothing about transport,
// idempotency, the action log or events' delivery, which the pipeline owns.
package tools

import (
	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Deps is what tools need beyond their execution context.
type Deps struct {
	// Pipeline runs a proposal on behalf of action.decide.
	Pipeline *pipeline.Pipeline
}

// RegisterAll fills the registry.
func RegisterAll(reg *tool.Registry, d Deps) {
	reg.Register(meTools()...)
	reg.Register(gradeTools()...)
	reg.Register(actionTools(d)...)
}

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
