// Reads a JSON list of reviews ({body, comments: [{body}]}) and parses each
// as CommonMark, failing when any renders HTML (but the review's own marker
// comment at the very start), holds a live @mention in any text or code, or
// lost its closing note as a top-level paragraph.
import { readFileSync } from "node:fs";
import { Parser } from "commonmark";

const [file, marker, note] = process.argv.slice(2);
const reviews = JSON.parse(readFileSync(file, "utf8"));
const parser = new Parser();
const problems = [];

function check(md, where, needNote) {
  const doc = parser.parse(md);
  const walker = doc.walker();
  let ev;
  while ((ev = walker.next())) {
    if (!ev.entering) continue;
    const n = ev.node;
    if (n.type === "html_block" || n.type === "html_inline") {
      const isMarker = n.type === "html_block" && n === doc.firstChild && n.literal.trim() === marker;
      if (!isMarker) problems.push(`${where}: ${n.type} renders: ${JSON.stringify(n.literal).slice(0, 120)}`);
    }
    if (n.literal && /@[A-Za-z0-9]/.test(n.literal) && !(n.type === "html_block" && n === doc.firstChild)) {
      problems.push(`${where}: a live mention in ${n.type}: ${JSON.stringify(n.literal).slice(0, 120)}`);
    }
  }
  if (needNote) {
    let found = false;
    for (let c = doc.firstChild; c; c = c.next) {
      if (c.type !== "paragraph") continue;
      let text = "";
      const w = c.walker();
      let e;
      while ((e = w.next())) if (e.entering && e.node.type === "text") text += e.node.literal;
      if (text === note) found = true;
    }
    if (!found) problems.push(`${where}: the closing note is not a top-level paragraph`);
  }
}

reviews.forEach((r, i) => {
  check(r.body, `review ${i}`, true);
  (r.comments || []).forEach((c, j) => check(c.body, `review ${i} comment ${j}`, false));
});
if (problems.length) {
  console.log(problems.slice(0, 30).join("\n"));
  console.log(`${problems.length} problem(s) in ${reviews.length} review(s)`);
  process.exit(1);
}
console.log(`ok: ${reviews.length} review(s)`);
