package reale2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Shallow cache/tmp activity observation (design 12a-3-cursor-worker,
// slice 3a FP-3). Alongside the before-first-executable and after-cleanup
// fingerprints, a bounded observer records shallow samples of three roots
// (~/.cache, /tmp, /var/tmp): at most 64 immediate entries of each root
// (depth 1), consuming at most one more to detect overflow, and for each
// sampled owned real directory at most 64 immediate children (depth 2),
// again with one sentinel; their lstat metadata is aggregated and nothing
// is descended further. It reads metadata only, never opens a regular
// file, follows no symlink (directory descriptors and no-follow child
// opens), creates nothing and never walks a tree recursively. Work is
// independent of deeper host clutter. Every bound, absence, foreign or
// unreadable entry and race is a report-only observation-quality flag:
// it never makes the probe fail, never exempts a protected path and never
// claims attribution to Cursor, a recursive total or confinement.

// Activity aggregate schema and bounds (policy, not measurements).
const (
	ActivitySchema     = "callsheet-cursor-activity-aggregate/v1"
	activitySampling   = "directory-prefix"
	activityMaxDepth   = 2
	activityMaxEntries = 64
	activityMaxOutput  = 256 << 10
	activityScanBudget = 5 * time.Second
	// activityMaxName bounds a recorded (encoded) name; a longer one is a
	// counted name_unrecorded observation, never a truncated identity.
	activityMaxName = 512
	// activitySkeleton reserves the serialized size of the record's fixed
	// fields and root summaries before any row is appended.
	activitySkeleton = 8 << 10
)

// Activity root scopes, in record order.
const (
	activityScopeCache  = "cache"
	activityScopeTmp    = "tmp"
	activityScopeVarTmp = "var-tmp"
)

// Activity statuses: named roots use foreign only as an ownership
// annotation (they are still enumerated); a foreign child is never
// descended.
const (
	actOK         = "ok"
	actAbsent     = "absent"
	actUnreadable = "unreadable"
	actForeign    = "foreign"
	actRaced      = "raced"
	actNotDir     = "not_directory"
)

// Activity entry types.
const (
	actFile    = "file"
	actDir     = "directory"
	actSymlink = "symlink"
	actOther   = "other"
	actUnknown = "unknown"
)

// ActivityCounts are saturated unsigned counts by entry type.
type ActivityCounts struct {
	File      uint64 `json:"file"`
	Directory uint64 `json:"directory"`
	Symlink   uint64 `json:"symlink"`
	Other     uint64 `json:"other"`
	Unknown   uint64 `json:"unknown"`
}

// ActivityFlags is a root's or row's closed observation-quality record.
type ActivityFlags struct {
	EntryLimited      bool   `json:"entry_limited"`
	DepthLimited      bool   `json:"depth_limited"`
	OutputLimited     bool   `json:"output_limited"`
	TimeLimited       bool   `json:"time_limited"`
	NumericOverflow   bool   `json:"numeric_overflow"`
	ForeignEntries    uint64 `json:"foreign_entries"`
	UnreadableEntries uint64 `json:"unreadable_entries"`
	RacedEntries      uint64 `json:"raced_entries"`
	NameUnrecorded    uint64 `json:"name_unrecorded"`
	ExcludedEntries   uint64 `json:"excluded_entries"`
}

// ActivityLimits are the record's bounds: 64/64/256 KiB/5s.
type ActivityLimits struct {
	TopLevelEntries int   `json:"top_level_entries"`
	ChildEntries    int   `json:"child_entries"`
	OutputBytes     int   `json:"output_bytes"`
	ScanBudgetMS    int64 `json:"scan_budget_ms"`
}

// ActivityRootCounts are a root's depth-1 observations (direct) and its
// retained rows' summed depth-2 samples (children).
type ActivityRootCounts struct {
	Direct   ActivityCounts `json:"direct"`
	Children ActivityCounts `json:"children"`
}

// ActivityRootBytes are the same two scopes' summed sampled regular-file
// st_size (logical size, never disk allocation; directories add none).
type ActivityRootBytes struct {
	Direct   uint64 `json:"direct"`
	Children uint64 `json:"children"`
}

// ActivityRow is one sampled top-level directory (or a failed or unknown
// top-level entry): its encoded basename, observed type and status, the
// SHA-256 of its own lstat metadata (null when unavailable), the counts
// and summed regular-file sizes of its sampled immediate children and
// their newest mtime (null without sampled metadata).
type ActivityRow struct {
	Name               string         `json:"name"`
	Type               string         `json:"type"`
	Status             string         `json:"status"`
	MetadataSHA256     *string        `json:"metadata_sha256"`
	ObservedCounts     ActivityCounts `json:"observed_counts"`
	ObservedFileBytes  uint64         `json:"observed_file_bytes"`
	NewestMtimeNS      *int64         `json:"newest_mtime_ns"`
	ChildrenExhaustive bool           `json:"children_exhaustive"`
	Flags              ActivityFlags  `json:"flags"`
}

// ActivityRoot is one root's summary and its retained rows (sorted by
// name for serialization only; sampling is directory-enumeration order).
type ActivityRoot struct {
	Scope              string             `json:"scope"`
	Path               string             `json:"path"`
	Status             string             `json:"status"`
	Exhaustive         bool               `json:"exhaustive"`
	TopLevelExhaustive bool               `json:"top_level_exhaustive"`
	Flags              ActivityFlags      `json:"flags"`
	ObservedCounts     ActivityRootCounts `json:"observed_counts"`
	ObservedFileBytes  ActivityRootBytes  `json:"observed_file_bytes"`
	Rows               []ActivityRow      `json:"rows"`
}

// ActivityAggregate is environment/cursor-activity-<phase>.json.
type ActivityAggregate struct {
	Schema   string         `json:"schema"`
	RunID    string         `json:"run_id"`
	Phase    string         `json:"phase"`
	Sampling string         `json:"sampling"`
	MaxDepth int            `json:"max_depth"`
	Limits   ActivityLimits `json:"limits"`
	Roots    []ActivityRoot `json:"roots"`
}

// activityRootSpec is one named root: its scope, its absolute path and
// its tokenized evidence spelling.
type activityRootSpec struct {
	Scope, Path, Token string
}

// activityStat is one lstat (or fstat) observation.
type activityStat struct {
	Type             string
	Mode, UID        uint32
	Dev, Ino         uint64
	Size             int64
	MtimeNS, CtimeNS int64
}

// activityFS opens and lstat's the named roots; activityDir is one open
// directory descriptor whose children are lstat'd and opened relative to
// it, never following a symlink.
type activityFS interface {
	Lstat(path string) (activityStat, error)
	OpenDir(path string) (activityDir, error)
}

type activityDir interface {
	// Stat is the open directory's own fstat.
	Stat() (activityStat, error)
	// Next returns at most n further entry names, name-only: it performs
	// no metadata read of any entry (the overflow sentinel included) and
	// drops none; io.EOF ends the directory. Every entry's type comes from
	// the observer's own explicit no-follow Lstat of a retained entry.
	Next(n int) ([]string, error)
	// Lstat is fstatat(name, AT_SYMLINK_NOFOLLOW).
	Lstat(name string) (activityStat, error)
	// OpenDir is openat(name, O_DIRECTORY|O_NOFOLLOW).
	OpenDir(name string) (activityDir, error)
	Close() error
}

// activityObserver runs one bounded observation phase.
type activityObserver struct {
	fs      activityFS
	clock   Clock
	uid     uint32
	exclude []string
	// output is the per-phase serialized bound (activityMaxOutput; tests
	// lower it).
	output int
}

// observe samples roots within deadline (the 5s budget clipped by the
// caller to the remaining attempt or finalization deadline). It never
// fails: every problem is a flag of the record.
func (o activityObserver) observe(runID, phase string, roots []activityRootSpec, deadline time.Time) *ActivityAggregate {
	limit := o.output
	if limit <= 0 {
		limit = activityMaxOutput
	}
	agg := &ActivityAggregate{Schema: ActivitySchema, RunID: runID, Phase: phase, Sampling: activitySampling, MaxDepth: activityMaxDepth,
		Limits: ActivityLimits{TopLevelEntries: activityMaxEntries, ChildEntries: activityMaxEntries, OutputBytes: activityMaxOutput,
			ScanBudgetMS: activityScanBudget.Milliseconds()}, Roots: []ActivityRoot{}}
	sc := &activityScan{o: o, deadline: deadline, used: activitySkeleton, limit: limit}
	for _, r := range roots {
		agg.Roots = append(agg.Roots, sc.root(r))
	}
	return agg
}

// activityScan is one phase's shared work and output budget.
type activityScan struct {
	o        activityObserver
	deadline time.Time
	used     int
	limit    int
}

// late reports the cooperative scan budget spent (checked between
// bounded filesystem operations; a blocked syscall is not interrupted).
func (sc *activityScan) late() bool { return !sc.o.clock.Now().Before(sc.deadline) }

// excluded reports the run's exact evidence or runtime subtree.
func (sc *activityScan) excluded(p string) bool {
	for _, x := range sc.o.exclude {
		if x != "" && under(p, x) {
			return true
		}
	}
	return false
}

// root observes one named root at depth 0 and its depth-1 sample.
func (sc *activityScan) root(spec activityRootSpec) ActivityRoot {
	r := ActivityRoot{Scope: spec.Scope, Path: spec.Token, Rows: []ActivityRow{}}
	if sc.late() {
		r.Status, r.Flags.TimeLimited = actUnreadable, true
		return r
	}
	st, err := sc.o.fs.Lstat(spec.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// An absent root is an explicit, complete observation.
		r.Status, r.Exhaustive, r.TopLevelExhaustive = actAbsent, true, true
		return r
	case err != nil:
		r.Status = actUnreadable
		return r
	case st.Type != actDir:
		// A symlink root is never followed.
		r.Status = actNotDir
		return r
	}
	r.Status = actOK
	if st.UID != sc.o.uid {
		// Ownership annotation only: /tmp and /var/tmp are still enumerated.
		r.Status = actForeign
	}
	d, err := sc.o.fs.OpenDir(spec.Path)
	if err != nil {
		r.Status = openStatus(err)
		return r
	}
	defer d.Close()
	if own, err := d.Stat(); err != nil || own.Dev != st.Dev || own.Ino != st.Ino {
		r.Status = actRaced
		return r
	}
	rd := sc.read(d)
	r.TopLevelExhaustive = rd.eof
	r.Flags.EntryLimited, r.Flags.TimeLimited = rd.limited, rd.timed
	if rd.failed {
		r.Flags.UnreadableEntries = satInc(r.Flags.UnreadableEntries, &r.Flags.NumericOverflow)
	}
	skipped := false
	for _, e := range rd.ents {
		if sc.late() {
			// The names were enumerated; the rest of the sample is unobserved.
			r.Flags.TimeLimited, skipped = true, true
			break
		}
		if !sc.entry(&r, d, spec.Path, e) {
			skipped = true
		}
	}
	sort.Slice(r.Rows, func(i, j int) bool { return r.Rows[i].Name < r.Rows[j].Name })
	r.Exhaustive = r.TopLevelExhaustive && !skipped && !r.Flags.TimeLimited && !r.Flags.OutputLimited && r.Flags.NameUnrecorded == 0 &&
		r.Flags.ExcludedEntries == 0 && r.Flags.UnreadableEntries == 0 && r.Flags.RacedEntries == 0 && r.Flags.ForeignEntries == 0
	for _, row := range r.Rows {
		r.Exhaustive = r.Exhaustive && row.ChildrenExhaustive && row.Flags.UnreadableEntries == 0 && row.Flags.RacedEntries == 0 &&
			row.Flags.ExcludedEntries == 0 && !row.Flags.TimeLimited
	}
	return r
}

// activityRead is one bounded enumeration: the sampled entries, whether
// EOF was reached within the bound, whether the overflow sentinel was
// consumed (entry limit), whether the time budget stopped it and whether
// the directory stream failed.
type activityRead struct {
	ents                        []string
	eof, limited, timed, failed bool
}

// read enumerates at most activityMaxEntries entries plus one overflow
// sentinel, in directory order, consuming no more than the remaining
// budget plus that sentinel per batch. The sentinel is consumed only to
// detect overflow and is never sampled or stat'd.
func (sc *activityScan) read(d activityDir) activityRead {
	var r activityRead
	for {
		if sc.late() {
			r.timed = true
			return r
		}
		batch, err := d.Next(activityMaxEntries + 1 - len(r.ents))
		r.ents = append(r.ents, batch...)
		switch {
		case len(r.ents) > activityMaxEntries:
			r.ents, r.limited = r.ents[:activityMaxEntries], true
			return r
		case errors.Is(err, io.EOF):
			r.eof = true
			return r
		case err != nil, len(batch) == 0:
			// An unreadable directory stream ends the sample incomplete.
			r.failed = true
			return r
		}
	}
}

// entry observes one depth-1 entry; it reports false when the entry was
// skipped or could not be fully observed.
func (sc *activityScan) entry(r *ActivityRoot, d activityDir, root string, entName string) bool {
	name := encodePath(entName)
	if len(name) > activityMaxName || entName == "" {
		r.Flags.NameUnrecorded = satInc(r.Flags.NameUnrecorded, &r.Flags.NumericOverflow)
		return false
	}
	if sc.excluded(filepath.Join(root, entName)) {
		r.Flags.ExcludedEntries = satInc(r.Flags.ExcludedEntries, &r.Flags.NumericOverflow)
		return false
	}
	// The name-only directory read and this explicit no-follow lstat are
	// two operations: a vanished entry is a race, recorded (never dropped)
	// with type unknown, and never followed.
	st, err := d.Lstat(entName)
	if err != nil {
		typ := actUnknown
		status := actUnreadable
		if errors.Is(err, fs.ErrNotExist) {
			status = actRaced
			r.Flags.RacedEntries = satInc(r.Flags.RacedEntries, &r.Flags.NumericOverflow)
		} else {
			r.Flags.UnreadableEntries = satInc(r.Flags.UnreadableEntries, &r.Flags.NumericOverflow)
		}
		countType(&r.ObservedCounts.Direct, typ, &r.Flags.NumericOverflow)
		sc.appendRow(r, ActivityRow{Name: name, Type: typ, Status: status})
		return false
	}
	countType(&r.ObservedCounts.Direct, st.Type, &r.Flags.NumericOverflow)
	foreign := st.UID != sc.o.uid
	if foreign {
		r.Flags.ForeignEntries = satInc(r.Flags.ForeignEntries, &r.Flags.NumericOverflow)
	}
	if st.Type == actFile {
		r.ObservedFileBytes.Direct = satAdd(r.ObservedFileBytes.Direct, uint64(max(st.Size, 0)), &r.Flags.NumericOverflow)
	}
	if st.Type != actDir {
		// Files, symlinks and other entries contribute counts only.
		return true
	}
	digest := metadataDigest(st)
	row := ActivityRow{Name: name, Type: actDir, Status: actOK, MetadataSHA256: &digest}
	if foreign {
		// Never traverse a foreign directory to compute its size.
		row.Status = actForeign
		sc.appendRow(r, row)
		return false
	}
	if r.Flags.OutputLimited {
		// No row can be retained: its children are not sampled either.
		return false
	}
	ok := sc.children(&row, d, filepath.Join(root, entName), entName, st, r)
	if !sc.appendRow(r, row) {
		return false
	}
	addCounts(&r.ObservedCounts.Children, row.ObservedCounts, &r.Flags.NumericOverflow)
	r.ObservedFileBytes.Children = satAdd(r.ObservedFileBytes.Children, row.ObservedFileBytes, &r.Flags.NumericOverflow)
	return ok
}

// children samples one owned real directory's immediate children (depth
// 2) through parent's descriptor, aggregating their lstat metadata
// without descending further. path is its absolute path (exclusions).
func (sc *activityScan) children(row *ActivityRow, parent activityDir, path, name string, st activityStat, r *ActivityRoot) bool {
	if sc.late() {
		row.Flags.TimeLimited, r.Flags.TimeLimited = true, true
		return false
	}
	d, err := parent.OpenDir(name)
	if err != nil {
		row.Status = openStatus(err)
		countFailure(row, r)
		return false
	}
	defer d.Close()
	if own, err := d.Stat(); err != nil || own.Dev != st.Dev || own.Ino != st.Ino {
		// Replaced (a symlink or another directory) since its lstat.
		row.Status = actRaced
		countFailure(row, r)
		return false
	}
	rd := sc.read(d)
	row.ChildrenExhaustive = rd.eof
	row.Flags.EntryLimited, row.Flags.TimeLimited = rd.limited, rd.timed
	if rd.failed {
		row.Flags.UnreadableEntries = satInc(row.Flags.UnreadableEntries, &row.Flags.NumericOverflow)
	}
	ok := rd.eof
	for _, e := range rd.ents {
		if sc.late() {
			row.Flags.TimeLimited = true
			ok = false
			break
		}
		if sc.excluded(filepath.Join(path, e)) {
			row.Flags.ExcludedEntries = satInc(row.Flags.ExcludedEntries, &row.Flags.NumericOverflow)
			ok = false
			continue
		}
		cs, err := d.Lstat(e)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				row.Flags.RacedEntries = satInc(row.Flags.RacedEntries, &row.Flags.NumericOverflow)
			} else {
				row.Flags.UnreadableEntries = satInc(row.Flags.UnreadableEntries, &row.Flags.NumericOverflow)
			}
			ok = false
			continue
		}
		countType(&row.ObservedCounts, cs.Type, &row.Flags.NumericOverflow)
		if cs.UID != sc.o.uid {
			row.Flags.ForeignEntries = satInc(row.Flags.ForeignEntries, &row.Flags.NumericOverflow)
		}
		switch cs.Type {
		case actFile:
			row.ObservedFileBytes = satAdd(row.ObservedFileBytes, uint64(max(cs.Size, 0)), &row.Flags.NumericOverflow)
		case actDir:
			// A depth-2 directory contributes its own type, never its contents.
			row.Flags.DepthLimited = true
		}
		if row.NewestMtimeNS == nil || cs.MtimeNS > *row.NewestMtimeNS {
			m := cs.MtimeNS
			row.NewestMtimeNS = &m
		}
	}
	if row.Flags.TimeLimited {
		r.Flags.TimeLimited = true
	}
	if row.Flags.NumericOverflow {
		r.Flags.NumericOverflow = true
	}
	return ok
}

