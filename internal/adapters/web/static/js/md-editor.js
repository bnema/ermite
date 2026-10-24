// Small progressive Markdown editor. The textarea owns all state; no HTML preview.
window.ermiteMdEditor = (() => {
  const escape = s => s.replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const span = (cls, s) => '<span class="md-' + cls + '">' + escape(s) + '</span>';
  const marker = s => span('marker', s);
  const list = /^([ \t]*)([-+*]|\d+\.)([ \t]+)(\[[ xX]\][ \t]+)?(.*)$/;
  function highlight(text) {
    let fence = false;
    return text.split('\n').map(line => {
      if (/^\s*```/.test(line)) { fence = !fence; return marker(line); }
      if (fence) return span('code', line);
      let prefix = '', cls = '';
      const block = /^(#{1,6} |> |\s*(?:[-+*]|\d+\.) (?:\[[ xX]\] )?)/.exec(line);
      if (block) {
        prefix = marker(block[0]); line = line.slice(block[0].length);
        cls = block[0][0] === '#' ? 'heading' : block[0][0] === '>' ? 'quote' : '';
      }
      // Single left-to-right scan. Delimited runs never scan across lines.
      let out = '';
      let linkMid = line.indexOf(']('), linkEnd = linkMid < 0 ? -1 : line.indexOf(')', linkMid + 2);
      for (let i = 0; i < line.length;) {
        const delim = line.startsWith('**', i) ? '**' : ['*', '`'].includes(line[i]) ? line[i] : null;
        if (delim) {
          const end = line.indexOf(delim, i + delim.length);
          if (end > i + delim.length) {
            out += marker(delim) + span(delim === '**' ? 'bold' : delim === '*' ? 'italic' : 'code', line.slice(i + delim.length, end)) + marker(delim);
            i = end + delim.length; continue;
          }
        }
        if (line[i] === '[') {
          if (linkMid <= i && linkMid >= 0) { linkMid = line.indexOf('](', i + 1); linkEnd = linkMid < 0 ? -1 : line.indexOf(')', linkMid + 2); }
          const end = linkEnd;
          if (end >= 0) { out += span('link', line.slice(i, end + 1)); i = end + 1; continue; }
        }
        out += escape(line[i++]);
      }
      return prefix + (cls ? '<span class="md-' + cls + '">' + out + '</span>' : out);
    }).join('\n');
  }
  const formats = {bold: ['**','**'], italic: ['*','*'], code: ['`','`'], fence: ['```\n','\n```'], link: ['[','](url)']};
  const prefixes = {heading: '## ', quote: '> ', bullet: '- ', number: '1. ', task: '- [ ] '};
  const blockPatterns = {heading: /^#{1,6} /, quote: /^> /, bullet: /^[-+*] /, number: /^\d+\. /, task: /^- \[[ xX]\] /};
  function format(value, start, end, kind) {
    if (formats[kind]) {
      const [a,b] = formats[kind];
      if (value.slice(start, end).startsWith(a) && value.slice(start, end).endsWith(b) && end-start >= a.length+b.length) {
        return {start, end, text: value.slice(start+a.length,end-b.length), select: [start,end-a.length-b.length]};
      }
      if (value.slice(Math.max(0,start-a.length),start) === a && value.slice(end,end+b.length) === b) {
        return {start:start-a.length,end:end+b.length,text:value.slice(start,end),select:[start-a.length,end-a.length]};
      }
      return {start,end,text:a+value.slice(start,end)+b,select:[start+a.length,end+a.length]};
    }
    start = start === 0 ? 0 : value.lastIndexOf('\n', start-1)+1;
    const next = value.indexOf('\n', end > start && value[end-1] === '\n' ? end-1 : end);
    end = next < 0 ? value.length : next;
    const lines = value.slice(start,end).split('\n'), p = prefixes[kind];
    const pattern = blockPatterns[kind];
    const remove = lines.every(l => pattern.test(l));
    const text = lines.map((l,i) => remove ? l.replace(pattern,'') : (kind === 'number' ? (i+1)+'. ' : p)+l).join('\n');
    return {start,end,text,select:[start,start+text.length]};
  }
  function continuation(value, start, end = start) {
    if (start !== end) return null;
    const beginning = start === 0 ? 0 : value.lastIndexOf('\n',start-1)+1;
    const m = list.exec(value.slice(beginning,start));
    if (!m) return null;
    if (!m[5].trim()) return {start:beginning,end:start,text:''};
    const bullet = /^\d/.test(m[2]) ? (Number.parseInt(m[2],10)+1)+'.' : m[2];
    return {start,end:start,text:'\n'+m[1]+bullet+' '+(m[4] ? '[ ] ' : '')};
  }
  function apply(t, change) {
    t.focus(); t.setSelectionRange(change.start,change.end);
    // insertText preserves the native undo stack where supported.
    let inserted = false;
    try { inserted = document.execCommand('insertText', false, change.text); } catch (_) { /* fallback below */ }
    if (!inserted) t.setRangeText(change.text,change.start,change.end,'end');
    // Chromium leaves the caret before a trailing newline after an empty insert.
    const end = change.start + change.text.length;
    t.setSelectionRange(...(change.select || [end, end]));
    t.dispatchEvent(new Event('input', {bubbles:true}));
  }
  function init(root = document) {
    const areas = [...root.querySelectorAll('textarea[data-md]')];
    if (root.matches?.('textarea[data-md]')) areas.unshift(root);
    areas.forEach(t => {
      if (t.closest('.md-editor')) return;
      const box = document.createElement('div'); box.className = 'md-editor';
      const bar = document.createElement('div'); bar.className = 'md-toolbar'; bar.setAttribute('role','group'); bar.setAttribute('aria-label','Markdown formatting');
      const surface = document.createElement('div'); surface.className = 'md-surface';
      const back = document.createElement('pre'); back.className = 'md-backdrop'; back.setAttribute('aria-hidden','true');
      t.before(box); box.append(bar,surface); surface.append(back,t);
      // Groups of buttons, separated by a thin rule; the icon is a CSS mask.
      const groups = [
        {heading:'Heading',bold:'Bold (Ctrl+B)',italic:'Italic (Ctrl+I)'},
        {code:'Code',fence:'Code block',link:'Link (Ctrl+K)',quote:'Quote'},
        {bullet:'Bulleted list',number:'Numbered list',task:'Task list'},
      ];
      const act = kind => apply(t,format(t.value,t.selectionStart,t.selectionEnd,kind));
      groups.forEach((group,i) => {
        if (i) { const sep = document.createElement('span'); sep.className = 'md-sep'; bar.append(sep); }
        Object.entries(group).forEach(([kind,label]) => {
          const b = document.createElement('button'); b.type = 'button'; b.className = 'md-i-'+kind; b.title = label; b.setAttribute('aria-label',label);
          // Keep the textarea's selection: a click must not move the focus first.
          b.addEventListener('mousedown',e => e.preventDefault());
          b.addEventListener('click',() => act(kind)); bar.append(b);
        });
      });
      const sync = () => { back.scrollTop = t.scrollTop; back.scrollLeft = t.scrollLeft; };
      let pending = false;
      const paint = () => { pending = false; back.innerHTML = highlight(t.value)+'\n'; sync(); };
      const schedule = () => { if (!pending) { pending = true; requestAnimationFrame(paint); } };
      t.addEventListener('input',schedule); t.addEventListener('scroll',sync);
      t.addEventListener('keydown',e => {
        if (e.isComposing) return;
        const shortcut = {b:'bold',i:'italic',k:'link'}[e.key.toLowerCase()];
        if ((e.ctrlKey || e.metaKey) && !e.altKey && shortcut) { e.preventDefault(); e.stopPropagation(); act(shortcut); }
        else if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
          const change = continuation(t.value,t.selectionStart,t.selectionEnd);
          if (change) { e.preventDefault(); apply(t,change); }
        }
      });
      // CSS grid sizes both layers together, including native textarea resizing.
      if (typeof ResizeObserver !== 'undefined') {
        const observer = new ResizeObserver(() => { if (!t.isConnected) observer.disconnect(); else sync(); });
        observer.observe(t);
      }
      paint();
    });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded',() => init());
  else init();
  document.addEventListener('htmx:load',e => init(e.target));
  // MR live refreshes also replace DOM without an htmx load event.
  if (typeof MutationObserver !== 'undefined') new MutationObserver(records => {
    for (const r of records) for (const node of r.addedNodes) {
      if (node.nodeType === 1 && !node.closest('.md-editor')) init(node);
    }
  }).observe(document.body, {childList:true,subtree:true});
  return {core:{highlight,format,continuation},init};
})();
