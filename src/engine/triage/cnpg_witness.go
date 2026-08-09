package triage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/PrPlanIT/HASteward/src/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The content witness resolves a WAL-lineage divergence by BUSINESS CONTENT.
//
// WAL divergence (cnpg_authority.go) only proves each branch wrote some WAL past a
// shared fork — and a crash-looping or idle instance accrues WAL (checkpoints,
// recovery, repeatable migrations) with zero business writes. The witness reads the
// authoritative business table on each instance and decides containment: if one
// branch's committed rows CONTAIN another's, the loser's extra WAL is churn and there
// is a safe winner; only when each branch holds rows the other lacks is it a true
// divergence.
//
// Containment is proven by a content HASH, never counts alone: A ⊇ B iff A's rows
// with position ≤ B's max position hash-match B's entire set. Same count over the same
// range does NOT imply the same rows across a fork; the hash does. Claimed ONLY for
// append-only tables (operator-asserted), where a monotonic, never-deleted position
// column makes the row set identity-stable.
//
// Collection is LAZY: the comparator gathers content only on its first call, and
// determineAuthority only calls it on a divergence — so a healthy cluster pays nothing.

const witnessAnnoPrefix = "hasteward.prplanit.com/"

// witnessSpec is the per-cluster declaration of where authoritative business content
// lives, read from Cluster CR annotations. Universal — no storage/engine assumptions,
// just a table, its append-only ordering column, and an optional churn filter. Absent
// witness-table/position → no witness → WAL-lineage-only authority (legacy).
type witnessSpec struct {
	DB         string
	Table      string
	Position   string
	Exclude    string // SQL predicate matching CHURN rows to exclude
	AppendOnly bool
}

func readWitnessSpec(cluster *unstructured.Unstructured, defaultDB string) (witnessSpec, bool) {
	anno := func(k string) string {
		return strings.TrimSpace(k8s.GetNestedString(cluster, "metadata", "annotations", witnessAnnoPrefix+k))
	}
	table, pos := anno("witness-table"), anno("witness-position")
	if table == "" || pos == "" {
		return witnessSpec{}, false
	}
	db := anno("witness-db")
	if db == "" {
		db = defaultDB
	}
	return witnessSpec{
		DB: db, Table: table, Position: pos, Exclude: anno("witness-exclude"),
		AppendOnly: strings.EqualFold(anno("witness-append-only"), "true"),
	}, true
}

// whereNotChurn renders the churn-exclusion fragment (empty when no filter).
func (w witnessSpec) whereNotChurn(leading string) string {
	if strings.TrimSpace(w.Exclude) == "" {
		return ""
	}
	return leading + " NOT (" + w.Exclude + ")"
}

// witnessBase is one instance's business-content fingerprint plus a `cut` that yields
// the (count, hash) of its rows up to any peer's max position. A live instance's cut
// queries on demand; a down instance's cut (read in a fenced window) returns
// precomputed values, and ok=false for a position it did not precompute (fail closed).
type witnessBase struct {
	ok     bool
	count  int64
	maxPos string
	hash   string
	cut    func(peerMax string) (count int64, hash string, ok bool)
}

// baseQuery / cutQuery render the fingerprint and up-to-position queries.
func (w witnessSpec) baseQuery() string {
	return fmt.Sprintf(
		"SELECT count(*)::text||'|'||coalesce(max(%[1]s)::text,'')||'|'||"+
			"coalesce(md5(string_agg(%[1]s::text, ',' ORDER BY %[1]s)),'') FROM %[2]s%[3]s;",
		w.Position, w.Table, w.whereNotChurn(" WHERE"))
}

func (w witnessSpec) cutQuery(peerMax string) string {
	return fmt.Sprintf(
		"SELECT count(*)::text||'|'||coalesce(md5(string_agg(%[1]s::text, ',' ORDER BY %[1]s)),'') "+
			"FROM %[2]s WHERE %[1]s <= %[3]s%[4]s;",
		w.Position, w.Table, peerMax, w.whereNotChurn(" AND"))
}

