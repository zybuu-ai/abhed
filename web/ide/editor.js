import { basicSetup, EditorView } from "codemirror";
import { EditorState, Compartment } from "@codemirror/state";
import { keymap } from "@codemirror/view";
import { indentWithTab } from "@codemirror/commands";
import { StreamLanguage } from "@codemirror/language";
import { go } from "@codemirror/lang-go";
import { python } from "@codemirror/lang-python";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";
import { markdown } from "@codemirror/lang-markdown";
import { yaml } from "@codemirror/lang-yaml";
import { html } from "@codemirror/lang-html";
import { css } from "@codemirror/lang-css";
import { rust } from "@codemirror/lang-rust";
import { sql } from "@codemirror/lang-sql";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { toml } from "@codemirror/legacy-modes/mode/toml";
import { dockerFile } from "@codemirror/legacy-modes/mode/dockerfile";
import { MergeView, unifiedMergeView, getOriginalDoc, acceptChunk, rejectChunk, getChunks, updateOriginalDoc } from "@codemirror/merge";
import { oneDark } from "@codemirror/theme-one-dark";

const byExtension = {
  go: () => go(),
  py: () => python(),
  js: () => javascript(),
  mjs: () => javascript(),
  cjs: () => javascript(),
  jsx: () => javascript({ jsx: true }),
  ts: () => javascript({ typescript: true }),
  tsx: () => javascript({ typescript: true, jsx: true }),
  json: () => json(),
  md: () => markdown(),
  yaml: () => yaml(),
  yml: () => yaml(),
  html: () => html(),
  htm: () => html(),
  css: () => css(),
  rs: () => rust(),
  sql: () => sql(),
  sh: () => StreamLanguage.define(shell),
  bash: () => StreamLanguage.define(shell),
  zsh: () => StreamLanguage.define(shell),
  toml: () => StreamLanguage.define(toml),
};

const byName = {
  dockerfile: () => StreamLanguage.define(dockerFile),
};

// Picks a language extension from the file name; null when none applies.
function languageFor(path) {
  if (typeof path !== "string") return null;
  const name = path.split("/").pop().toLowerCase();
  const named = byName[name];
  if (named) return named();
  const dot = name.lastIndexOf(".");
  if (dot < 0) return null;
  const make = byExtension[name.slice(dot + 1)];
  return make ? make() : null;
}

window.AbhedEditor = {
  getOriginalDoc, acceptChunk, rejectChunk, getChunks, updateOriginalDoc,
  EditorView,
  EditorState,
  Compartment,
  basicSetup,
  keymap,
  indentWithTab,
  oneDark,
  MergeView,
  unifiedMergeView,
  languageFor,
};
