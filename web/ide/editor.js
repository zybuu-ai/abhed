import * as monaco from "monaco-editor";

// Workers are same-origin files beside this bundle, so the page's CSP needs
// no blob: or eval. Each language service runs in its own worker.
const workers = {
  json: "json",
  css: "css", scss: "css", less: "css",
  html: "html", handlebars: "html", razor: "html",
  typescript: "ts", javascript: "ts",
};
self.MonacoEnvironment = {
  getWorker(_, label) {
    return new Worker("/ide/vendor/" + (workers[label] || "editor") + ".worker.js", { name: label });
  },
};

// Language servers (go to definition, hover, diagnostics) are not wired yet.
// A provider registered here is attached to each model the workbench opens.
const languageServers = new Map();

window.AbhedEditor = {
  monaco,
  // registerLanguageServer(languageId, {attach(model, editor) -> {dispose()}})
  registerLanguageServer(languageId, provider) { languageServers.set(languageId, provider); },
  // attachLanguageServer returns a disposable, a no-op when none is registered.
  attachLanguageServer(model, editor) {
    const p = languageServers.get(model.getLanguageId());
    return (p && p.attach(model, editor)) || { dispose() {} };
  },
};
