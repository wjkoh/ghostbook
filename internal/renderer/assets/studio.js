const ACTIVE_SLUG = document.body.getAttribute('data-active-slug');

function resolveSystemTheme() {
  if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) {
    return 'dark';
  }
  return 'light';
}

function setReaderTheme(themePref) {
  const pref = themePref || 'system';
  localStorage.setItem('ghostbook_theme', pref);
  const resolved = (pref === 'system') ? resolveSystemTheme() : pref;
  document.documentElement.setAttribute('data-theme', resolved);
  const sel = document.getElementById('themeSelect');
  if (sel) sel.value = pref;
}

function closeMobileSidebar() {
  const sb = document.getElementById('bookSidebar');
  const bd = document.getElementById('sidebarBackdrop');
  if (sb) sb.classList.remove('mobile-open');
  if (bd) bd.classList.remove('open');
}

function toggleSidebar() {
  const sb = document.getElementById('bookSidebar');
  const bd = document.getElementById('sidebarBackdrop');
  if (!sb) return;
  if (window.innerWidth <= 900) {
    const isOpen = sb.classList.toggle('mobile-open');
    if (bd) bd.classList.toggle('open', isOpen);
  } else {
    sb.classList.toggle('collapsed');
  }
}

function toggleTocPart(headerEl) {
  if (!headerEl) return;
  const partTitle = headerEl.getAttribute('data-part-header') || '';
  const isCollapsed = headerEl.classList.toggle('collapsed');
  document.querySelectorAll('.toc-item').forEach(item => {
    if (item.getAttribute('data-part') === partTitle) {
      item.classList.toggle('part-collapsed', isCollapsed);
    }
  });
  const outline = document.getElementById('activeChapterOutline');
  if (outline && outline.getAttribute('data-part') === partTitle) {
    outline.classList.toggle('part-collapsed', isCollapsed);
  }
}

function stripLeadingOutlinePrefix(text) {
  return String(text || '').trim().replace(/^(?:section\s+)?(?:\d+(?:\.\d+)*|[A-Za-z])\s*[:.\-—)]\s*/i, '');
}

function toAlphaLabel(index, lower) {
  const base = lower ? 97 : 65;
  let n = index;
  let out = '';
  while (n >= 0) {
    out = String.fromCharCode(base + (n % 26)) + out;
    n = Math.floor(n / 26) - 1;
  }
  return out;
}

function setExpandedH2Section(sectionIdx, forceToggle) {
  const outline = document.getElementById('activeChapterOutline');
  if (!outline) return;
  const secLink = outline.querySelector('.toc-section-link[data-section-idx="' + sectionIdx + '"]');
  const chev = secLink ? secLink.querySelector('.toc-section-chevron') : null;
  const subs = outline.querySelectorAll('.toc-subsection-link[data-parent-section="' + sectionIdx + '"]');
  if (!subs.length) return;

  const currentlyFolded = subs[0].classList.contains('sub-folded');
  const nextFolded = forceToggle ? !currentlyFolded : false;

  if (!nextFolded) {
    outline.querySelectorAll('.toc-subsection-link').forEach(el => {
      if (el.getAttribute('data-parent-section') !== String(sectionIdx)) {
        el.classList.add('sub-folded');
      }
    });
    outline.querySelectorAll('.toc-section-chevron').forEach(c => {
      c.textContent = '▸';
    });
  }

  subs.forEach(el => el.classList.toggle('sub-folded', nextFolded));
  if (chev) chev.textContent = nextFolded ? '▸' : '▾';
}

