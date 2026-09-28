package domain_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// The trigger that keeps the department tree shallow writes its limit as a
// literal. The tools refuse a tree too deep by the constant before the
// trigger ever sees one, so the two must say the same number: a trigger that
// allowed more would let a race past the tools, one that allowed less would
// turn the tools' too_deep into a fault of ours.
func TestMaxDepartmentDepthIsTheTriggers(t *testing.T) {
	src, err := os.ReadFile("../../src/migrations/0010_department_admins.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`IF above \+ below > (\d+) THEN`).FindSubmatch(src)
	if found == nil {
		t.Fatal("the migration no longer says how deep the tree may be where this test looks")
	}
	if n, _ := strconv.Atoi(string(found[1])); n != domain.MaxDepartmentDepth {
		t.Fatalf("the trigger allows %d levels, domain.MaxDepartmentDepth %d", n, domain.MaxDepartmentDepth)
	}
}
