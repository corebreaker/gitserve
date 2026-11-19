[![CircleCI]](https://circleci.com/gh/corebreaker/gitserve/tree/main)
[![Go Version]](https://github.com/corebreaker/gitserve/blob/main/go.mod)
[![License]](https://github.com/corebreaker/gitserve/blob/main/LICENSE)

# gitserve
_A little http server for Git_

This can be useful to get easily a little http server locally to serve your own repositories,
without registering them on a public Git server like GitHub or GitLab.
The repositories can live in a directory, or in a single zip archive that is easy to carry on a USB key.

gitserve is written in pure Go with [go-git](https://github.com/go-git/go-git):
the serving machine needs no Git installation.


## Installation
```bash
go install github.com/corebreaker/gitserve@latest
```


## Start server

```bash
gitserve create gitroot my-repo   # create the bare repository gitroot/my-repo
gitserve serve gitroot            # serve the repositories of gitroot on port 8085
```

When started, you can clone the repository with `git clone http://localhost:8085/my-repo`, and push to it.

To run the server in the background:

```bash
nohup gitserve serve gitroot >gitserve.log 2>&1 &
```


## Stores

The repositories live in a store: a directory, or a zip archive when its name ends with `.zip`.
Both have the same layout, one bare repository per directory, possibly in subdirectories:
the repository `team/tools` is served at `http://localhost:8085/team/tools`.
A repository whose directory name ends with `.git` is also served without that suffix,
and repositories created with `git init --bare` are served as well.

A zip archive acts as the image of a filesystem: the server loads it in memory, and writes it back after each push.
The new archive replaces the previous one only once fully written, so that an interrupted write never corrupts it.
The archive holds the repositories at its root, next to a `.gitserve` file that records the version of its format;
a zip made from a directory store is a valid archive too.

Since the whole archive is held in memory and rewritten at each push, it suits small and medium repositories.
An archive must be served by a single server at a time,
but the other commands can change it while it is served: the server reloads it.


## Commands

| Command                                                   | Description                                 |
|-----------------------------------------------------------|---------------------------------------------|
| `gitserve serve [--port 8085] [--prefix /] <store>`       | Serve the repositories of a store over HTTP |
| `gitserve init <store>`                                   | Create an empty store                       |
| `gitserve create [--branch main] <store> <repository>...` | Create bare repositories                    |
| `gitserve delete <store> <repository>...`                 | Delete repositories                         |
| `gitserve list <store>`                                   | List the repositories of a store            |

- `serve` and `create` initialize the store when it does not exist yet.
- `--port` (`-p`) sets the port on which the server listens, on all network interfaces.
- `--prefix` sets the URL path under which the repositories are served:
  with `--prefix /git`, the repository `my-repo` is served at `http://localhost:8085/git/my-repo`.
- `--branch` (`-b`) sets the branch that HEAD points to in the new repositories.

Run `gitserve help <command>` for the details of a command.


## Quick start, no install

```bash
git clone https://github.com/corebreaker/gitserve
cd gitserve
./start.sh
```

`start.sh` builds gitserve if needed, then serves the `gitroot` directory in the background, logging to `gitserve.log`.

[CircleCI]: https://img.shields.io/circleci/build/github/corebreaker/gitserve/main?style=for-the-badge
[Go Version]: https://img.shields.io/github/go-mod/go-version/corebreaker/gitserve?style=for-the-badge
[License]: https://img.shields.io/github/license/corebreaker/gitserve?style=for-the-badge
