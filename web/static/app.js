'use strict';

var state = {
  sessions: [],
  filteredSessions: [],
  pinnedSessions: [],
  sidebarIdx: 0,
  transcript: null,
  currentSession: null,
  selectedBlocks: {},
  filterText: '',
  copyFormat: 'jsonl',
  roundOrder: 'asc',
};

// Blocks that start folded by default.
var FOLD_CLOSED = {context: true, tool: true, thinking: true};

var COPY_PATH_SVG = '<svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">' +
  '<path d="M4 1.5H3a2 2 0 0 0-2 2V14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V3.5a2 2 0 0 0-2-2h-1v1h1a1 1 0 0 1 1 1V14a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V3.5a1 1 0 0 1 1-1h1v-1z"/>' +
  '<path d="M9.5 1a.5.5 0 0 1 .5.5v1a.5.5 0 0 1-.5.5h-3a.5.5 0 0 1-.5-.5v-1a.5.5 0 0 1 .5-.5h3zm-3-1A1.5 1.5 0 0 0 5 1.5v1A1.5 1.5 0 0 0 6.5 4h3A1.5 1.5 0 0 0 11 2.5v-1A1.5 1.5 0 0 0 9.5 0h-3z"/>' +
  '</svg>';

var PIN_SVG = '<svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">' +
  '<path d="M9.828.722a.5.5 0 0 1 .354.146l4.95 4.95a.5.5 0 0 1 0 .707c-.48.48-1.072.588-1.503.588-.177 0-.335-.018-.46-.039l-3.134 3.134a5.927 5.927 0 0 1 .16 1.013c.046.702-.032 1.687-.72 2.375a.5.5 0 0 1-.707 0l-2.829-2.828-3.182 3.182c-.195.195-1.219.902-1.414.707-.195-.195.512-1.22.707-1.414l3.182-3.182-2.828-2.829a.5.5 0 0 1 0-.707c.688-.688 1.673-.767 2.375-.72a5.922 5.922 0 0 1 1.013.16l3.134-3.133a2.772 2.772 0 0 1-.04-.461c0-.43.108-1.022.589-1.503a.5.5 0 0 1 .353-.146z"/>' +
  '</svg>';

var dragSrcId = null;

// SSE connections.
var sessionSource = null;
var transcriptSource = null;

// --- SSE Session Stream ---

var sessionStreamInitialized = false;

function startSessionStream() {
  if (sessionSource) sessionSource.close();
  sessionSource = new EventSource('/api/sessions/stream');

  sessionSource.addEventListener('sessions', function(e) {
    var data = JSON.parse(e.data);
    state.sessions = data || [];
    applyFilter();

    if (!sessionStreamInitialized) {
      sessionStreamInitialized = true;
      renderSidebar();
      updateSessionCount();

      // Auto-load first (or URL-targeted) transcript.
      var targetIdx = 0;
      if (state._initialSessionID) {
        for (var i = 0; i < state.filteredSessions.length; i++) {
          if (state.filteredSessions[i].session_id === state._initialSessionID) {
            targetIdx = i;
            break;
          }
        }
        delete state._initialSessionID;
      } else if (state._initialSessionSource && state._initialRawSessionID) {
        for (var j = 0; j < state.filteredSessions.length; j++) {
          var sess = state.filteredSessions[j];
          if (sess.source === state._initialSessionSource && rawSessionID(sess) === state._initialRawSessionID) {
            targetIdx = j;
            break;
          }
        }
        delete state._initialSessionSource;
        delete state._initialRawSessionID;
      }
      if (state.filteredSessions.length > 0) {
        loadTranscript(targetIdx);
      }
    } else {
      // Update: preserve current selection.
      if (state.currentSession) {
        var sid = state.currentSession.session_id;
        state.sidebarIdx = -1;
        for (var i = 0; i < state.filteredSessions.length; i++) {
          if (state.filteredSessions[i].session_id === sid) {
            state.sidebarIdx = i;
            break;
          }
        }
      }
      renderSidebar();
      updateSessionCount();
    }
  });

  sessionSource.onerror = function() {
    // EventSource auto-reconnects. Nothing to do.
  };
}

// --- SSE Transcript Stream ---

function stopTranscriptStream() {
  if (transcriptSource) {
    transcriptSource.close();
    transcriptSource = null;
  }
}