// countFailure records a top-level directory that could not be opened
// or was replaced.
func countFailure(row *ActivityRow, r *ActivityRoot) {
	if row.Status == actRaced {
		r.Flags.RacedEntries = satInc(r.Flags.RacedEntries, &r.Flags.NumericOverflow)
		return
	}
	r.Flags.UnreadableEntries = satInc(r.Flags.UnreadableEntries, &r.Flags.NumericOverflow)
}

// appendRow retains row when its serialized size still fits the phase's
// output bound; otherwise the root keeps its summary and output_limited.
func (sc *activityScan) appendRow(r *ActivityRoot, row ActivityRow) bool {
	if r.Flags.OutputLimited {
		return false
	}
	b, err := json.Marshal(row)
	if err != nil || sc.used+len(b)+1 > sc.limit {
		r.Flags.OutputLimited = true
		return false
	}
	sc.used += len(b) + 1
	r.Rows = append(r.Rows, row)
	return true
}

// openStatus classifies a failed no-follow directory open: a symlink or
// non-directory substituted (or a vanished entry) is raced, anything
// else unreadable.
func openStatus(err error) string {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, fs.ErrNotExist) {
		return actRaced
	}
	return actUnreadable
}

// metadataDigest is the SHA-256 of the canonical JSON of a directory's
// own {type,mode,uid,dev,ino,mtime_ns,ctime_ns}, in that field order.
func metadataDigest(st activityStat) string {
	b, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Mode    uint32 `json:"mode"`
		UID     uint32 `json:"uid"`
		Dev     uint64 `json:"dev"`
		Ino     uint64 `json:"ino"`
		MtimeNS int64  `json:"mtime_ns"`
		CtimeNS int64  `json:"ctime_ns"`
	}{st.Type, st.Mode, st.UID, st.Dev, st.Ino, st.MtimeNS, st.CtimeNS})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// satAdd adds b to a, saturating at the uint64 maximum and setting
