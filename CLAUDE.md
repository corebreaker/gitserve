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

The server is pure Go, and runs natively on this Windows machine.
`go test -race`, as run by the CI, needs a cgo toolchain that this machine lacks: run it under WSL.
In Git Bash, prefix commands with `MSYS_NO_PATHCONV=1`, otherwise an argument such as `/git` becomes a Windows path.

## CI

- CircleCI (`.circleci/config.yml`) runs three jobs on `cimg/go`: `lint` (`gofmt -l`, `go mod tidy -diff`, `go vet`),
  `build` and `test` (`go test -race`). Images are pulled with `$DOCKERHUB_ACCESS_TOKEN`.
- Branch protection on `main` requires the `all-checks` check from the `circleci-checks` app (id 18001),
  named after the CircleCI workflow. Renaming that workflow blocks every merge
  until the required check is updated in the protection of `main`.
- `.github/workflows/claude-review.yml` runs a Claude review when the repository owner comments on a PR:
  `/claude review` posts a review only, `/claude review and approve` also approves when no Critical
  or Important point is found. It needs the `BOT_APP_ID`, `BOT_APP_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` secrets.
  GitHub runs it from `main` only: changes to the workflow take effect once merged.

## Architecture

- Git itself is implemented by go-git v6, a beta pinned in `go.mod`: the v5 line has no usable server side.
  `gitHandler` (`server.go`) routes the smart HTTP endpoints (`info/refs`, `git-upload-pack`, `git-receive-pack`)
  found under the URL prefix, which it strips, then runs `transport.UploadPack` or `transport.ReceivePack`
  on the storage of the repository. The dumb protocol is refused.
  The handler reads the whole request into a `spool` (`spool.go`: in memory up to 4 MiB, then in a temporary file)
  before accessing the store, and sends the response from a spool once that access is over:
  no network transfer ever happens while the store is locked, so a slow client cannot stall the others.
  go-git's own `backend` package is not used: it answers 401 to anonymous pushes, and 500 for a missing repository.
- A `store` (`store.go`) holds the repositories as a go-billy filesystem:
  `osfs` for a directory, `memfs` for a zip archive, which is any path ending with `.zip`.
  `view` gives shared access, `change` exclusive access; a push runs under `change`.
  An archive is reloaded when its file changes on disk, as when another gitserve command changes it.
  `persist` writes it back atomically (temporary file, then rename); a push calls it from the post-receive hook
  of go-git, which reports a failure to the client as a `remote:` message.
- `archive.go` converts between zip and `memfs`. The `.gitserve` marker at the root of an archive records
  the format version (`{"version":1}`); an archive without marker is accepted, and gets one on its next write.
  Files under `objects/` are stored without compression, since Git already compresses them.
- `repository.go`: a repository is a directory holding `HEAD` and `config` files. A name is a slash-separated path
  checked by `checkName`; `findRepository` also tries it with a `.git` suffix, for the server and `delete` alike.
  Repositories are created with `git.Init`, and never nested in another repository.
- The command line (`gitserve.go`) uses cobra: `serve`, `init`, `create`, `delete` and `list`.
  `run` returns the exit status: 2 for a usage error, detected as an error raised before the command runs,
  and 1 when the command fails.
- `requestLogger` wraps the handler with a `statusRecorder` that captures the status code, to log each request;
  `statusRecorder.Unwrap` lets `http.ResponseController` reach the wrapped writer.
- The server listens on `:<port>`, 8085 by default. On SIGINT or SIGTERM, `serve` shuts the server down gracefully:
  in-flight requests get 5 s, then `server.Close` releases the rest. `serve` returns only once the shutdown is over,
  then `runServer` waits for an archive write in progress (`store.close`).
- `gitroot/` is the default store used by `start.sh`; its contents are git-ignored except `.git*` files.