function loadTranscript(idx) {
  if (idx < 0 || idx >= state.filteredSessions.length) return;
  stopTranscriptStream();

  state.sidebarIdx = idx;
  state.currentSession = state.filteredSessions[idx];
  state.selectedBlocks = {};
  state.transcript = { session_id: state.currentSession.session_id, rounds: [] };
  renderSidebar();

  var sid = state.currentSession.session_id;
  if (state._initialRoundIdx === undefined) {
    history.replaceState(null, '', buildSessionPath(state.currentSession));
  }

  // Show loading state.
  var title = document.getElementById('viewer-title');
  var content = document.getElementById('viewer-content');
  var s = state.currentSession;
  title.textContent = s ? s.project_name + ' -- loading...' : 'loading...';
  content.innerHTML = '';
  updateSelectionUI();

  // Per-round cumulative usage (aggregated client-side for display updates).
  var roundUsage = {};
  var renderedRounds = 0;
  var lastRenderedBlockCount = 0;
  var renderFrame = null;

  function scheduleRender() {
    if (!renderFrame) {
      renderFrame = requestAnimationFrame(flushRender);
    }
  }

  function flushRender() {
    renderFrame = null;
    var content = document.getElementById('viewer-content');
    var rounds = state.transcript.rounds;

    // Re-render the last previously-rendered round if it got new blocks.
    if (renderedRounds > 0 && renderedRounds <= rounds.length) {
      var lastRound = rounds[renderedRounds - 1];
      var bc = lastRound.blocks.length;
      if (bc !== lastRenderedBlockCount) {
        var oldEl = content.querySelector('.round[data-round-idx="' + lastRound.index + '"]');
        if (oldEl) {
          var tmp = document.createElement('div');
          tmp.innerHTML = renderRound(lastRound, lastRound.index);
          var newEl = tmp.firstChild;
          content.replaceChild(newEl, oldEl);
          initBlocks(newEl);
        }
        lastRenderedBlockCount = bc;
      }
    }

    // Append new rounds.
    for (var i = renderedRounds; i < rounds.length; i++) {
      var html = renderRound(rounds[i], rounds[i].index);
      if (state.roundOrder === 'desc') {
        content.insertAdjacentHTML('afterbegin', html);
      } else {
        content.insertAdjacentHTML('beforeend', html);
      }
      var el = content.querySelector('.round[data-round-idx="' + rounds[i].index + '"]');
      if (el) initBlocks(el);
    }
    if (rounds.length > renderedRounds) {
      lastRenderedBlockCount = rounds[rounds.length - 1].blocks.length;
    }
    renderedRounds = rounds.length;

    // Update header.
    var s = state.currentSession;
    var title = document.getElementById('viewer-title');
    title.textContent = (s ? s.project_name + ' -- ' : '') + rounds.length + ' rounds';
    updateSelectionUI();

    // Handle initial scroll target.
    if (state._initialRoundIdx !== undefined) {
      var targetRound = null;
      for (var j = 0; j < rounds.length; j++) {
        if (rounds[j].index === state._initialRoundIdx) {
          targetRound = rounds[j];
          break;
        }
      }
      if (targetRound) {
        scrollToTarget(state._initialRoundIdx, state._initialBlockIdx);
        delete state._initialRoundIdx;
        delete state._initialBlockIdx;
      }
    }
  }

  transcriptSource = new EventSource('/api/transcript/' + sid + '/stream');

  transcriptSource.addEventListener('block', function(e) {
    var block = JSON.parse(e.data);
    var ri = block.round_index;
    var rounds = state.transcript.rounds;

    // Find or create round.
    var round;
    if (rounds.length === 0 || rounds[rounds.length - 1].index !== ri) {
      // New round.
      round = {
        index: ri,
        user_timestamp: block.user_timestamp || '',
        is_context: !!block.is_context,
        blocks: [],
        usage: { input_tokens: 0, output_tokens: 0, cache_read: 0, cache_creation: 0 },
      };
      rounds.push(round);
    } else {
      round = rounds[rounds.length - 1];
    }

    // Add block.
    round.blocks.push({
      role: block.role,
      html: block.html || '',
      name: block.name || '',
      input_summary: block.input_summary || '',
      input_json: block.input_json || '',
    });

    scheduleRender();
  });

  transcriptSource.addEventListener('usage', function(e) {
    var usage = JSON.parse(e.data);
    var ri = usage.round_index;
    var rounds = state.transcript.rounds;

    for (var i = rounds.length - 1; i >= 0; i--) {
      if (rounds[i].index === ri) {
        rounds[i].usage = {
          input_tokens: usage.input_tokens,
          output_tokens: usage.output_tokens,
          cache_read: usage.cache_read,
          cache_creation: usage.cache_creation,
        };
        break;
      }
    }

    scheduleRender();
  });

  transcriptSource.onerror = function() {
    // EventSource auto-reconnects. Nothing to do.
  };
}

// --- API ---

function doCopy() {
  if (!state.currentSession) return;
  var body = {
    session_id: state.currentSession.session_id,
    format: state.copyFormat,
  };

  var blocks = getSelectedBlocks();
  if (blocks) {
    body.blocks = blocks;
  }

  fetch('/api/export', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).then(function(r) {
    return r.text();
  }).then(function(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function() {
        setStatus('Copied (' + state.copyFormat + ')');
      });
    }
  });
  closeCopyModal();
}

function doPublish() {
  if (!state.currentSession) return;
  var titleInput = document.getElementById('publish-title');
  var title = titleInput.value.trim();
  if (!title) return;

  var errorEl = document.getElementById('publish-error');
  errorEl.classList.add('hidden');
  errorEl.textContent = '';

  var confirmBtn = document.getElementById('publish-confirm');
  confirmBtn.disabled = true;
  confirmBtn.textContent = 'Publishing...';

  var body = {
    session_id: state.currentSession.session_id,
    title: title,
  };

  var blocks = getSelectedBlocks();
  if (blocks) {
    body.blocks = blocks;
  }

  fetch('/api/publish', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).then(function(r) {
    if (!r.ok) {
      return r.text().then(function(text) {
        throw {status: r.status, message: text, isAuth: r.headers.get('X-Publish-Error') === 'auth'};
      });
    }
    return r.json();
  }).then(function(data) {
    closePublishModal();
    showToast('Published: <a href="' + escapeHtml(data.url) + '" target="_blank">' + escapeHtml(data.url) + '</a>');
  }).catch(function(err) {
    if (err && err.isAuth) {
      errorEl.textContent = 'Run `glab auth login` to authenticate.';
    } else {
      errorEl.textContent = err.message || 'Publish failed';
    }
    errorEl.classList.remove('hidden');
  }).finally(function() {
    confirmBtn.disabled = false;
    confirmBtn.textContent = 'Publish';
  });
}

// --- Filter ---

function loadPinnedSessions() {
  try {
    var s = localStorage.getItem('agent-sessions-pinned') || localStorage.getItem('cc-viewer-pinned');
    if (s) state.pinnedSessions = JSON.parse(s);
  } catch(e) {}
}

function savePinnedSessions() {
  try { localStorage.setItem('agent-sessions-pinned', JSON.stringify(state.pinnedSessions)); }
  catch(e) {}
}

