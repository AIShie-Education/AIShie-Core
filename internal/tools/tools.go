// Package tools is the catalogue: every tool the system offers, one file per
// noun. A tool here is a declaration — who may call it, what it acts on, what
// the domain's rules are, what it does — and nothing about transport,
// idempotency, the action log or events' delivery, which the pipeline owns.
package tools

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/memory"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
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
	// Attachments bounds the files messages of conversations carry; a zero
	// limit is its default.
	Attachments AttachmentLimits
	// Documents bounds the files one version of a document holds; a zero
	// limit is its default.
	Documents DocumentLimits
	// Exports bounds an export of conversations, and says how long its
	// files are kept; a zero limit is its default.
	Exports ExportLimits
	// Renditions bounds the PDFs Office files are converted into; a zero
	// limit is its default.
	Renditions RenditionLimits
	// DisableAgentSelfService stops people registering agents of their own
	// (agent.create): administrators still do, with actor.register. What is
	// already registered is left as it is. The zero value, self-service on,
	// is the default.
	DisableAgentSelfService bool
	// MaxAgentsPerOwner bounds the agents one person may have that are not
	// suspended; zero means DefaultMaxAgentsPerOwner.
	MaxAgentsPerOwner int
	// Memory is agents' memory: whether this installation keeps it (off
	// by default, and every memory tool refuses), and its limits, a zero
	// one its default.
	Memory memory.Config
	// SSO is single sign-on's providers: the one the server's operator
	// sets, which the sso tools list read-only, the keys that seal the
	// site's providers' client secrets, and the redirect URI. Nil is none:
	// no operator's provider, and no key, so that no provider is added.
	SSO *sso.Registry
	// Drafts bounds how often one conversation's draft is written
	// (conversation.draft), in this process, keyed by the conversation.
	// Nil means DraftWritesPerSecond, in bursts of as many.
	Drafts *ratelimit.Limiter
}

// DefaultMaxUploadBytes is 50 MiB: a scanned exam script, not a video.
const DefaultMaxUploadBytes = 50 << 20

// RegisterAll fills the registry.
func RegisterAll(reg *tool.Registry, d Deps) {
	if d.MaxUploadBytes <= 0 {
		d.MaxUploadBytes = DefaultMaxUploadBytes
	}
	d.Attachments = d.Attachments.withDefaults(d.MaxUploadBytes)
	d.Documents = d.Documents.withDefaults()
	d.Exports = d.Exports.withDefaults()
	d.Renditions = d.Renditions.withDefaults()
	if d.MaxAgentsPerOwner <= 0 {
		d.MaxAgentsPerOwner = DefaultMaxAgentsPerOwner
	}
	d.Memory = d.Memory.WithDefaults()
	if d.Uploads == nil {
		// A random key: fine for one process, until it restarts.
		d.Uploads, _ = blob.NewSigner("")
	}
	if d.SSO == nil {
		d.SSO = sso.New(sso.Config{})
	}
	if d.Drafts == nil {
		d.Drafts = ratelimit.New(60*DraftWritesPerSecond, DraftWritesPerSecond)
	}
	reg.Register(meTools()...)
	reg.Register(agentTools(d)...)
	reg.Register(platformTools()...)
	reg.Register(departmentTools()...)
	reg.Register(courseTools()...)
	reg.Register(memberTools()...)
	reg.Register(joinLinkTools()...)
	reg.Register(componentTools()...)
	reg.Register(assignmentTools()...)
	reg.Register(assignmentDeleteTools(d)...)
	reg.Register(groupTools()...)
	reg.Register(submissionTools()...)
	reg.Register(documentTools(d)...)
	reg.Register(textTools(d)...)
	reg.Register(serviceTools()...)
	reg.Register(agentRuntimeTools()...)
	reg.Register(renditionTools(d)...)
	reg.Register(gradeTools(d)...)
	reg.Register(gradeReadTools()...)
	reg.Register(actionTools(d)...)
	reg.Register(conversationTools(d)...)
	reg.Register(memoryTools(d)...)
	reg.Register(eventTools()...)
	reg.Register(systemTools()...)
	reg.Register(ssoTools(d)...)
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
