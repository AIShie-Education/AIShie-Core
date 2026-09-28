package tool

import (
	"fmt"
	"sort"
)

// Registry is the catalogue. It is filled at start-up and only read after.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds tools. A duplicate name is a programming error.
func (r *Registry) Register(tools ...Tool) {
	for _, t := range tools {
		if _, dup := r.tools[t.Name]; dup {
			panic(fmt.Sprintf("tool %q registered twice", t.Name))
		}
		r.tools[t.Name] = t
	}
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns every tool, sorted by name.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Exposed returns the tools an adapter may offer: everything neither
// Internal nor Unlisted.
func (r *Registry) Exposed() []Tool {
	var out []Tool
	for _, t := range r.All() {
		if !t.Internal && !t.Unlisted {
			out = append(out, t)
		}
	}
	return out
}
