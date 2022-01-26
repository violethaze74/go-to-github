// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modload

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"cmd/go/internal/base"
	"cmd/go/internal/cfg"
	"cmd/go/internal/lockedfile"
	"cmd/go/internal/modfetch"
	"cmd/go/internal/par"
	"cmd/go/internal/trace"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// lazyLoadingVersion is the Go version (plus leading "v") at which lazy module
// loading takes effect.
const lazyLoadingVersionV = "v1.16"
const go116EnableLazyLoading = true

var modFile *modfile.File

// A modFileIndex is an index of data corresponding to a modFile
// at a specific point in time.
type modFileIndex struct {
	data         []byte
	dataNeedsFix bool // true if fixVersion applied a change while parsing data
	module       module.Version
	goVersionV   string // GoVersion with "v" prefix
	require      map[module.Version]requireMeta
	replace      map[module.Version]module.Version
	exclude      map[module.Version]bool
}

// index is the index of the go.mod file as of when it was last read or written.
var index *modFileIndex

type requireMeta struct {
	indirect bool
}

// CheckAllowed returns an error equivalent to ErrDisallowed if m is excluded by
// the main module's go.mod or retracted by its author. Most version queries use
// this to filter out versions that should not be used.
func CheckAllowed(ctx context.Context, m module.Version) error {
	if err := CheckExclusions(ctx, m); err != nil {
		return err
	}
	if err := checkRetractions(ctx, m); err != nil {
		return err
	}
	return nil
}

// ErrDisallowed is returned by version predicates passed to Query and similar
// functions to indicate that a version should not be considered.
var ErrDisallowed = errors.New("disallowed module version")

// CheckExclusions returns an error equivalent to ErrDisallowed if module m is
// excluded by the main module's go.mod file.
func CheckExclusions(ctx context.Context, m module.Version) error {
	if index != nil && index.exclude[m] {
		return module.VersionError(m, errExcluded)
	}
	return nil
}

var errExcluded = &excludedError{}

type excludedError struct{}

func (e *excludedError) Error() string     { return "excluded by go.mod" }
func (e *excludedError) Is(err error) bool { return err == ErrDisallowed }

// checkRetractions returns an error if module m has been retracted by
// its author.
func checkRetractions(ctx context.Context, m module.Version) error {
	if m.Version == "" {
		// Main module, standard library, or file replacement module.
		// Cannot be retracted.
		return nil
	}

	// Look up retraction information from the latest available version of
	// the module. Cache retraction information so we don't parse the go.mod
	// file repeatedly.
	type entry struct {
		retract []retraction
		err     error
	}
	path := m.Path
	e := retractCache.Do(path, func() (v interface{}) {
		ctx, span := trace.StartSpan(ctx, "checkRetractions "+path)
		defer span.Done()

		if repl := Replacement(module.Version{Path: m.Path}); repl.Path != "" {
			// All versions of the module were replaced with a local directory.
			// Don't load retractions.
			return &entry{nil, nil}
		}

		// Find the latest version of the module.
		// Ignore exclusions from the main module's go.mod.
		// We may need to account for the current version: for example,
		// v2.0.0+incompatible is not "latest" if v1.0.0 is current.
		rev, err := Query(ctx, path, "latest", findCurrentVersion(path), nil)
		if err != nil {
			return &entry{err: err}
		}

		// Load go.mod for that version.
		// If the version is replaced, we'll load retractions from the replacement.
		// If there's an error loading the go.mod, we'll return it here.
		// These errors should generally be ignored by callers of checkRetractions,
		// since they happen frequently when we're offline. These errors are not
		// equivalent to ErrDisallowed, so they may be distinguished from
		// retraction errors.
		summary, err := goModSummary(module.Version{Path: path, Version: rev.Version})
		if err != nil {
			return &entry{err: err}
		}
		return &entry{retract: summary.retract}
	}).(*entry)

	if e.err != nil {
		return fmt.Errorf("loading module retractions: %v", e.err)
	}

	var rationale []string
	isRetracted := false
	for _, r := range e.retract {
		if semver.Compare(r.Low, m.Version) <= 0 && semver.Compare(m.Version, r.High) <= 0 {
			isRetracted = true
			if r.Rationale != "" {
				rationale = append(rationale, r.Rationale)
			}
		}
	}
	if isRetracted {
		return &retractedError{rationale: rationale}
	}
	return nil
}

