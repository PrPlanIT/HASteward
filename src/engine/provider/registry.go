package provider

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
)

// EngineProvider holds engine identity, validates prerequisites, and provides
// access to engine-specific state. Operation packages use the provider to
// access the engine's validated state.
type EngineProvider interface {
	// Name returns the engine identifier ("cnpg" or "galera").
	Name() string
	// Validate verifies the target CR exists and sets engine-specific state.
	Validate(ctx context.Context, cfg *common.Config) error
	// Config returns the runtime configuration.
	Config() *common.Config
	// PodSelector returns the label selector matching all of the cluster's pods.
	PodSelector() string
	// DataPVCName returns the datadir PVC name for the given pod.
	DataPVCName(pod string) string
}

// registry maps engine names to provider constructors.
var registry = map[string]func() EngineProvider{}

// RegisterProvider adds an engine provider constructor to the registry.
func RegisterProvider(name string, constructor func() EngineProvider) {
	registry[name] = constructor
}

// GetProvider returns a new provider instance by name, or an error if not found.
func GetProvider(name string) (EngineProvider, error) {
	constructor, ok := registry[name]
	if !ok {
		valid := make([]string, 0, len(registry))
		for k := range registry {
			valid = append(valid, k)
		}
		return nil, fmt.Errorf("unknown engine %q (valid: %v)", name, valid)
	}
	return constructor(), nil
}

// ValidEngines returns the list of registered engine names.
func ValidEngines() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
}
