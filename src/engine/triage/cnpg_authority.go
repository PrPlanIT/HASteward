package triage

import (
	"fmt"
	"sort"
	"strings"
)

// This file holds the CNPG authority decision: given what triage could read from
// every expected instance, decide — provably, never by guess — which instance
// holds the winning data, or refuse.
//
// Two invariants drive every branch:
//
//  1. Legibility gates the decision. Authority is decided ONLY when every expected
//     instance is either Read (we have its pg_controldata + timeline history) or
//     AbsentNoData (proven to hold nothing). A single instance that COULD hold data
//     but could not be read makes authority undeterminable — we refuse and report
//     exactly what to fix, rather than decide past a node we never inspected. An
//     illegible node never becomes disposable by default.
//
//  2. Recency is judged by WAL lineage, not the timeline NUMBER. A restore lands on
//     a higher timeline number while carrying OLDER data; "highest timeline wins"
//     would crown it and shred the real data. We reconstruct each instance's lineage
//     from its .history files and compare at the fork points, so a lower-timeline
//     node with committed WAL past the fork is correctly ranked ahead of a
//     higher-numbered stale restore — and when BOTH branches carry committed data
//     past a shared fork, that is a true divergence with no safe winner: refuse and
//     surface both.

// ReadState is the terminal legibility of one expected instance after triage has
// exhausted every read attempt.
type ReadState int

const (
	// ReadStateUnread: data may exist but could not be read. BLOCKS every
	// destructive decision — authority cannot be determined while blind to it.
	ReadStateUnread ReadState = iota
	// ReadStateRead: pg_controldata (timeline + checkpoint LSN) and any timeline
	// history files were obtained.
	ReadStateRead
	// ReadStateAbsentNoData: provably nothing to lose — no PVC at all, or a volume
	// we mounted and found to contain no PGDATA. Never blocks; excluded from ranking.
	ReadStateAbsentNoData
)

// switchPoint is one line of a PostgreSQL timeline-history file: the parent timeline
// and the WAL LSN at which the NEXT timeline branched off it.
type switchPoint struct {
	ParentTimeline int64
	SwitchLSN      int64
}

// authorityInput is one instance as seen by the authority decision. It is a pure
// value: the collection phase fills it, and determineAuthority does no I/O.
type authorityInput struct {
	Pod           string
	IsPrimary     bool
	ReadState     ReadState
	UnreadReason  string // why Unread + what was already tried (operator-actionable)
	Timeline      int64
	CheckpointLSN int64
	Switches      []switchPoint // lineage of Timeline, parsed from <Timeline>.history (ancestors first)
}

// authorityDecision is the provable verdict over the whole instance set.
type authorityDecision struct {
	Determinable bool     // false → could not read enough to decide; NEVER heal
	Leader       string   // the decisive authoritative pod (may or may not be the primary); "" when none
	LeaderReason string   // why Leader holds authority
	Diverged     bool     // determinable, but committed data on >1 lineage past a shared fork → no safe winner
	Blockers     []string // per-instance reasons authority is undeterminable (the unread nodes)
	Divergences  []string // human-readable divergence findings (fork LSN + what each side holds)
	ContentNotes []string // WAL-divergences resolved by the content witness (churn-only), for operator visibility
}

// relation is the provable data-extent relationship between two readable instances.
type relation int

const (
	relDiverge     relation = iota // committed data on both branches past a shared fork — no safe winner
	relEqual                       // identical position — either is a safe authority
	relADominatesB                 // a is a safe authority over b: b re-clones from a and loses nothing
	relBDominatesA
)

// contentRelation is the provable BUSINESS-CONTENT relationship between two
// instances. It exists to RESOLVE a WAL-lineage divergence: WAL divergence means
// each branch wrote SOME WAL past a shared fork, but WAL volume includes churn a
// stale/crash-looping instance accrues with zero business writes (checkpoints,
// restart recovery, repeatable migrations). When a witness proves one branch's
// committed business rows contain the other's, the "divergence" is churn-only and
// there is a safe winner; only when each branch holds business rows the other lacks
// is it a true divergence. Content is compared instance-to-instance (not against the
// WAL fork LSN) because a witness's ordering column is a business position, not a WAL LSN.
type contentRelation int

const (
	// contentUnknown: no witness configured, or an instance was not content-readable.
	// Cannot refine — the WAL verdict stands (fail closed, legacy behavior).
	contentUnknown contentRelation = iota
	contentEqual                      // identical business extent
	contentAContainsB                 // a holds every business row b holds (and maybe more)
	contentBContainsA
	contentCrossed // each holds committed business rows the other lacks — a TRUE divergence
)

