# Agent instructions

This is a personal fork of littleFedi. Read `FORK.md` first: it explains the
`upstream` and `main` branches and lists every change from upstream.

## Working in the repo

- `main` is what runs in production. Commit there and push to `origin`
  (`git@github.com:feoh/littlefedi.git`). The repository is public: never
  commit addresses, hostnames from the operator's SSH config, or anything
  from the VM's secret files.
- Never commit to `upstream`. It holds only unmodified upstream releases.
- Add a `FORK.md` bullet for every user-visible change from upstream.
- Fork database migrations are numbered from 9001 up (see `FORK.md`).
- Before pushing, run the whole suite both ways:

      go test ./...
      go test -tags blog ./...

- Hooks run through prek (`.pre-commit-config.yaml`); `prek install` sets up
  both stages. On commit: JSON, merge-conflict and large-file checks,
  gofmt, `go vet` with and without `-tags blog`, eslint on the web UI's
  handwritten scripts, and golangci-lint. On push: `go test -short ./...`.
  Run them all with `prek run --all-files`. Nothing in them rewrites files,
  on purpose: reformatting upstream files would make every upstream release
  conflict. Do not add formatters or whitespace fixers.
- golangci-lint reports only lines changed since the `upstream` branch
  (`.golangci.yml`), because upstream's own code has findings the fork does
  not own. In a fresh clone, create that branch first:
  `git branch upstream origin/upstream`. Editing an upstream line that
  already has a finding makes it the fork's: fix it, or leave the line alone.
- The operator also commits on GitHub, so `origin/main` can move under you.
  `git fetch origin` and `git rebase origin/main` before pushing, then check
  the push's own exit status: piping `git push` into anything hides a
  rejected push.

## Deploying to fediverse.feoh.org

Production is one Docker container, `little-fedi-littlefedi-1`, on the
`little-fedi` VM. Its image is built from a binary built here, never from
source on the VM.

### Access

The SSH user, host and key are in `~/.config/monit-homelab/ssh-little-fedi/`
(`user`, `hostname`, `identity`, `known_hosts`):

    D=~/.config/monit-homelab/ssh-little-fedi
    ssh -i $D/identity -o UserKnownHostsFile=$D/known_hosts -o BatchMode=yes \
        "$(cat $D/user)@$(cat $D/hostname)"

The user has passwordless sudo.

### Layout on the VM

- Build context and Compose project: `/nas/container_configs/compose/little_fedi`
  (a CIFS share). The SSH user cannot `cd` into it or list it without sudo,
  and the share does not expand globs for that user either. Run everything
  there through a root shell: `sudo sh -c "cd … && …"`.
- `README.md` in that directory is the deployment log. Its top entry names
  the running version and fork commit.
- `bin/littlefedi` is the binary the image copies in. `compose.yaml` pins the
  image tag and `Dockerfile` the version label.
- Every earlier deploy is kept beside them as `bin/littlefedi-<version>`,
  `compose.yaml.<version>` and `Dockerfile.<version>`, and every earlier image
  stays in Docker, so a rollback needs no rebuild.
- SQLite and media: `/var/lib/littlefedi` (local disk). Secrets:
  `/etc/littlefedi/secrets.env` (root only). Never print the expanded
  Compose config (`docker compose config` without `--quiet`): it contains
  private keys.
- `little-fedi-compose.service` runs `docker compose up -d --no-build`. A
  restart never builds the image.

### Versions

Deploys are numbered `<upstream version>-feoh.<N>`: the image tag is
`feoh/little-fedi:26.10.13-feoh.N`, and the binary reports
`littlefedi 26.10.13+feoh.N`. Read the running N from `compose.yaml` and
deploy N+1. After a new upstream release, N starts again at 1.

### Steps

1. Push the commit you are deploying, and run the suite on that exact
   commit (in the worktree from step 2) if anything was rebased since you
   last ran it.

2. Build the binary from that commit, in a clean worktree, with the
   **official** Go toolchain. The system Go is a distro build and produces a
   different binary; the official one reproduces the deployed binaries byte
   for byte.

       git worktree add /tmp/littlefedi-<sha> <sha>
       cd /tmp/littlefedi-<sha>
       GOTOOLCHAIN=go1.27.1 GOOS=linux GOARCH=amd64 make build-blog VERSION=26.10.13+feoh.N
       ./littlefedi-blog --version
       sha256sum littlefedi-blog

   Release binaries use `-tags blog` (`make build-blog`). Remove the worktree
   afterwards.

3. Back up first. This takes about 25 seconds:

       sudo systemctl start little-fedi-backup.service
       systemctl show little-fedi-backup.service -p Result   # Result=success

