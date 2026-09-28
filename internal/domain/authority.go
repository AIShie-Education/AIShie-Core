package domain

import "github.com/google/uuid"

// MaxDepartmentDepth is how many levels the department tree may have. The
// migration's trigger (department_tree_valid) holds the database to the same
// number; a test compares them.
const MaxDepartmentDepth = 8

// The capacities a call outside any course is allowed in, as
// action.authority records them: a platform role, or a department
// administrator's appointment. A call made from a seat, or on one's own
// account, is made in neither.
const (
	AuthorityPlatform   = "platform"
	AuthorityDepartment = "department"
)

// Authority is a department administrator's authority over what a call is
// about, found on the call: their appointment at its department, or at the
// nearest department above it. It reaches the departments beneath the
// appointment and their courses as a whole, never what is inside a course.
type Authority struct {
	AppointmentID uuid.UUID
	// DeptID is the department of that appointment: the nearest one to what
	// the call is about.
	DeptID uuid.UUID
}