// contentComparator answers the business-content relationship between two readable
// instances. It performs the witness I/O (queries, fenced reads); determineAuthority
// stays pure by receiving it as a dependency. A nil comparator means "no content
// refinement" — WAL lineage alone decides, exactly as before this feature existed.
type contentComparator func(a, b authorityInput) contentRelation

// parseTimelineHistory parses a PostgreSQL <n>.history file into ordered switch
// points. Each meaningful line is "<parentTimeline>\t<switchLSN>\t<reason>"; blank
// lines and malformed lines are skipped. A timeline-1 instance has no history file
// and yields an empty slice — the common healthy case.
func parseTimelineHistory(raw string) []switchPoint {
	var sps []switchPoint
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		tl := parseTimelineInt(fields[0])
		lsn := parseLSNValue(fields[1])
		if tl == 0 || lsn == 0 {
			// A 0 timeline or unparseable LSN is not a usable switch point. Skip it
			// rather than fabricate a fork at 0/0 (which would corrupt the lineage).
			continue
		}
		sps = append(sps, switchPoint{ParentTimeline: tl, SwitchLSN: lsn})
	}
	return sps
}

// historyForTimeline selects one timeline's history from a raw blob that emits each
// pg_wal/*.history file under a "###<filename>" marker (see cnpgHistoryCmd). A node on
// timeline N has its full, clean lineage in 0000000N.history; every OTHER file repeats
// only the earlier switches, so concatenating them all corrupts the lineage. We take
// exactly the current timeline's file. Backward/degenerate cases: a blob with NO
// markers is assumed to already be a single clean lineage and returned as-is (keeps
// hand-built history working); timeline 0/unknown or a missing file yields "".
func historyForTimeline(raw string, timeline int64) string {
	if !strings.Contains(raw, "###") {
		return raw
	}
	if timeline <= 0 {
		return ""
	}
	want := fmt.Sprintf("###%08X.history", timeline)
	var out []string
	in := false
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "###") {
			in = strings.TrimSpace(line) == want
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// commonPrefixLen returns how many leading switch points two lineages share exactly.
func commonPrefixLen(a, b []switchPoint) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// relate computes the provable data-extent relationship between two readable
// instances and the LSN of the fork that separates their lineages, oriented for the
// (a, b) argument order. It is deliberately conservative: any structure it cannot
// prove is a clean ancestor/descendant relationship is reported as divergence
// (fail closed).
func relate(a, b authorityInput) (relation, int64) {
	k := commonPrefixLen(a.Switches, b.Switches)
	la, lb := len(a.Switches), len(b.Switches)

	switch {
	case k == la && k == lb:
		// Identical switch chains → same lineage. Timelines must agree; if they
		// somehow don't, refuse to rank (treat as divergence).
		if a.Timeline != b.Timeline {
			return relDiverge, lastSwitchLSN(a.Switches, k)
		}
		switch {
		case a.CheckpointLSN > b.CheckpointLSN:
			return relADominatesB, 0
		case b.CheckpointLSN > a.CheckpointLSN:
			return relBDominatesA, 0
		default:
			return relEqual, 0
		}

	case k == lb && k < la:
		// b's whole chain is the shared prefix and a is longer → a extends b.
		return extensionRelation(a, b, true /* extender is a */)

	case k == la && k < lb:
		// a's whole chain is the shared prefix and b is longer → b extends a.
		return extensionRelation(b, a, false /* extender is b */)

	default:
		// k < la && k < lb: real fork — they took different switches after a shared
		// ancestor. The provably-shared history ends at the last agreed switch.
		forkLSN := lastSwitchLSN(a.Switches, k)
		aPast := a.CheckpointLSN > forkLSN
		bPast := b.CheckpointLSN > forkLSN
		switch {
		case aPast && bPast:
			return relDiverge, forkLSN
		case aPast && !bPast:
			// b never wrote past the fork — its branch is dataless there; a wins.
			return relADominatesB, forkLSN
		case bPast && !aPast:
			return relBDominatesA, forkLSN
		default:
			return relEqual, forkLSN
		}
	}
}

// extensionRelation handles the case where ext's lineage strictly extends anc's
// (ext forked beyond anc's tip). ext dominates anc only when anc has no committed
// data past the fork where ext left it; otherwise anc carries orphaned committed
// data and the two have diverged. extenderIsA fixes the returned relation's
// orientation to the caller's original (a, b) order.
func extensionRelation(ext, anc authorityInput, extenderIsA bool) (relation, int64) {
	fork := ext.Switches[len(anc.Switches)] // where ext branched beyond anc's history
	// The branch beyond anc must fork from anc's own timeline to be a clean
	// extension; anything else is an unexpected structure — refuse.
	if fork.ParentTimeline != anc.Timeline {
		return relDiverge, fork.SwitchLSN
	}
	// Past the fork the two are on different branches: anc continued on the lower
	// timeline, ext on the higher one. Whichever wrote committed WAL past the fork
	// holds data the other lacks.
	ancPast := anc.CheckpointLSN > fork.SwitchLSN
	extPast := ext.CheckpointLSN > fork.SwitchLSN
	switch {
	case !ancPast:
		// anc is a pure ancestor — everything it holds is on ext's line too → ext wins.
		return dom(extenderIsA), fork.SwitchLSN
	case !extPast:
		// Only anc wrote past the fork; ext is a dataless higher-timeline branch (the
		// classic fresh restore that sits on a bumped timeline with no writes). The
		// higher timeline NUMBER must not win — anc holds the real data → anc wins.
		return dom(!extenderIsA), fork.SwitchLSN
	default:
		// Both wrote committed WAL past the fork on divergent branches → no safe winner.
		return relDiverge, fork.SwitchLSN
	}
}

// dom returns relADominatesB when a is the dominant instance, else relBDominatesA.
func dom(aDominates bool) relation {
	if aDominates {
		return relADominatesB
	}
	return relBDominatesA
}

func lastSwitchLSN(sw []switchPoint, upto int) int64 {
	if upto <= 0 || len(sw) == 0 {
		return 0
	}
	if upto > len(sw) {
		upto = len(sw)
	}
	return sw[upto-1].SwitchLSN
}

// determineAuthority is the whole verdict. It never does I/O and never guesses:
// unread instances block, lineage decides recency, and true divergence refuses.
func determineAuthority(insts []authorityInput, content contentComparator) authorityDecision {
	var d authorityDecision

	// 1) Legibility gate. Any instance that could hold data but wasn't read blocks
	//    the decision outright — we do not rank past a node we are blind to.
	for _, in := range insts {
		if in.ReadState == ReadStateUnread {
			reason := in.UnreadReason
			if reason == "" {
				reason = "position UNREAD"
			}
			d.Blockers = append(d.Blockers, fmt.Sprintf("%s: %s", in.Pod, reason))
		}
	}
	if len(d.Blockers) > 0 {
		d.Determinable = false
		return d
	}

	// 2) Only Read instances carry data to rank; AbsentNoData holds nothing and
	//    cannot lead.
	var readable []authorityInput
	for _, in := range insts {
		if in.ReadState == ReadStateRead {
			readable = append(readable, in)
		}
	}
	if len(readable) == 0 {
		// Every instance is provably empty. Determinable, but there is no authority
		// to protect — the caller treats "" as an empty cluster.
		d.Determinable = true
		return d
	}
	if len(readable) == 1 {
		d.Determinable = true
		d.Leader = readable[0].Pod
		d.LeaderReason = "the only instance with readable data"
		return d
	}

	// 3) Pairwise lineage comparison. Any divergence → determinable-but-no-winner.
	n := len(readable)
	dominatedBy := make([]int, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			rel, forkLSN := relate(readable[i], readable[j])
			// A WAL divergence is not the final word: it only proves each branch wrote
			// SOME WAL past the fork. If a content witness proves one branch's committed
			// business rows contain the other's, the divergence is churn-only and has a
			// safe winner. Content can only ever RESOLVE a divergence to a clean
			// relationship; it never manufactures one where WAL saw none.
			if rel == relDiverge && content != nil {
				switch content(readable[i], readable[j]) {
				case contentAContainsB:
					rel = relADominatesB
					d.ContentNotes = append(d.ContentNotes, contentResolvedNote(readable[i], readable[j], forkLSN))
				case contentBContainsA:
					rel = relBDominatesA
					d.ContentNotes = append(d.ContentNotes, contentResolvedNote(readable[j], readable[i], forkLSN))
				case contentEqual:
					rel = relEqual
					d.ContentNotes = append(d.ContentNotes, contentEqualNote(readable[i], readable[j], forkLSN))
				case contentCrossed, contentUnknown:
					// The witness could not clear the WAL divergence (genuine business
					// divergence, or an instance was not content-readable) — it stands.
				}
			}
			switch rel {
			case relDiverge:
				d.Divergences = append(d.Divergences, describeDivergence(readable[i], readable[j], forkLSN))
			case relADominatesB:
				dominatedBy[j]++
			case relBDominatesA:
				dominatedBy[i]++
			case relEqual:
				// neither strictly dominates; both remain leader candidates
			}
		}
	}
	if len(d.Divergences) > 0 {
		d.Determinable = true
		d.Diverged = true
		return d
	}

	// 4) The leader is dominated by no one. Every zero-dominated instance is,
	//    transitively, pairwise-equal to the others (a strict dominator would have
	//    bumped its count), so any of them is a safe authority — prefer the current
	//    primary, else the furthest checkpoint LSN, else a stable name order.
	var candidates []authorityInput
	for i := 0; i < n; i++ {
		if dominatedBy[i] == 0 {
			candidates = append(candidates, readable[i])
		}
	}
	if len(candidates) == 0 {
		// No maximum (should be impossible without divergence) — refuse rather than guess.
		d.Determinable = false
		d.Blockers = append(d.Blockers,
			"no instance dominates the rest and none diverged — authority is inconsistent; escrow all and inspect manually")
		return d
	}
	leader := pickLeader(candidates)
	d.Determinable = true
	d.Leader = leader.Pod
	if leader.IsPrimary {
		d.LeaderReason = "the primary holds the most-advanced committed WAL of every readable instance"
	} else {
		d.LeaderReason = fmt.Sprintf(
			"%s holds committed WAL that the current primary lacks (a lower-numbered timeline can lead a stale restore) — "+
				"do NOT heal from the primary; escrow, then promote this instance", leader.Pod)
	}
	return d
}