// *overflow.
func satAdd(a, b uint64, overflow *bool) uint64 {
	if a > math.MaxUint64-b {
		*overflow = true
		return math.MaxUint64
	}
	return a + b
}

func satInc(a uint64, overflow *bool) uint64 { return satAdd(a, 1, overflow) }

// countType counts one entry of type typ.
func countType(c *ActivityCounts, typ string, overflow *bool) {
	switch typ {
	case actFile:
		c.File = satInc(c.File, overflow)
	case actDir:
		c.Directory = satInc(c.Directory, overflow)
	case actSymlink:
		c.Symlink = satInc(c.Symlink, overflow)
	case actOther:
		c.Other = satInc(c.Other, overflow)
	default:
		c.Unknown = satInc(c.Unknown, overflow)
	}
}

// addCounts adds b into a, saturating.
func addCounts(a *ActivityCounts, b ActivityCounts, overflow *bool) {
	a.File = satAdd(a.File, b.File, overflow)
	a.Directory = satAdd(a.Directory, b.Directory, overflow)
	a.Symlink = satAdd(a.Symlink, b.Symlink, overflow)
	a.Other = satAdd(a.Other, b.Other, overflow)
	a.Unknown = satAdd(a.Unknown, b.Unknown, overflow)
}

// ---- Comparison ----

