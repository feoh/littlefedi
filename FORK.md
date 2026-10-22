## Crazy pants important note for folks running "Stock" LittleFedi!

There have already been questions from folks wanting to run my fork "over" an
already existing LittleFedi instance.

Don't do it. My fork adds several database migrations, and I've done zero work
testing that path, because I don't *have* a pre-existing LittleFedi to test 
with!

## About This Fork

This is an unofficial personal fork of [littleFedi](https://littlefedi.org),
the small ActivityPub server by Stefano Marinelli and the Little Computer
People. Upstream does not publish a public git repository; it releases source
tarballs at <https://littlefedi.org/downloads/>.

If you have ANY questions or concerns about this fork, please contact me at 
feoh@feoh.org or on social media. DO NOT contact the Little Computer People!

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
- Attachments you pick, paste or drop show as thumbnails right under the
  composer's text, each with its own alt text field and a remove button, as
  in Mastodon. Upstream only listed their file names in the media panel. The
  CSP now allows `blob:` images and media, which these thumbnails, the
  preview tab's attachments and the poster frame drawn from a picked video
  all need; upstream's CSP blocked all three.
- Streamed posts wait while you are writing in a composer (focus is in it, or
  your last click or tap was), so the editor no longer jumps away when one
  arrives. They are added once you click, tap or tab elsewhere.
- A unit test suite (upstream omits its own from the public release). Every
  package has happy-path tests; shared fixtures live in `internal/testutil`.
- Push notifications work with Mastodon's mobile apps, Ivory and Tuba. The
  push API follows Mastodon's behaviour: it accepts form-encoded requests,
  returns `server_key` and `standard`, sends the legacy `aesgcm` encoding
  unless a client asks for RFC 8291, and sends Mastodon's payload fields,
  including the app's access token (sealed at rest when
  `LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY` is set) and the numeric
  notification ID. It also accepts the `follower` policy, keeps alerts a
  client turned off, and lets a new login on the same device take over its
  push endpoint.
- Three Catppuccin themes (Frappé, Macchiato, Mocha), each pairing its dark
  flavor with Latte for light mode, from the official palette v1.8.0.
- The client API, its push and its streaming use Mastodon's notification
  types only; littleFedi's own (follow_accept, emoji reactions, follow
  imports and so on) reach API clients only when they ask with `types[]`,
  and stay visible in the web UI. littleFedi also sends Mastodon's `poll`
  (a poll you created or voted in ended) and `admin.sign_up` notifications.
- The client API adds `GET /api/v1/accounts?id[]=`, `/api/v1/accounts/lookup`
  and `/api/v1/accounts/familiar_followers`, which Mastodon for iOS uses.
- The OAuth consent page also answers at `/oauth/authorize/`, with a
  trailing slash, which is the URL toot opens. Mastodon accepts both.
- The instance documents carry every media limit Mastodon's apps require
  (without them Mastodon for iOS cannot load notifications), and
  `image_matrix_limit` is a pixel count, as in Mastodon.

Fork database migrations are numbered from 9001 up. bun names a migration by
its number alone, so sharing a number with a later upstream migration would
skip that migration.

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

## Lint

[prek](https://github.com/j178/prek) runs gofmt, `go vet`, eslint and
golangci-lint before each commit and `go test -short` before each push:

    prek install                 # once per clone
    prek run --all-files

golangci-lint reports only lines changed since the `upstream` branch, since
upstream's own code has findings this fork leaves alone. None of the hooks
reformat files, so upstream files stay as released.

## Releases

Release binaries are built like upstream's, with the official Go toolchain
(not a distribution's patched one) and `-tags blog`; with those, the upstream
source reproduces upstream's published binary byte for byte. Fork builds add
`+feoh.N` to the version.

## License

The original MIT license and notices in `LICENSE`, `NOTICE.md` and
`THIRD_PARTY_LICENSES.md` apply. Changes made in this fork are released under
the same MIT License.
