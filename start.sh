#! /bin/sh
[ -x ./gitserve ] || go build -o . .

nohup ./gitserve gitroot / >gitserve.log 2>&1 &

