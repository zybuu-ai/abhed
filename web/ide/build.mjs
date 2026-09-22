import { build } from "esbuild";
import { copyFileSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, "..", "..", "server", "ide", "vendor");
mkdirSync(out, { recursive: true });

await build({
  entryPoints: [join(here, "editor.js")],
  bundle: true,
  format: "iife",
  minify: true,
  target: "es2020",
  legalComments: "none",
  logLevel: "info",
  outfile: join(out, "editor.js"),
});

const copies = [
  ["@xterm/xterm/lib/xterm.js", "xterm.js"],
  ["@xterm/xterm/css/xterm.css", "xterm.css"],
  ["@xterm/addon-fit/lib/addon-fit.js", "addon-fit.js"],
];
for (const [src, dst] of copies) {
  copyFileSync(join(here, "node_modules", src), join(out, dst));
}

// NOTICE lists every installed package, direct or transitive, that esbuild
// can inline into the bundles. Only the build tool itself is left out.
const modules = join(here, "node_modules");
const names = [];
for (const entry of readdirSync(modules)) {
  if (entry.startsWith(".")) continue;
  if (entry.startsWith("@")) {
    for (const sub of readdirSync(join(modules, entry))) names.push(`${entry}/${sub}`);
  } else {
    names.push(entry);
  }
}
const parts = [
  "Third-party packages bundled into the files in this directory.",
  "Each entry gives the package name, the version built, and its license text.",
  "",
];
for (const name of names.filter((n) => n !== "esbuild" && !n.startsWith("@esbuild/")).sort()) {
  const dir = join(modules, name);
  const meta = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  let license;
  for (const f of ["LICENSE", "LICENSE.md", "LICENSE.txt"]) {
    try {
      license = readFileSync(join(dir, f), "utf8");
      break;
    } catch {}
  }
  if (!license) throw new Error(`no LICENSE file for ${name}`);
  parts.push("=".repeat(72), `${name} ${meta.version} (${meta.license})`, "=".repeat(72), "", license.trim(), "");
}
writeFileSync(join(out, "NOTICE"), parts.join("\n"));