// pickLeader chooses among mutually-equal leader candidates: the primary first,
// then the furthest checkpoint LSN, then a stable pod-name order. All candidates
// are equivalent in data, so this only affects which one we name — never safety.
func pickLeader(cands []authorityInput) authorityInput {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].IsPrimary != cands[j].IsPrimary {
			return cands[i].IsPrimary
		}
		if cands[i].CheckpointLSN != cands[j].CheckpointLSN {
			return cands[i].CheckpointLSN > cands[j].CheckpointLSN
		}
		return cands[i].Pod < cands[j].Pod
	})
	return cands[0]
}

// contentResolvedNote records a WAL divergence that the content witness cleared:
// winner's committed business rows contain loser's, so the divergent WAL on loser's
// branch is churn (checkpoints, restart recovery, repeatable migrations), not data.
func contentResolvedNote(winner, loser authorityInput, forkLSN int64) string {
	return fmt.Sprintf(
		"%s and %s diverged in WAL past the fork at %s, but the content witness proves %s's committed "+
			"business rows contain %s's — %s wrote no business data past the fork (its extra WAL is churn). "+
			"Resolved: %s is a safe authority over %s.",
		winner.Pod, loser.Pod, formatLSN(forkLSN), winner.Pod, loser.Pod, loser.Pod, winner.Pod, loser.Pod)
}

