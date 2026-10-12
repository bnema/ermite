// log.js: the live log of a running job (page /<repo>/actions/runs/<id>/job/<id>).
// The lines arrive as "ci-log" events (internal/adapters/web/actions.go,
// logStream), each one the <li> of the lines of a batch, and are appended to the
// list. The browser reconnects by itself with Last-Event-ID, the number of the
// next line; "ci-end" closes the feed when the job is over. The page follows the
// log (scrolls to its end) only while the reader is at the end already. A piece
// that continues a line begun in an earlier batch (data-cont) is joined to the
// last line of the list.
window.ermiteLog = (() => {
  const nearEnd = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;

  // follow starts the feed of the box (#job-log) and returns the EventSource,
  // or null when the box is not a running job's.
  function follow(box) {
    if (!box || !box.hasAttribute('data-follow') || !box.dataset.stream || !window.EventSource) return null;
    const list = box.querySelector('ol.log');
    if (!list) return null;
    const es = new window.EventSource(box.dataset.stream + '?from=' + encodeURIComponent(box.dataset.next || '0'));
    es.addEventListener('ci-log', (e) => {
      const t = document.createElement('template');
      t.innerHTML = e.data;
      const atEnd = nearEnd();
      for (const line of [...t.content.children]) {
        const last = list.children[list.children.length - 1];
        if (line.hasAttribute('data-cont') && last) {
          const text = line.querySelector('.t');
          const into = last.querySelector('.t');
          if (text && into) into.textContent += text.textContent;
        } else {
          list.append(line);
        }
      }
      if (atEnd) window.scrollTo(0, document.body.scrollHeight);
    });
    es.addEventListener('ci-end', () => es.close());
    es.onerror = () => { if (es.readyState === window.EventSource.CLOSED) es.close(); };
    return es;
  }

  return { follow };
})();

document.addEventListener('DOMContentLoaded', () => window.ermiteLog.follow(document.getElementById('job-log')));
