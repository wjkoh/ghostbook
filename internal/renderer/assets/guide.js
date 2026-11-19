const BASE_URL = document.body.getAttribute('data-base-url') || window.location.origin;
const MCP_SERVER_NAME = document.body.getAttribute('data-mcp-name') || 'ghostbook';
let CACHED_URL_AUTH_SEGMENT = null;

function captureAndScrubUrlAuthSegment() {
  const pathMatch = window.location.pathname.match(/^\/u\/([^/]+)/);
  if (!pathMatch || !pathMatch[1]) {
    return null;
  }
  const rawSeg = decodeURIComponent(pathMatch[1]);
  if (rawSeg.includes(':')) {
    CACHED_URL_AUTH_SEGMENT = rawSeg;
  }
  if (window.history && typeof window.history.replaceState === 'function') {
    window.history.replaceState(null, '', '/guide' + (window.location.search || '') + (window.location.hash || ''));
  }
  return CACHED_URL_AUTH_SEGMENT;
}

function listGuideProfileButtons() {
  return Array.from(document.querySelectorAll('.profile-pill-btn[data-author-email]'));
}

function resolveUrlAuthSegmentForEmail(email, authorId) {
  const rawSeg = CACHED_URL_AUTH_SEGMENT;
  if (!rawSeg || !rawSeg.includes(':')) {
    return null;
  }
  const aliasPart = rawSeg.split(':')[0].toLowerCase();
  const targetEmail = (email || '').toLowerCase();
  const targetId = (authorId || '').toLowerCase();
  if (aliasPart === targetEmail || (targetId && aliasPart === targetId)) {
    return rawSeg;
  }
  return null;
}

function selectGuideProfile(email, displayName, explicitAuthSegment, authorId) {
  if (email) {
    localStorage.setItem('ghostbook_author', email);
  }
  const urlSeg = explicitAuthSegment || resolveUrlAuthSegmentForEmail(email, authorId);
  const effectiveEmail = email || 'you@example.com';
  const effectiveName = displayName || 'Co-Author';
  const authSegment = (urlSeg && urlSeg.includes(':'))
    ? urlSeg
    : (effectiveEmail + ':<secret-key>');

  listGuideProfileButtons().forEach(btn => {
    const btnEmail = (btn.getAttribute('data-author-email') || '').toLowerCase();
    btn.classList.toggle('active', btnEmail === effectiveEmail.toLowerCase());
  });

  const mcpUrl = BASE_URL + '/u/' + authSegment + '/mcp';
  const labelClaude = document.getElementById('labelAuthorClaude');
  if (labelClaude) labelClaude.textContent = effectiveName;
  const claudeMcp = document.getElementById('claudeMcpUrl');
  if (claudeMcp) claudeMcp.textContent = mcpUrl;
  const claudeCli = document.getElementById('claudeCliCmd');
  if (claudeCli) claudeCli.textContent = 'claude mcp add --transport http ' + MCP_SERVER_NAME + ' ' + mcpUrl;
  const chatgptMcp = document.getElementById('chatgptMcpUrl');
  if (chatgptMcp) chatgptMcp.textContent = mcpUrl;
  const geminiWebMcp = document.getElementById('geminiWebMcpUrl');
  if (geminiWebMcp) geminiWebMcp.textContent = mcpUrl;
  const geminiJson = document.getElementById('geminiJsonConfig');
  if (geminiJson) {
    const servers = {};
    servers[MCP_SERVER_NAME] = { serverUrl: mcpUrl };
    geminiJson.textContent = JSON.stringify({ mcpServers: servers }, null, 2);
  }
}

function copyTextFrom(elId, btn) {
  const el = document.getElementById(elId);
  if (!el) return;
  const txt = (el.textContent || '').trim().replace(/^“|”$/g, '');
  if (!navigator.clipboard || typeof navigator.clipboard.writeText !== 'function') {
    return;
  }
  navigator.clipboard.writeText(txt).then(() => {
    if (!btn) return;
    const orig = btn.textContent;
    btn.textContent = '✓ Copied!';
    setTimeout(() => { btn.textContent = orig; }, 1600);
  }).catch(() => {});
}

document.addEventListener('DOMContentLoaded', () => {
  const buttons = listGuideProfileButtons();
  const rawSeg = captureAndScrubUrlAuthSegment();
  if (rawSeg) {
    const aliasPart = rawSeg.split(':')[0].toLowerCase();
    const matched = buttons.find(btn => {
      const e = (btn.getAttribute('data-author-email') || '').toLowerCase();
      const id = (btn.getAttribute('data-author-id') || '').toLowerCase();
      return e === aliasPart || id === aliasPart || (e && aliasPart.startsWith(e.split('@')[0]));
    });
    if (matched) {
      selectGuideProfile(
        matched.getAttribute('data-author-email'),
        matched.getAttribute('data-author-name'),
        rawSeg,
        matched.getAttribute('data-author-id')
      );
      return;
    }
    selectGuideProfile(aliasPart, aliasPart, rawSeg, '');
    return;
  }

  const savedAuthor = (localStorage.getItem('ghostbook_author') || '').toLowerCase();
  const savedBtn = buttons.find(btn => (btn.getAttribute('data-author-email') || '').toLowerCase() === savedAuthor);
  const initialBtn = savedBtn || buttons[0];
  if (initialBtn) {
    selectGuideProfile(
      initialBtn.getAttribute('data-author-email'),
      initialBtn.getAttribute('data-author-name'),
      null,
      initialBtn.getAttribute('data-author-id')
    );
  } else {
    selectGuideProfile('you@example.com', 'Co-Author', null, '');
  }
});