function applyFilter() {
  var text = state.filterText.toLowerCase();
  if (!text) {
    state.filteredSessions = state.sessions.slice();
  } else {
    state.filteredSessions = state.sessions.filter(function(s) {
      return (s.project_name && s.project_name.toLowerCase().indexOf(text) >= 0) ||
        (s.project && s.project.toLowerCase().indexOf(text) >= 0) ||
        (s.all_messages && s.all_messages.toLowerCase().indexOf(text) >= 0);
    });
  }
  // Sort pinned sessions to the top, in pin order.
  var pinned = [], unpinned = [];
  state.filteredSessions.forEach(function(s) {
    (state.pinnedSessions.indexOf(s.session_id) >= 0 ? pinned : unpinned).push(s);
  });
  pinned.sort(function(a, b) {
    return state.pinnedSessions.indexOf(a.session_id) - state.pinnedSessions.indexOf(b.session_id);
  });
  state.filteredSessions = pinned.concat(unpinned);
}

// --- Render ---

function renderSidebar() {
  var list = document.getElementById('session-list');
  var html = '';
  var pinnedCount = state.filteredSessions.filter(function(s) {
    return state.pinnedSessions.indexOf(s.session_id) >= 0;
  }).length;
  for (var i = 0; i < state.filteredSessions.length; i++) {
    var s = state.filteredSessions[i];
    var active = i === state.sidebarIdx ? ' active' : '';
    var isPinned = state.pinnedSessions.indexOf(s.session_id) >= 0;
    var pinnedCls = isPinned ? ' pinned' : '';
    var draggable = isPinned ? ' draggable="true"' : '';
    var ts = formatTime(s.last_ts);
    var msg = escapeHtml(truncate(s.first_message || '', 60));
    var pinBtnCls = 'pin-btn' + (isPinned ? ' pinned' : '');
    var source = (s && s.source) || splitSessionID(s.session_id || '').source || 'claude';
    var sourceCls = source === 'codex' ? ' source-codex' : ' source-claude';
    var copyPathBtn = '<button class="copy-path-btn" data-session-id="' + escapeHtml(s.session_id) + '" data-file-path="' +
      escapeHtml(s.file_path || '') + '" title="Copy file path">' + COPY_PATH_SVG + '</button>';
    html += '<div class="session-item' + active + pinnedCls + '" data-idx="' + i + '" data-session-id="' + escapeHtml(s.session_id) + '"' + draggable + '>' +
      '<div class="session-row"><span class="session-project' + sourceCls + '">' + escapeHtml(s.project_name || '?') + '</span>' +
      '<span class="session-time">' + ts + '</span></div>' +
      '<div class="session-message">"' + msg + '"</div>' +
      copyPathBtn +
      '<button class="' + pinBtnCls + '" data-session-id="' + escapeHtml(s.session_id) + '" title="Pin/Unpin">' + PIN_SVG + '</button>' +
      '</div>';
    // Divider between last pinned and first unpinned.
    if (isPinned && i === pinnedCount - 1 && pinnedCount < state.filteredSessions.length) {
      html += '<div class="session-divider"></div>';
    }
  }
  if (state.filteredSessions.length === 0) {
    html = '<div class="empty-state">No sessions</div>';
  }
  list.innerHTML = html;

  var activeEl = list.querySelector('.session-item.active');
  if (activeEl) activeEl.scrollIntoView({block: 'nearest'});
}

function renderViewer() {
  var title = document.getElementById('viewer-title');
  var content = document.getElementById('viewer-content');

  if (!state.transcript || !state.transcript.rounds) {
    title.textContent = '';
    content.innerHTML = '<div class="empty-state">Select a session</div>';
    return;
  }

  var rounds = state.transcript.rounds;
  var s = state.currentSession;
  title.textContent = (s ? s.project_name + ' -- ' : '') + rounds.length + ' rounds';

  var order = [];
  for (var i = 0; i < rounds.length; i++) order.push(i);
  if (state.roundOrder === 'desc') order.reverse();

  var html = '';
  for (var j = 0; j < order.length; j++) {
    html += renderRound(rounds[order[j]], rounds[order[j]].index);
  }
  content.innerHTML = html;

  // Fill fold summaries and attach toggle handlers.
  fillFoldSummaries();
  var blockHeaders = content.querySelectorAll('.block-header');
  for (var k = 0; k < blockHeaders.length; k++) {
    blockHeaders[k].addEventListener('click', foldToggleHandler);
  }
  var groupHeaders = content.querySelectorAll('.group-header');
  for (var k2 = 0; k2 < groupHeaders.length; k2++) {
    groupHeaders[k2].addEventListener('click', groupFoldToggleHandler);
  }
  var roundHeaders = content.querySelectorAll('.round-header');
  for (var k3 = 0; k3 < roundHeaders.length; k3++) {
    roundHeaders[k3].addEventListener('click', roundFoldToggleHandler);
  }
}

// Build HTML content for a single block (tool or markdown).
function buildBlockContent(b) {
  if (b.role === 'tool') {
    var html = '<div class="tool-list"><div class="tool-item">' +
      '<span class="tool-name">' + escapeHtml(b.name || '') + '</span>';
    if (b.input_summary) {
      html += '<span class="tool-input">' + escapeHtml(b.input_summary) + '</span>';
    }
    html += '</div>';
    if (b.input_json) {
      html += '<details class="tool-input-json"><summary>params</summary>' +
        '<pre>' + escapeHtml(b.input_json) + '</pre></details>';
    }
    html += '</div>';
    return html;
  }
  return b.html || '';
}

