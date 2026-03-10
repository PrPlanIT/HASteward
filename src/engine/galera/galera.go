package galera

import (
	"context"
	"encoding/base64"
	"fmt"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/engine"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func init() {
	engine.Register("galera", func() engine.Engine { return &Engine{} })
}

// Engine implements the Galera (MariaDB Galera) engine.
type Engine struct {
	cfg    *common.Config
	mariadb *unstructured.Unstructured

	// Parsed from MariaDB CR
	mariadbSpec     map[string]interface{}
	mariadbStatus   map[string]interface{}
	replicas        int64
	isSuspended     bool
	rootPassword    string
	readyCondition  map[string]interface{}
	galeraCondition map[string]interface{}
	galeraRecovery  map[string]interface{}
}

func (e *Engine) Name() string { return "galera" }

// PruneWAL is not applicable to Galera clusters (MariaDB uses InnoDB redo logs, not WAL).
func (e *Engine) PruneWAL(_ context.Context) error {
	return fmt.Errorf("prune wal is not supported for Galera clusters")
}

func (e *Engine) Validate(ctx context.Context, cfg *common.Config) error {
	e.cfg = cfg

	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}

	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("MariaDB %s/%s not found: %w", cfg.Namespace, cfg.ClusterName, err)
	}
	e.mariadb = obj

	e.mariadbSpec = k8s.GetNestedMap(obj, "spec")
	e.mariadbStatus = k8s.GetNestedMap(obj, "status")
	e.replicas = k8s.GetNestedInt64(obj, "spec", "replicas")
	e.isSuspended = k8s.GetNestedBool(obj, "spec", "suspend")

	// Extract conditions
	e.readyCondition = findCondition(e.mariadbStatus, "Ready")
	e.galeraCondition = findCondition(e.mariadbStatus, "GaleraReady")
	e.galeraRecovery = k8s.GetNestedMap(obj, "status", "galeraRecovery")

	// Get root password from secret
	if err := e.fetchRootPassword(ctx); err != nil {
		return fmt.Errorf("failed to get root password: %w", err)
	}

	return nil
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }

// fetchRootPassword reads the root password from the Kubernetes Secret referenced
// in the MariaDB CR spec.rootPasswordSecretKeyRef.
func (e *Engine) fetchRootPassword(ctx context.Context) error {
	secretName := k8s.GetNestedString(e.mariadb, "spec", "rootPasswordSecretKeyRef", "name")
	secretKey := k8s.GetNestedString(e.mariadb, "spec", "rootPasswordSecretKeyRef", "key")
	if secretName == "" || secretKey == "" {
		return fmt.Errorf("MariaDB CR missing spec.rootPasswordSecretKeyRef")
	}

	c := k8s.GetClients()
	secret, err := c.Clientset.CoreV1().Secrets(e.cfg.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("secret %s/%s not found: %w", e.cfg.Namespace, secretName, err)
	}

	data, ok := secret.Data[secretKey]
	if !ok {
		return fmt.Errorf("key %q not found in secret %s", secretKey, secretName)
	}

	// Secret data is already base64-decoded by the K8s API
	e.rootPassword = string(data)

	// Register secret for log sanitization
	common.RegisterSecret(e.rootPassword)
	// Also register the base64-encoded version in case it leaks
	common.RegisterSecret(base64.StdEncoding.EncodeToString(data))

	return nil
}

// findCondition extracts a condition by type from the status.conditions array.
func findCondition(status map[string]interface{}, condType string) map[string]interface{} {
	conditions, ok := status["conditions"]
	if !ok {
		return nil
	}
	condSlice, ok := conditions.([]interface{})
	if !ok {
		return nil
	}
	for _, c := range condSlice {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			return cond
		}
	}
	return nil
}