function initActiveChapterSectionOutline() {
  const activeItem = document.querySelector('#bookSidebar .toc-item.active');
  const prose = document.getElementById('chapterProse');
  if (!activeItem || !prose) return;

  const existing = document.getElementById('activeChapterOutline');
  if (existing) existing.remove();

  const headings = Array.from(prose.querySelectorAll('h2, h3'));
  if (headings.length === 0) return;

  const outline = document.createElement('div');
  outline.className = 'toc-outline';
  outline.id = 'activeChapterOutline';
  outline.setAttribute('data-part', activeItem.getAttribute('data-part') || '');

  let h2Index = -1;
  let h3Index = 0;
  const h2Elements = [];

  headings.forEach((hEl, rawIdx) => {
    if (!hEl.id) {
      hEl.id = 'sec-' + rawIdx;
    }
    const tag = hEl.tagName.toLowerCase();
    const cleanText = stripLeadingOutlinePrefix(hEl.textContent);

    if (tag === 'h2') {
      h2Index++;
      h3Index = 0;
      const secIdx = h2Index;
      const label = toAlphaLabel(secIdx, false) + '. ' + cleanText;
      const a = document.createElement('a');
      a.href = '#' + hEl.id;
      a.className = 'toc-section-link';
      a.setAttribute('data-section-idx', String(secIdx));
      a.setAttribute('data-target-id', hEl.id);

      const span = document.createElement('span');
      span.textContent = label;
      a.appendChild(span);

      a.addEventListener('click', () => {
        setExpandedH2Section(secIdx, false);
        if (window.innerWidth <= 900) closeMobileSidebar();
      });

      outline.appendChild(a);
      h2Elements.push({ secIdx, hEl, linkEl: a, hasSub: false });
    } else if (tag === 'h3' && h2Index >= 0) {
      const parentObj = h2Elements[h2Index];
      if (parentObj && !parentObj.hasSub) {
        parentObj.hasSub = true;
        const chev = document.createElement('span');
        chev.className = 'toc-section-chevron';
        chev.textContent = '▸';
        chev.title = 'Toggle subsections';
        chev.addEventListener('click', (ev) => {
          ev.preventDefault();
          ev.stopPropagation();
          setExpandedH2Section(parentObj.secIdx, true);
        });
        parentObj.linkEl.appendChild(chev);
      }

      const subLabel = toAlphaLabel(h3Index, true) + '. ' + cleanText;
      h3Index++;
      const subA = document.createElement('a');
      subA.href = '#' + hEl.id;
      subA.className = 'toc-subsection-link sub-folded';
      subA.setAttribute('data-parent-section', String(h2Index));
      subA.setAttribute('data-target-id', hEl.id);
      subA.textContent = subLabel;
      subA.addEventListener('click', () => {
        if (window.innerWidth <= 900) closeMobileSidebar();
      });
      outline.appendChild(subA);
    }
  });

  activeItem.insertAdjacentElement('afterend', outline);
}

function initTocFolding() {
  const activeItem = document.querySelector('.toc-item.active');
  const activeSlug = activeItem ? activeItem.getAttribute('data-slug') : (ACTIVE_SLUG || '');
  const activeParent = activeItem ? activeItem.getAttribute('data-parent') : '';

  document.querySelectorAll('.toc-item').forEach(item => {
    const depth = parseInt(item.getAttribute('data-depth') || '0', 10);
    if (depth >= 1) {
      const parentSlug = item.getAttribute('data-parent') || '';
      const itemSlug = item.getAttribute('data-slug') || '';
      const shouldShow = (itemSlug === activeSlug) || (parentSlug !== '' && (parentSlug === activeSlug || parentSlug === activeParent));
      item.classList.toggle('sub-collapsed', !shouldShow);
    }
  });

  initActiveChapterSectionOutline();
}

function copyChapterPermalink() {
  const anchor = document.getElementById('chapterPermalinkAnchor');
  const btn = document.getElementById('btnCopyPermalink');
  if (!anchor || !navigator.clipboard || typeof navigator.clipboard.writeText !== 'function') return;
  const fullURL = window.location.origin + anchor.getAttribute('href');
  navigator.clipboard.writeText(fullURL).then(() => {
    if (btn) {
      const prev = btn.textContent;
      btn.textContent = '✓ Permalink Copied';
      setTimeout(() => { btn.textContent = prev; }, 1600);
    }
  }).catch(() => {});
}

function focusSidebarOrPageSearch() {
  const pageInp = document.getElementById('searchPageInput');
  if (pageInp) {
    pageInp.focus();
    pageInp.select();
    return;
  }
  const sb = document.getElementById('bookSidebar');
  const bd = document.getElementById('sidebarBackdrop');
  if (sb) {
    if (window.innerWidth <= 900) {
      sb.classList.add('mobile-open');
      if (bd) bd.classList.add('open');
    } else {
      sb.classList.remove('collapsed');
    }
  }
  const sideInp = document.getElementById('sidebarSearchInput');
  if (sideInp) {
    sideInp.focus();
    sideInp.select();
  }
}

