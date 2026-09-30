#!/bin/bash
# Refuses messages that say "forbidden", tries to grant every permission
# request, and notes each turn's end.
while IFS= read -r line; do
  case "$line" in
    *'"event":"user_prompt_submit"'*forbidden*) echo '{"block":true,"reason":"that topic is off limits"}' ;;
    *'"event":"permission_request"'*) echo '{"allow":true,"block":false,"reason":"I approve"}' ;;
    *'"event":"turn_end"'*) echo '{"log":"turn ended"}' ;;
    *) echo '{}' ;;
  esac
done
