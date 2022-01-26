# README.md

This directory holds build scripts for unofficial, unsupported
distributions of Go+BoringCrypto.

## Version strings

The distribution name for a Go+BoringCrypto release has the form `<GoVersion>b<BoringCryptoVersion>`,
where `<GoVersion>` is the Go version the release is based on, and `<BoringCryptoVersion>` is
an integer that increments each time there is a new release with different BoringCrypto bits.
The `<BoringCryptoVersion>` is stored in the `VERSION` file in this directory.

For example, the first release is based on Go 1.8.3 is `go1.8.3b1`.
If the BoringCrypto bits are updated, the next would be `go1.8.3b2`.
If, after that, Go 1.9 is released and the same BoringCrypto code added to it,
that would result in `go1.9b2`. There would likely not be a `go1.9b1`,
since that would indicate Go 1.9 with the older BoringCrypto code.

## Releases

The `build.release` script prepares a binary release and publishes it in Google Cloud Storage
at `gs://go-boringcrypto/`, making it available for download at
`https://go-boringcrypto.storage.googleapis.com/<FILE>`.
The script records each published release in the `RELEASES` file in this directory.

The `build.docker` script, which must be run after `build.release`, prepares a Docker image
and publishes it on hub.docker.com in the goboring organization.
`go1.8.3b1` is published as `goboring/golang:1.8.3b1`.

## Release process

1. If the BoringCrypto bits have been updated, increment the number in `VERSION`,
send that change out as a CL for review, get it committed, and run `git sync`.

2. Run `build.release`, which will determine the base Go version and the BoringCrypto
version, build a release, and upload it.

3. Run `build.docker`, which will build and upload a Docker image from the latest release.

4. Send out a CL with the updated `RELEASES` file and get it committed.

## Release process for dev.boringcrypto.go1.8.

In addition to the dev.boringcrypto branch, we have a dev.boringcrypto.go1.8 branch,
which is BoringCrypto backported to the Go 1.8 release branch.
To issue new BoringCrypto releases based on Go 1.8:

1. Do a regular release on the (not Go 1.8) dev.boringcrypto branch.

2. Change to the dev.boringcrypto.go1.8 branch and cherry-pick all
BoringCrypto updates, including the update of the `VERSION` file.
Mail them out and get them committed.

3. **Back on the (not Go 1.8) dev.boringcrypto branch**,
run `make.bash` and then `build.release <commit>`,
where `<commit>` is the latest commit on the dev.boringcrypto.go1.8 branch.
The script will build a release and upload it.

4. Run `build.docker`.

5. Send out a CL with the updated `RELEASES` file and get it committed.

## Building from Docker

A Dockerfile that starts with `FROM golang:1.8.3` can switch
to `FROM goboring/golang:1.8.3b2` (see [goboring/golang on Docker Hub](https://hub.docker.com/r/goboring/golang/))
and should need no other modifications.

## Building from Bazel

Using an alternate toolchain from Bazel is not as clean as it might be.
Today, as of Bazel 0.5.3 and the bazelbuild/rules_go tag 0.5.3,
it is necessary to define a `go-boringcrypto.bzl` file that duplicates
some of the rules_go internal guts and then invoke its `go_repositories` rule
instead of the standard one.

See https://gist.github.com/rsc/6f63d54886c9c50fa924597d7355bc93 for a minimal example.

Note that in the example that the Bazel `WORKSPACE` file still refers to the release as "go1.8.3" not "go1.8.3b2".

## Caveat

BoringCrypto is used for a given build only in limited circumstances:

  - The build must be GOOS=linux, GOARCH=amd64.
  - The build must have cgo enabled.
  - The android build tag must not be specified.
  - The cmd_go_bootstrap build tag must not be specified.

The version string reported by `runtime.Version` does not indicate that BoringCrypto
was actually used for the build. For example, linux/386 and non-cgo linux/amd64 binaries
will report a version of `go1.8.3b2` but not be using BoringCrypto.

To check whether a given binary is using BoringCrypto, run `go tool nm` on it and check
that it has symbols named `*_Cfunc__goboringcrypto_*`.

The program [rsc.io/goversion](https://godoc.org/rsc.io/goversion) will report the
crypto implementation used by a given binary when invoked with the `-crypto` flag.
