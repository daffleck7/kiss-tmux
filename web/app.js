// State
let ws = null;
let sessions = [];
let terminals = {};
let activeSession = null;
let mode = 'grid';
let contextMenuTarget = null;

// WebSocket
function connect() {
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(proto + '//' + location.host + '/ws');

  ws.onopen = function() {
    console.log('connected');
  };

  ws.onclose = function() {
    console.log('disconnected, reconnecting in 2s...');
    setTimeout(connect, 2000);
  };

  ws.onmessage = function(e) {
    var msg = JSON.parse(e.data);
    switch (msg.type) {
      case 'output':   handleOutput(msg); break;
      case 'exited':   handleExited(msg); break;
      case 'sessions': handleSessions(msg); break;
      case 'spawned':  handleSpawned(msg); break;
      case 'error':    handleError(msg); break;
    }
  };
}

function send(msg) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

// Message Handlers
function handleOutput(msg) {
  var t = terminals[msg.session];
  if (t) {
    t.term.write(msg.data);
  }
}

function handleExited(msg) {
  console.log('session exited:', msg.session, 'code:', msg.code);
}

function handleSessions(msg) {
  sessions = msg.sessions || [];

  sessions.forEach(function(s) {
    if (!terminals[s.id]) {
      createTerminal(s.id);
    }
  });

  var sessionIds = {};
  sessions.forEach(function(s) { sessionIds[s.id] = true; });
  Object.keys(terminals).forEach(function(id) {
    if (!sessionIds[id]) {
      destroyTerminal(id);
    }
  });

  render();
}

function handleSpawned(msg) {
  console.log('spawned:', msg.session);
}

function handleError(msg) {
  console.error('server error:', msg.error, msg.session || '');
}

// Terminal Management
function createTerminal(id) {
  var term = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: '"Cascadia Code", "Consolas", "Courier New", monospace',
    lineHeight: 1.2,
    letterSpacing: 0,
    theme: {
      background: '#1a1a2e',
      foreground: '#e0e0e0',
      cursor: '#e94560',
      selectionBackground: '#0f3460'
    },
    scrollback: 10000,
    allowTransparency: false,
    macOptionIsMeta: true,
    rightClickSelectsWord: false,
    windowsMode: true
  });

  var fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);

  term.onData(function(data) {
    send({ type: 'input', session: id, data: data });
  });

  // Open terminal into a persistent wrapper div ONCE — xterm.js
  // term.open() must only be called once per Terminal instance.
  var wrapper = document.createElement('div');
  wrapper.style.height = '100%';
  term.open(wrapper);

  // Load WebGL renderer for smooth scrolling and crisp text.
  // Falls back to default canvas/DOM renderer if WebGL is unavailable
  // (e.g., headless Chrome, software rendering, GPU context loss).
  try {
    if (window.WebglAddon && window.WebglAddon.WebglAddon) {
      var webgl = new WebglAddon.WebglAddon();
      webgl.onContextLoss(function() {
        console.warn('WebGL context lost for session', id, '— disposing addon');
        webgl.dispose();
      });
      term.loadAddon(webgl);
    }
  } catch (e) {
    console.warn('WebGL renderer failed, using default:', e);
  }

  // Ctrl+C: copy selection instead of sending SIGINT when text is selected.
  // Ctrl+V: read clipboard and paste into terminal.
  // When nothing is selected, Ctrl+C passes through as normal (SIGINT).
  term.attachCustomKeyEventHandler(function(e) {
    if (e.type !== 'keydown') return true;

    // Ctrl+C with selection → copy
    if (e.ctrlKey && !e.shiftKey && (e.key === 'c' || e.key === 'C') && term.hasSelection()) {
      e.preventDefault();
      var text = term.getSelection();
      if (navigator.clipboard) {
        navigator.clipboard.writeText(text).catch(function(err) {
          console.error('clipboard write failed:', err);
        });
      }
      return false;
    }

    // Ctrl+V → paste from clipboard
    if (e.ctrlKey && !e.shiftKey && (e.key === 'v' || e.key === 'V')) {
      e.preventDefault();
      if (navigator.clipboard) {
        navigator.clipboard.readText().then(function(text) {
          if (text) term.paste(text);
        }).catch(function(err) {
          console.error('clipboard read failed:', err);
        });
      }
      return false;
    }

    return true;
  });

  terminals[id] = { term: term, fitAddon: fitAddon, el: null, wrapper: wrapper };
}

