package threeway

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/workspace"
)

func file(p, sum string) workspace.Entry {
	return workspace.Entry{Path: p, Type: workspace.TypeFile, SHA256: sum, Size: int64(len(sum))}
}
func dir(p string) workspace.Entry { return workspace.Entry{Path: p, Type: workspace.TypeDir} }
func link(p, to string) workspace.Entry {
	return workspace.Entry{Path: p, Type: workspace.TypeSymlink, Link: to}
}

func man(es ...workspace.Entry) workspace.Manifest {
	m := workspace.Manifest{}
	for _, e := range es {
		m[e.Path] = e
	}
	return m
}

func baseOf(es ...workspace.Entry) *workspace.Baseline {
	return &workspace.Baseline{Content: man(es...)}
}

func paths(es []workspace.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Path)
	}
	return out
}

func sums(es []workspace.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Path+"="+string(e.Type)+":"+e.SHA256+e.Link)
	}
	return out
}

func conflictPaths(cs []Conflict) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

type want struct {
	toGuest, delGuest, toHost, delHost []string // "path=type:content" for copies
	renGuest, renHost                  []Rename
	conflicts                          []string
	unresolved                         bool
	pending                            []string
}

func check(t *testing.T, pl *Plan, w want) {
	t.Helper()
	eq := func(name string, got, want any) {
		t.Helper()
		g, wv := reflect.ValueOf(got), reflect.ValueOf(want)
		if g.Len() == 0 && wv.Len() == 0 {
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	eq("ToGuest", sums(pl.ToGuest), w.toGuest)
	eq("DeleteOnGuest", pl.DeleteOnGuest, w.delGuest)
	eq("ToHost", sums(pl.ToHost), w.toHost)
	eq("DeleteOnHost", pl.DeleteOnHost, w.delHost)
	eq("RenameOnGuest", pl.RenameOnGuest, w.renGuest)
	eq("RenameOnHost", pl.RenameOnHost, w.renHost)
	eq("Conflicts", conflictPaths(pl.Conflicts), w.conflicts)
	eq("Pending", pl.Pending, w.pending)
	if pl.Unresolved != w.unresolved {
		t.Errorf("Unresolved = %v, want %v", pl.Unresolved, w.unresolved)
	}
}

func TestNewPlan(t *testing.T) {
	type tc struct {
		name        string
		base        *workspace.Baseline
		host, guest workspace.Manifest
		dir         Direction
		res         Resolution
		want        want
	}
	tcs := []tc{
		{
			name:  "identical without baseline",
			host:  man(dir("d"), file("d/a", "1")),
			guest: man(dir("d"), file("d/a", "1")),
		},
		{
			name:  "push host changes",
			base:  baseOf(file("a", "1"), file("b", "1"), dir("d")),
			host:  man(file("a", "2"), dir("d"), dir("d/new"), file("d/new/c", "3"), dir("empty")),
			guest: man(file("a", "1"), file("b", "1"), dir("d")),
			dir:   Push,
			want: want{
				toGuest:  []string{"a=file:2", "d/new=dir:", "d/new/c=file:3", "empty=dir:"},
				delGuest: []string{"b"},
			},
		},
		{
			name:  "pull leaves host changes pending",
			base:  baseOf(file("a", "1"), file("b", "1")),
			host:  man(file("a", "2")),
			guest: man(file("a", "1"), file("b", "1"), file("c", "3")),
			dir:   Pull,
			want: want{
				toHost:  []string{"c=file:3"},
				pending: []string{"a", "b"},
			},
		},
		{
			name:  "pull guest changes, push leaves them pending",
			base:  baseOf(file("a", "1"), link("l", "x")),
			host:  man(file("a", "1"), link("l", "x")),
			guest: man(file("a", "2"), link("l", "y")),
			dir:   Push,
			want:  want{pending: []string{"a", "l"}},
		},
		{
			name:  "pull guest changes",
			base:  baseOf(file("a", "1"), link("l", "x"), file("gone", "1")),
			host:  man(file("a", "1"), link("l", "x"), file("gone", "1")),
			guest: man(file("a", "2"), link("l", "y")),
			dir:   Pull,
			want: want{
				toHost:  []string{"a=file:2", "l=symlink:y"},
				delHost: []string{"gone"},
			},
		},
		{
			name:  "same change on both sides is no conflict",
			base:  baseOf(file("a", "1"), file("b", "1")),
			host:  man(file("a", "2"), file("c", "3")),
			guest: man(file("a", "2"), file("c", "3")),
			dir:   Push,
		},
		{
			name:  "exec bit change is a modification",
			base:  baseOf(file("a", "1")),
			host:  man(workspace.Entry{Path: "a", Type: workspace.TypeFile, SHA256: "1", Exec: true}),
			guest: man(file("a", "1")),
			dir:   Push,
			want:  want{toGuest: []string{"a=file:1"}},
		},
		{
			name:  "conflict stops the plan",
			base:  baseOf(file("a", "1"), file("b", "1")),
			host:  man(file("a", "2"), file("b", "2")),
			guest: man(file("a", "3"), file("b", "1")),
			dir:   Push,
			want:  want{conflicts: []string{"a"}, unresolved: true},
		},
		{
			name:  "prefer host on pull",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2")),
			guest: man(file("a", "3")),
			dir:   Pull, res: PreferHost,
			want: want{toGuest: []string{"a=file:2"}, conflicts: []string{"a"}},
		},
		{
			name:  "prefer vm on push",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2")),
			guest: man(file("a", "3")),
			dir:   Push, res: PreferVM,
			want: want{toHost: []string{"a=file:3"}, conflicts: []string{"a"}},
		},
		{
			name:  "delete vs modify, prefer host deletes",
			base:  baseOf(file("a", "1")),
			host:  man(),
			guest: man(file("a", "3")),
			dir:   Pull, res: PreferHost,
			want: want{delGuest: []string{"a"}, conflicts: []string{"a"}},
		},
		{
			name:  "delete vs modify, prefer vm restores",
			base:  baseOf(file("a", "1")),
			host:  man(),
			guest: man(file("a", "3")),
			dir:   Push, res: PreferVM,
			want: want{toHost: []string{"a=file:3"}, conflicts: []string{"a"}},
		},
		{
			name:  "both added differently",
			host:  man(file("a", "2")),
			guest: man(link("a", "x")),
			dir:   Push,
			want:  want{conflicts: []string{"a"}, unresolved: true},
		},
		{
			name:  "keep both on push",
			base:  baseOf(dir("d"), file("d/a", "1")),
			host:  man(dir("d"), file("d/a", "2")),
			guest: man(dir("d"), file("d/a", "3")),
			dir:   Push, res: KeepBoth,
			want: want{
				renGuest:  []Rename{{"d/a", "d/a.pvm-conflict-vm"}},
				toGuest:   []string{"d/a=file:2"},
				toHost:    []string{"d/a.pvm-conflict-vm=file:3"},
				conflicts: []string{"d/a"},
			},
		},
		{
			name:  "keep both on pull",
			base:  baseOf(link("l", "x")),
			host:  man(link("l", "h")),
			guest: man(file("l", "3")),
			dir:   Pull, res: KeepBoth,
			want: want{
				renHost:   []Rename{{"l", "l.pvm-conflict-host"}},
				toHost:    []string{"l=file:3"},
				toGuest:   []string{"l.pvm-conflict-host=symlink:h"},
				conflicts: []string{"l"},
			},
		},
		{
			name:  "keep both avoids taken names",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2"), file("a.pvm-conflict-vm", "x")),
			guest: man(file("a", "3"), file("a.pvm-conflict-vm-2", "y")),
			dir:   Push, res: KeepBoth,
			want: want{
				renGuest:  []Rename{{"a", "a.pvm-conflict-vm-3"}},
				toGuest:   []string{"a=file:2", "a.pvm-conflict-vm=file:x"},
				toHost:    []string{"a.pvm-conflict-vm-3=file:3"},
				conflicts: []string{"a"},
				pending:   []string{"a.pvm-conflict-vm-2"},
			},
		},
		{
			name:  "keep both with a deletion keeps the existing version",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2")),
			guest: man(),
			dir:   Pull, res: KeepBoth,
			want: want{toGuest: []string{"a=file:2"}, conflicts: []string{"a"}},
		},
		{
			name:  "directory deleted on host is deleted deepest first",
			base:  baseOf(dir("d"), dir("d/x"), file("d/x/y", "1"), file("d/z", "1"), file("keep", "1")),
			host:  man(file("keep", "1")),
			guest: man(dir("d"), dir("d/x"), file("d/x/y", "1"), file("d/z", "1"), file("keep", "1")),
			dir:   Push,
			want:  want{delGuest: []string{"d/z", "d/x/y", "d/x", "d"}},
		},
		{
			name:  "directory deleted on host while vm adds inside",
			base:  baseOf(dir("d"), file("d/c", "1")),
			host:  man(),
			guest: man(dir("d"), file("d/c", "1"), file("d/new", "2")),
			dir:   Push,
			want:  want{conflicts: []string{"d"}, unresolved: true},
		},
		{
			name:  "directory deleted on host while vm adds inside, prefer host",
			base:  baseOf(dir("d"), file("d/c", "1")),
			host:  man(),
			guest: man(dir("d"), file("d/c", "1"), file("d/new", "2")),
			dir:   Pull, res: PreferHost,
			want: want{delGuest: []string{"d/new", "d/c", "d"}, conflicts: []string{"d"}},
		},
		{
			name:  "directory deleted on host while vm adds inside, keep both restores",
			base:  baseOf(dir("d"), file("d/c", "1")),
			host:  man(),
			guest: man(dir("d"), file("d/c", "1"), file("d/new", "2")),
			dir:   Push, res: KeepBoth,
			want: want{toHost: []string{"d=dir:", "d/c=file:1", "d/new=file:2"}, conflicts: []string{"d"}},
		},
		{
			name:  "directory deleted on vm while host modifies inside",
			base:  baseOf(dir("d"), file("d/c", "1")),
			host:  man(dir("d"), file("d/c", "2")),
			guest: man(),
			dir:   Pull,
			want:  want{conflicts: []string{"d"}, unresolved: true},
		},
		{
			name:  "deletes on both sides of a directory agree",
			base:  baseOf(dir("d"), file("d/c", "1"), file("d/e", "1")),
			host:  man(),
			guest: man(dir("d"), file("d/e", "1")),
			dir:   Push,
			want:  want{delGuest: []string{"d/e", "d"}},
		},
		{
			name:  "file becomes directory on host",
			base:  baseOf(file("a", "1")),
			host:  man(dir("a"), file("a/x", "2")),
			guest: man(file("a", "1")),
			dir:   Push,
			want:  want{delGuest: []string{"a"}, toGuest: []string{"a=dir:", "a/x=file:2"}},
		},
		{
			name:  "directory becomes symlink on vm",
			base:  baseOf(dir("a"), file("a/x", "1")),
			host:  man(dir("a"), file("a/x", "1")),
			guest: man(link("a", "elsewhere")),
			dir:   Pull,
			want:  want{delHost: []string{"a/x", "a"}, toHost: []string{"a=symlink:elsewhere"}},
		},
		{
			name:  "type conflict decides the subtree",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2")),
			guest: man(dir("a"), dir("a/s"), file("a/s/x", "3")),
			dir:   Push, res: KeepBoth,
			want: want{
				renGuest:  []Rename{{"a", "a.pvm-conflict-vm"}},
				toGuest:   []string{"a=file:2"},
				toHost:    []string{"a.pvm-conflict-vm=dir:", "a.pvm-conflict-vm/s=dir:", "a.pvm-conflict-vm/s/x=file:3"},
				conflicts: []string{"a"},
			},
		},
		{
			name:  "keep both keeps the host subtree a vm file replaced",
			base:  baseOf(dir("d"), file("d/a", "1")),
			host:  man(dir("d"), file("d/a", "2"), file("d/notes.txt", "n")),
			guest: man(file("d", "f")),
			dir:   Pull, res: KeepBoth,
			want: want{
				renHost:   []Rename{{"d", "d.pvm-conflict-host"}},
				toHost:    []string{"d=file:f"},
				toGuest:   []string{"d.pvm-conflict-host=dir:", "d.pvm-conflict-host/a=file:2", "d.pvm-conflict-host/notes.txt=file:n"},
				conflicts: []string{"d"},
			},
		},
		{
			name:  "type conflict, prefer vm",
			base:  baseOf(file("a", "1")),
			host:  man(file("a", "2")),
			guest: man(dir("a"), file("a/x", "3")),
			dir:   Push, res: PreferVM,
			want: want{
				delHost:   []string{"a"},
				toHost:    []string{"a=dir:", "a/x=file:3"},
				conflicts: []string{"a"},
			},
		},
	}
	for _, c := range tcs {
		t.Run(c.name, func(t *testing.T) {
			pl := NewPlan(c.base, c.host, c.guest, c.dir, c.res)
			check(t, pl, c.want)
			if pl.Unresolved {
				if pl.Synced != nil {
					t.Error("unresolved plan has Synced")
				}
				return
			}
			// Applying the plan must leave both sides equal at every
			// synced path, and every path either synced or pending.
			h, g := apply(c.host, pl.RenameOnHost, pl.DeleteOnHost, pl.ToHost), apply(c.guest, pl.RenameOnGuest, pl.DeleteOnGuest, pl.ToGuest)
			all := union(h, g, content(c.base))
			for _, p := range all {
				if pl.Synced[p] == slices.Contains(pl.Pending, p) {
					t.Errorf("%s: synced=%v pending=%v", p, pl.Synced[p], slices.Contains(pl.Pending, p))
				}
				if pl.Synced[p] && !same(lookup(h, p), lookup(g, p)) {
					t.Errorf("%s synced but sides differ: %v vs %v", p, lookup(h, p), lookup(g, p))
				}
			}
			for _, m := range []workspace.Manifest{h, g} {
				for p := range m {
					if d := parent(p); d != "" && !isDir(lookup(m, d)) {
						t.Errorf("%s has no parent directory after apply", p)
					}
				}
			}
		})
	}
}

// apply simulates the plan on one side; copies arrive with their source's
// content.
func apply(m workspace.Manifest, ren []Rename, del []string, to []workspace.Entry) workspace.Manifest {
	out := clone(m)
	for _, r := range ren {
		// A rename moves the whole subtree, like rename(2).
		for p := range m {
			if p == r.From || strings.HasPrefix(p, r.From+"/") {
				move(out, p, r.To+p[len(r.From):])
			}
		}
	}
	if !sort.IsSorted(sort.Reverse(sort.StringSlice(del))) {
		panic("deletes not deepest first")
	}
	for _, p := range del {
		delete(out, p)
	}
	for i, e := range to {
		if i > 0 && to[i-1].Path >= e.Path {
			panic("copies not parents first")
		}
		out[e.Path] = e
	}
	return out
}

func TestDiffAndStatus(t *testing.T) {
	host := man(file("a", "1"), file("n", "1"))
	if got := Diff(nil, host); !reflect.DeepEqual(got, []Change{{"a", Added}, {"n", Added}}) {
		t.Errorf("Diff(nil) = %v", got)
	}
	base := baseOf(file("a", "1"), file("b", "1"), file("c", "1"))
	host = man(file("a", "2"), file("c", "1"), file("n", "1"))
	guest := man(file("a", "3"), file("b", "1"))
	s := Status(base, host, guest)
	if want := []Change{{"a", Modified}, {"b", Deleted}, {"n", Added}}; !reflect.DeepEqual(s.HostChanged, want) {
		t.Errorf("HostChanged = %v", s.HostChanged)
	}
	if want := []Change{{"a", Modified}, {"c", Deleted}}; !reflect.DeepEqual(s.GuestChanged, want) {
		t.Errorf("GuestChanged = %v", s.GuestChanged)
	}
	if len(s.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v", s.Conflicts)
	}
	c := s.Conflicts[0]
	if c.Path != "a" || c.Host.SHA256 != "2" || c.Guest.SHA256 != "3" || c.Base.SHA256 != "1" {
		t.Errorf("conflict = %+v", c)
	}
}

func TestNextBaseline(t *testing.T) {
	old := baseOf(file("a", "1"), file("b", "1"), file("gone", "1"), file("pend", "1"))
	host := man(file("a", "2"), file("b", "1"), file("new", "5"), file("pend", "9"))
	guest := man(file("a", "2"), file("b", "1"), file("new", "5"), file("pend", "1"))
	pl := NewPlan(old, host, guest, Pull, None)
	if !slices.Equal(pl.Pending, []string{"pend"}) {
		t.Fatalf("Pending = %v", pl.Pending)
	}
	nb := NextBaseline(old, host, guest, pl.Synced)
	want := man(file("a", "2"), file("b", "1"), file("new", "5"), file("pend", "1"))
	if !reflect.DeepEqual(nb.Content, want) {
		t.Errorf("Content = %v", nb.Content)
	}
	if !reflect.DeepEqual(nb.Host, host) || !reflect.DeepEqual(nb.Guest, guest) {
		t.Error("stat caches not recorded")
	}

	// A synced path whose sides disagree after all keeps its old entry.
	nb = NextBaseline(old, man(file("a", "7")), man(file("a", "8")), map[string]bool{"a": true, "zz": true})
	if !reflect.DeepEqual(nb.Content["a"], file("a", "1")) {
		t.Errorf("a = %v", nb.Content["a"])
	}
	if _, ok := nb.Content["zz"]; ok {
		t.Error("zz recorded")
	}
	if nb := NextBaseline(nil, host, guest, map[string]bool{"b": true}); !reflect.DeepEqual(nb.Content, man(file("b", "1"))) {
		t.Errorf("nil old: %v", nb.Content)
	}
}