4. Copy the binary to the VM's `/tmp` with `scp`, using the same options as
   `ssh`, and check its SHA-256 there. Delete it from `/tmp` once the deploy
   is verified, including after a step fails partway.

5. Keep the current files as rollback copies, then install the new ones:

       sudo sh -c '
         set -e
         cd /nas/container_configs/compose/little_fedi
         cp -p bin/littlefedi bin/littlefedi-26.10.13-feoh.<N-1>
         cp -p compose.yaml compose.yaml.26.10.13-feoh.<N-1>
         cp -p Dockerfile Dockerfile.26.10.13-feoh.<N-1>
         cp /tmp/littlefedi-feoh.N bin/littlefedi
         sed -i "s|feoh/little-fedi:26.10.13-feoh.<N-1>|feoh/little-fedi:26.10.13-feoh.N|" compose.yaml
         sed -i "s|26.10.13+feoh.<N-1>|26.10.13+feoh.N|" Dockerfile
         sha256sum bin/littlefedi
         grep -n "feoh\." compose.yaml Dockerfile'

6. **Build the image, and confirm it exists, before restarting.** Restarting
   first stops the running container and then fails to start the new one,
   because the unit never builds. That caused 14 seconds of downtime on
   2026-10-16.

       sudo sh -c 'cd /nas/container_configs/compose/little_fedi && docker compose build -q'
       sudo docker image ls feoh/little-fedi:26.10.13-feoh.N

   The "Docker Compose is configured to build using Bake, but buildx isn't
   installed" warning is harmless.

7. Restart and verify. Stopping the old container can take about 30 seconds
   while open streaming connections close (31 s on 2026-10-16, against 1 s
   with none open), and the site is likely unavailable meanwhile:

       sudo systemctl restart little-fedi-compose.service
       sudo docker ps --format '{{.Image}} {{.Status}}'      # new tag, then (healthy)
       sudo docker exec little-fedi-littlefedi-1 /usr/local/bin/littlefedi --version
       curl -fsS http://127.0.0.1:8080/readyz                # on the VM
       curl -fsS https://fediverse.feoh.org/readyz           # from anywhere
       sudo docker logs --since 2m little-fedi-littlefedi-1 2>&1 | grep -iE 'panic|fatal|error'

   Warnings about remote actors that are gone (HTTP 410) are normal
   federation noise. Also check the behaviour the change was for, against
   the live site.

8. Add an entry to the top of the VM's `README.md`, below the heading, in
   the same form as the others: the time in UTC, the image tag, the fork
   commit, what changed, whether the schema changed, the backup time, and
   the rollback. Write it from a file, as the other steps do: `scp` it to
   `/tmp` and splice it in with a root shell (`head -n 2`, the entry,
   `tail -n +3`). Then delete the temporary files on the VM.

### Rollback

Restore the `<N-1>` copies of `bin/littlefedi`, `compose.yaml` and
`Dockerfile` with `cp -p` (from a root shell, as above), then
`sudo systemctl restart little-fedi-compose.service`. The older image is
still present. A deploy that ran a schema migration may need the backup
restored instead: an older binary does not undo a migration. See the VM's
`README.md` for restoring a backup.

## The Gist

<https://gist.github.com/feoh/2552f8074a4f8c91af94fd21776a949a> publishes the
fork's **client API compatibility work only**: Mastodon client API, push and
streaming behaviour, plus the locale labels those changes need. It leaves out
the web UI features (Ctrl+Enter, themes, held streamed posts), the test
suite and `FORK.md`. When a client API change lands, update it; for any
other change, leave it alone.

It holds `littlefedi-26.10.13-feoh.patch` and `LittleFedi_Patch_README.md`.
To rebuild the patch:

1. List the API commits: `git log --oneline upstream..main`, and pick the
   ones that change client-facing behaviour.
2. In a detached worktree at `upstream`, check out from `main` every
   non-test file those commits touch. A file that a non-API commit also
   touches needs the API commits' hunks only: apply
   `git diff <commit>^ <commit> -- <file>` for each API commit instead. The
   locale JSON files are like this.
3. `git add -A && git diff --cached upstream > littlefedi-26.10.13-feoh.patch`.
4. Confirm that, excluding tests and `FORK.md`, the only differences between
   that tree and `main` are the left-out features; that it builds and passes
   `go vet` with and without `-tags blog`; and that `main`'s tests copied onto
   it fail only where they test those features.
5. Update the README's sections to match, upload both files with
   `gh gist edit <id> -f <name> <localfile>`, and verify them with
   `gh gist view <id> --raw -f <name> | cmp - <localfile>`. Change the
   description with `gh api -X PATCH gists/<id> -f description=…`: with two
   files, `gh gist edit -d` fails.
