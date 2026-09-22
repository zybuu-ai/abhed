# IDE vendor bundles

Source for the editor and terminal bundles that `server/ide.html` loads from `/ide/vendor/`. Rebuild with `npm ci && npm run build`, which bundles `editor.js`, copies the xterm files, and regenerates `NOTICE` into `server/ide/vendor/`. The built files are committed and embedded in the Go binary, so `go build` needs no Node and the IDE loads nothing from the network.
