// Catalog explorer: renders the platform catalog (embedded in the page as
// JSON by the server) as an interactive cytoscape graph.
//
// The graph follows the event-modeling notation rather than a generic
// box-and-arrow style: events are pastel-orange post-its, read models are
// green, aggregates are neutral rectangles and consumers are outlined
// ellipses (dashed when they are reactors). Those two notation colours are
// deliberately FIXED palette tints rather than theme tokens — an event is
// orange on a whiteboard, in the docs and here, in either colour scheme —
// so they are paired with a pinned dark label colour. Everything that is
// chrome rather than notation (aggregates, consumers, edges, selection)
// uses semantic tokens and follows the active theme.
//
// Blue is reserved for command post-its (M13); white UI/screen cards are
// not modelled until M14. The legend says so.
(function () {
  const container = document.getElementById('explorer');
  const dataEl = document.getElementById('catalog-data');
  if (!container || !dataEl || typeof cytoscape === 'undefined') return;

  const cat = JSON.parse(dataEl.textContent);
  const details = document.getElementById('explorer-details');

  // Read live rather than once: the chrome colours resolve differently in
  // light and dark, and the OS scheme can change mid-session. getComputedStyle
  // returns a live view, but the token VALUES must be re-read after the
  // scheme class flips — hence readColors() rather than a snapshot.
  const readColors = () => {
    const css = getComputedStyle(document.documentElement);
    const tok = (name, fallback) => css.getPropertyValue(name).trim() || fallback;
    return {
      surface: tok('--wa-color-surface-raised', '#ffffff'),
      lowered: tok('--wa-color-surface-lowered', '#f4f4f5'),
      border: tok('--wa-color-surface-border', '#d4d4d8'),
      text: tok('--wa-color-text-normal', '#27272a'),
      quiet: tok('--wa-color-text-quiet', '#71717a'),
      brand: tok('--wa-color-brand-fill-loud', '#4463d8'),
      warning: tok('--wa-color-warning-fill-loud', '#d97706'),
      neutralFill: tok('--wa-color-neutral-fill-quiet', '#e4e4e7'),
      neutralBorder: tok('--wa-color-neutral-border-loud', '#a1a1aa'),
      // notation colours — fixed tints, not theme tokens (see the note above)
      eventFill: tok('--wa-color-orange-90', '#ffdfca'),
      eventBorder: tok('--wa-color-orange-70', '#ff9266'),
      modelFill: tok('--wa-color-green-90', '#c2f2c1'),
      modelBorder: tok('--wa-color-green-70', '#5dc36f'),
      onNote: tok('--wa-color-gray-05', '#101219'),
    };
  };
  const colors = readColors();

  const elements = [];
  const ids = new Set();
  const edgeKeys = new Set();
  const node = (id, label, cls, extra) => {
    if (ids.has(id)) return;
    ids.add(id);
    elements.push({ data: { id, label, ...extra }, classes: cls });
  };
  // One edge per ordered pair: a reactor's flow edge and its trigger edge
  // describe the same hop, so whichever is added first (flows are) wins and
  // the graph does not draw them twice.
  const edge = (source, target, cls) => {
    if (!ids.has(source) || !ids.has(target)) return; // skip dangling edges
    const key = source + '>' + target;
    if (edgeKeys.has(key)) return;
    edgeKeys.add(key);
    elements.push({ data: { id: key, source, target }, classes: cls });
  };

  const eventId = (aggregate, type) => 'evt:' + aggregate + '/' + type;

  // which aggregates empirically produce each event type
  const producers = {};
  (cat.aggregates || []).forEach((a) =>
    (a.events || []).forEach((e) => (producers[e.type] = producers[e.type] || []).push(a.name)),
  );

  // aggregates and the events they have actually emitted
  (cat.aggregates || []).forEach((a) => {
    node('agg:' + a.name, a.name, 'agg', { kind: 'aggregate', origin: a.origin, streams: a.streams });
    (a.events || []).forEach((e) => {
      node(eventId(a.name, e.type), e.type, 'evt', {
        kind: 'event',
        aggregate: a.name,
        count: e.count,
        versions: e.minVersion === e.maxVersion ? 'v' + e.maxVersion : 'v' + e.minVersion + '–v' + e.maxVersion,
      });
      edge('agg:' + a.name, eventId(a.name, e.type), '');
    });
  });

  (cat.consumers || []).forEach((c) =>
    node('cons:' + c.name, c.name, c.kind === 'reactor' ? 'cons reactor' : 'cons', {
      kind: c.kind,
      checkpoint: c.checkpoint,
      triggers: (c.eventTypes || []).join(', '),
      collections: (c.collections || []).join(', '),
    }),
  );

  // reactor flows first, so the dashed styling survives the edge dedupe
  (cat.flows || []).forEach((f) => {
    const [causeAgg, causeType] = (f.cause || '/').split('/');
    const [targetAgg, targetType] = (f.target || '/').split('/');
    edge(eventId(causeAgg, causeType), 'cons:' + f.reactor, 'flow');
    edge('cons:' + f.reactor, eventId(targetAgg, targetType), 'flow');
  });

  (cat.consumers || []).forEach((c) => {
    (c.eventTypes || []).forEach((t) =>
      (producers[t] || []).forEach((agg) => edge(eventId(agg, t), 'cons:' + c.name, '')),
    );
    (c.collections || []).forEach((col) => {
      node('col:' + col, col, 'col', { kind: 'read model (collection)' });
      edge('cons:' + c.name, 'col:' + col, '');
    });
  });

  // Built from a colour set rather than closing over one, so the same
  // definition can be re-applied after a colour-scheme change.
  const buildStyle = (colors) => [
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
      {
        selector: '.agg',
        style: {
          'background-color': colors.neutralFill,
          'border-width': 1,
          'border-color': colors.neutralBorder,
          shape: 'rectangle',
        },
      },
      {
        selector: '.evt',
        style: {
          'background-color': colors.eventFill,
          'border-width': 1.5,
          'border-color': colors.eventBorder,
          color: colors.onNote,
          shape: 'round-rectangle',
          'font-weight': 'bold',
        },
      },
      {
        selector: '.cons',
        style: {
          'background-color': colors.surface,
          shape: 'ellipse',
          'border-width': 2,
          'border-color': colors.neutralBorder,
        },
      },
      { selector: '.reactor', style: { 'border-style': 'dashed', 'border-color': colors.warning } },
      {
        selector: '.col',
        style: {
          'background-color': colors.modelFill,
          'border-width': 1.5,
          'border-color': colors.modelBorder,
          color: colors.onNote,
          shape: 'rectangle',
        },
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
        },
      },
      {
        selector: '.flow',
        style: { 'line-style': 'dashed', 'line-color': colors.warning, 'target-arrow-color': colors.warning },
      },
      {
        selector: ':selected',
        style: {
          'border-width': 3,
          'border-color': colors.brand,
          'line-color': colors.brand,
          'target-arrow-color': colors.brand,
        },
      },
  ];

  const cy = cytoscape({
    container,
    elements,
    style: buildStyle(colors),
    layout: { name: 'breadthfirst', directed: true, spacingFactor: 1.15, padding: 24 },
    wheelSensitivity: 0.3,
  });

  // The page chrome follows the OS colour scheme through CSS; the graph
  // cannot, because cytoscape resolves its colours once into a canvas style.
  // layout.html fires this AFTER flipping the scheme class, so the tokens
  // read here are the new ones. Notation colours are fixed by design and
  // come back unchanged — only the chrome moves.
  document.addEventListener('pcqrs:colorscheme', () => {
    cy.style(buildStyle(readColors()));
  });

  cy.on('tap', 'node', (evt) => {
    const d = evt.target.data();
    const facts = [];
    if (d.origin) facts.push('origin: ' + d.origin);
    if (d.streams !== undefined) facts.push('streams: ' + d.streams);
    if (d.aggregate) facts.push('aggregate: ' + d.aggregate);
    if (d.count !== undefined) facts.push('recorded: ' + d.count);
    if (d.versions) facts.push('versions: ' + d.versions);
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
