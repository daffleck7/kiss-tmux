// State
let ws = null;
let sessions = [];
let terminals = {};
let activeSession = null;
let mode = 'grid';
let contextMenuTarget = null;

// Default working directory for new local sessions (loaded from server config)
var DEFAULT_CWD = '';

// Machines: id '' is this Windows PC; others come from config.json "hosts".
var HOSTS = [{ id: '', label: 'Windows', color: '#3b82f6', default_cwd: '' }];
var activeHost = loadActiveHost();
var hostErrors = {};
var configLoaded = false;
var configLoading = false;

function loadActiveHost() {
  try { return localStorage.getItem('kiss-tmux.activeHost') || ''; } catch (e) { return ''; }
}

function saveActiveHost() {
  try { localStorage.setItem('kiss-tmux.activeHost', activeHost); } catch (e) {}
}

// Fetches /api/config once; a call while a fetch is in flight or after success is a no-op.
function loadConfig() {
  if (configLoaded || configLoading) return;
  configLoading = true;
  fetch('/api/config')
    .then(function(r) { return r.json(); })
    .then(function(cfg) {
      configLoading = false;
      configLoaded = true;
      if (cfg.default_cwd) DEFAULT_CWD = cfg.default_cwd;
      HOSTS = [{
        id: '',
        label: cfg.local_label || 'Windows',
        color: cfg.local_color || '#3b82f6',
        default_cwd: DEFAULT_CWD
      }];
      (cfg.hosts || []).forEach(function(h) {
        HOSTS.push({ id: h.id, label: h.label, color: h.color, default_cwd: h.default_cwd });
      });
      if (!HOSTS.some(function(h) { return h.id === activeHost; })) activeHost = '';
      // ws.onopen skipped discovery because the remote hosts weren't known yet.
      if (ws && ws.readyState === WebSocket.OPEN) discoverAllHosts();
      render();
    })
    .catch(function(err) {
      configLoading = false;
      console.error('config load failed:', err);
    });
}

// Asks the server to adopt existing sessions on every remote machine.
function discoverAllHosts() {
  HOSTS.forEach(function(h) {
    if (h.id) send({ type: 'discover', host: h.id });
  });
}

function hostById(id) {
  return HOSTS.find(function(h) { return h.id === id; }) || HOSTS[0];
}

// The machine actually shown; falls back to local until config lists the remembered host.
function currentHostId() {
  return hostById(activeHost).id;
}

function sessionHost(s) { return s.host || ''; }
function isRemote(s) { return !!s.host; }
function sessionState(s) { return s.state || (s.running ? 'running' : 'exited'); }

function visibleSessions() {
  var hostId = currentHostId();
  return sessions.filter(function(s) { return sessionHost(s) === hostId; });
}

function findSession(id) {
  return sessions.find(function(s) { return s.id === id; });
}

function dotClass(s) {
  var st = sessionState(s);
  if (st === 'running') return 'running';
  if (st === 'reconnecting') return 'reconnecting';
  return 'stopped';
}

