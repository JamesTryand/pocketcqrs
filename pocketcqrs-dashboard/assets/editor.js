// Function editor: attaches CodeMirror to the plain <textarea> that the
// form actually submits.
//
// The textarea stays the source of truth. CodeMirror is an enhancement over
// it, for two reasons: the page keeps working with scripting off (you get a
// plain textarea and no highlighting), and htmx collects the form's values
// from real form controls — it knows nothing about an editor widget. So
// every change is written straight back to the textarea rather than only on
// native submit, which is the one case htmx never takes.
(function () {
  if (typeof CodeMirror === 'undefined') return;

  const ATTACHED = 'data-cm-attached';

  function attach(root) {
    const area = (root || document).querySelector('#function-source');
    if (!area || area.hasAttribute(ATTACHED)) return;
    area.setAttribute(ATTACHED, 'true');

    const cm = CodeMirror.fromTextArea(area, {
      mode: 'javascript',
      lineNumbers: true,
      lineWrapping: false,
      indentUnit: 2,
      tabSize: 2,
      viewportMargin: Infinity, // the card grows with the file; no inner scrollbar
      extraKeys: {
        // a tab inside a code editor is an indent, not a focus move — but
        // Shift-Tab still leaves, so the form stays keyboard-navigable
        Tab: (editor) => editor.execCommand('indentMore'),
        'Shift-Tab': false,
      },
    });

    // write through on every change: htmx reads the textarea, not the editor
    cm.on('change', () => cm.save());
  }

  // The editor panel is replaced wholesale by htmx after a save, a delete or
  // a dry run, so the textarea in the new panel is a different element and
  // needs its own editor. htmx 4 namespaces its events with colons.
  document.addEventListener('htmx:after:swap', (e) => attach(e.target));
  attach(document);
})();
