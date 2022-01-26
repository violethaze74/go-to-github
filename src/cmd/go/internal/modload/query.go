// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modload

import (
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"

	"cmd/go/internal/cfg"
	"cmd/go/internal/imports"
	"cmd/go/internal/modfetch"
	"cmd/go/internal/search"
	"cmd/go/internal/str"
	"cmd/go/internal/trace"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Query looks up a revision of a given module given a version query string.
// The module must be a complete module path.
// The version must take one of the following forms:
//
// - the literal string "latest", denoting the latest available, allowed
//   tagged version, with non-prereleases preferred over prereleases.
//   If there are no tagged versions in the repo, latest returns the most
//   recent commit.
// - the literal string "upgrade", equivalent to "latest" except that if
//   current is a newer version, current will be returned (see below).
// - the literal string "patch", denoting the latest available tagged version
//   with the same major and minor number as current (see below).
// - v1, denoting the latest available tagged version v1.x.x.
// - v1.2, denoting the latest available tagged version v1.2.x.
// - v1.2.3, a semantic version string denoting that tagged version.
// - <v1.2.3, <=v1.2.3, >v1.2.3, >=v1.2.3,
//   denoting the version closest to the target and satisfying the given operator,
//   with non-prereleases preferred over prereleases.
// - a repository commit identifier or tag, denoting that commit.
//
// current denotes the current version of the module; it may be "" if the
// current version is unknown or should not be considered. If query is
// "upgrade" or "patch", current will be returned if it is a newer
// semantic version or a chronologically later pseudo-version than the
// version that would otherwise be chosen. This prevents accidental downgrades
// from newer pre-release or development versions.
//
// The allowed function (which may be nil) is used to filter out unsuitable
// versions (see AllowedFunc documentation for details). If the query refers to
// a specific revision (for example, "master"; see IsRevisionQuery), and the
// revision is disallowed by allowed, Query returns the error. If the query
// does not refer to a specific revision (for example, "latest"), Query
// acts as if versions disallowed by allowed do not exist.
//
// If path is the path of the main module and the query is "latest",
// Query returns Target.Version as the version.
func Query(ctx context.Context, path, query, current string, allowed AllowedFunc) (*modfetch.RevInfo, error) {
	var info *modfetch.RevInfo
	err := modfetch.TryProxies(func(proxy string) (err error) {
		info, err = queryProxy(ctx, proxy, path, query, current, allowed)
		return err
	})
	return info, err
}

// AllowedFunc is used by Query and other functions to filter out unsuitable
// versions, for example, those listed in exclude directives in the main
// module's go.mod file.
//
// An AllowedFunc returns an error equivalent to ErrDisallowed for an unsuitable
// version. Any other error indicates the function was unable to determine
// whether the version should be allowed, for example, the function was unable
// to fetch or parse a go.mod file containing retractions. Typically, errors
// other than ErrDisallowd may be ignored.
type AllowedFunc func(context.Context, module.Version) error

var errQueryDisabled error = queryDisabledError{}

type queryDisabledError struct{}

func (queryDisabledError) Error() string {
	if cfg.BuildModReason == "" {
		return fmt.Sprintf("cannot query module due to -mod=%s", cfg.BuildMod)
	}
	return fmt.Sprintf("cannot query module due to -mod=%s\n\t(%s)", cfg.BuildMod, cfg.BuildModReason)
}

func queryProxy(ctx context.Context, proxy, path, query, current string, allowed AllowedFunc) (*modfetch.RevInfo, error) {
	ctx, span := trace.StartSpan(ctx, "modload.queryProxy "+path+" "+query)
	defer span.Done()

	if current != "" && !semver.IsValid(current) {
		return nil, fmt.Errorf("invalid previous version %q", current)
	}
	if cfg.BuildMod == "vendor" {
		return nil, errQueryDisabled
	}
	if allowed == nil {
		allowed = func(context.Context, module.Version) error { return nil }
	}

	// Parse query to detect parse errors (and possibly handle query)
	// before any network I/O.
	badVersion := func(v string) (*modfetch.RevInfo, error) {
		return nil, fmt.Errorf("invalid semantic version %q in range %q", v, query)
	}
	matchesMajor := func(v string) bool {
		_, pathMajor, ok := module.SplitPathVersion(path)
		if !ok {
			return false
		}
		return module.CheckPathMajor(v, pathMajor) == nil
	}
	var (
		match = func(m module.Version) bool { return true }

		prefix             string
		preferOlder        bool
		mayUseLatest       bool
		preferIncompatible bool = strings.HasSuffix(current, "+incompatible")
	)
	switch {
	case query == "latest":
		mayUseLatest = true

	case query == "upgrade":
		mayUseLatest = true

	case query == "patch":
		if current == "" {
			mayUseLatest = true
		} else {
			prefix = semver.MajorMinor(current)
			match = func(m module.Version) bool {
				return matchSemverPrefix(prefix, m.Version)
			}
		}

	case strings.HasPrefix(query, "<="):
		v := query[len("<="):]
		if !semver.IsValid(v) {
			return badVersion(v)
		}
		if isSemverPrefix(v) {
			// Refuse to say whether <=v1.2 allows v1.2.3 (remember, @v1.2 might mean v1.2.3).
			return nil, fmt.Errorf("ambiguous semantic version %q in range %q", v, query)
		}
		match = func(m module.Version) bool {
			return semver.Compare(m.Version, v) <= 0
		}
		if !matchesMajor(v) {
			preferIncompatible = true
		}

	case strings.HasPrefix(query, "<"):
		v := query[len("<"):]
		if !semver.IsValid(v) {
			return badVersion(v)
		}
		match = func(m module.Version) bool {
			return semver.Compare(m.Version, v) < 0
		}
		if !matchesMajor(v) {
			preferIncompatible = true
		}

	case strings.HasPrefix(query, ">="):
		v := query[len(">="):]
		if !semver.IsValid(v) {
			return badVersion(v)
		}
		match = func(m module.Version) bool {
			return semver.Compare(m.Version, v) >= 0
		}
		preferOlder = true
		if !matchesMajor(v) {
			preferIncompatible = true
		}

	case strings.HasPrefix(query, ">"):
		v := query[len(">"):]
		if !semver.IsValid(v) {
			return badVersion(v)
		}
		if isSemverPrefix(v) {
			// Refuse to say whether >v1.2 allows v1.2.3 (remember, @v1.2 might mean v1.2.3).
			return nil, fmt.Errorf("ambiguous semantic version %q in range %q", v, query)
		}
		match = func(m module.Version) bool {
			return semver.Compare(m.Version, v) > 0
		}
		preferOlder = true
		if !matchesMajor(v) {
			preferIncompatible = true
		}

	case semver.IsValid(query) && isSemverPrefix(query):
		match = func(m module.Version) bool {
			return matchSemverPrefix(query, m.Version)
		}
		prefix = query + "."
		if !matchesMajor(query) {
			preferIncompatible = true
		}

	default:
		// Direct lookup of semantic version or commit identifier.
		//
		// If the identifier is not a canonical semver tag — including if it's a
		// semver tag with a +metadata suffix — then modfetch.Stat will populate
		// info.Version with a suitable pseudo-version.
		info, err := modfetch.Stat(proxy, path, query)
		if err != nil {
			queryErr := err
			// The full query doesn't correspond to a tag. If it is a semantic version
			// with a +metadata suffix, see if there is a tag without that suffix:
			// semantic versioning defines them to be equivalent.
			if vers := module.CanonicalVersion(query); vers != "" && vers != query {
				info, err = modfetch.Stat(proxy, path, vers)
				if !errors.Is(err, os.ErrNotExist) {
					return info, err
				}
			}
			if err != nil {
				return nil, queryErr
			}
		}
		if err := allowed(ctx, module.Version{Path: path, Version: info.Version}); errors.Is(err, ErrDisallowed) {
			return nil, err
		}
		return info, nil
	}

	if path == Target.Path {
		if query != "latest" {
			return nil, fmt.Errorf("can't query specific version (%q) for the main module (%s)", query, path)
		}
		if err := allowed(ctx, Target); err != nil {
			return nil, fmt.Errorf("internal error: main module version is not allowed: %w", err)
		}
		return &modfetch.RevInfo{Version: Target.Version}, nil
	}

	if str.HasPathPrefix(path, "std") || str.HasPathPrefix(path, "cmd") {
		return nil, fmt.Errorf("explicit requirement on standard-library module %s not allowed", path)
	}

	// Load versions and execute query.
	repo, err := modfetch.Lookup(proxy, path)
	if err != nil {
		return nil, err
	}
	versions, err := repo.Versions(prefix)
	if err != nil {
		return nil, err
	}
	matchAndAllowed := func(ctx context.Context, m module.Version) error {
		if !match(m) {
			return ErrDisallowed
		}
		return allowed(ctx, m)
	}
	releases, prereleases, err := filterVersions(ctx, path, versions, matchAndAllowed, preferIncompatible)
	if err != nil {
		return nil, err
	}

	lookup := func(v string) (*modfetch.RevInfo, error) {
		rev, err := repo.Stat(v)
		if err != nil {
			return nil, err
		}

		// For "upgrade" and "patch", make sure we don't accidentally downgrade
		// from a newer prerelease or from a chronologically newer pseudoversion.
		if current != "" && (query == "upgrade" || query == "patch") {
			currentTime, err := modfetch.PseudoVersionTime(current)
			if semver.Compare(rev.Version, current) < 0 || (err == nil && rev.Time.Before(currentTime)) {
				return repo.Stat(current)
			}
		}

		return rev, nil
	}

	if preferOlder {
		if len(releases) > 0 {
			return lookup(releases[0])
		}
		if len(prereleases) > 0 {
			return lookup(prereleases[0])
		}
	} else {
		if len(releases) > 0 {
			return lookup(releases[len(releases)-1])
		}
		if len(prereleases) > 0 {
			return lookup(prereleases[len(prereleases)-1])
		}
	}

	if mayUseLatest {
		// Special case for "latest": if no tags match, use latest commit in repo
		// if it is allowed.
		latest, err := repo.Latest()
		if err == nil {
			m := module.Version{Path: path, Version: latest.Version}
			if err := allowed(ctx, m); !errors.Is(err, ErrDisallowed) {
				return lookup(latest.Version)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	return nil, &NoMatchingVersionError{query: query, current: current}
}

// IsRevisionQuery returns true if vers is a version query that may refer to
// a particular version or revision in a repository like "v1.0.0", "master",
// or "0123abcd". IsRevisionQuery returns false if vers is a query that
// chooses from among available versions like "latest" or ">v1.0.0".
func IsRevisionQuery(vers string) bool {
	if vers == "latest" ||
		vers == "upgrade" ||
		vers == "patch" ||
		strings.HasPrefix(vers, "<") ||
		strings.HasPrefix(vers, ">") ||
		(semver.IsValid(vers) && isSemverPrefix(vers)) {
		return false
	}
	return true
}

// isSemverPrefix reports whether v is a semantic version prefix: v1 or v1.2 (not v1.2.3).
// The caller is assumed to have checked that semver.IsValid(v) is true.
func isSemverPrefix(v string) bool {
	dots := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '-', '+':
			return false
		case '.':
			dots++
			if dots >= 2 {
				return false
			}
		}
	}
	return true
}

// matchSemverPrefix reports whether the shortened semantic version p
// matches the full-width (non-shortened) semantic version v.
func matchSemverPrefix(p, v string) bool {
	return len(v) > len(p) && v[len(p)] == '.' && v[:len(p)] == p && semver.Prerelease(v) == ""
}

// filterVersions classifies versions into releases and pre-releases, filtering
// out:
// 	1. versions that do not satisfy the 'allowed' predicate, and
// 	2. "+incompatible" versions, if a compatible one satisfies the predicate
// 	   and the incompatible version is not preferred.
//
// If the allowed predicate returns an error not equivalent to ErrDisallowed,
// filterVersions returns that error.
func filterVersions(ctx context.Context, path string, versions []string, allowed AllowedFunc, preferIncompatible bool) (releases, prereleases []string, err error) {
	var lastCompatible string
	for _, v := range versions {
		if err := allowed(ctx, module.Version{Path: path, Version: v}); errors.Is(err, ErrDisallowed) {
			continue
		}

		if !preferIncompatible {
			if !strings.HasSuffix(v, "+incompatible") {
				lastCompatible = v
			} else if lastCompatible != "" {
				// If the latest compatible version is allowed and has a go.mod file,
				// ignore any version with a higher (+incompatible) major version. (See
				// https://golang.org/issue/34165.) Note that we even prefer a
				// compatible pre-release over an incompatible release.

				ok, err := versionHasGoMod(ctx, module.Version{Path: path, Version: lastCompatible})
				if err != nil {
					return nil, nil, err
				}
				if ok {
					break
				}

				// No acceptable compatible release has a go.mod file, so the versioning
				// for the module might not be module-aware, and we should respect
				// legacy major-version tags.
				preferIncompatible = true
			}
		}

		if semver.Prerelease(v) != "" {
			prereleases = append(prereleases, v)
		} else {
			releases = append(releases, v)
		}
	}

	return releases, prereleases, nil
}

type QueryResult struct {
	Mod      module.Version
	Rev      *modfetch.RevInfo
	Packages []string
}

// QueryPackage looks up the module(s) containing path at a revision matching
// query. The results are sorted by module path length in descending order.
//
// If the package is in the main module, QueryPackage considers only the main
// module and only the version "latest", without checking for other possible
// modules.
func QueryPackage(ctx context.Context, path, query string, allowed AllowedFunc) ([]QueryResult, error) {
	m := search.NewMatch(path)
	if m.IsLocal() || !m.IsLiteral() {
		return nil, fmt.Errorf("pattern %s is not an importable package", path)
	}
	return QueryPattern(ctx, path, query, allowed)
}

// QueryPattern looks up the module(s) containing at least one package matching
// the given pattern at the given version. The results are sorted by module path
// length in descending order.
//
// QueryPattern queries modules with package paths up to the first "..."
// in the pattern. For the pattern "example.com/a/b.../c", QueryPattern would
// consider prefixes of "example.com/a". If multiple modules have versions
// that match the query and packages that match the pattern, QueryPattern
// picks the one with the longest module path.
//
// If any matching package is in the main module, QueryPattern considers only
// the main module and only the version "latest", without checking for other
// possible modules.
func QueryPattern(ctx context.Context, pattern, query string, allowed AllowedFunc) ([]QueryResult, error) {
	ctx, span := trace.StartSpan(ctx, "modload.QueryPattern "+pattern+" "+query)
	defer span.Done()

	base := pattern

	firstError := func(m *search.Match) error {
		if len(m.Errs) == 0 {
			return nil
		}
		return m.Errs[0]
	}

	var match func(mod module.Version, root string, isLocal bool) *search.Match

	if i := strings.Index(pattern, "..."); i >= 0 {
		base = pathpkg.Dir(pattern[:i+3])
		match = func(mod module.Version, root string, isLocal bool) *search.Match {
			m := search.NewMatch(pattern)
			matchPackages(ctx, m, imports.AnyTags(), omitStd, []module.Version{mod})
			return m
		}
	} else {
		match = func(mod module.Version, root string, isLocal bool) *search.Match {
			m := search.NewMatch(pattern)
			prefix := mod.Path
			if mod == Target {
				prefix = targetPrefix
			}
			if _, ok, err := dirInModule(pattern, prefix, root, isLocal); err != nil {
				m.AddError(err)
			} else if ok {
				m.Pkgs = []string{pattern}
			}
			return m
		}
	}

	if HasModRoot() {
		m := match(Target, modRoot, true)
		if len(m.Pkgs) > 0 {
			if query != "latest" {
				return nil, fmt.Errorf("can't query specific version for package %s in the main module (%s)", pattern, Target.Path)
			}
			if err := allowed(ctx, Target); err != nil {
				return nil, fmt.Errorf("internal error: package %s is in the main module (%s), but version is not allowed: %w", pattern, Target.Path, err)
			}
			return []QueryResult{{
				Mod:      Target,
				Rev:      &modfetch.RevInfo{Version: Target.Version},
				Packages: m.Pkgs,
			}}, nil
		}
		if err := firstError(m); err != nil {
			return nil, err
		}
	}

	var (
		results          []QueryResult
		candidateModules = modulePrefixesExcludingTarget(base)
	)
	if len(candidateModules) == 0 {
		return nil, &PackageNotInModuleError{
			Mod:     Target,
			Query:   query,
			Pattern: pattern,
		}
	}

	err := modfetch.TryProxies(func(proxy string) error {
		queryModule := func(ctx context.Context, path string) (r QueryResult, err error) {
			ctx, span := trace.StartSpan(ctx, "modload.QueryPattern.queryModule ["+proxy+"] "+path)
			defer span.Done()

			current := findCurrentVersion(path)
			r.Mod.Path = path
			r.Rev, err = queryProxy(ctx, proxy, path, query, current, allowed)
			if err != nil {
				return r, err
			}
			r.Mod.Version = r.Rev.Version
			root, isLocal, err := fetch(ctx, r.Mod)
			if err != nil {
				return r, err
			}
			m := match(r.Mod, root, isLocal)
			r.Packages = m.Pkgs
			if len(r.Packages) == 0 {
				if err := firstError(m); err != nil {
					return r, err
				}
				return r, &PackageNotInModuleError{
					Mod:         r.Mod,
					Replacement: Replacement(r.Mod),
					Query:       query,
					Pattern:     pattern,
				}
			}
			return r, nil
		}

		var err error
		results, err = queryPrefixModules(ctx, candidateModules, queryModule)
		return err
	})

	return results, err
}

// modulePrefixesExcludingTarget returns all prefixes of path that may plausibly
// exist as a module, excluding targetPrefix but otherwise including path
// itself, sorted by descending length.
func modulePrefixesExcludingTarget(path string) []string {
	prefixes := make([]string, 0, strings.Count(path, "/")+1)

	for {
		if path != targetPrefix {
			if _, _, ok := module.SplitPathVersion(path); ok {
				prefixes = append(prefixes, path)
			}
		}

		j := strings.LastIndexByte(path, '/')
		if j < 0 {
			break
		}
		path = path[:j]
	}

	return prefixes
}

func findCurrentVersion(path string) string {
	for _, m := range buildList {
		if m.Path == path {
			return m.Version
		}
	}
	return ""
}

type prefixResult struct {
	QueryResult
	err error
}

func queryPrefixModules(ctx context.Context, candidateModules []string, queryModule func(ctx context.Context, path string) (QueryResult, error)) (found []QueryResult, err error) {
	ctx, span := trace.StartSpan(ctx, "modload.queryPrefixModules")
	defer span.Done()

	// If the path we're attempting is not in the module cache and we don't have a
	// fetch result cached either, we'll end up making a (potentially slow)
	// request to the proxy or (often even slower) the origin server.
	// To minimize latency, execute all of those requests in parallel.
	type result struct {
		QueryResult
		err error
	}
	results := make([]result, len(candidateModules))
	var wg sync.WaitGroup
	wg.Add(len(candidateModules))
	for i, p := range candidateModules {
		ctx := trace.StartGoroutine(ctx)
		go func(p string, r *result) {
			r.QueryResult, r.err = queryModule(ctx, p)
			wg.Done()
		}(p, &results[i])
	}
	wg.Wait()

	// Classify the results. In case of failure, identify the error that the user
	// is most likely to find helpful: the most useful class of error at the
	// longest matching path.
	var (
		noPackage   *PackageNotInModuleError
		noVersion   *NoMatchingVersionError
		notExistErr error
	)
	for _, r := range results {
		switch rErr := r.err.(type) {
		case nil:
			found = append(found, r.QueryResult)
		case *PackageNotInModuleError:
			// Given the option, prefer to attribute “package not in module”
			// to modules other than the main one.
			if noPackage == nil || noPackage.Mod == Target {
				noPackage = rErr
			}
		case *NoMatchingVersionError:
			if noVersion == nil {
				noVersion = rErr
			}
		default:
			if errors.Is(rErr, os.ErrNotExist) {
				if notExistErr == nil {
					notExistErr = rErr
				}
			} else if err == nil {
				if len(found) > 0 || noPackage != nil {
					// golang.org/issue/34094: If we have already found a module that
					// could potentially contain the target package, ignore unclassified
					// errors for modules with shorter paths.

					// golang.org/issue/34383 is a special case of this: if we have
					// already found example.com/foo/v2@v2.0.0 with a matching go.mod
					// file, ignore the error from example.com/foo@v2.0.0.
				} else {
					err = r.err
				}
			}
		}
	}

	// TODO(#26232): If len(found) == 0 and some of the errors are 4xx HTTP
	// codes, have the auth package recheck the failed paths.
	// If we obtain new credentials for any of them, re-run the above loop.

	if len(found) == 0 && err == nil {
		switch {
		case noPackage != nil:
			err = noPackage
		case noVersion != nil:
			err = noVersion
		case notExistErr != nil:
			err = notExistErr
		default:
			panic("queryPrefixModules: no modules found, but no error detected")
		}
	}

	return found, err
}

// A NoMatchingVersionError indicates that Query found a module at the requested
// path, but not at any versions satisfying the query string and allow-function.
//
// NOTE: NoMatchingVersionError MUST NOT implement Is(os.ErrNotExist).
//
// If the module came from a proxy, that proxy had to return a successful status
// code for the versions it knows about, and thus did not have the opportunity
// to return a non-400 status code to suppress fallback.
type NoMatchingVersionError struct {
	query, current string
}

func (e *NoMatchingVersionError) Error() string {
	currentSuffix := ""
	if (e.query == "upgrade" || e.query == "patch") && e.current != "" {
		currentSuffix = fmt.Sprintf(" (current version is %s)", e.current)
	}
	return fmt.Sprintf("no matching versions for query %q", e.query) + currentSuffix
}

// A PackageNotInModuleError indicates that QueryPattern found a candidate
// module at the requested version, but that module did not contain any packages
// matching the requested pattern.
//
// NOTE: PackageNotInModuleError MUST NOT implement Is(os.ErrNotExist).
//
// If the module came from a proxy, that proxy had to return a successful status
// code for the versions it knows about, and thus did not have the opportunity
// to return a non-400 status code to suppress fallback.
type PackageNotInModuleError struct {
	Mod         module.Version
	Replacement module.Version
	Query       string
	Pattern     string
}

func (e *PackageNotInModuleError) Error() string {
	if e.Mod == Target {
		if strings.Contains(e.Pattern, "...") {
			return fmt.Sprintf("main module (%s) does not contain packages matching %s", Target.Path, e.Pattern)
		}
		return fmt.Sprintf("main module (%s) does not contain package %s", Target.Path, e.Pattern)
	}

	found := ""
	if r := e.Replacement; r.Path != "" {
		replacement := r.Path
		if r.Version != "" {
			replacement = fmt.Sprintf("%s@%s", r.Path, r.Version)
		}
		if e.Query == e.Mod.Version {
			found = fmt.Sprintf(" (replaced by %s)", replacement)
		} else {
			found = fmt.Sprintf(" (%s, replaced by %s)", e.Mod.Version, replacement)
		}
	} else if e.Query != e.Mod.Version {
		found = fmt.Sprintf(" (%s)", e.Mod.Version)
	}

	if strings.Contains(e.Pattern, "...") {
		return fmt.Sprintf("module %s@%s found%s, but does not contain packages matching %s", e.Mod.Path, e.Query, found, e.Pattern)
	}
	return fmt.Sprintf("module %s@%s found%s, but does not contain package %s", e.Mod.Path, e.Query, found, e.Pattern)
}

func (e *PackageNotInModuleError) ImportPath() string {
	if !strings.Contains(e.Pattern, "...") {
		return e.Pattern
	}
	return ""
}

// ModuleHasRootPackage returns whether module m contains a package m.Path.
func ModuleHasRootPackage(ctx context.Context, m module.Version) (bool, error) {
	root, isLocal, err := fetch(ctx, m)
	if err != nil {
		return false, err
	}
	_, ok, err := dirInModule(m.Path, m.Path, root, isLocal)
	return ok, err
}

func versionHasGoMod(ctx context.Context, m module.Version) (bool, error) {
	root, _, err := fetch(ctx, m)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil && !fi.IsDir(), nil
}
