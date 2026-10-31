// The file manager's code editor (CodeMirror 6). It is loaded lazily, in a
// chunk of its own, the first time a file is opened; languages load lazily
// too, one chunk each.
//
// The editor lives in a shadow root: in a document CodeMirror (style-mod)
// injects its styles as a <style> element, which the panel's CSP refuses,
// while in a shadow root it uses constructed style sheets, which CSP allows.
import { useEffect, useRef } from "react";
import { Compartment, EditorState, type Extension } from "@codemirror/state";
import {
  EditorView,
  drawSelection,
  highlightActiveLine,
  highlightActiveLineGutter,
  highlightSpecialChars,
  keymap,
  lineNumbers,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { HighlightStyle, StreamLanguage, bracketMatching, indentOnInput, syntaxHighlighting, type StreamParser } from "@codemirror/language";
import { highlightSelectionMatches, search, searchKeymap } from "@codemirror/search";
import { tags as t } from "@lezer/highlight";

const legacy = (p: Promise<StreamParser<unknown>>) => p.then((m) => StreamLanguage.define(m));

// language picks the highlighting for a file name, or none.
function language(name: string): Promise<Extension> | null {
  const lower = name.toLowerCase();
  const ext = lower.includes(".") ? lower.slice(lower.lastIndexOf(".") + 1) : "";
  switch (ext) {
    case "php":
      return import("@codemirror/lang-php").then((m) => m.php());
    case "html":
    case "htm":
    case "vue":
    case "svelte":
      return import("@codemirror/lang-html").then((m) => m.html());
    case "js":
    case "mjs":
    case "cjs":
    case "jsx":
    case "ts":
    case "tsx":
      return import("@codemirror/lang-javascript").then((m) =>
        m.javascript({ jsx: ext.endsWith("x"), typescript: ext.startsWith("t") }),
      );
    case "css":
      return import("@codemirror/lang-css").then((m) => m.css());
    case "json":
    case "map":
    case "webmanifest":
      return import("@codemirror/lang-json").then((m) => m.json());
    case "xml":
    case "svg":
      return import("@codemirror/lang-xml").then((m) => m.xml());
    case "yml":
    case "yaml":
      return import("@codemirror/lang-yaml").then((m) => m.yaml());
    case "sql":
      return import("@codemirror/lang-sql").then((m) => m.sql());
    case "md":
    case "markdown":
      return import("@codemirror/lang-markdown").then((m) => m.markdown());
    case "sh":
      return legacy(import("@codemirror/legacy-modes/mode/shell").then((m) => m.shell));
    case "py":
      return legacy(import("@codemirror/legacy-modes/mode/python").then((m) => m.python));
    case "rb":
      return legacy(import("@codemirror/legacy-modes/mode/ruby").then((m) => m.ruby));
    case "go":
      return legacy(import("@codemirror/legacy-modes/mode/go").then((m) => m.go));
    case "toml":
      return legacy(import("@codemirror/legacy-modes/mode/toml").then((m) => m.toml));
    case "ini":
    case "conf":
    case "env":
    case "htaccess":
      return legacy(import("@codemirror/legacy-modes/mode/properties").then((m) => m.properties));
  }
  if (lower === "composer.lock") return import("@codemirror/lang-json").then((m) => m.json());
  return null;
}

// Colors come from the panel's CSS variables, so they follow its theme.
const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.controlKeyword, t.moduleKeyword, t.operatorKeyword, t.definitionKeyword], color: "var(--hl-keyword)" },
  { tag: [t.string, t.special(t.string), t.regexp, t.character], color: "var(--hl-string)" },
  { tag: [t.number, t.bool, t.null, t.atom, t.unit], color: "var(--hl-number)" },
  { tag: [t.comment, t.meta, t.processingInstruction], color: "var(--hl-comment)", fontStyle: "italic" },
  { tag: [t.typeName, t.className, t.tagName, t.namespace], color: "var(--hl-type)" },
  { tag: [t.propertyName, t.attributeName, t.labelName], color: "var(--hl-property)" },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.definition(t.variableName), t.macroName], color: "var(--hl-function)" },
  { tag: [t.special(t.variableName), t.self, t.variableName], color: "var(--hl-variable)" },
  { tag: t.heading, color: "var(--hl-keyword)", fontWeight: "600" },
  { tag: t.strong, fontWeight: "600" },
  { tag: t.emphasis, fontStyle: "italic" },
  { tag: [t.link, t.url], color: "var(--accent)", textDecoration: "underline" },
  { tag: t.invalid, color: "var(--err)" },
]);