// Activity comparison marks.
const (
	markChanged            = "changed"
	markSampleChanged      = "sample_changed"
	markNew                = "new"
	markNewlyObserved      = "newly_observed"
	markRemoved            = "removed"
	markNotObservedAfter   = "not_observed_after"
	markMetadataUnobserved = "metadata_unobserved"
)

// ActivityChange is one compared row's marker (a changed sample is not a
// complete diff, and nothing is inferred from a truncated sample).
type ActivityChange struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
	Mark  string `json:"mark"`
}

// nameCoverage reports whether a root's retained directory-name coverage
// was exhaustive: complete top-level enumeration and no output limit,
// unrecorded name, unreadable, raced or unknown top-level entry, sample
// limit or time limit that could hide a name.
func nameCoverage(r ActivityRoot) bool {
	if r.Status == actAbsent {
		return true
	}
	if r.Status != actOK && r.Status != actForeign {
		return false
	}
	f := r.Flags
	if !r.TopLevelExhaustive || f.OutputLimited || f.NameUnrecorded > 0 || f.UnreadableEntries > 0 || f.RacedEntries > 0 || f.EntryLimited || f.TimeLimited {
		return false
	}
	for _, row := range r.Rows {
		if row.Type == actUnknown {
			return false
		}
	}
	return true
}

