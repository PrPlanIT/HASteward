package triage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Generic, config-free content comparison: point at the databases, compare every
// user table by row-content hash, and report containment or the exact divergence —
// no app knowledge, no witness, no catalogue. This is the DEFAULT authority resolver
// for a WAL divergence; the witness (cnpg_witness.go) is only an optional refinement.
//
// Row identity is the md5 of the whole row (md5(t.*::text)). Two rows are "the same"
// iff every column matches — so a replicated (pre-fork) row is byte-identical across
// instances and matches, while any post-fork write or update shows up as a difference.
// It is deliberately conservative: it over-reports divergence (an updated row reads as
// removed+added) rather than ever calling two different rows equal. Comparison is by
// MULTISET of row hashes, so no primary key is required — works on any table.
//
// A ⊇ B iff, for every table, B holds no row A lacks. Strict superset across all
// tables ⇒ A is a safe authority (reseed the rest from it, lose nothing). Otherwise
// the per-table divergence counts are surfaced for the operator (or an optional
// witness) to judge.

// instanceExec runs a read-only query against one instance and returns tuple-only,
// newline-separated output. Bound to a live pod or a down instance's standalone copy.
type instanceExec func(query string) (string, error)

// tableSig is the quick per-table equal/differ signature: row count + a multiset hash
// of all row hashes (order-independent). Equal sig on two instances ⇒ identical table.
type tableSig struct {
	count int64
	hash  string
}

// discoverTables lists ordinary user tables as schema.table, excluding system schemas.
func discoverTables(exec instanceExec) ([]string, error) {
	const q = `SELECT n.nspname||'.'||c.relname FROM pg_class c ` +
		`JOIN pg_namespace n ON n.oid=c.relnamespace ` +
		`WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema') ` +
		`AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%' ORDER BY 1;`
	out, err := exec(q)
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			tables = append(tables, t)
		}
	}
	return tables, nil
}

// sigOf returns a table's quick signature (count + multiset hash of row hashes).
func sigOf(exec instanceExec, table string) (tableSig, error) {
	q := fmt.Sprintf(
		"SELECT count(*)::text||'|'||coalesce(md5(string_agg(rh, ',' ORDER BY rh)),'') "+
			"FROM (SELECT md5(t.*::text) rh FROM %s t) s;", table)
	out, err := exec(q)
	if err != nil {
		return tableSig{}, err
	}
	f := strings.SplitN(strings.TrimSpace(out), "|", 2)
	if len(f) != 2 {
		return tableSig{}, fmt.Errorf("bad sig for %s: %q", table, out)
	}
	c, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return tableSig{}, err
	}
	return tableSig{count: c, hash: f[1]}, nil
}

// hashesOf returns the multiset of row-content hashes for a table (hash → count).
func hashesOf(exec instanceExec, table string) (map[string]int64, error) {
	q := fmt.Sprintf("SELECT md5(t.*::text) FROM %s t;", table)
	out, err := exec(q)
	if err != nil {
		return nil, err
	}
	ms := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if h := strings.TrimSpace(line); h != "" {
			ms[h]++
		}
	}
	return ms, nil
}

// tableDivergence is, for one table, how many rows each side holds that the other lacks.
type tableDivergence struct {
	table    string
	aMissing int64 // rows B has that A lacks
	bMissing int64 // rows A has that B lacks
}

// diffTable computes, for one table where signatures differ, the directional missing
// counts by comparing row-hash multisets.
func diffTable(table string, ah, bh map[string]int64) tableDivergence {
	d := tableDivergence{table: table}
	for h, bc := range bh {
		if extra := bc - ah[h]; extra > 0 {
			d.aMissing += extra // B has `extra` copies A doesn't
		}
	}
	for h, ac := range ah {
		if extra := ac - bh[h]; extra > 0 {
			d.bMissing += extra
		}
	}
	return d
}

// diffReport is the full pairwise comparison across all tables.
type diffReport struct {
	relation    contentRelation
	divergences []tableDivergence // only tables that actually differ
}