function renderRound(round, idx) {
  var sid = state.currentSession ? state.currentSession.session_id : '';
  var routeBase = state.currentSession ? buildSessionPath(state.currentSession) : '';
  var ts = formatShortTime(round.user_timestamp || '');
  var tokens = '';
  if (round.usage) {
    var u = round.usage;
    tokens = '<span class="round-tokens">' +
      'in:' + compactNum(u.input_tokens) +
      ' out:' + compactNum(u.output_tokens) +
      ' cr:' + compactNum(u.cache_read) +
      ' cw:' + compactNum(u.cache_creation) +
      '</span>';
  }

  var html = '<div class="round" data-round-idx="' + idx + '">' +
    '<div class="round-header">' +
    '<span class="fold-arrow open">&#9654;</span>' +
    '<span class="round-index">#' + (idx + 1) + '</span>' +
    '<span class="round-timestamp">' + escapeHtml(ts) + '</span>' +
    '<a class="anchor-link" data-anchor="' + routeBase + '/' + idx + '" title="Copy link">#</a>' +
    '<span class="round-fold-summary"></span>' +
    tokens +
    '</div>';

  // Group consecutive same-role blocks.
  var blocks = round.blocks || [];
  var groups = [];
  for (var i = 0; i < blocks.length; i++) {
    if (groups.length > 0 && groups[groups.length - 1].role === blocks[i].role) {
      groups[groups.length - 1].indices.push(i);
    } else {
      groups.push({role: blocks[i].role, indices: [i]});
    }
  }

  html += '<div class="round-body open">';

  for (var g = 0; g < groups.length; g++) {
    var group = groups[g];
    var groupRoleKey = roleKeyForDisplay(group.role);
    var groupRoleLabel = roleLabelForDisplay(group.role);
    if (group.indices.length === 1) {
      var bi = group.indices[0];
      html += renderBlock(idx, bi, blocks[bi].role, buildBlockContent(blocks[bi]), routeBase);
    } else {
      var startOpen = !FOLD_CLOSED[group.role];
      // Tool groups get a single checkbox for the entire group.
      var checkboxHtml = '';
      var groupCheckedClass = '';
      if (group.role === 'tool') {
        var groupBlockIds = [];
        var allGroupSelected = true;
        for (var ci = 0; ci < group.indices.length; ci++) {
          var gid = 'b-' + idx + '-' + group.indices[ci];
          groupBlockIds.push(gid);
          if (!state.selectedBlocks[gid]) allGroupSelected = false;
        }
        var groupChecked = (allGroupSelected && groupBlockIds.length > 0) ? ' checked' : '';
        groupCheckedClass = (allGroupSelected && groupBlockIds.length > 0) ? ' block-checked' : '';
        checkboxHtml = '<input type="checkbox" class="group-checkbox" data-group-blocks="' + groupBlockIds.join(',') + '"' + groupChecked + '>';
      }
      html += '<div class="block-group block-group-' + groupRoleKey + groupCheckedClass + '">' +
        '<div class="group-header">' +
        checkboxHtml +
        '<span class="fold-arrow' + (startOpen ? ' open' : '') + '">&#9654;</span>' +
        '<span class="chat-role">' + groupRoleLabel + '</span>' +
        '<span class="group-count">(' + group.indices.length + ')</span>' +
        '<a class="anchor-link" data-anchor="' + routeBase + '/' + idx + '/' + group.indices[0] + '" title="Copy link">#</a>' +
        '<span class="fold-summary"></span>' +
        '</div>' +
        '<div class="group-body' + (startOpen ? ' open' : '') + '">';
      for (var k = 0; k < group.indices.length; k++) {
        var bi2 = group.indices[k];
        if (group.role === 'tool') {
          html += renderCompactToolBlock(idx, bi2, blocks[bi2], routeBase);
        } else {
          html += renderBlock(idx, bi2, blocks[bi2].role, buildBlockContent(blocks[bi2]), routeBase);
        }
      }
      html += '</div></div>';
    }
  }

  html += '</div></div>';
  return html;
}

// Render a single chat block with fold toggle and checkbox.
function renderBlock(roundIdx, blockIdx, role, contentHtml, routeBase) {
  var blockId = 'b-' + roundIdx + '-' + blockIdx;
  var roleLabel = roleLabelForDisplay(role);
  var roleKey = roleKeyForDisplay(role);
  var startOpen = !FOLD_CLOSED[role];
  var checked = state.selectedBlocks[blockId] ? ' checked' : '';
  var checkedClass = state.selectedBlocks[blockId] ? ' block-checked' : '';

  var html = '<div class="chat-block chat-' + roleKey + checkedClass + '" data-block-id="' + blockId + '">';

  // Header row: checkbox + fold arrow + role label + anchor link + summary.
  html += '<div class="block-header">' +
    '<input type="checkbox" class="block-checkbox" data-block-id="' + blockId + '"' + checked + '>' +
    '<span class="fold-arrow' + (startOpen ? ' open' : '') + '">&#9654;</span>' +
    '<span class="chat-role">' + roleLabel + '</span>' +
    '<a class="anchor-link" data-anchor="' + routeBase + '/' + roundIdx + '/' + blockIdx + '" title="Copy link">#</a>' +
    '<span class="fold-summary"></span>' +
    '</div>';

  // Fold body.
  html += '<div class="fold-body' + (startOpen ? ' open' : '') + '">' + contentHtml + '</div>';

  html += '</div>';
  return html;
}

// Render a compact tool block for use inside a tool group (no fold mechanism).
function renderCompactToolBlock(roundIdx, blockIdx, block, routeBase) {
  var blockId = 'b-' + roundIdx + '-' + blockIdx;

  var html = '<div class="chat-block chat-tool compact-tool" data-block-id="' + blockId + '">' +
    '<div class="block-header">' +
    '<span class="tool-name">' + escapeHtml(block.name || '') + '</span>' +
    '<span class="tool-input">' + escapeHtml(block.input_summary || '') + '</span>' +
    '<a class="anchor-link" data-anchor="' + routeBase + '/' + roundIdx + '/' + blockIdx + '" title="Copy link">#</a>' +
    '</div>';
  if (block.input_json) {
    html += '<details class="tool-input-json"><summary>params</summary>' +
      '<pre>' + escapeHtml(block.input_json) + '</pre></details>';
  }
  html += '</div>';
  return html;
}

