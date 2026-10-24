// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  function mount(rail) {
    const mobile = matchMedia('(max-width: 1080px)');
    const make = (tag, cls) => {
      const el = document.createElement(tag);
      el.className = cls;
      return el;
    };
    const bar = make('nav', 'rail-tabs');
    const header = make('div', 'rail-drawer-head');
    const tabs = make('nav', 'rail-tabs');
    const closeButton = make('button', 'btn');
    closeButton.type = 'button';
    closeButton.textContent = 'Close';
    const backdrop = make('div', 'rail-backdrop');
    backdrop.setAttribute('aria-hidden', 'true');
    for (const nav of [bar, tabs]) nav.setAttribute('aria-label', 'Context sections');
    header.append(tabs, closeButton);
    rail.prepend(header);
    rail.parentNode.insertBefore(bar, rail.parentNode.querySelector('main'));
    document.body.append(backdrop);
    let sections = [], selected = null, opener = null, signature = '', inert = [], serial = 0;
    const buttons = () => [...bar.children, ...tabs.children];
    function paint() {
      const open = mobile.matches && selected !== null;
      rail.toggleAttribute('data-drawer-open', open);
      for (const section of sections) section.toggleAttribute('data-rail-selected', section.id === selected);
      for (const button of buttons()) button.setAttribute('aria-expanded', String(open && button.getAttribute('aria-controls') === selected));
    }
    function close(restore = true) {
      selected = null;
      rail.removeAttribute('role');
      rail.removeAttribute('aria-modal');
      for (const [el, was] of inert) el.inert = was;
      inert = [];
      document.body.classList.remove('rail-open');
      paint();
      if (restore && opener?.isConnected) opener.focus({ preventScroll: true });
    }
    function choose(id, button) {
      if (!mobile.matches) return;
      if (selected === id) return close();
      if (selected === null) {
        opener = button;
        // Inert the branches outside the aside, without moving swap targets.
        for (let node = rail; node.parentElement; node = node.parentElement) {
          for (const sibling of node.parentElement.children) {
            if (sibling === node || sibling === backdrop) continue;
            inert.push([sibling, sibling.inert]);
            sibling.inert = true;
          }
          if (node.parentElement === document.body) break;
        }
      }
      selected = id;
      rail.setAttribute('role', 'dialog');
      rail.setAttribute('aria-modal', 'true');
      document.body.classList.add('rail-open');
      paint();
      [...tabs.children].find(b => b.getAttribute('aria-controls') === id)?.focus();
    }
    function refresh() {
      sections = [...rail.children].filter(el => el.tagName === 'SECTION' && el.querySelector('h2'));
      const labels = sections.map(section => {
        if (!section.id) {
          const headingID = section.querySelector('h2').id;
          if (headingID) section.id = headingID + '-panel';
          else do { section.id = 'rail-section-' + serial++; } while (sections.some(other => other !== section && other.id === section.id));
        }
        const heading = section.querySelector('h2').cloneNode(true);
        heading.querySelectorAll('a, kbd, .sr').forEach(el => el.remove());
        return heading.textContent.trim().replace(/\s+/g, ' ');
      });
      const next = JSON.stringify(sections.map((s, i) => [s.id, labels[i]]));
      if (signature !== next) {
        signature = next;
        const focusedTab = tabs.contains(document.activeElement);
        const openerID = opener?.getAttribute('aria-controls');
        for (const nav of [bar, tabs]) {
          nav.replaceChildren();
          sections.forEach((section, i) => {
            const button = make('button', 'btn');
            button.type = 'button';
            button.textContent = labels[i];
            button.setAttribute('aria-controls', section.id);
            button.addEventListener('click', () => choose(section.id, button));
            nav.append(button);
          });
        }
        opener = [...bar.children].find(b => b.getAttribute('aria-controls') === openerID) || bar.children[0];
        if (focusedTab && selected) [...tabs.children].find(b => b.getAttribute('aria-controls') === selected)?.focus();
      }
      if (selected && !sections.some(s => s.id === selected)) close();
      bar.hidden = !sections.length;
      paint();
    }
    closeButton.addEventListener('click', () => close());
    backdrop.addEventListener('click', () => close());
    rail.addEventListener('click', e => {
      // Let the original file link navigate or load its diff unchanged.
      if (selected !== null && e.target.closest('.r-files a[href]')) close();
    });
    document.addEventListener('keydown', e => {
      if (selected === null) return;
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopImmediatePropagation();
        close();
      } else if (e.key === 'Tab') {
        const focusable = [...rail.querySelectorAll('button, a[href], input, select, textarea, [tabindex]')]
          .filter(el => !el.disabled && el.tabIndex >= 0 && el.getClientRects().length);
        const first = focusable[0], last = focusable.at(-1);
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      }
    }, true);
    mobile.addEventListener('change', () => close(false));
    // Observe content, not attributes: selection changes must not trigger rebuilds.
    new MutationObserver(refresh).observe(rail, { childList: true, subtree: true, characterData: true });
    rail.setAttribute('data-drawer', '');
    refresh();
    return { refresh, choose, close };
  }
  // Not ermiteRail: ermite.js owns that name for the keys of the rail.
  window.ermiteRailDrawer = { mount };
  document.querySelectorAll('.work > .rail').forEach(mount);
})();
