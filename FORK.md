# About this fork

This is an unofficial personal fork of [littleFedi](https://littlefedi.org),
the small ActivityPub server by Stefano Marinelli and the Little Computer
People. Upstream does not publish a public git repository; it releases source
tarballs at <https://littlefedi.org/downloads/>.

## Branches

- `upstream` holds unmodified upstream source releases, one commit per release,
  each tagged `upstream/<version>` and recording the tarball's SHA-256.
- `main` is `upstream` plus the changes in this fork.

To pick up a new release, verify the tarball against its `.sha256` file, then
replace the tree on `upstream` with its contents, commit and tag it, and merge
`upstream` into `main`.

## Changes from upstream

- The composer sends the post with Ctrl+Enter (Cmd+Enter on macOS) from any of
  its fields, and the Post button's tooltip says so.
- A unit test suite (upstream omits its own from the public release). Every
  package has happy-path tests; shared fixtures live in `internal/testutil`.

## Tests

    go test ./...                # everything, including a build-and-run smoke test
    go test -short ./...         # skips the binary smoke test and the go list check
    go test -tags blog ./...     # the build the release binaries use

The database-backed tests use a throwaway SQLite file per test, so they need no
setup. Tests that would reach the network instead point the code's HTTP client
at a local server, keeping its policy checks in place.

One test is skipped on purpose: `TestRegisterRequiresApproval` in
`internal/accounts` documents an upstream bug. `models.Account.Approved` is
tagged `default:true`, and bun writes DEFAULT for a zero-valued field, so an
account registered while `auth.require_approval` is on is stored approved.

## Releases

Release binaries are built like upstream's, with the official Go toolchain
(not a distribution's patched one) and `-tags blog`; with those, the upstream
source reproduces upstream's published binary byte for byte. Fork builds add
`+feoh.N` to the version.

## License

The original MIT license and notices in `LICENSE`, `NOTICE.md` and
`THIRD_PARTY_LICENSES.md` apply. Changes made in this fork are released under
the same MIT License.
