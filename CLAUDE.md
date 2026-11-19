# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`gitserve` is a minimal HTTP server exposing local bare Git repositories over the smart HTTP protocol,
so they can be cloned/pushed with `git clone http://localhost:8085/<repo>` without a public forge.
The repositories live in a store: a directory, or a zip archive loaded in memory.
The program is the package `main`, one file per concern, each tested by its `_test.go` file:
`gitserve.go` (command line), `server.go` (HTTP), `spool.go` (request and response buffers), `store.go` (stores),
`archive.go` (zip archives) and `repository.go` (repositories). `e2e_test.go` drives the real `git` client
against the server.

The goal is decentralized collaboration: bare repositories carried on a USB key or any other medium,
served with zero setup so that collaborators can clone and push without a forge. Simplicity is the point.
Security is deliberately out of scope: no authentication, no access control,
and listening on all interfaces is intended. Do not add them, and do not flag their absence in reviews.

## Language

Everything written into the project is in English: code, comments, documentation, commit messages.
Conversation with the user happens in French.

## Commands

```bash
go build -o . .                     # build the ./gitserve binary
go vet ./...                        # static checks
go test ./...                       # run all tests; the end-to-end test needs git, and is skipped without it
go test -run TestServe ./...        # run only the tests matching a regexp
./gitserve create gitroot my-repo   # create a bare repository in the gitroot directory
./gitserve serve gitroot            # serve ./gitroot on port 8085 (foreground)
./gitserve serve -p 9000 repos.zip  # serve a zip archive on port 9000, creating it if needed
./start.sh                          # build if needed, then run in background, logging to gitserve.log
```

Sources follow standard Go formatting: run `gofmt -w .` before committing, the CI rejects unformatted code.
Lines stay within 120 columns, code included, a tab counting as 4.

The server is pure Go, and runs natively on this Windows machine.
In Git Bash, which also runs the commands that the user types with `!`, prefix commands with `MSYS_NO_PATHCONV=1`,
otherwise an argument such as `/git` becomes a Windows path. Keep the commands given to the user short:
a long one may break into several lines when pasted.
`go test -race` needs cgo, which fails with the MSYS2 gcc of this machine: run the test job in the image of the CI,
which also runs the tests skipped on Windows (`~/project` already exists in the image, hence the copy to `~/work`):

```bash
MSYS_NO_PATHCONV=1 docker run --rm -e CI=true -v "$(pwd -W):/src:ro" cimg/go:1.27 \
  bash -c 'cp -r /src ~/work && cd ~/work && go test -race ./...'
```

