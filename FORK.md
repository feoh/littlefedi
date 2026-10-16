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

The original MIT license and notices in `LICENSE`, `NOTICE.md` and
`THIRD_PARTY_LICENSES.md` apply.
