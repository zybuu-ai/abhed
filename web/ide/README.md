# IDE vendor bundles

Source for the editor and terminal bundles that `server/ide.html` loads from `/ide/vendor/`. Rebuild with `npm ci && npm run build`, which bundles `editor.js` (Monaco, with its stylesheet and icon font), builds one worker per language service (`editor`, `json`, `css`, `html`, `ts`), copies the xterm files, gzips them all (only the `.gz` files are kept; the server unpacks one for a client without gzip), and regenerates `NOTICE` into `server/ide/vendor/`. The built files are committed and embedded in the Go binary, so `go build` needs no Node and the IDE loads nothing from the network.

The workers are same-origin files, so the page's content security policy needs no `blob:` or `unsafe-eval`. `editor.js` also holds the seam language servers attach through (`registerLanguageServer`); the client that uses it is in `server/ide.html`.

Monaco carries its own copy of DOMPurify; `build.mjs` swaps it for the `dompurify` package pinned through `overrides` in `package.json`, so a sanitizer fix is a version bump there, and the build fails if the swap no longer applies.