function buildMermaidDiagramURL(idx) {
  const slug = (document.body && document.body.getAttribute('data-active-slug')) || '';
  const rev = (document.body && document.body.getAttribute('data-active-rev')) || '';
  if (slug && rev) {
    return '/chapters/' + slug + '/rev/' + rev + '/diagrams/' + idx;
  }
  if (slug) {
    return '/chapters/' + slug + '/diagrams/' + idx;
  }
  const path = (window.location.pathname || '').replace(/^\/u\/[^/]+/, '');
  const base = path.replace(/\/+$/, '').replace(/\/diagrams\/\d+$/, '');
  return (base || '/chapters') + '/diagrams/' + idx;
}

function attachMermaidExpandAffordances() {
  document.querySelectorAll('.mermaid-container').forEach((container, i) => {
    const idx = i + 1;
    const diagURL = buildMermaidDiagramURL(idx);
    container.setAttribute('data-diagram-index', String(idx));
    container.setAttribute('data-diagram-url', diagURL);
    const pre = container.querySelector('.mermaid');
    let linkWrap = container.querySelector('a.mermaid-diagram-link');
    if (!linkWrap && pre) {
      linkWrap = document.createElement('a');
      linkWrap.className = 'mermaid-diagram-link';
      linkWrap.href = diagURL;
      linkWrap.title = 'Click to open diagram (' + diagURL + ')';
      pre.parentNode.insertBefore(linkWrap, pre);
      linkWrap.appendChild(pre);
    } else if (linkWrap) {
      linkWrap.href = diagURL;
      linkWrap.title = 'Click to open diagram (' + diagURL + ')';
    }
    let badge = container.querySelector('.mermaid-expand-badge');
    if (!badge) {
      badge = document.createElement('a');
      badge.className = 'mermaid-expand-badge';
      badge.href = diagURL;
      badge.textContent = '↗ Open Diagram';
      badge.title = 'Open standalone diagram (' + diagURL + ')';
      container.appendChild(badge);
    } else {
      badge.href = diagURL;
      badge.title = 'Open standalone diagram (' + diagURL + ')';
    }
  });
}

async function renderMermaidDiagramsOnce() {
  if (!window.mermaid) return;
  window.mermaid.initialize({ startOnLoad: false, theme: 'neutral', securityLevel: 'strict' });
  const nodes = Array.from(document.querySelectorAll('.mermaid-container .mermaid'));
  if (nodes.length > 0) {
    try {
      if (typeof window.mermaid.run === 'function') {
        await window.mermaid.run({ nodes });
      } else if (typeof window.mermaid.init === 'function') {
        window.mermaid.init(undefined, nodes);
      }
    } catch (_) {}
  }
  attachMermaidExpandAffordances();
}

document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    focusSidebarOrPageSearch();
  } else if (e.key === 'Escape') {
    closeMobileSidebar();
  }
});

function formatTimelineTimestamps() {
  const nodes = document.querySelectorAll('time.timeline-time[datetime]');
  nodes.forEach((el) => {
    const raw = el.getAttribute('datetime');
    if (!raw) return;
    const dt = new Date(raw);
    if (Number.isNaN(dt.getTime())) return;
    try {
      const formatted = new Intl.DateTimeFormat(undefined, {
        month: 'short',
        day: 'numeric',
        year: 'numeric',
        hour: 'numeric',
        minute: '2-digit',
        timeZoneName: 'short'
      }).format(dt);
      el.textContent = formatted;
    } catch (_) {}
  });
}

document.addEventListener('DOMContentLoaded', async () => {
  initTocFolding();
  formatTimelineTimestamps();
  attachMermaidExpandAffordances();
  const savedTheme = localStorage.getItem('ghostbook_theme') || 'system';
  setReaderTheme(savedTheme);
  if (window.matchMedia) {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onSystemThemeChange = () => {
      if ((localStorage.getItem('ghostbook_theme') || 'system') === 'system') {
        const resolved = resolveSystemTheme();
        document.documentElement.setAttribute('data-theme', resolved);
      }
    };
    if (typeof mq.addEventListener === 'function') {
      mq.addEventListener('change', onSystemThemeChange);
    }
  }
  const proseEl = document.getElementById('chapterProse');
  if (window.renderMathInElement && proseEl) {
    renderMathInElement(proseEl, {
      delimiters: [
        {left: '$$', right: '$$', display: true},
        {left: '\\(', right: '\\)', display: false},
        {left: '\\[', right: '\\]', display: true}
      ],
      throwOnError: false
    });
  }
  if (window.mermaid) {
    await renderMermaidDiagramsOnce();
  }
});