// genericCompare compares two instances across all shared tables and returns the
// containment relation plus the per-table divergences. hashCache memoizes each
// instance's per-table row-hash multisets so repeated pairs don't re-ship rows.
func genericCompare(aExec, bExec instanceExec, aKey, bKey string, tables []string,
	sig map[string]map[string]tableSig, hashCache map[string]map[string]map[string]int64) (diffReport, error) {

	rep := diffReport{}
	for _, tbl := range tables {
		sa, err := cachedSig(aExec, aKey, tbl, sig)
		if err != nil {
			return diffReport{}, err
		}
		sb, err := cachedSig(bExec, bKey, tbl, sig)
		if err != nil {
			return diffReport{}, err
		}
		if sa == sb {
			continue // identical table — no need to ship rows
		}
		ah, err := cachedHashes(aExec, aKey, tbl, hashCache)
		if err != nil {
			return diffReport{}, err
		}
		bh, err := cachedHashes(bExec, bKey, tbl, hashCache)
		if err != nil {
			return diffReport{}, err
		}
		if d := diffTable(tbl, ah, bh); d.aMissing > 0 || d.bMissing > 0 {
			rep.divergences = append(rep.divergences, d)
		}
	}
	rep.relation = relationFromDivergences(rep.divergences)
	return rep, nil
}

// relationFromDivergences reduces per-table divergences to a containment relation.
func relationFromDivergences(divs []tableDivergence) contentRelation {
	if len(divs) == 0 {
		return contentEqual
	}
	var aLacks, bLacks bool // A lacks some of B's rows / B lacks some of A's
	for _, d := range divs {
		if d.aMissing > 0 {
			aLacks = true
		}
		if d.bMissing > 0 {
			bLacks = true
		}
	}
	switch {
	case aLacks && bLacks:
		return contentCrossed
	case bLacks: // A holds everything B does, plus more
		return contentAContainsB
	case aLacks:
		return contentBContainsA
	default:
		return contentEqual
	}
}

func cachedSig(exec instanceExec, key, tbl string, sig map[string]map[string]tableSig) (tableSig, error) {
	if sig[key] == nil {
		sig[key] = map[string]tableSig{}
	}
	if s, ok := sig[key][tbl]; ok {
		return s, nil
	}
	s, err := sigOf(exec, tbl)
	if err != nil {
		return tableSig{}, err
	}
	sig[key][tbl] = s
	return s, nil
}

func cachedHashes(exec instanceExec, key, tbl string, cache map[string]map[string]map[string]int64) (map[string]int64, error) {
	if cache[key] == nil {
		cache[key] = map[string]map[string]int64{}
	}
	if h, ok := cache[key][tbl]; ok {
		return h, nil
	}
	h, err := hashesOf(exec, tbl)
	if err != nil {
		return nil, err
	}
	cache[key][tbl] = h
	return h, nil
}

// genericContentComparator builds a config-free contentComparator over live/opened
// instance execs. Table discovery and per-table signatures/hashes are memoized, so the
// N² pairwise walk ships each instance's rows at most once. Any query error on a pair
// → contentUnknown (fail closed). summarize(a,b) exposes the per-table divergence for
// operator-facing output on a cross.
func genericContentComparator(execs map[string]instanceExec) (contentComparator, func(a, b string) []tableDivergence) {
	sig := map[string]map[string]tableSig{}
	hashCache := map[string]map[string]map[string]int64{}
	var tables []string
	var tablesErr error
	tablesOnce := false
	lastDivs := map[string][]tableDivergence{}

	ensureTables := func() {
		if tablesOnce {
			return
		}
		tablesOnce = true
		for _, ex := range execs { // discover from any reachable instance
			if t, err := discoverTables(ex); err == nil && len(t) > 0 {
				tables = t
				return
			} else if err != nil {
				tablesErr = err
			}
		}
	}

	cmp := func(a, b authorityInput) contentRelation {
		ax, aok := execs[a.Pod]
		bx, bok := execs[b.Pod]
		if !aok || !bok {
			return contentUnknown
		}
		ensureTables()
		if len(tables) == 0 {
			_ = tablesErr
			return contentUnknown
		}
		rep, err := genericCompare(ax, bx, a.Pod, b.Pod, tables, sig, hashCache)
		if err != nil {
			return contentUnknown
		}
		lastDivs[a.Pod+"\x00"+b.Pod] = rep.divergences
		return rep.relation
	}
	summarize := func(a, b string) []tableDivergence {
		d := lastDivs[a+"\x00"+b]
		sort.Slice(d, func(i, j int) bool {
			return (d[i].aMissing + d[i].bMissing) > (d[j].aMissing + d[j].bMissing)
		})
		return d
	}
	return cmp, summarize
}