const theme = EditorView.theme({
  "&": { height: "100%", color: "var(--text)", backgroundColor: "var(--surface)", fontSize: "13px" },
  "&.cm-focused": { outline: "none" },
  ".cm-scroller": { fontFamily: "var(--mono)", lineHeight: "1.55" },
  ".cm-content": { caretColor: "var(--text)", padding: "10px 0" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--text)" },
  ".cm-gutters": { backgroundColor: "var(--surface-2)", color: "var(--text-3)", borderRight: "1px solid var(--border)" },
  ".cm-activeLine": { backgroundColor: "color-mix(in srgb, var(--accent-soft) 45%, transparent)" },
  ".cm-activeLineGutter": { backgroundColor: "var(--accent-soft)", color: "var(--text-2)" },
  "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground": {
    backgroundColor: "color-mix(in srgb, var(--accent) 28%, transparent)",
  },
  ".cm-selectionMatch": { backgroundColor: "color-mix(in srgb, var(--warn) 22%, transparent)" },
  "&.cm-focused .cm-matchingBracket": { backgroundColor: "color-mix(in srgb, var(--ok) 25%, transparent)" },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, var(--warn) 30%, transparent)" },
  ".cm-searchMatch-selected": { backgroundColor: "color-mix(in srgb, var(--warn) 55%, transparent)" },
  ".cm-panels": { backgroundColor: "var(--surface-2)", color: "var(--text)" },
  ".cm-panels.cm-panels-bottom": { borderTop: "1px solid var(--border)" },
  ".cm-textfield, .cm-button": { fontFamily: "var(--sans)", fontSize: "12px", color: "var(--text)" },
  ".cm-textfield": { backgroundColor: "var(--surface)", border: "1px solid var(--border)", borderRadius: "6px" },
  ".cm-button": { backgroundImage: "none", backgroundColor: "var(--surface)", border: "1px solid var(--border)", borderRadius: "6px" },
});

export default function CodeEditor({
  name,
  initial,
  readOnly,
  onChange,
  onSave,
}: {
  name: string;
  initial: string;
  readOnly: boolean;
  onChange: (content: string) => void;
  onSave: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  // The editor is created once; callbacks are read through refs.
  const cb = useRef({ onChange, onSave });
  cb.current = { onChange, onSave };

  useEffect(() => {
    const el = host.current!;
    const root = el.shadowRoot ?? el.attachShadow({ mode: "open" });
    const lang = new Compartment();
    const view = new EditorView({
      root,
      parent: root,
      state: EditorState.create({
        doc: initial,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightSpecialChars(),
          history(),
          drawSelection(),
          indentOnInput(),
          bracketMatching(),
          highlightActiveLine(),
          highlightSelectionMatches(),
          search({ top: false }),
          syntaxHighlighting(highlight),
          theme,
          lang.of([]),
          EditorState.tabSize.of(4),
          EditorState.readOnly.of(readOnly),
          EditorView.editable.of(!readOnly),
          keymap.of([
            { key: "Mod-s", preventDefault: true, run: () => (cb.current.onSave(), true) },
            ...defaultKeymap,
            ...historyKeymap,
            ...searchKeymap,
            indentWithTab,
          ]),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) cb.current.onChange(u.state.doc.toString());
          }),
        ],
      }),
    });
    view.focus();
    let live = true;
    language(name)
      ?.then((ext) => live && view.dispatch({ effects: lang.reconfigure(ext) }))
      .catch(() => {}); // a missing chunk leaves plain text
    return () => {
      live = false;
      view.destroy();
    };
  }, []); // the document is set once, when the editor opens

  return <div ref={host} className="code-editor" />;
}
