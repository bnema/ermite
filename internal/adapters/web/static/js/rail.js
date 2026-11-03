// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  const narrow = matchMedia('(max-width: 1080px)');
  const phone = matchMedia('(max-width: 720px)');
  // the wide-screen choice is shared with store.js, which sets rail-hidden on
  // <html> before the first paint so a collapsed panel never flashes open
  const store = window.ermiteStore;
  let wideOpen = store?.get('ermite.rail.hidden') !== '1';
  let rail = null, open = narrow.matches ? false : wideOpen, inert = [], original = null;
  const make = (tag, cls) => {
    const el = document.createElement(tag);
    el.className = cls;
    return el;
  };
  const trigger = make('button', 'btn icon rail-context');
  trigger.type = 'button';
  trigger.setAttribute('aria-label', 'Toggle side panel');
  trigger.title = 'side panel';
  const icon = make('span', 'ico i-panel-right-open');
  icon.setAttribute('aria-hidden', 'true');
  trigger.append(icon);
  const controls = make('div', 'rail-controls');
  const closeButton = make('button', 'btn icon rail-close');
  closeButton.type = 'button';
  // an id lets a live refresh put the focus back on it (refreshPage in mr.js)
  closeButton.id = 'rail-close';
  closeButton.textContent = '×';
  closeButton.setAttribute('aria-label', 'Close side panel');
  controls.append(closeButton);
  const backdrop = make('div', 'rail-backdrop');
  backdrop.setAttribute('aria-hidden', 'true');
  backdrop.addEventListener('click', () => toggle(false));

  function release() {
    for (const [el, was] of inert) el.inert = was;
    inert = [];
    if (rail && original) {
      for (const [attr, value] of Object.entries(original)) {
        if (value == null) rail.removeAttribute(attr);
        else rail.setAttribute(attr, value);
      }
      rail.removeAttribute('aria-modal');
    }
  }
  function paint() {
    release();
    const modal = open && narrow.matches && phone.matches;
    document.body.classList.toggle('rail-open', open && !!rail);
    document.documentElement.classList.toggle('rail-hidden', !wideOpen);
    document.body.classList.toggle('rail-modal', modal);
    trigger.hidden = !rail;
    trigger.setAttribute('aria-expanded', String(open && !!rail));
    if (!rail) return;
    if (modal) {
      rail.setAttribute('role', 'dialog');
      rail.setAttribute('aria-modal', 'true');
      for (let node = rail; node.parentElement; node = node.parentElement) {
        for (const sibling of node.parentElement.children) {
          if (sibling === node || sibling === backdrop) continue;
          inert.push([sibling, sibling.inert]);
          sibling.inert = true;
        }
        if (node.parentElement === document.body) break;
      }
    }
  }
  // set shows or hides the panel for now. Only toggle (the user's own choice)
  // changes the remembered wide-screen state.
  function set(value, focus = true) {
    const was = open, inside = rail?.contains(document.activeElement);
    open = !!(value && rail);
    paint();
    // the slide-in plays when the phone sheet opens, not on a live refresh
    if (open && !was) rail?.classList.add('rail-enter');
    if (!focus) return;
    // a non-modal column keeps the focus on its toggle; the phone sheet is modal
    if (open && phone.matches) closeButton.focus({ preventScroll: true });
    else if (!open && inside) trigger.focus({ preventScroll: true });
  }
  function toggle(value, focus = true) {
    if (!narrow.matches) {
      wideOpen = !!value;
      store?.set('ermite.rail.hidden', value ? '0' : '1');
    }
    set(value, focus);
  }
  // State belongs to this controller, not the aside: live refresh can replace
  // the entire rail. Reattach controls without closing or stealing focus.
  function refresh() {
    const next = document.querySelector('.work > .rail');
    // a region swap removes the old rail before it inserts the new one: keep
    // the current state until the new rail is there
    if (next === rail || (!next && rail && !rail.isConnected && document.querySelector('.work.has-rail'))) return;
    release();
    rail = next;
    if (rail) {
      original = { role: rail.getAttribute('role'), 'aria-label': rail.getAttribute('aria-label') };
      rail.id ||= 'context-rail';
      rail.tabIndex = -1;
      trigger.setAttribute('aria-controls', rail.id);
      closeButton.setAttribute('aria-controls', rail.id);
      rail.prepend(controls);
      rail.addEventListener('animationend', () => rail.classList.remove('rail-enter'));
      if (!backdrop.parentElement) document.body.append(backdrop);
      // the tabs row stays on screen when the page scrolls: the toggle lives
      // at its right end, else at the end of the top row
      (document.querySelector('.hdr .tabs') ?? document.querySelector('.hdr-acts'))?.append(trigger);
    }
    paint();
  }
  trigger.addEventListener('click', () => toggle(!open));
  closeButton.addEventListener('click', () => toggle(false));
  // The phone sheet is modal: it owns Escape and Tab, in capture.
  document.addEventListener('keydown', e => {
    if (!open || !phone.matches || e.isComposing || document.querySelector('dialog[open]')) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopImmediatePropagation();
      toggle(false);
    } else if (e.key === 'Tab') {
      const items = [...rail.querySelectorAll('button, a[href], input, select, textarea, summary')]
        .filter(el => !el.disabled && !el.closest('[hidden]') && el.getClientRects().length);
      const first = items[0], last = items.at(-1);
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); }
    }
  }, true);
  // Wider, the column does not own Escape: it collapses only from its toggle
  // or its own controls, and after a menu or a field had its say.
  document.addEventListener('keydown', e => {
    if (!open || phone.matches || e.isComposing || e.key !== 'Escape' || e.defaultPrevented || document.querySelector('dialog[open]')) return;
    const focus = document.activeElement;
    if (focus !== trigger && !rail?.contains(focus)) return;
    if (focus?.closest?.('details.menu[open]')) return;
    e.preventDefault();
    toggle(false);
  });
  narrow.addEventListener('change', () => {
    const inside = rail?.contains(document.activeElement);
    open = narrow.matches ? false : wideOpen;
    paint();
    if (inside && !open) trigger.focus({ preventScroll: true });
  });
  phone.addEventListener('change', () => {
    paint();
    if (open && phone.matches) closeButton.focus({ preventScroll: true });
  });
  // For the keys (l / h) and dialogs: open shows the panel for now; close
  // dismisses it only where it covers or squeezes the content (narrow).
  window.ermiteRailPanel = {
    open: (focus = true) => set(true, focus),
    close: (focus = true) => { if (narrow.matches) set(false, focus); },
    refresh,
  };
  new MutationObserver(refresh).observe(document.body, { childList: true, subtree: true });
  refresh();
})();