function destroyTerminal(id) {
  var t = terminals[id];
  if (t) {
    t.term.dispose();
    if (t.wrapper.parentNode) {
      t.wrapper.parentNode.removeChild(t.wrapper);
    }
    delete terminals[id];
  }
}

function attachTerminal(id, container) {
  var t = terminals[id];
  if (!t) return;

  // Always re-append — the wrapper may have been detached by innerHTML=''
  t.el = container;
  container.appendChild(t.wrapper);
  t.fitAddon.fit();
  t.term.scrollToBottom();

  var cols = t.term.cols;
  var rows = t.term.rows;
  send({ type: 'resize', session: id, cols: cols, rows: rows });
}

// Rendering
function render() {
  if (mode === 'grid') {
    renderGrid();
  } else {
    renderFocus();
  }
}

function renderGrid() {
  document.getElementById('grid-view').classList.remove('hidden');
  document.getElementById('focus-view').classList.add('hidden');

  var container = document.getElementById('grid-container');
  container.innerHTML = '';
  container.dataset.count = sessions.length + 1;

  sessions.forEach(function(s) {
    var cell = document.createElement('div');
    cell.className = 'grid-cell';
    cell.dataset.session = s.id;

    var header = document.createElement('div');
    header.className = 'grid-cell-header';

    var dot = document.createElement('div');
    dot.className = 'dot ' + (s.running ? 'running' : 'stopped');
    header.appendChild(dot);

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = s.name;
    name.addEventListener('dblclick', function(e) {
      e.stopPropagation();
      startRename(s.id, name);
    });
    header.appendChild(name);

    header.addEventListener('contextmenu', function(e) {
      e.preventDefault();
      showContextMenu(e, s.id);
    });

    cell.appendChild(header);

    var termDiv = document.createElement('div');
    termDiv.className = 'grid-cell-terminal';
    cell.appendChild(termDiv);

    header.addEventListener('click', function() {
      switchToFocus(s.id);
    });

    container.appendChild(cell);

    requestAnimationFrame(function() {
      attachTerminal(s.id, termDiv);
    });
  });

  var newCell = document.createElement('button');
  newCell.className = 'grid-cell-new';
  newCell.textContent = '+ New Session';
  newCell.addEventListener('click', function() {
    showNewSessionDialog();
  });
  container.appendChild(newCell);
}

function renderFocus() {
  document.getElementById('grid-view').classList.add('hidden');
  document.getElementById('focus-view').classList.remove('hidden');

  var sidebar = document.getElementById('sidebar');
  sidebar.innerHTML = '';

  // Grid button — back to grid view
  var gridBtn = document.createElement('button');
  gridBtn.className = 'sidebar-tab sidebar-action';
  gridBtn.textContent = '\u2630'; // hamburger menu icon ☰
  gridBtn.title = 'Grid view';
  gridBtn.addEventListener('click', function() {
    switchToGrid();
  });
  sidebar.appendChild(gridBtn);

  // Session tabs
  sessions.forEach(function(s) {
    var tab = document.createElement('button');
    tab.className = 'sidebar-tab' + (s.id === activeSession ? ' active' : '');

    var abbr = s.name.substring(0, 6).toUpperCase();
    tab.textContent = abbr;
    tab.title = s.name;
    tab.dataset.name = s.name;

    var tabDot = document.createElement('div');
    tabDot.className = 'tab-dot ' + (s.running ? 'running' : 'stopped');
    tab.appendChild(tabDot);

    tab.addEventListener('click', function() {
      switchToFocus(s.id);
    });

    tab.addEventListener('contextmenu', function(e) {
      e.preventDefault();
      showContextMenu(e, s.id);
    });

    sidebar.appendChild(tab);
  });

  // Spacer pushes + button to bottom
  var spacer = document.createElement('div');
  spacer.style.flex = '1';
  sidebar.appendChild(spacer);

  // New session button
  var newBtn = document.createElement('button');
  newBtn.className = 'sidebar-tab sidebar-action';
  newBtn.textContent = '+';
  newBtn.title = 'New session';
  newBtn.addEventListener('click', function() {
    showNewSessionDialog();
  });
  sidebar.appendChild(newBtn);

  var focusDiv = document.getElementById('focus-terminal');
  focusDiv.innerHTML = '';

  if (activeSession && terminals[activeSession]) {
    attachTerminal(activeSession, focusDiv);
    terminals[activeSession].term.focus();
  }
}

