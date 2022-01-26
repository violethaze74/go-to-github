// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/internal/objabi"
	"cmd/internal/sys"
	"encoding/binary"
)

// Target holds the configuration we're building for.
type Target struct {
	Arch *sys.Arch

	HeadType objabi.HeadType

	LinkMode  LinkMode
	BuildMode BuildMode

	linkShared    bool
	canUsePlugins bool
	IsELF         bool
}

//
// Target type functions
//

func (t *Target) IsShared() bool {
	return t.BuildMode == BuildModeShared
}

func (t *Target) IsPlugin() bool {
	return t.BuildMode == BuildModePlugin
}

func (t *Target) IsInternal() bool {
	return t.LinkMode == LinkInternal
}

func (t *Target) IsExternal() bool {
	return t.LinkMode == LinkExternal
}

func (t *Target) IsPIE() bool {
	return t.BuildMode == BuildModePIE
}

func (t *Target) IsSharedGoLink() bool {
	return t.linkShared
}

func (t *Target) CanUsePlugins() bool {
	return t.canUsePlugins
}

func (t *Target) IsElf() bool {
	return t.IsELF
}

func (t *Target) IsDynlinkingGo() bool {
	return t.IsShared() || t.IsSharedGoLink() || t.IsPlugin() || t.CanUsePlugins()
}

// UseRelro reports whether to make use of "read only relocations" aka
// relro.
func (t *Target) UseRelro() bool {
	switch t.BuildMode {
	case BuildModeCArchive, BuildModeCShared, BuildModeShared, BuildModePIE, BuildModePlugin:
		return t.IsELF || t.HeadType == objabi.Haix
	default:
		return t.linkShared || (t.HeadType == objabi.Haix && t.LinkMode == LinkExternal)
	}
}

//
// Processor functions
//

func (t *Target) Is386() bool {
	return t.Arch.Family == sys.I386
}

func (t *Target) IsARM() bool {
	return t.Arch.Family == sys.ARM
}

func (t *Target) IsAMD64() bool {
	return t.Arch.Family == sys.AMD64
}

func (t *Target) IsPPC64() bool {
	return t.Arch.Family == sys.PPC64
}

func (t *Target) IsS390X() bool {
	return t.Arch.Family == sys.S390X
}

//
// OS Functions
//

func (t *Target) IsLinux() bool {
	return t.HeadType == objabi.Hlinux
}

func (t *Target) IsDarwin() bool {
	return t.HeadType == objabi.Hdarwin
}

func (t *Target) IsWindows() bool {
	return t.HeadType == objabi.Hwindows
}

func (t *Target) IsPlan9() bool {
	return t.HeadType == objabi.Hplan9
}

func (t *Target) IsAIX() bool {
	return t.HeadType == objabi.Haix
}

func (t *Target) IsSolaris() bool {
	return t.HeadType == objabi.Hsolaris
}

func (t *Target) IsNetbsd() bool {
	return t.HeadType == objabi.Hnetbsd
}

func (t *Target) IsOpenbsd() bool {
	return t.HeadType == objabi.Hopenbsd
}

//
// MISC
//

func (t *Target) IsBigEndian() bool {
	return t.Arch.ByteOrder == binary.BigEndian
}
