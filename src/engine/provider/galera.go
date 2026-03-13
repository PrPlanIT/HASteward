package provider

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func init() {
	RegisterProvider("galera", func() EngineProvider { return &GaleraProvider{} })
}

// GaleraProvider holds validated state for a Galera (MariaDB) engine.
type GaleraProvider struct {
	cfg     *common.Config
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

func (p *GaleraProvider) Name() string            { return "galera" }
func (p *GaleraProvider) Config() *common.Config   { return p.cfg }
func (p *GaleraProvider) MariaDB() *unstructured.Unstructured { return p.mariadb }
func (p *GaleraProvider) Spec() map[string]interface{}        { return p.mariadbSpec }
func (p *GaleraProvider) Status() map[string]interface{}      { return p.mariadbStatus }
func (p *GaleraProvider) Replicas() int64          { return p.replicas }
func (p *GaleraProvider) IsSuspended() bool        { return p.isSuspended }
func (p *GaleraProvider) RootPassword() string     { return p.rootPassword }
func (p *GaleraProvider) ReadyCondition() map[string]interface{}  { return p.readyCondition }
func (p *GaleraProvider) GaleraCondition() map[string]interface{} { return p.galeraCondition }
func (p *GaleraProvider) GaleraRecovery() map[string]interface{}  { return p.galeraRecovery }
func (p *GaleraProvider) Image() string                           { return k8s.GetNestedString(p.mariadb, "spec", "image") }

// SetMariaDB replaces the cached CR state (used after re-fetch during repair/bootstrap).
func (p *GaleraProvider) SetMariaDB(obj *unstructured.Unstructured) {
	p.mariadb = obj
	p.mariadbSpec = k8s.GetNestedMap(obj, "spec")
	p.mariadbStatus = k8s.GetNestedMap(obj, "status")
	p.readyCondition = FindCondition(p.mariadbStatus, "Ready")
	p.galeraCondition = FindCondition(p.mariadbStatus, "GaleraReady")
	p.galeraRecovery = k8s.GetNestedMap(obj, "status", "galeraRecovery")
	p.isSuspended = k8s.GetNestedBool(obj, "spec", "suspend")
}

func (p *GaleraProvider) Validate(ctx context.Context, cfg *common.Config) error {
	p.cfg = cfg

	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}

	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("MariaDB %s/%s not found: %w", cfg.Namespace, cfg.ClusterName, err)
	}

	p.SetMariaDB(obj)
	p.replicas = k8s.GetNestedInt64(obj, "spec", "replicas")

	// Get root password from secret
	if err := p.fetchRootPassword(ctx); err != nil {
		return fmt.Errorf("failed to get root password: %w", err)
	}

	return nil
}

// fetchRootPassword reads the root password from the Kubernetes Secret referenced
// in the MariaDB CR spec.rootPasswordSecretKeyRef.
func (p *GaleraProvider) fetchRootPassword(ctx context.Context) error {
	secretName := k8s.GetNestedString(p.mariadb, "spec", "rootPasswordSecretKeyRef", "name")
	secretKey := k8s.GetNestedString(p.mariadb, "spec", "rootPasswordSecretKeyRef", "key")
	if secretName == "" || secretKey == "" {
		return fmt.Errorf("MariaDB CR missing spec.rootPasswordSecretKeyRef")
	}

	c := k8s.GetClients()
	secret, err := c.Clientset.CoreV1().Secrets(p.cfg.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("secret %s/%s not found: %w", p.cfg.Namespace, secretName, err)
	}

	data, ok := secret.Data[secretKey]
	if !ok {
		return fmt.Errorf("key %q not found in secret %s", secretKey, secretName)
	}

	// Secret data is already base64-decoded by the K8s API
	p.rootPassword = string(data)

	// Register secret for log sanitization
	common.RegisterSecret(p.rootPassword)
	// Also register the base64-encoded version in case it leaks
	common.RegisterSecret(base64.StdEncoding.EncodeToString(data))

	return nil
}

// FindCondition extracts a condition by type from the status.conditions array.
func FindCondition(status map[string]interface{}, condType string) map[string]interface{} {
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