// After innerHTML is set, fill in fold summaries from rendered content.
function fillFoldSummaries() {
  // Block fold summaries.
  var blocks = document.querySelectorAll('#viewer-content .chat-block');
  for (var i = 0; i < blocks.length; i++) {
    var block = blocks[i];
    var summaryEl = block.querySelector('.fold-summary');
    if (!summaryEl) continue;

    var body = block.querySelector('.fold-body');
    if (!body) continue;

    var text = '';
    if (block.classList.contains('chat-tool')) {
      var nameEl = body.querySelector('.tool-name');
      var inputEl = body.querySelector('.tool-input');
      text = (nameEl ? nameEl.textContent : '');
      if (inputEl && inputEl.textContent) text += ': ' + inputEl.textContent;
    } else {
      text = (body.textContent || '').trim().replace(/\s+/g, ' ');
      text = truncate(text, 80);
    }
    summaryEl.textContent = text;
  }

  // Group fold summaries.
  var groups = document.querySelectorAll('#viewer-content .block-group');
  for (var gi = 0; gi < groups.length; gi++) {
    var group = groups[gi];
    var gSummary = group.querySelector('.group-header .fold-summary');
    if (!gSummary) continue;
    var gBody = group.querySelector('.group-body');
    if (!gBody) continue;

    var gText = '';
    if (group.classList.contains('block-group-tool')) {
      var names = gBody.querySelectorAll('.tool-name');
      var nameList = [];
      for (var ni = 0; ni < names.length; ni++) {
        var n = names[ni].textContent;
        if (n && nameList.indexOf(n) < 0) nameList.push(n);
      }
      gText = nameList.join(', ');
    } else {
      var cnt = group.querySelector('.group-count');
      gText = (cnt ? cnt.textContent.replace(/[()]/g, '') : '') + ' blocks';
    }
    gSummary.textContent = gText;
  }

  // Round fold summaries.
  var rounds = document.querySelectorAll('#viewer-content .round');
  for (var ri = 0; ri < rounds.length; ri++) {
    var round = rounds[ri];
    var rfSummary = round.querySelector('.round-fold-summary');
    if (!rfSummary) continue;
    var rBody = round.querySelector('.round-body');
    if (!rBody) continue;

    var firstBlock = rBody.querySelector('.chat-block');
    var rText = '';
    if (firstBlock) {
      var fb = firstBlock.querySelector('.fold-body');
      if (fb) {
        rText = (fb.textContent || '').trim().replace(/\s+/g, ' ');
        rText = truncate(rText, 80);
      }
    }
    var bc = rBody.querySelectorAll('.chat-block').length;
    rText += ' (' + bc + ' block' + (bc !== 1 ? 's' : '') + ')';
    rfSummary.textContent = rText;
  }
}

// Initialize fold summaries and toggle handlers for blocks within a container element.
function initBlocks(container) {
  // Block fold summaries + handlers.
  var blocks = container.querySelectorAll('.chat-block');
  for (var i = 0; i < blocks.length; i++) {
    var block = blocks[i];
    var summaryEl = block.querySelector('.fold-summary');
    var body = block.querySelector('.fold-body');
    if (summaryEl && body) {
      var text;
      if (block.classList.contains('chat-tool')) {
        var nameEl = body.querySelector('.tool-name');
        var inputEl = body.querySelector('.tool-input');
        text = (nameEl ? nameEl.textContent : '');
        if (inputEl && inputEl.textContent) text += ': ' + inputEl.textContent;
      } else {
        text = (body.textContent || '').trim().replace(/\s+/g, ' ');
        text = truncate(text, 80);
      }
      summaryEl.textContent = text;
    }
    var header = block.querySelector('.block-header');
    if (header) header.addEventListener('click', foldToggleHandler);
  }

  // Group fold summaries + handlers.
  var groups = container.querySelectorAll('.block-group');
  for (var gi = 0; gi < groups.length; gi++) {
    var group = groups[gi];
    var gSummary = group.querySelector('.group-header .fold-summary');
    if (gSummary) {
      var gBody = group.querySelector('.group-body');
      if (gBody) {
        var gText = '';
        if (group.classList.contains('block-group-tool')) {
          var names = gBody.querySelectorAll('.tool-name');
          var nameList = [];
          for (var ni = 0; ni < names.length; ni++) {
            var n = names[ni].textContent;
            if (n && nameList.indexOf(n) < 0) nameList.push(n);
          }
          gText = nameList.join(', ');
        } else {
          var cnt = group.querySelector('.group-count');
          gText = (cnt ? cnt.textContent.replace(/[()]/g, '') : '') + ' blocks';
        }
        gSummary.textContent = gText;
      }
    }
    var gh = group.querySelector('.group-header');
    if (gh) gh.addEventListener('click', groupFoldToggleHandler);
  }

  // Round fold summary + handler.
  var rfSummary = container.querySelector('.round-fold-summary');
  if (rfSummary) {
    var rBody = container.querySelector('.round-body');
    if (rBody) {
      var firstBlock = rBody.querySelector('.chat-block');
      var rText = '';
      if (firstBlock) {
        var fb = firstBlock.querySelector('.fold-body');
        if (fb) {
          rText = (fb.textContent || '').trim().replace(/\s+/g, ' ');
          rText = truncate(rText, 80);
        }
      }
      var bc = rBody.querySelectorAll('.chat-block').length;
      rText += ' (' + bc + ' block' + (bc !== 1 ? 's' : '') + ')';
      rfSummary.textContent = rText;
    }
  }
  var rh = container.querySelector('.round-header');
  if (rh) rh.addEventListener('click', roundFoldToggleHandler);
}

