// Marking characters that would not print as themselves.
//
// A tool call's arguments are the model's, unescaped: a value can hold a
// right-to-left override, a terminal escape, a zero-width character, or a run
// of blanks that pushes the rest of a line out of view. visible() shows each
// as a marked escape (⟨U+202E⟩, ⟨\e⟩, ⟨80 spaces⟩) and drops nothing. It is a
// port of the engine's own escaper (internal/visible), checked against the
// engine's output for a shared set of cases (sdk/testdata/visible.json); the
// one difference is that a lone surrogate is shown as ⟨U+D800⟩ where the
// engine would see invalid UTF-8. Character classes come from Node's ICU,
// so a format character newer than its Unicode version goes unmarked where
// the engine marks it. An approval's args_hidden is the engine's own verdict
// and the authoritative one.

const CF = /^\p{Cf}$/u;
const ZS = /^\p{Zs}$/u;
const SO_SM = /^[\p{So}\p{Sm}]$/u;

function hidden(r: number): boolean {
  if (r < 0x20 || r === 0x7f || (r >= 0x80 && r < 0xa0)) return true;
  if (r === 0x2028 || r === 0x2029) return true;
  if (r >= 0xd800 && r <= 0xdfff) return true;
  return CF.test(String.fromCodePoint(r));
}

function pictograph(r: number): boolean {
  return (
    (r >= 0x1f000 && r <= 0x1faff) ||
    (r >= 0x2600 && r <= 0x27bf) ||
    (r >= 0x2300 && r <= 0x23ff) ||
    (r >= 0x2b00 && r <= 0x2bff) ||
    [0x00a9, 0x00ae, 0x203c, 0x2049, 0x2122, 0x2139].includes(r)
  );
}

function keepJoiner(rs: number[], i: number): boolean {
  if (i === 0 || i + 1 >= rs.length) return false;
  let left = rs[i - 1]!;
  for (let j = i - 1; j > 0 && (left === 0xfe0f || (left >= 0x1f3fb && left <= 0x1f3ff)); j--) left = rs[j - 1]!;
  return pictograph(left) && pictograph(rs[i + 1]!);
}

function emojiBase(r: number): boolean {
  return SO_SM.test(String.fromCodePoint(r)) || (r >= 0x30 && r <= 0x39) || r === 0x23 || r === 0x2a ||
    r === 0x203c || r === 0x2049 || r === 0x2139;
}

function drawsNothing(r: number, prev: number): boolean {
  if ([0x034f, 0x115f, 0x1160, 0x2800, 0x3164, 0xffa0].includes(r)) return true;
  if (r === 0xfe0f) return !emojiBase(prev);
  return false;
}

const otherSpace = (r: number): boolean => r !== 0x20 && ZS.test(String.fromCodePoint(r));
const plural = (n: number, one: string, many: string): string => (n === 1 ? `1 ${one}` : `${n} ${many}`);

function blankRun(rs: number[], i: number): [number, string] {
  let j = i, sp = 0, tb = 0;
  for (; j < rs.length && (rs[j] === 0x20 || rs[j] === 0x09); j++) {
    if (rs[j] === 0x20) sp++;
    else tb++;
  }
  const limit = i > 0 && rs[i - 1] === 0x0a ? 32 : 8;
  if (sp + tb < 2 || sp + 8 * tb < limit) return [j, ""];
  const count = tb === 0 ? plural(sp, "space", "spaces") : sp === 0 ? plural(tb, "tab", "tabs")
    : `${plural(sp, "space", "spaces")}, ${plural(tb, "tab", "tabs")}`;
  return [j, `⟨${count}⟩`];
}

const NAMED: Record<number, string> = { 0x0d: "⟨\\r⟩", 0x08: "⟨\\b⟩", 0x07: "⟨\\a⟩", 0x1b: "⟨\\e⟩", 0x00: "⟨\\0⟩" };

function codePoints(s: string): number[] {
  const out: number[] = [];
  for (const ch of s) out.push(ch.codePointAt(0)!);
  return out;
}

/**
 * `text` as a person should see it before approving it: every character that
 * would not print as itself as a marked escape, and a run of eight columns of
 * blanks or more (32 at the start of a line) as a count. With `oneLine`, a
 * newline is shown too, so a value cannot start a line that looks like part of
 * the prompt around it.
 */
export function visible(text: string, opts: { oneLine?: boolean } = {}): string {
  const rs = codePoints(text);
  let out = "";
  for (let i = 0; i < rs.length; i++) {
    const r = rs[i]!;
    if (r === 0x20 || r === 0x09) {
      const [end, mark] = blankRun(rs, i);
      out += mark || String.fromCodePoint(...rs.slice(i, end)).replaceAll("\t", "    ");
      i = end - 1;
      continue;
    }
    const prev = i > 0 ? rs[i - 1]! : 0;
    if (r === 0x0a) out += opts.oneLine ? "⟨\\n⟩" : "\n";
    else if (r === 0x200d && keepJoiner(rs, i)) out += String.fromCodePoint(r);
    else if (NAMED[r] !== undefined) out += NAMED[r];
    else if (hidden(r) || r === 0x200d || drawsNothing(r, prev) || otherSpace(r))
      out += `⟨U+${r.toString(16).toUpperCase().padStart(4, "0")}⟩`;
    else out += String.fromCodePoint(r);
  }
  return out;
}

/** Whether visible() would change `text` beyond widening its tabs. */
export function hasHidden(text: string): boolean {
  return visible(text) !== text.replaceAll("\t", "    ");
}

/**
 * Whether any key or string in a call's arguments, at any depth, holds
 * something visible() would mark, as the engine's args_hidden decides it. A
 * string that is itself JSON is read too.
 */
export function argsHidden(args: unknown, depth = 0): boolean {
  if (typeof args === "string") {
    if (hasHidden(args)) return true;
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
    return Object.entries(args).some(([k, v]) => hasHidden(k) || argsHidden(v, depth));
  }
  return false;
}
