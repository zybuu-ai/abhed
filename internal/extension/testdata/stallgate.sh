#!/bin/bash
# Takes three seconds to decide each message, so a test can stop a run meanwhile.
while IFS= read -r line; do
  sleep 3
  echo '{}'
done
