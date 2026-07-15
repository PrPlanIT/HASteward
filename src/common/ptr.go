package common

// Ptr returns a pointer to v — handy for the many optional/pointer fields in the
// Kubernetes API types (GracePeriodSeconds, Replicas, …).
func Ptr[T any](v T) *T { return &v }

// Default operation timeouts, in seconds, used when the config leaves them unset.
const (
	// DefaultDeleteTimeout bounds pod-termination / PVC-detach waits.
	DefaultDeleteTimeout = 300
	// DefaultHealTimeout bounds a node heal / cluster-rejoin wait.
	DefaultHealTimeout = 600
)
