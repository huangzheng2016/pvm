// Package threeway compares a sync workspace's baseline with the host and
// guest trees and plans push and pull. It does no I/O.
package threeway

import (
	"fmt"
	"sort"
	"strings"

	"github.com/huangzheng2016/pvm/internal/workspace"
)

type Direction int

const (
	Push Direction = iota // host to VM
	Pull                  // VM to host
)

type Resolution int

const (
	None Resolution = iota
	PreferHost
	PreferVM
	KeepBoth
)

type Kind string

const (
	Added    Kind = "added"
	Modified Kind = "modified"
	Deleted  Kind = "deleted"
)

type Change struct {
	Path string
	Kind Kind
}

type Rename struct{ From, To string }

// Conflict is a path both sides changed differently since the baseline. A
// nil entry means the path is absent on that side.
type Conflict struct {
	Path              string
	Host, Guest, Base *workspace.Entry
}

// Plan lists the writes that make a push or pull happen. Apply the renames
// on both sides first; after that deletes (deepest first) and copies
// (parents first) are independent. Copies name the source side's entry:
// ToGuest entries are read from the host and ToHost entries from the guest.
// A path whose type changes appears both as a delete and as a copy.
type Plan struct {
	ToGuest       []workspace.Entry
	DeleteOnGuest []string
	ToHost        []workspace.Entry
	DeleteOnHost  []string
	RenameOnGuest []Rename
	RenameOnHost  []Rename
	Conflicts     []Conflict
	// Unresolved is set when there are conflicts and no resolution; the
	// plan then holds only Conflicts and nothing may be written.
	Unresolved bool
	// Synced holds every path, including conflict copies and paths deleted
	// on both sides, whose two sides are equal once the plan is applied.
	Synced map[string]bool
	// Pending lists paths that still differ, changed only on the side this
	// direction does not read from.
	Pending []string
}

// Summary is what Status reports.
type Summary struct {
	HostChanged  []Change
	GuestChanged []Change
	Conflicts    []Conflict
}

func content(base *workspace.Baseline) workspace.Manifest {
	if base == nil {
		return nil
	}
	return base.Content
}

func lookup(m workspace.Manifest, p string) *workspace.Entry {
	e, ok := m[p]
	if !ok {
		return nil
	}
	return &e
}

func same(a, b *workspace.Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.SameContent(*b)
}

func isDir(e *workspace.Entry) bool { return e != nil && e.Type == workspace.TypeDir }

