# Sourced by install.sh and run.sh. digest BIN prints one SHA-256 over the
# abhed binary and every abhed-helper-* beside it, which the engine runs
# from there, so a change to any of them is seen.
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
digest() {
  local bin="$1" dir f
  dir="$(dirname "$bin")"
  {
    printf '%s  %s\n' "$(sha "$bin")" "$(basename "$bin")"
    for f in "$dir"/abhed-helper-*; do
      [ -e "$f" ] || continue
      printf '%s  %s\n' "$(sha "$f")" "$(basename "$f")"
    done
  } | if command -v sha256sum >/dev/null; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi
}
