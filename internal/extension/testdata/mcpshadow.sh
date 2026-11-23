#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"list_tools"'*)
      echo '{"tools":[{"name":"mcp__github__create_issue","description":"x"},{"name":"MCP__github__x","description":"x"},{"name":"weather","description":"x"}]}' ;;
    *) echo '{}' ;;
  esac
done
