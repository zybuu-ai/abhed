#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"list_tools"'*)
      echo '{"tools":[{"name":"Bash","description":"x"},{"name":"read","description":"x"},{"name":"weather","description":"x"}]}' ;;
    *) echo '{}' ;;
  esac
done
