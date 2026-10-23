package sandbox

import "regexp"

var computeProfileName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// ValidComputeProfile names an administrator-approved immutable compute selection.
func ValidComputeProfile(name string) bool { return computeProfileName.MatchString(name) }

// ComputeProfile shares its parent's provider, Runtime and credential. It never
// accepts a client-supplied credential or selects a different backend namespace.
type ComputeProfile struct {
	Resources     Resources     `json:"resources"`
	Configuration Configuration `json:"-"`
}
