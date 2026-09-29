/* Navigation helper for the embedded statistics dashboard.
   It adds exactly one sidebar link and reads no upstream store state. Unknown
   navigation markup is left untouched, so an unrecognized upstream build simply
   keeps its original sidebar and the dashboard stays reachable by URL. */
(function (root) {
  'use strict';
  const entryAttribute = 'data-cpa-stats-entry', href = '/stats.html', label = 'Usage statistics';
  const svgNamespace = 'http://www.w3.org/2000/svg';
  // Axis plus three bars: the upstream nav renders whatever element it is given.
  const iconPaths = ['M3 3v18h18', 'M7 16v4', 'M12 10v10', 'M17 6v14'];
  function icon(document) {
    const svg = document.createElementNS(svgNamespace, 'svg');
    for (const [name, value] of [['width', '16'], ['height', '16'], ['viewBox', '0 0 24 24'], ['fill', 'none'], ['stroke', 'currentColor'], ['stroke-width', '2'], ['stroke-linecap', 'round'], ['stroke-linejoin', 'round'], ['aria-hidden', 'true']]) svg.setAttribute(name, value);
    for (const path of iconPaths) {
      const shape = document.createElementNS(svgNamespace, 'path');
      shape.setAttribute('d', path);
      svg.appendChild(shape);
    }
    return svg;
  }
  function plainLeftClick(event) {
    return !!event && !event.defaultPrevented && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey;
  }
  // The bridge owns the management key. It writes the same tab-scoped entry the
  // upstream "remember for this tab" option uses, and the dashboard reads it once
  // at load; the key value never enters this document's navigation markup.
  function arm(bridge) {
    try {if (bridge && typeof bridge.armStatistics === 'function') bridge.armStatistics();} catch {}
  }
  function navigate(target) {
    const location = root.location;
    if (location && typeof location.assign === 'function') location.assign(target);
  }
  function element(document, bridge, open = navigate) {
    const group = document.createElement('div');
    group.className = 'nav-group';
    group.setAttribute(entryAttribute, '');
    const link = document.createElement('a');
    link.className = 'nav-item';
    link.setAttribute('href', href);
    link.setAttribute('title', label);
    const iconHolder = document.createElement('span');
    iconHolder.className = 'nav-icon';
    iconHolder.appendChild(icon(document));
    const text = document.createElement('span');
    text.className = 'nav-text';
    const caption = document.createElement('span');
    caption.className = 'nav-label';
    caption.textContent = label;
    text.appendChild(caption);
    link.appendChild(iconHolder);
    link.appendChild(text);
    link.addEventListener('click', event => {
      // Modified clicks keep their native behavior: a new tab loads the dashboard
      // without a handed-over key instead of being silently rewritten.
      if (!plainLeftClick(event)) return;
      event.preventDefault();
      arm(bridge);
      open(href);
    });
    group.appendChild(link);
    return group;
  }
  function mount(document, bridge, open) {
    try {
      if (document.querySelector('[' + entryAttribute + ']')) return true;
      const section = document.querySelector('.sidebar .nav-section');
      if (!section || typeof section.appendChild !== 'function') return false;
      section.appendChild(element(document, bridge, open));
      return true;
    } catch {return false;}
  }
  function start(document, bridge, observer) {
    mount(document, bridge);
    if (typeof observer !== 'function' || !document.documentElement) return;
    let scheduled = false;
    // The sidebar is re-rendered by the upstream router (login, logout, mobile
    // drawer). Re-adding the link is idempotent and stops once it is present.
    new observer(() => {
      if (scheduled) return;
      scheduled = true;
      const resume = () => {scheduled = false; mount(document, bridge);};
      if (typeof root.requestAnimationFrame === 'function') root.requestAnimationFrame(resume);
      else root.setTimeout(resume, 250);
    }).observe(document.documentElement, {childList: true, subtree: true});
  }
  const api = {mount, element, plainLeftClick, arm, start, entryAttribute, href, label};
  if (typeof module !== 'undefined' && module.exports) {module.exports = api; return;}
  if (root.CPAStatsNav) return;
  root.CPAStatsNav = api;
  const boot = () => start(document, root.CPAQuotaPersistence, root.MutationObserver);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot, {once: true});
  else boot();
})(typeof window === 'undefined' ? globalThis : window);