var retractCache par.Cache

type retractedError struct {
	rationale []string
}

func (e *retractedError) Error() string {
	msg := "retracted by module author"
	if len(e.rationale) > 0 {
		// This is meant to be a short error printed on a terminal, so just
		// print the first rationale.
		msg += ": " + ShortRetractionRationale(e.rationale[0])
	}
	return msg
}

func (e *retractedError) Is(err error) bool {
	return err == ErrDisallowed
}

// ShortRetractionRationale returns a retraction rationale string that is safe
// to print in a terminal. It returns hard-coded strings if the rationale
// is empty, too long, or contains non-printable characters.
func ShortRetractionRationale(rationale string) string {
	const maxRationaleBytes = 500
	if i := strings.Index(rationale, "\n"); i >= 0 {
		rationale = rationale[:i]
	}
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return "retracted by module author"
	}
	if len(rationale) > maxRationaleBytes {
		return "(rationale omitted: too long)"
	}
	for _, r := range rationale {
		if !unicode.IsGraphic(r) && !unicode.IsSpace(r) {
			return "(rationale omitted: contains non-printable characters)"
		}
	}
	// NOTE: the go.mod parser rejects invalid UTF-8, so we don't check that here.
	return rationale
}

// Replacement returns the replacement for mod, if any, from go.mod.
// If there is no replacement for mod, Replacement returns
// a module.Version with Path == "".
func Replacement(mod module.Version) module.Version {
	if index != nil {
		if r, ok := index.replace[mod]; ok {
			return r
		}
		if r, ok := index.replace[module.Version{Path: mod.Path}]; ok {
			return r
		}
	}
	return module.Version{}
}

// indexModFile rebuilds the index of modFile.
// If modFile has been changed since it was first read,
// modFile.Cleanup must be called before indexModFile.
func indexModFile(data []byte, modFile *modfile.File, needsFix bool) *modFileIndex {
	i := new(modFileIndex)
	i.data = data
	i.dataNeedsFix = needsFix

	i.module = module.Version{}
	if modFile.Module != nil {
		i.module = modFile.Module.Mod
	}

	i.goVersionV = ""
	if modFile.Go != nil {
		// We're going to use the semver package to compare Go versions, so go ahead
		// and add the "v" prefix it expects once instead of every time.
		i.goVersionV = "v" + modFile.Go.Version
	}

	i.require = make(map[module.Version]requireMeta, len(modFile.Require))
	for _, r := range modFile.Require {
		i.require[r.Mod] = requireMeta{indirect: r.Indirect}
	}

	i.replace = make(map[module.Version]module.Version, len(modFile.Replace))
	for _, r := range modFile.Replace {
		if prev, dup := i.replace[r.Old]; dup && prev != r.New {
			base.Fatalf("go: conflicting replacements for %v:\n\t%v\n\t%v", r.Old, prev, r.New)
		}
		i.replace[r.Old] = r.New
	}

	i.exclude = make(map[module.Version]bool, len(modFile.Exclude))
	for _, x := range modFile.Exclude {
		i.exclude[x.Mod] = true
	}

	return i
}

// allPatternClosesOverTests reports whether the "all" pattern includes
// dependencies of tests outside the main module (as in Go 1.11–1.15).
// (Otherwise — as in Go 1.16+ — the "all" pattern includes only the packages
// transitively *imported by* the packages and tests in the main module.)
func (i *modFileIndex) allPatternClosesOverTests() bool {
	if !go116EnableLazyLoading {
		return true
	}
	if i != nil && semver.Compare(i.goVersionV, lazyLoadingVersionV) < 0 {
		// The module explicitly predates the change in "all" for lazy loading, so
		// continue to use the older interpretation. (If i == nil, we not in any
		// module at all and should use the latest semantics.)
		return true
	}
	return false
}

