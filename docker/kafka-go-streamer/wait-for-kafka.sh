#!/bin/sh

set -e

CMD="$1"
shift
CMD_ARGS="$@"

LOOPS=10
while :
do
  NUM_BROKERS=$(echo dump | nc ${ZOOKEEPER_HOST} 2181 | grep -c broker|cat -)
  if [ $NUM_BROKERS -ne 0 ]
  then
    break
  fi
  if [ $LOOPS -eq 10 ]
  then
    break
  fi
  sleep 10
done

>&2 echo "Kafka is up - executing command"

exec $CMD $CMD_ARGS
