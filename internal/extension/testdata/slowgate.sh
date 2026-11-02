#!/bin/bash
# Takes a while to decide each message, and refuses those saying "forbidden".
while IFS= read -r line; do
  sleep 0.3
  case "$line" in
    *forbidden*) echo '{"block":true,"reason":"that topic is off limits"}' ;;
    *) echo '{}' ;;
  esac
done
