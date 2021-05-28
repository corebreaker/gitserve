# gitserve
_A little http server for Git_

This can be useful to get easily a little http server locally to server its own repository without register its repo on a public Git server like Github/Gitlab.
You can also put published repo in a USB key.


## Installation
```bash
go install github.com/corebreaker/gitserve
```


## Start server

```bash
nohup gitserve gitroot / >gitserve.log 2>&1 &
```

When started, you can clone a repository with `git clone http://localhost:8085/my-repo`, the cloned repository is in the directory `gitroot/my-repo`


## Quick start, no install

```bash
git clone https://github.com/corebreaker/gitserve
cd gitserve
./start.sh
```


# Notices

Create a repository with `git init --bare`