// modFileIsDirty reports whether the go.mod file differs meaningfully
// from what was indexed.
// If modFile has been changed (even cosmetically) since it was first read,
// modFile.Cleanup must be called before modFileIsDirty.
func (i *modFileIndex) modFileIsDirty(modFile *modfile.File) bool {
	if i == nil {
		return modFile != nil
	}

	if i.dataNeedsFix {
		return true
	}

	if modFile.Module == nil {
		if i.module != (module.Version{}) {
			return true
		}
	} else if modFile.Module.Mod != i.module {
		return true
	}

	if modFile.Go == nil {
		if i.goVersionV != "" {
			return true
		}
	} else if "v"+modFile.Go.Version != i.goVersionV {
		if i.goVersionV == "" && cfg.BuildMod == "readonly" {
			// go.mod files did not always require a 'go' version, so do not error out
			// if one is missing — we may be inside an older module in the module
			// cache, and should bias toward providing useful behavior.
		} else {
			return true
		}
	}

	if len(modFile.Require) != len(i.require) ||
		len(modFile.Replace) != len(i.replace) ||
		len(modFile.Exclude) != len(i.exclude) {
		return true
	}

	for _, r := range modFile.Require {
		if meta, ok := i.require[r.Mod]; !ok {
			return true
		} else if r.Indirect != meta.indirect {
			if cfg.BuildMod == "readonly" {
				// The module's requirements are consistent; only the "// indirect"
				// comments that are wrong. But those are only guaranteed to be accurate
				// after a "go mod tidy" — it's a good idea to run those before
				// committing a change, but it's certainly not mandatory.
			} else {
				return true
			}
		}
	}

	for _, r := range modFile.Replace {
		if r.New != i.replace[r.Old] {
			return true
		}
	}

	for _, x := range modFile.Exclude {
		if !i.exclude[x.Mod] {
			return true
		}
	}

	return false
}

// rawGoVersion records the Go version parsed from each module's go.mod file.
//
// If a module is replaced, the version of the replacement is keyed by the
// replacement module.Version, not the version being replaced.
var rawGoVersion sync.Map // map[module.Version]string

// A modFileSummary is a summary of a go.mod file for which we do not need to
// retain complete information — for example, the go.mod file of a dependency
// module.
type modFileSummary struct {
	module     module.Version
	goVersionV string // GoVersion with "v" prefix
	require    []module.Version
	retract    []retraction
}

// A retraction consists of a retracted version interval and rationale.
// retraction is like modfile.Retract, but it doesn't point to the syntax tree.
type retraction struct {
	modfile.VersionInterval
	Rationale string
}