function foldToggleHandler(e) {
  // Don't toggle fold when clicking the checkbox or anchor link.
  if (e.target.classList.contains('block-checkbox')) return;
  if (e.target.closest('.anchor-link')) return;
  var block = this.closest('.chat-block');
  if (!block) return;
  var body = block.querySelector('.fold-body');
  var arrow = block.querySelector('.fold-arrow');
  if (body) body.classList.toggle('open');
  if (arrow) arrow.classList.toggle('open');
}

function roundFoldToggleHandler(e) {
  if (e.target.closest('.anchor-link')) return;
  var round = this.closest('.round');
  if (!round) return;
  var body = round.querySelector('.round-body');
  var arrow = this.querySelector('.fold-arrow');
  if (body) body.classList.toggle('open');
  if (arrow) arrow.classList.toggle('open');
}

function groupFoldToggleHandler(e) {
  if (e.target.classList.contains('group-checkbox')) return;
  if (e.target.closest('.anchor-link')) return;
  var group = this.closest('.block-group');
  if (!group) return;
  var body = group.querySelector('.group-body');
  var arrow = this.querySelector('.fold-arrow');
  if (body) body.classList.toggle('open');
  if (arrow) arrow.classList.toggle('open');
}

function updateSelectionUI() {
  var count = selectedBlockCount();
  var actions = document.getElementById('selection-actions');
  if (count > 0) {
    actions.classList.remove('hidden');
  } else {
    actions.classList.add('hidden');
  }
}

function updateSessionCount() {
  var countEl = document.getElementById('session-count');
  var input = document.getElementById('filter-input');
  if (input.value) {
    countEl.textContent = '';
  } else {
    countEl.textContent = state.sessions.length + ' sessions';
  }
}

function selectedBlockCount() {
  var count = 0;
  var keys = Object.keys(state.selectedBlocks);
  for (var i = 0; i < keys.length; i++) {
    if (state.selectedBlocks[keys[i]]) count++;
  }
  return count;
}

// Build a list of [roundIdx, blockIdx] pairs from selected blocks.
// Returns null if no blocks are selected.
function getSelectedBlocks() {
  var keys = Object.keys(state.selectedBlocks);
  var result = [];
  for (var i = 0; i < keys.length; i++) {
    if (!state.selectedBlocks[keys[i]]) continue;
    var parts = keys[i].split('-');
    if (parts.length < 3) continue;
    var roundIdx = parseInt(parts[1], 10);
    var blockIdx = parseInt(parts[2], 10);
    if (isNaN(roundIdx) || isNaN(blockIdx)) continue;
    result.push([roundIdx, blockIdx]);
  }
  result.sort(function(a, b) { return a[0] !== b[0] ? a[0] - b[0] : a[1] - b[1]; });
  return result.length > 0 ? result : null;
}

// --- Selection actions ---

function clearAllSelections() {
  state.selectedBlocks = {};
  var checkboxes = document.querySelectorAll('#viewer-content .block-checkbox');
  for (var i = 0; i < checkboxes.length; i++) {
    checkboxes[i].checked = false;
    var block = checkboxes[i].closest('.chat-block');
    if (block) block.classList.remove('block-checked');
  }
  var groupCheckboxes = document.querySelectorAll('#viewer-content .group-checkbox');
  for (var j = 0; j < groupCheckboxes.length; j++) {
    groupCheckboxes[j].checked = false;
    var grp = groupCheckboxes[j].closest('.block-group');
    if (grp) grp.classList.remove('block-checked');
  }
  updateSelectionUI();
}

// --- Copy modal ---

function openCopyModal() {
  state.copyFormat = 'jsonl';
  var modal = document.getElementById('copy-modal');
  modal.classList.remove('hidden');
  updateCopyFormatButtons();
}

function closeCopyModal() {
  document.getElementById('copy-modal').classList.add('hidden');
}

function updateCopyFormatButtons() {
  var btns = document.querySelectorAll('#copy-modal .modal-options .modal-btn');
  for (var i = 0; i < btns.length; i++) {
    btns[i].classList.toggle('selected', btns[i].getAttribute('data-format') === state.copyFormat);
  }
}

// --- Publish modal ---

function openPublishModal() {
  var modal = document.getElementById('publish-modal');
  var titleInput = document.getElementById('publish-title');
  var errorEl = document.getElementById('publish-error');

  // Pre-fill title from session's first message.
  var defaultTitle = '';
  if (state.currentSession && state.currentSession.first_message) {
    defaultTitle = state.currentSession.first_message;
    if (defaultTitle.length > 80) defaultTitle = defaultTitle.substring(0, 80) + '...';
  }
  titleInput.value = defaultTitle;
  errorEl.classList.add('hidden');
  errorEl.textContent = '';
  modal.classList.remove('hidden');
  titleInput.focus();
  titleInput.select();
}

function closePublishModal() {
  document.getElementById('publish-modal').classList.add('hidden');
}

// --- Toast ---

var toastTimeout = null;
function showToast(html) {
  var toast = document.getElementById('toast');
  toast.innerHTML = html;
  toast.classList.remove('hidden');
  clearTimeout(toastTimeout);
  toastTimeout = setTimeout(function() { toast.classList.add('hidden'); }, 8000);
}

// --- Status message ---

var statusTimeout = null;
function setStatus(msg) {
  var title = document.getElementById('viewer-title');
  var original = title.textContent;
  title.textContent = msg;
  clearTimeout(statusTimeout);
  statusTimeout = setTimeout(function() { title.textContent = original; }, 3000);
}

// --- Event handlers ---

// Filter input.
document.getElementById('filter-input').addEventListener('input', function() {
  state.filterText = this.value;
  applyFilter();
  renderSidebar();
  updateSessionCount();

  // If filter changed, reset viewer state.
  if (state.filteredSessions.length > 0) {
    loadTranscript(0);
  } else {
    stopTranscriptStream();
    state.currentSession = null;
    state.transcript = null;
    state.selectedBlocks = {};
    state.sidebarIdx = -1;
    renderViewer();
    updateSelectionUI();
    updateSessionCount();
  }
});

