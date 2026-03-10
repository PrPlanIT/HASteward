package cnpg

import (
	"context"
	"fmt"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func init() {
	engine.Register("cnpg", func() engine.Engine { return &Engine{} })
}

// Engine implements the CNPG (CloudNativePG PostgreSQL) engine.
type Engine struct {
	cfg     *common.Config
	cluster *unstructured.Unstructured

	// Parsed from Cluster CR
	clusterSpec        map[string]interface{}
	clusterStatus      map[string]interface{}
	clusterAnnotations map[string]interface{}
	instances          int64
	fencedInstances    []string
}

func (e *Engine) Name() string { return "cnpg" }

func (e *Engine) Validate(ctx context.Context, cfg *common.Config) error {
	e.cfg = cfg

	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}

	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("CNPG Cluster %s/%s not found: %w", cfg.Namespace, cfg.ClusterName, err)
	}
	e.cluster = obj

	e.clusterSpec = k8s.GetNestedMap(obj, "spec")
	e.clusterStatus = k8s.GetNestedMap(obj, "status")
	e.clusterAnnotations = k8s.GetNestedMap(obj, "metadata", "annotations")
	e.instances = k8s.GetNestedInt64(obj, "spec", "instances")

	// Parse fenced instances from annotation
	e.fencedInstances = parseFencedInstances(e.clusterAnnotations)

	return nil
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }

// parseFencedInstances extracts the fenced instances list from cluster annotations.
func parseFencedInstances(annotations map[string]interface{}) []string {
	raw, ok := annotations["cnpg.io/fencedInstances"]
	if !ok {
		return nil
	}
	s, ok := raw.(string)
	if !ok || s == "" || s == "[]" {
		return nil
	}
	// Simple JSON array parse for string arrays like ["pod-1","pod-2"]
	return parseJSONStringArray(s)
}

// parseJSONStringArray is a lightweight parser for JSON string arrays
// to avoid pulling in encoding/json for this one use.
func parseJSONStringArray(s string) []string {
	var result []string
	inQuote := false
	var current []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inQuote:
			inQuote = true
			current = current[:0]
		case c == '"' && inQuote:
			inQuote = false
			result = append(result, string(current))
		case inQuote:
			current = append(current, c)
		}
	}
	return result
}