// goModSummary returns a summary of the go.mod file for module m,
// taking into account any replacements for m, exclusions of its dependencies,
// and/or vendoring.
//
// goModSummary cannot be used on the Target module, as its requirements
// may change.
//
// The caller must not modify the returned summary.
func goModSummary(m module.Version) (*modFileSummary, error) {
	if m == Target {
		panic("internal error: goModSummary called on the Target module")
	}

	type cached struct {
		summary *modFileSummary
		err     error
	}
	c := goModSummaryCache.Do(m, func() interface{} {
		if cfg.BuildMod == "vendor" {
			summary := &modFileSummary{
				module: module.Version{Path: m.Path},
			}
			if vendorVersion[m.Path] != m.Version {
				// This module is not vendored, so packages cannot be loaded from it and
				// it cannot be relevant to the build.
				return cached{summary, nil}
			}

			// For every module other than the target,
			// return the full list of modules from modules.txt.
			readVendorList()

			// TODO(#36876): Load the "go" version from vendor/modules.txt and store it
			// in rawGoVersion with the appropriate key.

			// We don't know what versions the vendored module actually relies on,
			// so assume that it requires everything.
			summary.require = vendorList
			return cached{summary, nil}
		}

		actual := Replacement(m)
		if actual.Path == "" {
			actual = m
		}
		summary, err := rawGoModSummary(actual)
		if err != nil {
			return cached{nil, err}
		}

		if actual.Version == "" {
			// The actual module is a filesystem-local replacement, for which we have
			// unfortunately not enforced any sort of invariants about module lines or
			// matching module paths. Anything goes.
			//
			// TODO(bcmills): Remove this special-case, update tests, and add a
			// release note.
		} else {
			if summary.module.Path == "" {
				return cached{nil, module.VersionError(actual, errors.New("parsing go.mod: missing module line"))}
			}

			// In theory we should only allow mpath to be unequal to m.Path here if the
			// version that we fetched lacks an explicit go.mod file: if the go.mod file
			// is explicit, then it should match exactly (to ensure that imports of other
			// packages within the module are interpreted correctly). Unfortunately, we
			// can't determine that information from the module proxy protocol: we'll have
			// to leave that validation for when we load actual packages from within the
			// module.
			if mpath := summary.module.Path; mpath != m.Path && mpath != actual.Path {
				return cached{nil, module.VersionError(actual, fmt.Errorf(`parsing go.mod:
	module declares its path as: %s
	        but was required as: %s`, mpath, m.Path))}
			}
		}

		if index != nil && len(index.exclude) > 0 {
			// Drop any requirements on excluded versions.
			nonExcluded := summary.require[:0]
			for _, r := range summary.require {
				if !index.exclude[r] {
					nonExcluded = append(nonExcluded, r)
				}
			}
			summary.require = nonExcluded
		}
		return cached{summary, nil}
	}).(cached)

	return c.summary, c.err
}

var goModSummaryCache par.Cache // module.Version → goModSummary result

// rawGoModSummary returns a new summary of the go.mod file for module m,
// ignoring all replacements that may apply to m and excludes that may apply to
// its dependencies.
//
// rawGoModSummary cannot be used on the Target module.
func rawGoModSummary(m module.Version) (*modFileSummary, error) {
	if m == Target {
		panic("internal error: rawGoModSummary called on the Target module")
	}

	summary := new(modFileSummary)
	var f *modfile.File
	if m.Version == "" {
		// m is a replacement module with only a file path.
		dir := m.Path
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(ModRoot(), dir)
		}
		gomod := filepath.Join(dir, "go.mod")

		data, err := lockedfile.Read(gomod)
		if err != nil {
			return nil, module.VersionError(m, fmt.Errorf("reading %s: %v", base.ShortPath(gomod), err))
		}
		f, err = modfile.ParseLax(gomod, data, nil)
		if err != nil {
			return nil, module.VersionError(m, fmt.Errorf("parsing %s: %v", base.ShortPath(gomod), err))
		}
	} else {
		if !semver.IsValid(m.Version) {
			// Disallow the broader queries supported by fetch.Lookup.
			base.Fatalf("go: internal error: %s@%s: unexpected invalid semantic version", m.Path, m.Version)
		}

		data, err := modfetch.GoMod(m.Path, m.Version)
		if err != nil {
			return nil, err
		}
		f, err = modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return nil, module.VersionError(m, fmt.Errorf("parsing go.mod: %v", err))
		}
	}

	if f.Module != nil {
		summary.module = f.Module.Mod
	}
	if f.Go != nil && f.Go.Version != "" {
		rawGoVersion.LoadOrStore(m, f.Go.Version)
		summary.goVersionV = "v" + f.Go.Version
	}
	if len(f.Require) > 0 {
		summary.require = make([]module.Version, 0, len(f.Require))
		for _, req := range f.Require {
			summary.require = append(summary.require, req.Mod)
		}
	}
	if len(f.Retract) > 0 {
		summary.retract = make([]retraction, 0, len(f.Retract))
		for _, ret := range f.Retract {
			summary.retract = append(summary.retract, retraction{
				VersionInterval: ret.VersionInterval,
				Rationale:       ret.Rationale,
			})
		}
	}

	return summary, nil
}