// liveBase fingerprints a running instance and returns a base whose cut queries the
// same live pod on demand. ok=false when the instance is not content-readable.
func liveBase(ctx context.Context, ns, pod string, spec witnessSpec) witnessBase {
	cnt, maxPos, hash, ok := parseBase(psqlOut(ctx, ns, pod, spec.DB, spec.baseQuery()))
	if !ok {
		return witnessBase{}
	}
	return witnessBase{
		ok: true, count: cnt, maxPos: maxPos, hash: hash,
		cut: func(peerMax string) (int64, string, bool) {
			c, h, ok2 := parseCut(psqlOut(ctx, ns, pod, spec.DB, spec.cutQuery(peerMax)))
			return c, h, ok2
		},
	}
}

// collectLiveBases fingerprints each candidate running pod. A pod whose postgres is
// down / not accepting connections yields ok=false — its pairs stay contentUnknown
// until a fenced read-only start closes the gap (deep content read).
func collectLiveBases(ctx context.Context, ns string, spec witnessSpec, pods []string) map[string]witnessBase {
	bases := make(map[string]witnessBase, len(pods))
	for _, pod := range pods {
		bases[pod] = liveBase(ctx, ns, pod, spec)
	}
	return bases
}

// lazyContentComparator memoizes `collect` on first use and answers containment from
// the resulting bases. determineAuthority only calls it on a divergence, so collection
// (and any fencing inside it) never runs on a healthy cluster.
func lazyContentComparator(spec witnessSpec, collect func() map[string]witnessBase) contentComparator {
	var once sync.Once
	var bases map[string]witnessBase
	return func(a, b authorityInput) contentRelation {
		once.Do(func() { bases = collect() })
		return decideContent(spec, bases[a.Pod], bases[b.Pod])
	}
}

// decideContent is the pure containment verdict between two bases.
func decideContent(spec witnessSpec, a, b witnessBase) contentRelation {
	if !a.ok || !b.ok || !spec.AppendOnly {
		return contentUnknown
	}
	switch {
	case a.count == 0 && b.count == 0:
		return contentEqual
	case b.count == 0:
		return contentAContainsB
	case a.count == 0:
		return contentBContainsA
	}
	aCB := containsPeer(a, b)
	bCA := containsPeer(b, a)
	switch {
	case aCB && bCA:
		return contentEqual
	case aCB:
		return contentAContainsB
	case bCA:
		return contentBContainsA
	default:
		return contentCrossed
	}
}

// containsPeer reports whether host's rows up to peer.maxPos hash-match peer's whole
// set (same count AND same ordered-position hash) — proof host ⊇ peer for append-only.
func containsPeer(host, peer witnessBase) bool {
	c, h, ok := host.cut(peer.maxPos)
	return ok && c == peer.count && h == peer.hash
}

func parseBase(out string, err error) (count int64, maxPos, hash string, ok bool) {
	if err != nil || out == "" {
		return 0, "", "", false
	}
	f := strings.Split(out, "|")
	if len(f) != 3 {
		return 0, "", "", false
	}
	c, perr := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
	if perr != nil {
		return 0, "", "", false
	}
	return c, strings.TrimSpace(f[1]), strings.TrimSpace(f[2]), true
}

func parseCut(out string, err error) (count int64, hash string, ok bool) {
	if err != nil || out == "" {
		return 0, "", false
	}
	f := strings.Split(out, "|")
	if len(f) != 2 {
		return 0, "", false
	}
	c, perr := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
	if perr != nil {
		return 0, "", false
	}
	return c, strings.TrimSpace(f[1]), true
}

// psqlOut runs a query in a pod's postgres container as the postgres superuser over
// the local socket (peer auth) and returns trimmed tuple-only output.
func psqlOut(ctx context.Context, ns, pod, db, query string) (string, error) {
	res, err := k8s.ExecCommand(ctx, pod, ns, "postgres",
		[]string{"psql", "-U", "postgres", "-d", db, "-tAqc", query})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
