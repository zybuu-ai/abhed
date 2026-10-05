#!/bin/bash
# Only logs: says what each tool call was, and decides nothing.
while IFS= read -r line; do
  case "$line" in
    *'"event":"tool_call"'*) echo '{"log":"saw a tool call"}' ;;
    *) echo '{}' ;;
  esac
done
