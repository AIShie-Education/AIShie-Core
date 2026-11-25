package tools_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

func TestMeGetSchoolTimeZone(t *testing.T) {
	const want = "Asia/Hong_Kong"
	zone, err := time.LoadLocation(want)
	if err != nil {
		t.Fatal(err)
	}
	p := testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.SchoolTimeZone = zone })

	// The school's clock is installation-wide, not a property of a person:
	// people and agents receive the same optional field.
	for _, actor := range []uuid.UUID{p.Root, p.Actor("agent", "Course assistant")} {
		out, err := p.Call(actor, "me.get", map[string]any{}, "")
		if err != nil || out.Status != domain.StatusExecuted {
			t.Fatalf("me.get: %+v, %v", out, err)
		}
		me := testkit.Result[tools.MeOut](t, out)
		if me.TimeZone == nil || *me.TimeZone != want {
			t.Fatalf("time_zone: %+v, want %q", me.TimeZone, want)
		}
		var raw map[string]any
		if err := json.Unmarshal(out.Result, &raw); err != nil || raw["time_zone"] != want {
			t.Fatalf("JSON time_zone: %q (%v), %s", raw["time_zone"], err, out.Result)
		}
	}

	// The generated REST/MCP catalogue describes the new contract, and keeps
	// it optional so an installation without a setting behaves as before.
	tl, ok := p.P.Registry().Get("me.get")
	if !ok {
		t.Fatal("me.get is not registered")
	}
	if _, ok := tl.OutputSchema.Properties["time_zone"]; !ok {
		t.Fatal("me.get's output schema has no time_zone")
	}
	if slices.Contains(tl.OutputSchema.Required, "time_zone") {
		t.Fatal("time_zone must be optional")
	}
}

func TestMeGetOmitsSchoolTimeZoneWhenUnset(t *testing.T) {
	p := testkit.NewPlatform(t)
	out, err := p.Call(p.Root, "me.get", map[string]any{}, "")
	if err != nil || out.Status != domain.StatusExecuted {
		t.Fatalf("me.get: %+v, %v", out, err)
	}
	if me := testkit.Result[tools.MeOut](t, out); me.TimeZone != nil {
		t.Fatalf("time_zone: %q, want absent", *me.TimeZone)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["time_zone"]; ok {
		t.Fatalf("time_zone is present without SCHOOL_TIME_ZONE: %s", out.Result)
	}
}
