import { build } from "esbuild";
import { copyFileSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { gzipSync } from "node:zlib";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, "..", "..", "server", "ide", "vendor");
mkdirSync(out, { recursive: true });
// The directory holds only what this script writes, so a dropped file never lingers.
for (const f of readdirSync(out)) rmSync(join(out, f));

const common = {
  bundle: true,
  format: "iife",
  minify: true,
  target: "es2022",
  legalComments: "none",
  logLevel: "info",
};

// The editor bundle, its stylesheet and the icon font, all under /ide/vendor/.
await build({
  ...common,
  entryPoints: { editor: join(here, "editor.js") },
  outdir: out,
  loader: { ".ttf": "file" },
  assetNames: "[name]",
});

// One same-origin worker per language service, loaded by the page as a classic script.
const monaco = join(here, "node_modules", "monaco-editor", "esm", "vs");
const workers = {
  "editor.worker": "editor/editor.worker.js",
  "json.worker": "language/json/json.worker.js",
  "css.worker": "language/css/css.worker.js",
  "html.worker": "language/html/html.worker.js",
  "ts.worker": "language/typescript/ts.worker.js",
};
await build({
  ...common,
  entryPoints: Object.fromEntries(Object.entries(workers).map(([k, v]) => [k, join(monaco, v)])),
  outdir: out,
});

const copies = [
  ["@xterm/xterm/lib/xterm.js", "xterm.js"],
  ["@xterm/xterm/css/xterm.css", "xterm.css"],
  ["@xterm/addon-fit/lib/addon-fit.js", "addon-fit.js"],
];
for (const [src, dst] of copies) {
  copyFileSync(join(here, "node_modules", src), join(out, dst));
}

// Only the gzipped files are kept and embedded: the server sends them as they
// are, and unpacks one for a client that does not take gzip.
for (const f of readdirSync(out)) {
  writeFileSync(join(out, f + ".gz"), gzipSync(readFileSync(join(out, f)), { level: 9 }));
  rmSync(join(out, f));
}

// NOTICE lists every installed package, direct or transitive, that esbuild
// can inline into the bundles. Only the build tool and type stubs are left out.
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
const skip = (n) => n === "esbuild" || n.startsWith("@esbuild/") || n.startsWith("@types/");
for (const name of names.filter((n) => !skip(n)).sort()) {
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
  // A package that carries notices for code it vendors has them passed on too.
  try {
    const notices = readFileSync(join(dir, "ThirdPartyNotices.txt"), "utf8");
    parts.push(`${name}: notices for the code it includes`, "", notices.trim(), "");
  } catch {}
}
// The icon font ships inside monaco-editor but carries its own licence.
parts.push("=".repeat(72), "Codicons icon font, codicon.ttf (CC-BY-4.0)", "=".repeat(72), "",
  "Copyright (c) Microsoft Corporation. The Codicons are licensed under the Creative",
  "Commons Attribution 4.0 International licence, https://creativecommons.org/licenses/by/4.0/.",
  "Source: https://github.com/microsoft/vscode-codicons. Shipped unmodified.", "");
writeFileSync(join(out, "NOTICE"), parts.join("\n"));
