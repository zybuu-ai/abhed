// A tool call's arguments are the model's, unescaped: a value can hold a
// right-to-left override, a terminal escape or a zero-width character that
// makes it read as something else. Pass anything shown to a person through
// visible() first. It marks more than the engine's own check does (a joiner
// inside an emoji, say), never less.

const HIDDEN = /[\p{Cc}\p{Cf}\p{Co}\p{Cs}\p{Cn}\p{Zl}\p{Zp}\p{Zs}]/u;

function hidden(ch: string, keepNewline: boolean): boolean {
  if (ch === "\t" || ch === " " || (keepNewline && ch === "\n")) return false;
  return HIDDEN.test(ch);
}

/**
 * `text` with each hidden character shown as `\u{XXXX}`. With `oneLine`, a
 * newline is marked too, so a value cannot start a line that looks like part
 * of the prompt around it.
 */
export function visible(text: string, opts: { oneLine?: boolean } = {}): string {
  let out = "";
  for (const ch of text) {
    out += hidden(ch, !opts.oneLine)
      ? `\\u{${ch.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}}`
      : ch;
  }
  return out;
}

/** Whether any key or string in a call's arguments, at any depth, holds a character visible() would mark. */
export function argsHidden(args: unknown, depth = 0): boolean {
  if (typeof args === "string") {
    for (const ch of args) if (hidden(ch, true)) return true;
    const t = args.trim();
    if (depth < 4 && (t.startsWith("{") || t.startsWith("["))) {
      try {
        return argsHidden(JSON.parse(t), depth + 1);
      } catch {
        return false;
      }
    }
    return false;
  }
  if (Array.isArray(args)) return args.some((v) => argsHidden(v, depth));
  if (args && typeof args === "object") {
    return Object.entries(args).some(([k, v]) => argsHidden(k, depth) || argsHidden(v, depth));
  }
  return false;
}