Lint the GitHub workflows with actionlint, which also runs shellcheck on their scripts:

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W):/repo" --workdir /repo rhysd/actionlint:latest
```

## CI

- CircleCI (`.circleci/config.yml`) runs three jobs on `cimg/go`: `lint` (`gofmt -l`, `go mod tidy -diff`, `go vet`),
  `build` and `test` (`go test -race`). Images are pulled with `$DOCKERHUB_ACCESS_TOKEN`.
- Branch protection on `main` requires the `all-checks` check from the `circleci-checks` app (id 18001),
  named after the CircleCI workflow. Renaming that workflow blocks every merge
  until the required check is updated in the protection of `main`.
  It also requires, even from administrators, an up-to-date branch, an approval given after the last push,
  and resolved review threads.
- CircleCI logs need a CircleCI token, and `gh pr checks` only gives the outcome:
  reproduce a failing job in the image of the CI (see Commands).
- `.github/workflows/claude-review.yml` runs a Claude review when the repository owner starts a PR comment with
  `/claude review`, which posts a review only, or `/claude review and approve`, which also approves when no Critical
  or Important point is found. It needs the `BOT_APP_ID`, `BOT_APP_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` secrets.
  GitHub runs it from `main` only: changes to the workflow take effect once merged,
  and the PR that carries them is reviewed by the previous version.
- claude-code-action forbids Claude to submit reviews: Claude only writes `review.json`, and a step of the workflow
  posts it, pinned to the reviewed commit, with an event that the job allows. That step stays inline in the workflow,
  and a `review.json` shipped by the PR is removed beforehand: the jobs check out the head of the PR,
  whose files must never decide an approval.

## Pull requests

- A change goes through an issue, a branch linked to it, and a PR that closes the issue:
  `gh issue develop <n> --base main --name <branch> --checkout` creates and checks out the linked branch.
- The approval of the review bot satisfies the protection of `main`, but any push dismisses it:
  after the last push, comment again `MSYS_NO_PATHCONV=1 gh pr comment <pr> --body "/claude review and approve"`.
  Without the variable, Git Bash turns the command into `C:/env/git/claude review and approve`.
- Answer each inline review comment in its thread with `Fixed in <sha>`, which the next review counts as resolved:
  `gh api -X POST repos/corebreaker/gitserve/pulls/<pr>/comments/<id>/replies -F body=@<file>`.
  The merge also needs every thread resolved, through the `resolveReviewThread` GraphQL mutation
  on the thread ids that the `reviewThreads` of the PR give.
- Merge with `gh pr merge <pr> --merge`, a merge commit, or with `--squash` when the user asks to combine commits,
  then delete the branch, which GitHub keeps after a merge. After a squash, `git branch -d` refuses the branch:
  delete it with `-D` once `git diff --quiet origin/main <branch>` confirms that `main` holds the same tree.

## Architecture

- Git itself is implemented by go-git v6, a beta pinned in `go.mod`: the v5 line has no usable server side.
  `gitHandler` (`server.go`) routes the smart HTTP endpoints (`info/refs`, `git-upload-pack`, `git-receive-pack`)
  found under the URL prefix, which it strips, then runs `transport.UploadPack` or `transport.ReceivePack`
  on the storage of the repository. The dumb protocol is refused.
  The handler reads the whole request into a `spool` (`spool.go`: in memory up to 4 MiB, then in a temporary file)
  before accessing the store, and sends the response from a spool once that access is over:
  no network transfer ever happens while the store is locked, so a slow client cannot stall the others.
  go-git's own `backend` package is not used: it answers 401 to anonymous pushes, and 500 for a missing repository.
- go-git advertises references in the order the filesystem lists them, which differs between systems:
  tests sort them (`git ls-remote --sort=refname`).
  `TestHasSideBand` and the end-to-end tests check the behaviors of go-git that the handler relies on,
  in case an upgrade changes them.
- A `store` (`store.go`) holds the repositories as a go-billy filesystem:
  `osfs` for a directory, `memfs` for a zip archive, which is any path ending with `.zip`.
  `view` gives shared access, `change` exclusive access; a push runs under `change`.
  An archive is reloaded when its file changes on disk, as when another gitserve command changes it.
  `persist` writes it back atomically (temporary file, then rename); a push calls it from the post-receive hook
  of go-git. When the write fails, `persist` has the next access reload the file, which discards the push,
  and the handler replaces the response with an error on the error band of the side-band (an HTTP 500 without
  side-band): the push fails on the client, which displays the reason as a `remote:` message.
- `archive.go` converts between zip and `memfs`. The `.gitserve` marker at the root of an archive records
  the format version (`{"version":1}`); an archive without marker is accepted, and gets one on its next write.
  Files under `objects/` are stored without compression, since Git already compresses them.
- `repository.go`: a repository is a directory holding `HEAD` and `config` files. A name is a slash-separated path
  checked by `checkName`; `findRepository` also tries it with a `.git` suffix, for the server and `delete` alike.
  Repositories are created with `git.Init`, and never nested in another repository.
- The command line (`gitserve.go`) uses cobra: `serve`, `init`, `create`, `delete` and `list`.
  `run` returns the exit status: 2 for a usage error, detected as an error raised before the command runs,
  and 1 when the command fails.
- `requestLogger` wraps the handler with a `statusRecorder` that captures the status code, to log each request.
- The server listens on `:<port>`, 8085 by default. On SIGINT or SIGTERM, `serve` shuts the server down gracefully:
  in-flight requests get 5 s, then `server.Close` releases the rest. `serve` returns only once the shutdown is over,
  then `runServer` waits for an archive write in progress (`store.close`).
- `gitroot/` is the default store used by `start.sh`; its contents are git-ignored except `.git*` files.
