package linkedin

// Matrix is what one collect run searches: every keyword in every location, up to MaxPages
// pages each on a cold start. Task 4.2 loads it from leadhound.yaml; the type stays.
type Matrix struct {
	Keywords  []string
	Locations []string
	MaxPages  int // pages per query on a cold start; below 1 counts as 1
}

// DemoMatrix is the small built-in matrix used when no other is given.
func DemoMatrix() Matrix {
	return Matrix{
		Keywords:  []string{"go engineer", "platform engineer"},
		Locations: []string{"European Union"},
		MaxPages:  2,
	}
}