// compareActivity compares the retained rows of two phases by scope and
// basename.
func compareActivity(before, after *ActivityAggregate) []ActivityChange {
	out := []ActivityChange{}
	if before == nil || after == nil {
		return out
	}
	for _, ar := range after.Roots {
		var br *ActivityRoot
		for i := range before.Roots {
			if before.Roots[i].Scope == ar.Scope {
				br = &before.Roots[i]
			}
		}
		if br == nil {
			continue
		}
		brows, arows := rowsByName(br.Rows), rowsByName(ar.Rows)
		for _, name := range unionNames(brows, arows) {
			b, inB := brows[name]
			a, inA := arows[name]
			add := func(mark string) { out = append(out, ActivityChange{Scope: ar.Scope, Name: name, Mark: mark}) }
			switch {
			case inB && inA:
				if b.MetadataSHA256 == nil || a.MetadataSHA256 == nil {
					add(markMetadataUnobserved)
					continue
				}
				if *b.MetadataSHA256 != *a.MetadataSHA256 {
					add(markChanged)
				}
				if sampleDiffers(b, a) {
					add(markSampleChanged)
				}
			case inA && nameCoverage(*br):
				add(markNew)
			case inA:
				add(markNewlyObserved)
			case nameCoverage(ar):
				add(markRemoved)
			default:
				add(markNotObservedAfter)
			}
		}
	}
	return out
}