// contentEqualNote records a WAL divergence where the witness proves identical
// business extent on both branches — either is a safe authority.
func contentEqualNote(a, b authorityInput, forkLSN int64) string {
	return fmt.Sprintf(
		"%s and %s diverged in WAL past the fork at %s, but the content witness proves identical business "+
			"extent on both — the divergence is churn-only; either is a safe authority.",
		a.Pod, b.Pod, formatLSN(forkLSN))
}

func describeDivergence(a, b authorityInput, forkLSN int64) string {
	return fmt.Sprintf(
		"%s (timeline %d, checkpoint %s, %s WAL past fork) and %s (timeline %d, checkpoint %s, %s WAL past fork) "+
			"both hold committed WAL past their shared fork at %s — no instance is a safe authority; escrow BOTH and "+
			"choose the surviving lineage manually. NOTE: WAL volume is how much each branch WROTE, not how much it "+
			"matters — a stale restore left idle still accrues WAL (checkpoints, heartbeats), so weigh the CONTENT, "+
			"not just the size.",
		a.Pod, a.Timeline, formatLSN(a.CheckpointLSN), formatWALPastFork(a.CheckpointLSN, forkLSN),
		b.Pod, b.Timeline, formatLSN(b.CheckpointLSN), formatWALPastFork(b.CheckpointLSN, forkLSN),
		formatLSN(forkLSN))
}

// formatWALPastFork renders how much WAL an instance wrote past the fork
// (checkpoint − fork), the quantitative "how far did this branch diverge" signal the
// operator needs to weigh the lineages. A negative delta (checkpoint at/behind the
// fork — a dataless branch) renders "0 B".
func formatWALPastFork(checkpointLSN, forkLSN int64) string {
	d := checkpointLSN - forkLSN
	switch {
	case d <= 0:
		return "0 B"
	case d >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(d)/(1<<30))
	case d >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(d)/(1<<20))
	case d >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(d)/(1<<10))
	default:
		return fmt.Sprintf("%d B", d)
	}
}

// formatLSN renders a parsed LSN back to PostgreSQL's HEX/HEX form for messages.
func formatLSN(v int64) string {
	if v == 0 {
		return "0/0"
	}
	return fmt.Sprintf("%X/%X", v>>32, v&0xFFFFFFFF)
}