func parent(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func union(ms ...workspace.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		for p := range m {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Diff lists how side changed since the baseline; a nil baseline is empty.
func Diff(base *workspace.Baseline, side workspace.Manifest) []Change {
	b := content(base)
	var out []Change
	for _, p := range union(b, side) {
		be, se := lookup(b, p), lookup(side, p)
		switch {
		case be == nil && se != nil:
			out = append(out, Change{p, Added})
		case be != nil && se == nil:
			out = append(out, Change{p, Deleted})
		case !same(be, se):
			out = append(out, Change{p, Modified})
		}
	}
	return out
}

type node struct {
	b, h, g *workspace.Entry
	hc, gc  bool
}

type analysis struct {
	paths []string
	nodes map[string]*node
	// roots are the topmost conflicting paths; top maps each root and
	// every path below it to its root, so the whole subtree follows the
	// root's resolution.
	roots []string
	top   map[string]string
}

func analyze(base *workspace.Baseline, host, guest workspace.Manifest) *analysis {
	b := content(base)
	a := &analysis{paths: union(b, host, guest), nodes: map[string]*node{}, top: map[string]string{}}
	for _, p := range a.paths {
		n := &node{b: lookup(b, p), h: lookup(host, p), g: lookup(guest, p)}
		n.hc, n.gc = !same(n.b, n.h), !same(n.b, n.g)
		a.nodes[p] = n
	}
	conflict := map[string]bool{}
	for _, p := range a.paths {
		n := a.nodes[p]
		if n.hc && n.gc && !same(n.h, n.g) {
			conflict[p] = true
		}
		// A side that removed or replaced a directory conflicts with
		// anything the other side added or changed below it.
		for _, hostSide := range []bool{true, false} {
			oc, oe := n.gc, n.g
			if hostSide {
				oc, oe = n.hc, n.h
			}
			if !oc || oe == nil {
				continue
			}
			for d := parent(p); d != ""; d = parent(d) {
				an := a.nodes[d]
				if an == nil {
					continue
				}
				xc, xe := an.hc, an.h
				if hostSide {
					xc, xe = an.gc, an.g
				}
				if xc && !isDir(xe) {
					conflict[d] = true
				}
			}
		}
	}
	for _, p := range a.paths {
		r := ""
		for d := p; d != ""; d = parent(d) {
			if conflict[d] {
				r = d
			}
		}
		if r == "" {
			continue
		}
		a.top[p] = r
		if r == p {
			a.roots = append(a.roots, p)
		}
	}
	return a
}

func (a *analysis) conflicts() []Conflict {
	var out []Conflict
	for _, p := range a.roots {
		n := a.nodes[p]
		out = append(out, Conflict{Path: p, Host: n.h, Guest: n.g, Base: n.b})
	}
	return out
}

func Status(base *workspace.Baseline, host, guest workspace.Manifest) *Summary {
	return &Summary{
		HostChanged:  Diff(base, host),
		GuestChanged: Diff(base, guest),
		Conflicts:    analyze(base, host, guest).conflicts(),
	}
}

// Plan works out a push or pull. Paths changed on one side only move in
// the given direction; the rest stay Pending. Conflicts stop the plan unless
// res resolves them so that both sides end up equal:
//   - PreferHost and PreferVM take that side's version, or its deletion.
//   - KeepBoth keeps the direction's source version at the path and the
//     other side's version, with everything below it, on both sides as
//     "<path>.pvm-conflict-host" or "<path>.pvm-conflict-vm", numbered when
//     that name is taken. When one side deleted the path the existing
//     version wins without a copy, since nothing is lost.
//
// A conflict at a directory decides its whole subtree.
func NewPlan(base *workspace.Baseline, host, guest workspace.Manifest, dir Direction, res Resolution) *Plan {
	a := analyze(base, host, guest)
	pl := &Plan{Conflicts: a.conflicts()}
	if len(pl.Conflicts) > 0 && res == None {
		pl.Unresolved = true
		return pl
	}

	curH, curG := clone(host), clone(guest)
	finH, finG := workspace.Manifest{}, workspace.Manifest{}
	taken := map[string]bool{}
	for _, p := range a.paths {
		taken[p] = true
	}
	hostWins := map[string]bool{}
	for _, p := range a.roots {
		n := a.nodes[p]
		h, keep := pick(n, dir, res)
		hostWins[p] = h
		if !keep {
			continue
		}
		if h {
			name := conflictName(p, "vm", taken)
			pl.RenameOnGuest = append(pl.RenameOnGuest, Rename{p, name})
			keepCopy(guest, curG, finH, finG, p, name, taken)
		} else {
			name := conflictName(p, "host", taken)
			pl.RenameOnHost = append(pl.RenameOnHost, Rename{p, name})
			keepCopy(host, curH, finH, finG, p, name, taken)
		}
	}
	for _, p := range a.paths {
		n := a.nodes[p]
		h, g := n.h, n.g
		if r, ok := a.top[p]; ok {
			if hostWins[r] {
				g = n.h
			} else {
				h = n.g
			}
		} else if !same(n.h, n.g) {
			if n.hc && dir == Push {
				g = n.h
			}
			if n.gc && dir == Pull {
				h = n.g
			}
		}
		set(finH, p, h)
		set(finG, p, g)
	}

	pl.DeleteOnHost, pl.ToHost = ops(curH, finH)
	pl.DeleteOnGuest, pl.ToGuest = ops(curG, finG)
	pl.Synced = map[string]bool{}
	for p := range taken {
		if same(lookup(finH, p), lookup(finG, p)) {
			pl.Synced[p] = true
		} else {
			pl.Pending = append(pl.Pending, p)
		}
	}
	sort.Strings(pl.Pending)
	return pl
}

// pick returns whether the host wins a conflict, and whether the loser's
// version is kept as a conflict copy.
func pick(n *node, dir Direction, res Resolution) (hostWins, keep bool) {
	switch res {
	case PreferHost:
		return true, false
	case PreferVM:
		return false, false
	}
	switch {
	case n.h == nil:
		return false, false
	case n.g == nil:
		return true, false
	}
	return dir == Push, true
}

// keepCopy moves the losing side's p, with its subtree, to name in cur,
// and makes that copy the final state on both sides.
func keepCopy(side, cur, finH, finG workspace.Manifest, p, name string, taken map[string]bool) {
	for q, e := range side {
		if q != p && !strings.HasPrefix(q, p+"/") {
			continue
		}
		to := name + q[len(p):]
		taken[to] = true
		delete(cur, q)
		set(cur, to, &e)
		set(finH, to, &e)
		set(finG, to, &e)
	}
}

func conflictName(p, side string, taken map[string]bool) string {
	base := p + ".pvm-conflict-" + side
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	taken[name] = true
	return name
}

func clone(m workspace.Manifest) workspace.Manifest {
	out := make(workspace.Manifest, len(m))
	for p, e := range m {
		out[p] = e
	}
	return out
}

func set(m workspace.Manifest, p string, e *workspace.Entry) {
	if e == nil {
		delete(m, p)
		return
	}
	c := *e
	c.Path = p
	m[p] = c
}

func move(m workspace.Manifest, from, to string) {
	set(m, to, lookup(m, from))
	delete(m, from)
}

func ops(cur, fin workspace.Manifest) (del []string, to []workspace.Entry) {
	for p, c := range cur {
		if f, ok := fin[p]; !ok || f.Type != c.Type {
			del = append(del, p)
		}
	}
	for p, f := range fin {
		if c, ok := cur[p]; !ok || !c.SameContent(f) {
			to = append(to, f)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(del)))
	sort.Slice(to, func(i, j int) bool { return to[i].Path < to[j].Path })
	return del, to
}

// NextBaseline records the state after a plan was applied; host and guest
// are the scans taken afterwards. A synced path takes its new content when
// both scans agree, and every other path keeps its old baseline entry.
func NextBaseline(old *workspace.Baseline, host, guest workspace.Manifest, synced map[string]bool) *workspace.Baseline {
	prev := content(old)
	c := workspace.Manifest{}
	for p, e := range prev {
		c[p] = e
	}
	for p, ok := range synced {
		if !ok {
			continue
		}
		h, g := lookup(host, p), lookup(guest, p)
		switch {
		case h == nil && g == nil:
			delete(c, p)
		case same(h, g):
			c[p] = *h
		}
	}
	return &workspace.Baseline{Content: c, Host: host, Guest: guest}
}