// sampleDiffers reports differing sampled aggregates (possibly sampling
// churn, never a complete diff).
func sampleDiffers(a, b ActivityRow) bool {
	ma, mb := int64(math.MinInt64), int64(math.MinInt64)
	if a.NewestMtimeNS != nil {
		ma = *a.NewestMtimeNS
	}
	if b.NewestMtimeNS != nil {
		mb = *b.NewestMtimeNS
	}
	return a.ObservedCounts != b.ObservedCounts || a.ObservedFileBytes != b.ObservedFileBytes || ma != mb ||
		a.ChildrenExhaustive != b.ChildrenExhaustive || a.Status != b.Status
}

func rowsByName(rows []ActivityRow) map[string]ActivityRow {
	m := map[string]ActivityRow{}
	for _, r := range rows {
		m[r.Name] = r
	}
	return m
}

func unionNames(a, b map[string]ActivityRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]ActivityRow{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- Live filesystem ----

// liveActivityFS is the Unix filesystem through no-follow descriptors.
type liveActivityFS struct{}

// openNoFollowDir is the shared no-follow, nonblocking directory open.
const openNoFollowDir = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

func (liveActivityFS) Lstat(path string) (activityStat, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return activityStat{}, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return statOf(&st), nil
}

func (liveActivityFS) OpenDir(path string) (activityDir, error) {
	fd, err := unix.Open(path, openNoFollowDir, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &liveActivityDir{fd: fd, f: os.NewFile(uintptr(fd), path)}, nil
}

// liveActivityDir is one open directory descriptor.
type liveActivityDir struct {
	fd int
	f  *os.File
}

func (d *liveActivityDir) Stat() (activityStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return activityStat{}, err
	}
	return statOf(&st), nil
}

// Next is name-only enumeration (Readdirnames): unlike ReadDir, which
// lstat's an entry whose directory type is unknown (the overflow sentinel
// included) and silently drops one that vanished, it reads no metadata
// and drops nothing; the observer lstat's only the retained entries.
func (d *liveActivityDir) Next(n int) ([]string, error) {
	return d.f.Readdirnames(n)
}

func (d *liveActivityDir) Lstat(name string) (activityStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return activityStat{}, &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	return statOf(&st), nil
}

func (d *liveActivityDir) OpenDir(name string) (activityDir, error) {
	fd, err := unix.Openat(d.fd, name, openNoFollowDir, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return &liveActivityDir{fd: fd, f: os.NewFile(uintptr(fd), name)}, nil
}

func (d *liveActivityDir) Close() error { return d.f.Close() }

// statOf converts a Unix stat result (Linux and Darwin field widths).
func statOf(st *unix.Stat_t) activityStat {
	mode := uint32(st.Mode)
	out := activityStat{Mode: mode, UID: st.Uid, Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: st.Size,
		MtimeNS: st.Mtim.Nano(), CtimeNS: st.Ctim.Nano()}
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		out.Type = actDir
	case unix.S_IFREG:
		out.Type = actFile
	case unix.S_IFLNK:
		out.Type = actSymlink
	default:
		out.Type = actOther
	}
	return out
}