function togglePin(sid) {
  var idx = state.pinnedSessions.indexOf(sid);
  if (idx >= 0) state.pinnedSessions.splice(idx, 1);
  else state.pinnedSessions.push(sid);
  savePinnedSessions();
  applyFilter();
  renderSidebar();
  updateSessionCount();
}

// Session click.
document.getElementById('session-list').addEventListener('click', function(e) {
  var pinBtn = e.target.closest('.pin-btn');
  if (pinBtn) {
    togglePin(pinBtn.getAttribute('data-session-id'));
    return;
  }
  var copyBtn = e.target.closest('.copy-path-btn');
  if (copyBtn) {
    var fp = copyBtn.getAttribute('data-file-path');
    var sid = copyBtn.getAttribute('data-session-id');
    var copyPath = function(path) {
      if (!path) return;
      navigator.clipboard.writeText(path).then(function() {
        copyBtn.classList.add('copied');
        setTimeout(function() { copyBtn.classList.remove('copied'); }, 1000);
      });
    };
    if (fp) {
      copyPath(fp);
      return;
    }
    fetch('/api/sessions/' + encodeURIComponent(sid) + '/path').then(function(res) {
      if (!res.ok) throw new Error('failed to resolve path');
      return res.json();
    }).then(function(data) {
      var resolved = (data && data.file_path) || '';
      if (!resolved) throw new Error('empty path');
      copyBtn.setAttribute('data-file-path', resolved);
      copyPath(resolved);
    }).catch(function() {
      copyBtn.title = 'Session file not found';
      setTimeout(function() { copyBtn.title = 'Copy file path'; }, 1500);
    });
    return;
  }
  var item = e.target.closest('.session-item');
  if (item) {
    var idx = parseInt(item.getAttribute('data-idx'), 10);
    loadTranscript(idx);
  }
});

// Drag-and-drop for pinned session reordering.
var sessionList = document.getElementById('session-list');

sessionList.addEventListener('dragstart', function(e) {
  var item = e.target.closest('.session-item[draggable="true"]');
  if (!item) return;
  dragSrcId = item.getAttribute('data-session-id');
  item.classList.add('dragging');
  e.dataTransfer.effectAllowed = 'move';
});

sessionList.addEventListener('dragover', function(e) {
  e.preventDefault();
  var item = e.target.closest('.session-item[draggable="true"]');
  if (!item || item.getAttribute('data-session-id') === dragSrcId) return;
  // Remove drag-over from all others.
  sessionList.querySelectorAll('.drag-over').forEach(function(el) { el.classList.remove('drag-over'); });
  item.classList.add('drag-over');
});

sessionList.addEventListener('dragleave', function(e) {
  var item = e.target.closest('.session-item');
  if (item) item.classList.remove('drag-over');
});

sessionList.addEventListener('dragend', function(e) {
  sessionList.querySelectorAll('.dragging, .drag-over').forEach(function(el) {
    el.classList.remove('dragging', 'drag-over');
  });
  dragSrcId = null;
});

sessionList.addEventListener('drop', function(e) {
  e.preventDefault();
  var target = e.target.closest('.session-item[draggable="true"]');
  if (!target || !dragSrcId) return;
  var targetId = target.getAttribute('data-session-id');
  if (targetId === dragSrcId) return;

  var srcIdx = state.pinnedSessions.indexOf(dragSrcId);
  var tgtIdx = state.pinnedSessions.indexOf(targetId);
  if (srcIdx < 0 || tgtIdx < 0) return;

  state.pinnedSessions.splice(srcIdx, 1);
  var newTgt = state.pinnedSessions.indexOf(targetId);
  state.pinnedSessions.splice(newTgt, 0, dragSrcId);

  savePinnedSessions();
  applyFilter();
  renderSidebar();
});

// Block checkbox toggle (delegated).
document.getElementById('viewer-content').addEventListener('change', function(e) {
  if (e.target.classList.contains('block-checkbox')) {
    var blockId = e.target.getAttribute('data-block-id');
    state.selectedBlocks[blockId] = e.target.checked;
    var block = e.target.closest('.chat-block');
    if (block) block.classList.toggle('block-checked', e.target.checked);
    updateSelectionUI();
  }
  if (e.target.classList.contains('group-checkbox')) {
    var ids = e.target.getAttribute('data-group-blocks').split(',');
    var checked = e.target.checked;
    for (var gi = 0; gi < ids.length; gi++) {
      state.selectedBlocks[ids[gi]] = checked;
    }
    var group = e.target.closest('.block-group');
    if (group) group.classList.toggle('block-checked', checked);
    updateSelectionUI();
  }
});

// Anchor link click (delegated).
document.getElementById('viewer-content').addEventListener('click', function(e) {
  var anchor = e.target.closest('.anchor-link');
  if (!anchor) return;
  e.preventDefault();
  e.stopPropagation();
  var path = anchor.getAttribute('data-anchor');
  var url = window.location.origin + path;
  history.replaceState(null, '', path);
  navigator.clipboard.writeText(url).then(function() { setStatus('Copied: ' + path); });
});

// Copy button (was "Dump").
document.getElementById('btn-dump').addEventListener('click', openCopyModal);

// Export button -> Publish.
document.getElementById('btn-export').addEventListener('click', openPublishModal);

// Clear button.
document.getElementById('btn-clear').addEventListener('click', clearAllSelections);

// Sort toggle.
document.getElementById('btn-sort').addEventListener('click', function() {
  state.roundOrder = state.roundOrder === 'asc' ? 'desc' : 'asc';
  this.textContent = state.roundOrder === 'asc' ? 'Oldest first' : 'Newest first';
  renderViewer();
  updateSelectionUI();
});

