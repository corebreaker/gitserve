# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`gitserve` is a minimal HTTP server exposing local bare Git repositories over the smart HTTP protocol,
so they can be cloned/pushed with `git clone http://localhost:8085/<repo>` without a public forge.
The program is a single file, `gitserve.go` (package `main`), tested by `gitserve_test.go`.

The goal is decentralized collaboration: bare repositories carried on a USB key or any other medium,
served with zero setup so that collaborators can clone and push without a forge. Simplicity is the point.
Security is deliberately out of scope: no authentication, no access control,
and listening on all interfaces is intended. Do not add them, and do not flag their absence in reviews.

## Language

Everything written into the project is in English: code, comments, documentation, commit messages.
Conversation with the user happens in French.

## Commands

```bash
go build -o . .                  # build the ./gitserve binary
go vet ./...                     # static checks
go test ./...                    # run all tests
go test -run TestServe ./...     # run only the tests matching a regexp
./gitserve gitroot /             # serve ./gitroot at URL prefix / on port 8085 (foreground)
./start.sh                       # build if needed, then run in background, logging to gitserve.log
git init --bare gitroot/my-repo  # create a servable repository
```

Sources follow standard Go formatting: run `gofmt -w .` before committing, the CI rejects unformatted code.

go-git-http runs Git from the hardcoded path `/usr/bin/git`: on this Windows machine, try the server under WSL.
In Git Bash, prefix commands with `MSYS_NO_PATHCONV=1`, otherwise an argument such as `/` becomes a Windows path.

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

- Git protocol handling is fully delegated to `github.com/AaronO/go-git-http` (`githttp.New(root)`),
  mounted on an `http.ServeMux` under the URL prefix given on the command line.
  It looks a repository up as the root joined with the whole URL path (the prefix is not stripped),
  so the prefix must be a directory under the root.
- `requestLogger` wraps the mux with a `statusRecorder` that captures the status code, to log each request.
- `parseArgs` handles `gitserve <path> [<prefix>]`; any other argument count prints the usage and exits with code 2:
  - with two args, `<path>` is the repositories root and `<prefix>` the URL mount point
    (normalized to start and end with `/`);
  - with one arg, the root is the *parent* directory of `<path>`, and the base name of `<path>` is the prefix.
- The listen address is hardcoded to `:8085`. On SIGINT or SIGTERM, `serve` shuts the server down gracefully:
  in-flight requests get 5 s, then `server.Close` releases the rest. `serve` returns only once the shutdown is over.
- `gitroot/` is the default repository root used by `start.sh`; its contents are git-ignored except `.git*` files.
