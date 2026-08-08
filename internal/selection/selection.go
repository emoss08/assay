package selection

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/emoss08/assay/internal/graph"
	"github.com/emoss08/assay/internal/vcs"
)

type Reason string

const (
	ReasonDirect    Reason = "direct"
	ReasonDependent Reason = "dependent"
	ReasonFallback  Reason = "fallback"
)

type Options struct {
	Graph   *graph.Graph
	Changes []vcs.Change
}

type Result struct {
	Packages        []string
	Reasons         map[string]Reason
	ChangedPackages []string
	Unattributed    []string
	SelectAll       bool
	SelectAllReason string
}

var globalTriggerFiles = map[string]struct{}{
	"go.mod":      {},
	"go.sum":      {},
	"go.work":     {},
	"go.work.sum": {},
}

func Select(opts Options) Result {
	g := opts.Graph

	changedPkgs := make(map[string]struct{})
	var unattributed []string

	for _, change := range opts.Changes {
		base := filepath.Base(change.Path)
		// A go.mod under a testdata directory is a fixture, not a module
		// boundary — the build ignores testdata entirely — so it attributes to
		// its owning package like any other fixture file instead of forcing a
		// whole-workspace run.
		if _, isTrigger := globalTriggerFiles[base]; isTrigger && !underTestdata(change.Path) {
			return All(g, "module definition changed: "+base)
		}

		pkg, ok := g.PackageForFile(change.Path)
		if ok {
			changedPkgs[pkg.ImportPath] = struct{}{}

			continue
		}

		if filepath.Ext(change.Path) == ".go" {
			return All(g, "changed Go file outside every known package: "+change.Path)
		}

		unattributed = append(unattributed, change.Path)
	}

	seeds := make([]string, 0, len(changedPkgs))
	for path := range changedPkgs {
		seeds = append(seeds, path)
	}
	sort.Strings(seeds)

	packages := g.AffectedTestPackages(seeds)

	reasons := make(map[string]Reason, len(packages))
	for _, path := range packages {
		if _, direct := changedPkgs[path]; direct {
			reasons[path] = ReasonDirect
		} else {
			reasons[path] = ReasonDependent
		}
	}

	sort.Strings(unattributed)

	return Result{
		Packages:        packages,
		Reasons:         reasons,
		ChangedPackages: seeds,
		Unattributed:    unattributed,
	}
}

// underTestdata reports whether any element of the path is a testdata
// directory, which the Go build system never looks inside.
func underTestdata(path string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == "testdata" {
			return true
		}
	}

	return false
}

func All(g *graph.Graph, reason string) Result {
	packages := g.TestablePackages()
	reasons := make(map[string]Reason, len(packages))
	for _, path := range packages {
		reasons[path] = ReasonFallback
	}

	return Result{
		Packages:        packages,
		Reasons:         reasons,
		SelectAll:       true,
		SelectAllReason: reason,
	}
}
