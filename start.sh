#! /bin/sh
[ -x ./gitserve ] || go build -o . .

nohup ./gitserve serve gitroot >gitserve.log 2>&1 &
