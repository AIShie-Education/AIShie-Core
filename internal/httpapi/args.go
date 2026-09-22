package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func contextWith(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, callerKey{}, p)
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// buildArgs assembles a tool's arguments from the three places a REST
// request keeps them: the JSON body (POST), the query string (GET), and the
// path. It produces the same JSON object an MCP client would have sent, and
// the tool's schema then judges it exactly as it judges that.
//
// The path wins. A body that names the same field with a different value is
// refused rather than silently overridden: /courses/A/... with course_id B in
// the body is a confused client, and guessing which it meant is how the wrong
// course gets graded.
func buildArgs(t tool.Tool, r *http.Request) ([]byte, error) {
	args := map[string]any{}

	if r.Method == http.MethodGet {
		for name, values := range r.URL.Query() {
			args[name] = coerce(propertySchema(t, name), values)
		}
	} else {
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			if err := dec.Decode(&args); err != nil {
				return nil, apperr.Invalid("the body must be a JSON object: %v", err)
			}
			if args == nil { // the literal null decodes into a map as nil
				return nil, apperr.Invalid("the body must be a JSON object, not null")
			}
		}
	}

	for _, m := range pathParam.FindAllStringSubmatch(t.HTTP.Pattern, -1) {
		name, value := m[1], r.PathValue(m[1])
		if prior, ok := args[name]; ok && prior != value {
			return nil, apperr.Invalid("%s is %q in the path and %v in the request; they must agree", name, value, prior)
		}
		args[name] = value
	}
	return json.Marshal(args)
}

func propertySchema(t tool.Tool, name string) *jsonschema.Schema {
	if t.InputSchema == nil {
		return nil
	}
	return t.InputSchema.Properties[name]
}

// coerce turns query-string text into the JSON type the schema asks for.
// What cannot be converted is passed through as text, and the schema's own
// error message is the one the caller sees.
func coerce(s *jsonschema.Schema, values []string) any {
	if s == nil || len(values) == 0 {
		return first(values)
	}
	types := s.Types
	if s.Type != "" {
		types = []string{s.Type}
	}
	if slices.Contains(types, "array") {
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = coerce(s.Items, []string{v})
		}
		return out
	}
	v := values[0]
	switch {
	case slices.Contains(types, "boolean") && (v == "true" || v == "false"):
		return v == "true"
	case slices.Contains(types, "integer"), slices.Contains(types, "number"):
		if !slices.Contains(types, "string") && json.Valid([]byte(v)) && isNumber(v) {
			return json.Number(v)
		}
	}
	return v
}

func first(values []string) any {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func isNumber(v string) bool {
	var n json.Number
	return json.Unmarshal([]byte(v), &n) == nil && n.String() == v
}
