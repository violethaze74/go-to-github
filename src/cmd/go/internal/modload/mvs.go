// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modload

import (
	"context"
	"errors"
	"sort"

	"cmd/go/internal/modfetch"
	"cmd/go/internal/mvs"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// mvsReqs implements mvs.Reqs for module semantic versions,
// with any exclusions or replacements applied internally.
type mvsReqs struct {
	buildList []module.Version
}

// Reqs returns the current module requirement graph.
// Future calls to SetBuildList do not affect the operation
// of the returned Reqs.
func Reqs() mvs.Reqs {
	r := &mvsReqs{
		buildList: buildList,
	}
	return r
}

func (r *mvsReqs) Required(mod module.Version) ([]module.Version, error) {
	if mod == Target {
		// Use the build list as it existed when r was constructed, not the current
		// global build list.
		return r.buildList[1:], nil
	}

	if mod.Version == "none" {
		return nil, nil
	}

	summary, err := goModSummary(mod)
	if err != nil {
		return nil, err
	}
	return summary.require, nil
}

// Max returns the maximum of v1 and v2 according to semver.Compare.
//
// As a special case, the version "" is considered higher than all other
// versions. The main module (also known as the target) has no version and must
// be chosen over other versions of the same module in the module dependency
// graph.
func (*mvsReqs) Max(v1, v2 string) string {
	if v1 != "" && semver.Compare(v1, v2) == -1 {
		return v2
	}
	return v1
}

// Upgrade is a no-op, here to implement mvs.Reqs.
// The upgrade logic for go get -u is in ../modget/get.go.
func (*mvsReqs) Upgrade(m module.Version) (module.Version, error) {
	return m, nil
}

func versions(ctx context.Context, path string, allowed AllowedFunc) ([]string, error) {
	// Note: modfetch.Lookup and repo.Versions are cached,
	// so there's no need for us to add extra caching here.
	var versions []string
	err := modfetch.TryProxies(func(proxy string) error {
		allVersions, err := modfetch.Lookup(proxy, path).Versions("")
		if err != nil {
			return err
		}
		allowedVersions := make([]string, 0, len(allVersions))
		for _, v := range allVersions {
			if err := allowed(ctx, module.Version{Path: path, Version: v}); err == nil {
				allowedVersions = append(allowedVersions, v)
			} else if !errors.Is(err, ErrDisallowed) {
				return err
			}
		}
		versions = allowedVersions
		return nil
	})
	return versions, err
}

// Previous returns the tagged version of m.Path immediately prior to
// m.Version, or version "none" if no prior version is tagged.
func (*mvsReqs) Previous(m module.Version) (module.Version, error) {
	// TODO(golang.org/issue/38714): thread tracing context through MVS.
	list, err := versions(context.TODO(), m.Path, CheckAllowed)
	if err != nil {
		return module.Version{}, err
	}
	i := sort.Search(len(list), func(i int) bool { return semver.Compare(list[i], m.Version) >= 0 })
	if i > 0 {
		return module.Version{Path: m.Path, Version: list[i-1]}, nil
	}
	return module.Version{Path: m.Path, Version: "none"}, nil
}

// next returns the next version of m.Path after m.Version.
// It is only used by the exclusion processing in the Required method,
// not called directly by MVS.
func (*mvsReqs) next(m module.Version) (module.Version, error) {
	// TODO(golang.org/issue/38714): thread tracing context through MVS.
	list, err := versions(context.TODO(), m.Path, CheckAllowed)
	if err != nil {
		return module.Version{}, err
	}
	i := sort.Search(len(list), func(i int) bool { return semver.Compare(list[i], m.Version) > 0 })
	if i < len(list) {
		return module.Version{Path: m.Path, Version: list[i]}, nil
	}
	return module.Version{Path: m.Path, Version: "none"}, nil
}
