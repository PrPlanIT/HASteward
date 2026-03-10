package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/restic"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	allNamespaces bool
	getType       string
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Display resources (backups, policies, repositories, status)",
}

func init() {
	getCmd.PersistentFlags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List across all namespaces")
	getCmd.PersistentFlags().StringVarP(&getType, "type", "t", "all", "Snapshot type filter: backup, diverged, or all")
	getCmd.AddCommand(getBackupsCmd, getPoliciesCmd, getRepositoriesCmd, getStatusCmd)
}

// --- get backups ---

var getBackupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "List restic backup snapshots",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := initGetClients(); err != nil {
			return err
		}

		tags := map[string]string{}
		if getType != "all" {
			tags["type"] = getType
		}
		if Cfg.Namespace != "" {
			tags["namespace"] = Cfg.Namespace
		}
		if Cfg.ClusterName != "" {
			tags["cluster"] = Cfg.ClusterName
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "REPOSITORY\tSNAPSHOT\tTYPE\tENGINE\tNAMESPACE\tCLUSTER\tAGE\n")

		if Cfg.BackupsPath != "" && Cfg.ResticPassword != "" {
			rc := restic.NewClient(Cfg.BackupsPath, Cfg.ResticPassword)
			snapshots, err := rc.Snapshots(cmd.Context(), tags)
			if err != nil {
				return fmt.Errorf("failed to list snapshots: %w", err)
			}
			for _, snap := range snapshots {
				tm := snap.TagMap()
				age := time.Since(snap.Time).Truncate(time.Second)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					Cfg.BackupsPath, snap.ShortID, tm["type"], tm["engine"], tm["namespace"], tm["cluster"], formatAge(age))
			}
			w.Flush()
			return nil
		}

		repos, err := listRepositories(cmd.Context())
		if err != nil {
			return err
		}

		for _, repo := range repos {
			rc, err := repoClient(cmd.Context(), &repo)
			if err != nil {
				common.WarnLog("Skipping repo %s: %v", repo.Name, err)
				continue
			}

			snapshots, err := rc.Snapshots(cmd.Context(), tags)
			if err != nil {
				common.WarnLog("Failed to list snapshots from %s: %v", repo.Name, err)
				continue
			}

			for _, snap := range snapshots {
				tm := snap.TagMap()
				age := time.Since(snap.Time).Truncate(time.Second)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					repo.Name, snap.ShortID, tm["type"], tm["engine"], tm["namespace"], tm["cluster"], formatAge(age))
			}
		}
		w.Flush()
		return nil
	},
}

// --- get policies ---

var getPoliciesCmd = &cobra.Command{
	Use:   "policies",
	Short: "List BackupPolicy resources",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := initGetClients(); err != nil {
			return err
		}

		rtClient, err := getRuntimeClient()
		if err != nil {
			return err
		}

		var list v1alpha1.BackupPolicyList
		if err := rtClient.List(cmd.Context(), &list); err != nil {
			return fmt.Errorf("failed to list BackupPolicies: %w", err)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "NAME\tBACKUP SCHEDULE\tTRIAGE SCHEDULE\tMODE\tREPOSITORIES\n")
		for _, p := range list.Items {
			repos := "-"
			if len(p.Spec.Repositories) > 0 {
				repos = fmt.Sprintf("%v", p.Spec.Repositories)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				p.Name, p.Spec.BackupSchedule, p.Spec.TriageSchedule, p.Spec.Mode, repos)
		}
		w.Flush()
		return nil
	},
}

// --- get repositories ---

var getRepositoriesCmd = &cobra.Command{
	Use:     "repositories",
	Short:   "List BackupRepository resources",
	Aliases: []string{"repos"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := initGetClients(); err != nil {
			return err
		}

		repos, err := listRepositories(cmd.Context())
		if err != nil {
			return err
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "NAME\tREPOSITORY\tREADY\tSNAPSHOTS\tTOTAL SIZE\tDEDUP SIZE\n")
		for _, r := range repos {
			fmt.Fprintf(w, "%s\t%s\t%v\t%d\t%s\t%s\n",
				r.Name, r.Spec.Restic.Repository, r.Status.Ready,
				r.Status.SnapshotCount, r.Status.TotalSize, r.Status.DeduplicatedSize)
		}
		w.Flush()
		return nil
	},
}

// --- get status ---

var getStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show triage status of managed database clusters",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := initGetClients(); err != nil {
			return err
		}

		type dbStatus struct {
			engine, namespace, name string
			managed, triageResult   string
			lastTriage, lastBackup  string
		}

		var results []dbStatus
		c := k8s.GetClients()

		cnpgList, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(Cfg.Namespace).List(cmd.Context(), k8s.ListOptions())
		if err == nil {
			for _, obj := range cnpgList.Items {
				if s := extractStatus(&obj, "cnpg"); s != nil {
					results = append(results, *s)
				}
			}
		}

		mariaList, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(Cfg.Namespace).List(cmd.Context(), k8s.ListOptions())
		if err == nil {
			for _, obj := range mariaList.Items {
				if s := extractStatus(&obj, "galera"); s != nil {
					results = append(results, *s)
				}
			}
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "ENGINE\tNAMESPACE\tCLUSTER\tMANAGED\tSTATUS\tLAST TRIAGE\tLAST BACKUP\n")
		for _, r := range results {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.engine, r.namespace, r.name, r.managed, r.triageResult, r.lastTriage, r.lastBackup)
		}
		w.Flush()
		return nil
	},
}

func extractStatus(obj *unstructured.Unstructured, eng string) *struct {
	engine, namespace, name string
	managed, triageResult   string
	lastTriage, lastBackup  string
} {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return nil
	}
	if annotations[v1alpha1.AnnotationManaged] != "true" && annotations[v1alpha1.AnnotationPolicy] == "" {
		if !allNamespaces {
			return nil
		}
	}

	managed := annotations[v1alpha1.AnnotationManaged]
	if managed == "" {
		managed = "false"
	}
	triageResult := annotations[v1alpha1.AnnotationLastTriageResult]
	if triageResult == "" {
		triageResult = "-"
	}
	lastTriage := annotations[v1alpha1.AnnotationLastTriage]
	if lastTriage == "" {
		lastTriage = "-"
	}
	lastBackup := annotations[v1alpha1.AnnotationLastBackup]
	if lastBackup == "" {
		lastBackup = "-"
	}

	return &struct {
		engine, namespace, name string
		managed, triageResult   string
		lastTriage, lastBackup  string
	}{eng, obj.GetNamespace(), obj.GetName(), managed, triageResult, lastTriage, lastBackup}
}

// --- helpers ---

func schemeWithCRDs() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func initGetClients() error {
	if Cfg.Verbose {
		os.Setenv(common.EnvPrefix+"LOG_LEVEL", "debug")
		common.InitLogging(false)
	}
	if _, err := k8s.Init(Cfg.Kubeconfig); err != nil {
		return fmt.Errorf("kubernetes init failed: %w", err)
	}
	return nil
}

func getRuntimeClient() (client.Client, error) {
	c := k8s.GetClients()
	scheme := schemeWithCRDs()
	return client.New(c.RestConfig, client.Options{Scheme: scheme})
}

func listRepositories(ctx context.Context) ([]v1alpha1.BackupRepository, error) {
	rtClient, err := getRuntimeClient()
	if err != nil {
		return nil, err
	}
	var list v1alpha1.BackupRepositoryList
	if err := rtClient.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("failed to list BackupRepositories: %w", err)
	}
	return list.Items, nil
}

func repoClient(ctx context.Context, repo *v1alpha1.BackupRepository) (*restic.Client, error) {
	rtClient, err := getRuntimeClient()
	if err != nil {
		return nil, err
	}

	secret := &unstructured.Unstructured{}
	secret.SetGroupVersionKind(k8s.SecretGVK)
	ref := repo.Spec.Restic.PasswordSecretRef
	if err := rtClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, secret); err != nil {
		return nil, fmt.Errorf("password secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	data := k8s.GetNestedMap(secret, "data")
	pwBytes, ok := data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in secret %s/%s", ref.Key, ref.Namespace, ref.Name)
	}

	pw := fmt.Sprintf("%s", pwBytes)
	common.RegisterSecret(pw)

	rc := restic.NewClient(repo.Spec.Restic.Repository, pw)

	if repo.Spec.Restic.EnvSecretRef != nil {
		envRef := repo.Spec.Restic.EnvSecretRef
		envSecret := &unstructured.Unstructured{}
		envSecret.SetGroupVersionKind(k8s.SecretGVK)
		if err := rtClient.Get(ctx, types.NamespacedName{Name: envRef.Name, Namespace: envRef.Namespace}, envSecret); err != nil {
			return nil, fmt.Errorf("env secret %s/%s: %w", envRef.Namespace, envRef.Name, err)
		}
		envData := k8s.GetNestedMap(envSecret, "data")
		rc.Env = make(map[string]string)
		for k, v := range envData {
			rc.Env[k] = fmt.Sprintf("%s", v)
		}
	}

	return rc, nil
}

func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