// Copy modal buttons.
var copyFormatBtns = document.querySelectorAll('#copy-modal .modal-options .modal-btn');
for (var fi = 0; fi < copyFormatBtns.length; fi++) {
  copyFormatBtns[fi].addEventListener('click', function() {
    state.copyFormat = this.getAttribute('data-format');
    updateCopyFormatButtons();
  });
}

document.getElementById('copy-confirm').addEventListener('click', doCopy);
document.getElementById('copy-cancel').addEventListener('click', closeCopyModal);

document.getElementById('copy-modal').addEventListener('click', function(e) {
  if (e.target === this) closeCopyModal();
});

// Publish modal buttons.
document.getElementById('publish-confirm').addEventListener('click', doPublish);
document.getElementById('publish-cancel').addEventListener('click', closePublishModal);

document.getElementById('publish-modal').addEventListener('click', function(e) {
  if (e.target === this) closePublishModal();
});

// Publish on Enter key in title input.
document.getElementById('publish-title').addEventListener('keydown', function(e) {
  if (e.key === 'Enter') doPublish();
});

// --- Helpers ---

function compactNum(n) {
  if (n === undefined || n === null) return '0';
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return String(n);
}

function formatShortTime(isoStr) {
  if (!isoStr) return '';
  var d = new Date(isoStr);
  if (isNaN(d.getTime())) return isoStr;
  var yyyy = d.getFullYear();
  var mm = String(d.getMonth() + 1).padStart(2, '0');
  var dd = String(d.getDate()).padStart(2, '0');
  var hh = String(d.getHours()).padStart(2, '0');
  var mi = String(d.getMinutes()).padStart(2, '0');
  var ss = String(d.getSeconds()).padStart(2, '0');
  return yyyy + '-' + mm + '-' + dd + ' ' + hh + ':' + mi + ':' + ss;
}

function formatTime(unixMs) {
  if (!unixMs) return '';
  var d = new Date(unixMs);
  var mm = String(d.getMonth() + 1).padStart(2, '0');
  var dd = String(d.getDate()).padStart(2, '0');
  var hh = String(d.getHours()).padStart(2, '0');
  var mi = String(d.getMinutes()).padStart(2, '0');
  return mm + '/' + dd + ' ' + hh + ':' + mi;
}

function truncate(str, max) {
  if (str.length <= max) return str;
  return str.substring(0, max) + '...';
}

function escapeHtml(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function splitSessionID(sessionID) {
  var idx = sessionID.indexOf(':');
  if (idx < 0) return { source: '', raw: sessionID };
  return { source: sessionID.substring(0, idx), raw: sessionID.substring(idx + 1) };
}

function roleKeyForDisplay(role) {
  if (role === 'claude' && state.currentSession && state.currentSession.source === 'codex') {
    return 'codex';
  }
  return role;
}

function roleLabelForDisplay(role) {
  var key = roleKeyForDisplay(role);
  return key.toUpperCase();
}

function rawSessionID(session) {
  var parts = splitSessionID(session.session_id || '');
  return parts.raw;
}

function buildSessionPath(session) {
  var source = (session && session.source) || splitSessionID(session.session_id || '').source || 'claude';
  var raw = rawSessionID(session);
  return '/' + encodeURIComponent(source) + '/' + encodeURIComponent(raw);
}

// --- Init ---

function initFromURL() {
  var path = window.location.pathname;
  if (path.length <= 1) return;
  var parts = path.substring(1).split('/');
  if (parts.length >= 2 && (parts[0] === 'claude' || parts[0] === 'codex') && parts[1]) {
    state._initialSessionSource = decodeURIComponent(parts[0]);
    state._initialRawSessionID = decodeURIComponent(parts[1]);
  } else if (parts.length >= 1 && parts[0]) {
    // Backward-compatible route: /source:raw
    state._initialSessionID = decodeURIComponent(parts[0]);
  }

  var roundPartIdx = (state._initialRawSessionID ? 2 : 1);
  if (parts.length >= roundPartIdx + 1 && parts[roundPartIdx] !== '') {
    var ri = parseInt(parts[roundPartIdx], 10);
    if (!isNaN(ri)) state._initialRoundIdx = ri;
  }
  if (parts.length >= roundPartIdx + 2 && parts[roundPartIdx + 1] !== '') {
    var bi = parseInt(parts[roundPartIdx + 1], 10);
    if (!isNaN(bi)) state._initialBlockIdx = bi;
  }
}

function scrollToTarget(roundIdx, blockIdx) {
  var roundEl = document.querySelector('.round[data-round-idx="' + roundIdx + '"]');
  if (!roundEl) return;

  // Ensure round is open.
  var roundBody = roundEl.querySelector('.round-body');
  if (roundBody && !roundBody.classList.contains('open')) {
    roundBody.classList.add('open');
    var ra = roundEl.querySelector('.round-header > .fold-arrow');
    if (ra) ra.classList.add('open');
  }

  var target = roundEl;

  if (blockIdx !== undefined) {
    var blockEl = roundEl.querySelector('[data-block-id="b-' + roundIdx + '-' + blockIdx + '"]');
    if (blockEl) {
      // Ensure containing group is open.
      var group = blockEl.closest('.block-group');
      if (group) {
        var gb = group.querySelector('.group-body');
        if (gb && !gb.classList.contains('open')) {
          gb.classList.add('open');
          var ga = group.querySelector('.group-header > .fold-arrow');
          if (ga) ga.classList.add('open');
        }
      }
      // Ensure block itself is open.
      var fb = blockEl.querySelector('.fold-body');
      if (fb && !fb.classList.contains('open')) {
        fb.classList.add('open');
        var ba = blockEl.querySelector('.fold-arrow');
        if (ba) ba.classList.add('open');
      }
      target = blockEl;
    }
  }

  target.scrollIntoView({block: 'start'});
}

initFromURL();
loadPinnedSessions();
startSessionStream();
