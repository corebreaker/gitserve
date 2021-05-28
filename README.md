# gitserve

_A little git http server_

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