// Mode Switching
function switchToFocus(sessionId) {
  activeSession = sessionId;
  mode = 'focus';
  render();
}

function switchToGrid() {
  mode = 'grid';
  render();
}


// New Session Dialog
function showNewSessionDialog() {
  document.getElementById('dialog-overlay').classList.remove('hidden');
  document.getElementById('dialog-name').value = '';
  document.getElementById('dialog-cwd').value = '';
  document.getElementById('dialog-cmd').value = 'claude --dangerously-skip-permissions';
  document.getElementById('dialog-name').focus();
}

function hideNewSessionDialog() {
  closeDirBrowser();
  document.getElementById('dialog-overlay').classList.add('hidden');
}

function createSessionFromDialog() {
  var name = document.getElementById('dialog-name').value.trim();
  var cwd = document.getElementById('dialog-cwd').value.trim();
  var cmd = document.getElementById('dialog-cmd').value.trim() || 'claude --dangerously-skip-permissions';

  send({ type: 'spawn', cmd: cmd, cwd: cwd, name: name });
  hideNewSessionDialog();
}

// Directory Browser
var dirBrowserPath = '';
var dirBrowserParent = '';

function openDirBrowser() {
  var startPath = document.getElementById('dialog-cwd').value.trim() || '';
  document.getElementById('dir-browser').classList.remove('hidden');
  fetchDirectory(startPath);
}

function closeDirBrowser() {
  document.getElementById('dir-browser').classList.add('hidden');
}

function fetchDirectory(path) {
  var url = '/api/browse';
  if (path) url += '?path=' + encodeURIComponent(path);

  fetch(url)
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (data.error) {
        console.error('browse error:', data.error);
        return;
      }
      dirBrowserPath = data.path;
      dirBrowserParent = data.parent;

      document.getElementById('dir-browser-path').textContent = data.path;

      var list = document.getElementById('dir-browser-list');
      list.innerHTML = '';

      // Show drive letters at root on Windows
      if (data.drives && data.drives.length > 0) {
        data.drives.forEach(function(drive) {
          var el = document.createElement('div');
          el.className = 'dir-entry drive';
          el.textContent = drive;
          el.addEventListener('click', function() {
            fetchDirectory(drive);
          });
          list.appendChild(el);
        });
      }

      var entries = data.entries || [];
      entries.forEach(function(entry) {
        var el = document.createElement('div');
        el.className = 'dir-entry';
        el.textContent = '\uD83D\uDCC1 ' + entry.name;
        el.addEventListener('click', function() {
          fetchDirectory(dirBrowserPath + '\\' + entry.name);
        });
        list.appendChild(el);
      });

      if (entries.length === 0 && (!data.drives || data.drives.length === 0)) {
        var empty = document.createElement('div');
        empty.className = 'dir-entry';
        empty.style.color = 'var(--text-dim)';
        empty.textContent = '(no subdirectories)';
        list.appendChild(empty);
      }
    })
    .catch(function(err) {
      console.error('browse fetch error:', err);
    });
}

function selectDirBrowser() {
  document.getElementById('dialog-cwd').value = dirBrowserPath;
  closeDirBrowser();
}

// Context Menu
function showContextMenu(e, sessionId) {
  contextMenuTarget = sessionId;
  var menu = document.getElementById('context-menu');
  menu.classList.remove('hidden');
  menu.style.left = e.clientX + 'px';
  menu.style.top = e.clientY + 'px';
}

function hideContextMenu() {
  document.getElementById('context-menu').classList.add('hidden');
  contextMenuTarget = null;
}

// Rename
function startRename(sessionId, nameEl) {
  var session = sessions.find(function(s) { return s.id === sessionId; });
  if (!session) return;

  var input = document.createElement('input');
  input.type = 'text';
  input.value = session.name;
  input.style.cssText = 'background:var(--bg);border:1px solid var(--accent);color:var(--text);font-size:12px;padding:0 4px;width:100%;border-radius:2px;';

  nameEl.textContent = '';
  nameEl.appendChild(input);
  input.focus();
  input.select();

  function finish() {
    var newName = input.value.trim();
    if (newName && newName !== session.name) {
      send({ type: 'rename', session: sessionId, name: newName });
    }
    nameEl.textContent = newName || session.name;
  }

  input.addEventListener('keydown', function(e) {
    if (e.key === 'Enter') { finish(); }
    if (e.key === 'Escape') { nameEl.textContent = session.name; }
    e.stopPropagation();
  });
  input.addEventListener('blur', finish);
}