// WebSocket
function connect() {
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(proto + '//' + location.host + '/ws');

  ws.onopen = function() {
    console.log('connected');
    if (!configLoaded) {
      // loadConfig's success handler sends discover once the hosts are known.
      loadConfig();
    } else {
      discoverAllHosts();
    }
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
      case 'host_ok':  handleHostOk(msg); break;
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
    // Throttle large replay chunks to avoid freezing the browser.
    // Split into 4KB pieces rendered across animation frames.
    var data = msg.data;
    if (data.length > 4096) {
      var offset = 0;
      var chunk = 4096;
      function writeChunk() {
        if (offset >= data.length) return;
        var end = Math.min(offset + chunk, data.length);
        t.term.write(data.substring(offset, end));
        offset = end;
        if (offset < data.length) {
          requestAnimationFrame(writeChunk);
        }
      }
      writeChunk();
    } else {
      t.term.write(data);
    }
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
  console.error('server error:', msg.error, msg.session || '', msg.host || '');
  // Typing into a dead session produces a stream of these; the dot already shows it.
  if (msg.session && /not running/.test(msg.error)) return;
  var hostId = msg.host || '';
  if (!msg.host && msg.session) {
    var s = findSession(msg.session);
    if (s) hostId = sessionHost(s);
  }
  hostErrors[hostId] = msg.error;
  render();
}

// A successful discovery means the machine is reachable again.
function handleHostOk(msg) {
  delete hostErrors[msg.host || ''];
  render();
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

// Never leave activeSession on a hidden machine: focus view, paste and copy would
// show or type into another machine's terminal under the current tab.
function normalizeActiveSession() {
  var visible = visibleSessions();
  if (!visible.some(function(s) { return s.id === activeSession; })) {
    activeSession = visible.length ? visible[0].id : null;
  }
  if (mode === 'focus' && !activeSession) mode = 'grid';
}

function render() {
  normalizeActiveSession();
  document.documentElement.style.setProperty('--host-color', hostById(activeHost).color);
  renderTabs();
  renderBanner();
  if (mode === 'grid') {
    renderGrid();
  } else {
    renderFocus();
  }
}

function renderTabs() {
  var bar = document.getElementById('host-tabs');
  bar.innerHTML = '';
  if (HOSTS.length < 2) {
    bar.classList.add('hidden');
    return;
  }
  bar.classList.remove('hidden');
  var currentId = currentHostId();
  HOSTS.forEach(function(h) {
    var count = sessions.filter(function(s) { return sessionHost(s) === h.id; }).length;
    var tab = document.createElement('button');
    tab.className = 'host-tab' + (h.id === currentId ? ' active' : '');
    tab.style.setProperty('--tab-color', h.color);
    var swatch = document.createElement('span');
    swatch.className = 'host-swatch';
    tab.appendChild(swatch);
    tab.appendChild(document.createTextNode(h.label.toUpperCase() + ' (' + count + ')'));
    if (hostErrors[h.id] && h.id !== currentId) {
      var alertEl = document.createElement('span');
      alertEl.className = 'host-tab-alert';
      alertEl.textContent = '!';
      alertEl.title = hostErrors[h.id];
      tab.appendChild(alertEl);
    }
    tab.addEventListener('click', function() { switchHost(h.id); });
    bar.appendChild(tab);
  });
}

function renderBanner() {
  var banner = document.getElementById('host-banner');
  var hostId = currentHostId();
  var text = hostErrors[hostId];
  banner.innerHTML = '';
  if (!text) {
    banner.classList.add('hidden');
    return;
  }
  banner.classList.remove('hidden');
  var span = document.createElement('span');
  span.textContent = text;
  banner.appendChild(span);
  var close = document.createElement('button');
  close.textContent = '\u00d7';
  close.title = 'Dismiss';
  close.addEventListener('click', function() {
    delete hostErrors[hostId];
    render();
  });
  banner.appendChild(close);
}

function switchHost(id) {
  activeHost = id;
  saveActiveHost();
  var hostId = currentHostId();
  if (hostId) {
    delete hostErrors[hostId];
    send({ type: 'discover', host: hostId });
  }
  normalizeActiveSession();
  render();
}

function renderGrid() {
  document.getElementById('grid-view').classList.remove('hidden');
  document.getElementById('focus-view').classList.add('hidden');

  var container = document.getElementById('grid-container');
  container.innerHTML = '';
  var visible = visibleSessions();
  container.dataset.count = visible.length + 1;

  visible.forEach(function(s) {
    var cell = document.createElement('div');
    cell.className = 'grid-cell';
    cell.dataset.session = s.id;

    var header = document.createElement('div');
    header.className = 'grid-cell-header';

    var dot = document.createElement('div');
    dot.className = 'dot ' + dotClass(s);
    header.appendChild(dot);

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = s.name;
    header.appendChild(name);

    var st = sessionState(s);
    if (isRemote(s) && st !== 'running') {
      var badge = document.createElement('span');
      badge.className = 'state-badge';
      badge.textContent = st === 'reconnecting' ? 'reconnecting\u2026' : st;
      header.appendChild(badge);
    }
    if (isRemote(s) && st === 'disconnected') {
      var reconnectBtn = document.createElement('button');
      reconnectBtn.className = 'reconnect-btn';
      reconnectBtn.textContent = 'Reconnect';
      reconnectBtn.addEventListener('click', function(e) {
        e.stopPropagation();
        send({ type: 'reconnect', session: s.id });
      });
      header.appendChild(reconnectBtn);
    }

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
  visibleSessions().forEach(function(s) {
    var tab = document.createElement('button');
    tab.className = 'sidebar-tab' + (s.id === activeSession ? ' active' : '');

    var abbr = s.name.substring(0, 6).toUpperCase();
    tab.textContent = abbr;
    var tabState = sessionState(s);
    var tabLabel = isRemote(s) && tabState !== 'running' ? s.name + ' (' + tabState + ')' : s.name;
    tab.title = tabLabel;
    tab.dataset.name = tabLabel;

    var tabDot = document.createElement('div');
    tabDot.className = 'tab-dot ' + dotClass(s);
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

  var current = activeSession ? findSession(activeSession) : null;
  if (current && isRemote(current) && sessionState(current) === 'disconnected') {
    focusDiv.appendChild(disconnectedBar(current));
  }

  // The terminal goes in its own flex child so the bar above it doesn't overflow.
  var termHost = document.createElement('div');
  termHost.className = 'focus-term-host';
  focusDiv.appendChild(termHost);

  if (activeSession && terminals[activeSession]) {
    attachTerminal(activeSession, termHost);
    terminals[activeSession].term.focus();
  }
}

// Bar shown above a disconnected remote session in focus view.
function disconnectedBar(s) {
  var bar = document.createElement('div');
  bar.className = 'disconnected-bar';
  var text = document.createElement('span');
  text.textContent = 'Disconnected from ' + hostById(s.host).label + '.';
  bar.appendChild(text);
  var btn = document.createElement('button');
  btn.className = 'reconnect-btn';
  btn.textContent = 'Reconnect';
  btn.addEventListener('click', function(e) {
    e.stopPropagation();
    send({ type: 'reconnect', session: s.id });
  });
  bar.appendChild(btn);
  return bar;
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
// Machine the open New Session dialog (and its folder browser) targets.
var dialogHost = '';

function showNewSessionDialog() {
  dialogHost = currentHostId();
  var host = hostById(dialogHost);
  var remote = !!host.id;
  document.getElementById('dialog-overlay').classList.remove('hidden');
  document.getElementById('dialog-title').textContent = 'New Session on ' + host.label;
  document.getElementById('dialog-name').value = '';
  document.getElementById('dialog-name').placeholder = remote ? 'optional, e.g. api-work' : 'optional';
  document.getElementById('dialog-cwd').value = remote ? (host.default_cwd || '') : DEFAULT_CWD;
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
  var msg = {
    type: 'spawn', cwd: cwd, name: name,
    cmd: document.getElementById('dialog-cmd').value.trim() ||
      'claude --dangerously-skip-permissions'
  };
  if (dialogHost) msg.host = dialogHost;
  send(msg);
  hideNewSessionDialog();
}

// Directory Browser
var dirBrowserPath = '';
var dirBrowserParent = '';
var browseSeq = 0;

function openDirBrowser() {
  var startPath = document.getElementById('dialog-cwd').value.trim() || '';
  document.getElementById('dir-browser').classList.remove('hidden');
  fetchDirectory(startPath);
}

function closeDirBrowser() {
  document.getElementById('dir-browser').classList.add('hidden');
}

function joinPath(base, name, sep) {
  return base.charAt(base.length - 1) === sep ? base + name : base + sep + name;
}

function fetchDirectory(path) {
  // Forget the previous listing so Select can't pick a stale (or other machine's) path.
  var seq = ++browseSeq;
  dirBrowserPath = '';
  dirBrowserParent = '';
  document.getElementById('dir-browser-path').textContent = '';

  var params = [];
  if (path) params.push('path=' + encodeURIComponent(path));
  if (dialogHost) params.push('host=' + encodeURIComponent(dialogHost));
  var url = '/api/browse' + (params.length ? '?' + params.join('&') : '');

  var list = document.getElementById('dir-browser-list');
  list.innerHTML = '';
  var loading = document.createElement('div');
  loading.className = 'dir-entry';
  loading.style.color = 'var(--text-dim)';
  loading.textContent = 'Loading\u2026';
  list.appendChild(loading);

  fetch(url)
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (seq !== browseSeq) return;
      list.innerHTML = '';
      if (data.error) {
        console.error('browse error:', data.error);
        var errEl = document.createElement('div');
        errEl.className = 'dir-entry';
        errEl.style.color = 'var(--accent)';
        errEl.textContent = data.error;
        list.appendChild(errEl);
        return;
      }
      var sep = data.sep || '\\';
      dirBrowserPath = data.path;
      dirBrowserParent = data.parent;

      document.getElementById('dir-browser-path').textContent = data.path;

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
          fetchDirectory(joinPath(dirBrowserPath, entry.name, sep));
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
      if (seq !== browseSeq) return;
      console.error('browse fetch error:', err);
      list.innerHTML = '';
      var failEl = document.createElement('div');
      failEl.className = 'dir-entry';
      failEl.style.color = 'var(--accent)';
      failEl.textContent = "Couldn't load folders";
      list.appendChild(failEl);
    });
}

function selectDirBrowser() {
  if (!dirBrowserPath) return;
  document.getElementById('dialog-cwd').value = dirBrowserPath;
  closeDirBrowser();
}

// Context Menu
function showContextMenu(e, sessionId) {
  contextMenuTarget = sessionId;
  var s = findSession(sessionId);
  var remote = !!(s && isRemote(s));
  var menu = document.getElementById('context-menu');
  menu.querySelectorAll('.local-only').forEach(function(el) {
    el.classList.toggle('hidden', remote);
  });
  menu.querySelectorAll('.remote-only').forEach(function(el) {
    el.classList.toggle('hidden', !remote);
  });
  if (remote) {
    var st = sessionState(s);
    menu.querySelector('[data-action="end"]').textContent =
      'End on ' + hostById(s.host).label + '\u2026';
    menu.querySelector('[data-action="reconnect"]').classList.toggle('hidden', st !== 'disconnected');
    menu.querySelector('[data-action="disconnect"]').classList.toggle('hidden',
      st === 'disconnected' || st === 'ended');
    menu.querySelector('[data-action="end"]').classList.toggle('hidden', st === 'ended');
  }
  menu.classList.remove('hidden');
  var left = e.clientX;
  var top = e.clientY;
  if (left + menu.offsetWidth > window.innerWidth) left = window.innerWidth - menu.offsetWidth - 4;
  if (top + menu.offsetHeight > window.innerHeight) top = window.innerHeight - menu.offsetHeight - 4;
  menu.style.left = Math.max(0, left) + 'px';
  menu.style.top = Math.max(0, top) + 'px';
}

function hideContextMenu() {
  document.getElementById('context-menu').classList.add('hidden');
  contextMenuTarget = null;
}

// Rename. A prompt works the same in grid and focus views and can't be wiped out by a
// re-render while typing.
function renameSession(sessionId) {
  var session = findSession(sessionId);
  if (!session) return;
  var newName = window.prompt('Rename session', session.name);
  if (newName === null) return;
  newName = newName.trim();
  if (newName && newName !== session.name) {
    send({ type: 'rename', session: sessionId, name: newName });
  }
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
  var current = activeSession ? findSession(activeSession) : null;
  if (current && sessionHost(current) === currentHostId() && terminals[activeSession]) {
    return terminals[activeSession].term;
  }
  var visible = visibleSessions();
  if (visible.length > 0 && terminals[visible[0].id]) return terminals[visible[0].id].term;
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
      if (action === 'disconnect' && contextMenuTarget) {
        send({ type: 'disconnect', session: contextMenuTarget });
      }
      if (action === 'reconnect' && contextMenuTarget) {
        send({ type: 'reconnect', session: contextMenuTarget });
      }
      if (action === 'end' && contextMenuTarget) {
        var target = findSession(contextMenuTarget);
        var csName = target && target.remote_name ? ' (cs-' + target.remote_name + ')' : '';
        if (target && confirm('Stop "' + target.name + '"' + csName + ' on ' +
            hostById(target.host).label +
            '? Claude will be shut down there, not just disconnected.')) {
          send({ type: 'end', session: contextMenuTarget });
        }
      }
      if (action === 'remove' && contextMenuTarget) {
        send({ type: 'remove', session: contextMenuTarget });
        if (activeSession === contextMenuTarget) {
          var removedId = contextMenuTarget;
          var next = visibleSessions().find(function(s) { return s.id !== removedId; });
          activeSession = next ? next.id : null;
          if (!next && mode === 'focus') mode = 'grid';
        }
      }
      if (action === 'rename' && contextMenuTarget) {
        renameSession(contextMenuTarget);
      }
      hideContextMenu();
    });
  });

  document.addEventListener('click', hideContextMenu);

  window.addEventListener('resize', handleResize);

  setupKeyboard();
  setupClipboard();

  loadConfig();
  connect();
}

window.addEventListener('DOMContentLoaded', init);
