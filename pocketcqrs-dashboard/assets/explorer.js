// Catalog explorer: renders the platform catalog (embedded in the page as
// JSON by the server) as an interactive cytoscape graph. Colors come from
// Web Awesome design tokens so the graph follows the active theme.
(function () {
  const container = document.getElementById('explorer');
  const dataEl = document.getElementById('catalog-data');
  if (!container || !dataEl || typeof cytoscape === 'undefined') return;

  const cat = JSON.parse(dataEl.textContent);
  const details = document.getElementById('explorer-details');

  const css = getComputedStyle(document.documentElement);
  const tok = (name, fallback) => css.getPropertyValue(name).trim() || fallback;
  const colors = {
    brand: tok('--wa-color-brand-fill-loud', '#4463d8'),
    onBrand: tok('--wa-color-brand-on-loud', '#ffffff'),
    surface: tok('--wa-color-surface-raised', '#ffffff'),
    lowered: tok('--wa-color-surface-lowered', '#f4f4f5'),
    border: tok('--wa-color-surface-border', '#d4d4d8'),
    text: tok('--wa-color-text-normal', '#27272a'),
    quiet: tok('--wa-color-text-quiet', '#71717a'),
    warning: tok('--wa-color-warning-fill-normal', '#d97706'),
  };

  const elements = [];
  const ids = new Set();
  const node = (id, label, cls, extra) => {
    if (ids.has(id)) return;
    ids.add(id);
    elements.push({ data: { id, label, ...extra }, classes: cls });
  };
  const edge = (source, target, label, cls) => {
    if (!ids.has(source) || !ids.has(target)) return; // skip dangling edges
    elements.push({ data: { id: source + '>' + target + ':' + label, source, target, label }, classes: cls });
  };

  // which aggregates empirically produce each event type
  const producers = {};
  (cat.aggregates || []).forEach((a) =>
    (a.events || []).forEach((e) => (producers[e.type] = producers[e.type] || []).push(a.name)),
  );

  (cat.aggregates || []).forEach((a) =>
    node('agg:' + a.name, a.name, 'agg', { kind: 'aggregate', origin: a.origin, streams: a.streams }),
  );
  (cat.consumers || []).forEach((c) =>
    node('cons:' + c.name, c.name, c.kind === 'reactor' ? 'cons reactor' : 'cons', {
      kind: c.kind,
      checkpoint: c.checkpoint,
      triggers: (c.eventTypes || []).join(', '),
      collections: (c.collections || []).join(', '),
    }),
  );
  (cat.consumers || []).forEach((c) => {
    (c.collections || []).forEach((col) => {
      node('col:' + col, col, 'col', { kind: 'collection (read model)' });
      edge('cons:' + c.name, 'col:' + col, '', '');
    });
    (c.eventTypes || []).forEach((t) =>
      (producers[t] || []).forEach((agg) => edge('agg:' + agg, 'cons:' + c.name, t, '')),
    );
  });
  (cat.flows || []).forEach((f) => {
    const [causeAgg, causeType] = (f.cause || '/').split('/');
    const [targetAgg, targetType] = (f.target || '/').split('/');
    edge('agg:' + causeAgg, 'cons:' + f.reactor, causeType, 'flow');
    edge('cons:' + f.reactor, 'agg:' + targetAgg, targetType, 'flow');
  });

  const cy = cytoscape({
    container,
    elements,
    style: [
      {
        selector: 'node',
        style: {
          label: 'data(label)',
          'font-size': 11,
          'text-valign': 'center',
          'text-halign': 'center',
          'text-wrap': 'wrap',
          'text-max-width': '10em',
          color: colors.text,
          padding: '10px',
        },
      },
      { selector: '.agg', style: { 'background-color': colors.brand, color: colors.onBrand, shape: 'round-rectangle' } },
      {
        selector: '.cons',
        style: { 'background-color': colors.surface, shape: 'ellipse', 'border-width': 2, 'border-color': colors.brand },
      },
      { selector: '.reactor', style: { 'border-style': 'dashed', 'border-color': colors.warning } },
      {
        selector: '.col',
        style: { 'background-color': colors.lowered, shape: 'barrel', 'border-width': 1, 'border-color': colors.border },
      },
      {
        selector: 'edge',
        style: {
          width: 1.5,
          'line-color': colors.border,
          'target-arrow-color': colors.border,
          'target-arrow-shape': 'triangle',
          'arrow-scale': 1.1,
          'curve-style': 'bezier',
          label: 'data(label)',
          'font-size': 8.5,
          color: colors.quiet,
          'text-rotation': 'autorotate',
          'text-margin-y': -6,
        },
      },
      {
        selector: '.flow',
        style: { 'line-style': 'dashed', 'line-color': colors.warning, 'target-arrow-color': colors.warning },
      },
      {
        selector: ':selected',
        style: { 'border-width': 3, 'border-color': colors.brand, 'line-color': colors.brand, 'target-arrow-color': colors.brand },
      },
    ],
    layout: { name: 'breadthfirst', directed: true, spacingFactor: 1.15, padding: 24 },
    wheelSensitivity: 0.3,
  });

  cy.on('tap', 'node', (evt) => {
    const d = evt.target.data();
    const facts = [];
    if (d.origin) facts.push('origin: ' + d.origin);
    if (d.streams !== undefined) facts.push('streams: ' + d.streams);
    if (d.checkpoint !== undefined) facts.push('checkpoint: ' + d.checkpoint);
    if (d.triggers) facts.push('triggers: ' + d.triggers);
    if (d.collections) facts.push('collections: ' + d.collections);

    const frag = document.createDocumentFragment();
    const title = document.createElement('strong');
    title.textContent = d.label + ' — ' + (d.kind || 'node');
    frag.append(title);
    facts.forEach((f) => frag.append(document.createElement('br'), document.createTextNode(f)));
    details.replaceChildren(frag);
  });
  cy.on('tap', (evt) => {
    if (evt.target === cy) details.textContent = 'Select a node for details.';
  });
})();