// Escape key — only for closing dialog (doesn't conflict with terminal)
function setupKeyboard() {
  document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') {
      if (!document.getElementById('dialog-overlay').classList.contains('hidden')) {
        hideNewSessionDialog();
      }
    }
  });
}

// Copy/paste support for xterm.js terminals
function activeTerminal() {
  if (activeSession && terminals[activeSession]) return terminals[activeSession].term;
  if (sessions.length > 0 && terminals[sessions[0].id]) return terminals[sessions[0].id].term;
  return null;
}

function setupClipboard() {
  // Paste: Ctrl+V, Ctrl+Shift+V, or right-click paste delivers clipboard
  // to the active terminal via term.paste() (handles bracketed paste mode).
  document.addEventListener('paste', function(e) {
    // Don't intercept paste into dialog inputs
    if (e.target.tagName === 'INPUT') return;

    var term = activeTerminal();
    if (!term) return;

    var text = (e.clipboardData || window.clipboardData).getData('text');
    if (text) {
      e.preventDefault();
      term.paste(text);
    }
  });

  // Copy event: right-click → copy menu, or Ctrl+C when DOM has selection.
  document.addEventListener('copy', function(e) {
    if (e.target.tagName === 'INPUT') return;

    var term = activeTerminal();
    if (!term || !term.hasSelection()) return;

    e.clipboardData.setData('text/plain', term.getSelection());
    e.preventDefault();
  });

  // Ctrl+Shift+C: explicit copy of terminal selection (since Ctrl+C sends SIGINT)
  document.addEventListener('keydown', function(e) {
    if (e.target.tagName === 'INPUT') return;

    if (e.ctrlKey && e.shiftKey && (e.key === 'C' || e.key === 'c')) {
      var term = activeTerminal();
      if (!term || !term.hasSelection()) return;

      e.preventDefault();
      var text = term.getSelection();
      if (navigator.clipboard) {
        navigator.clipboard.writeText(text).catch(function(err) {
          console.error('clipboard write failed:', err);
        });
      }
    }
  });
}

// Window Resize
function handleResize() {
  if (mode === 'focus') {
    // Only resize the active terminal in focus mode
    if (activeSession && terminals[activeSession] && terminals[activeSession].el) {
      var t = terminals[activeSession];
      t.fitAddon.fit();
      send({ type: 'resize', session: activeSession, cols: t.term.cols, rows: t.term.rows });
    }
  } else {
    // Resize all visible terminals in grid mode
    Object.keys(terminals).forEach(function(id) {
      var t = terminals[id];
      if (t.el && t.wrapper.parentNode && t.wrapper.offsetParent !== null) {
        t.fitAddon.fit();
        send({ type: 'resize', session: id, cols: t.term.cols, rows: t.term.rows });
      }
    });
  }
}

// Init
function init() {
  document.getElementById('dialog-create').addEventListener('click', createSessionFromDialog);
  document.getElementById('dialog-cancel').addEventListener('click', hideNewSessionDialog);
  document.getElementById('dialog-browse').addEventListener('click', openDirBrowser);
  document.getElementById('dir-browser-up').addEventListener('click', function() {
    if (dirBrowserParent) fetchDirectory(dirBrowserParent);
  });
  document.getElementById('dir-browser-cancel').addEventListener('click', closeDirBrowser);
  document.getElementById('dir-browser-select').addEventListener('click', selectDirBrowser);
  document.getElementById('dialog-overlay').addEventListener('click', function(e) {
    if (e.target === this) hideNewSessionDialog();
  });

  document.getElementById('new-session-dialog').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      createSessionFromDialog();
    }
  });

  document.querySelectorAll('#context-menu .menu-item').forEach(function(item) {
    item.addEventListener('click', function() {
      var action = this.dataset.action;
      if (action === 'kill' && contextMenuTarget) {
        send({ type: 'kill', session: contextMenuTarget });
      }
      if (action === 'rename' && contextMenuTarget) {
        var nameEl = document.querySelector('[data-session="' + contextMenuTarget + '"] .name');
        if (nameEl) {
          startRename(contextMenuTarget, nameEl);
        }
      }
      hideContextMenu();
    });
  });

  document.addEventListener('click', hideContextMenu);

  window.addEventListener('resize', handleResize);

  setupKeyboard();
  setupClipboard();

  connect();
}

window.addEventListener('DOMContentLoaded', init);
