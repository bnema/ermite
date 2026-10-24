// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  function mount(rail) {
    const mobile = matchMedia('(max-width: 1080px)');
    const make = (tag, cls) => {
      const el = document.createElement(tag);
      el.className = cls;
      return el;
    };
    // Keep live swap targets in place; only the controls belong to the drawer.
    const bar = make('nav', 'rail-tabs');
    bar.setAttribute('aria-label', 'Panel sections');
    // "Panel" and a side-panel icon: the button opens what the side panel
    // shows on a wide screen.
    const trigger = make('button', 'btn rail-context');
    trigger.type = 'button';
    const icon = make('span', 'ico i-panel-right-open');
    icon.setAttribute('aria-hidden', 'true');
    trigger.append(icon, document.createTextNode('Panel'));
    const header = document.querySelector('.hdr');
    header.append(trigger);
    const controls = make('div', 'rail-controls');
    const closeButton = make('button', 'btn rail-close');
    closeButton.type = 'button';
    closeButton.textContent = '×';
    closeButton.setAttribute('aria-label', 'Close');
    const backdrop = make('div', 'rail-backdrop');
    backdrop.setAttribute('aria-hidden', 'true');
    controls.append(bar, closeButton);
    rail.prepend(controls);
    document.body.append(backdrop);
    let sections = [], selected = null, opener = null, signature = '', inert = [], serial = 0;
    const buttons = () => [...bar.children];
    function paint() {
      const open = mobile.matches && selected !== null;
      rail.toggleAttribute('data-drawer-open', open);
      for (const section of sections) section.toggleAttribute('data-rail-selected', section.id === selected);
      trigger.setAttribute('aria-expanded', String(open));
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
        opener = trigger;
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
      // All section controls stay inside the modal drawer.
      rail.setAttribute('role', 'dialog');
      rail.setAttribute('aria-modal', 'true');
      rail.setAttribute('aria-label', button?.textContent || 'Panel');
      document.body.classList.add('rail-open');
      paint();
      buttons().find(b => b.getAttribute('aria-controls') === selected)?.focus();
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
        const focusedTab = bar.contains(document.activeElement);
        bar.replaceChildren();
        sections.forEach((section, i) => {
          const button = make('button', 'btn');
          button.type = 'button';
          button.textContent = labels[i];
          button.setAttribute('aria-controls', section.id);
          button.addEventListener('click', () => choose(section.id, button));
          bar.append(button);
        });
        if (focusedTab && selected) [...bar.children].find(b => b.getAttribute('aria-controls') === selected)?.focus();
      }
      if (selected && !sections.some(s => s.id === selected)) close();
      trigger.hidden = !sections.length;
      paint();
    }
    trigger.addEventListener('click', () => {
      if (sections.length) choose(sections[0].id, buttons()[0]);
    });
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
