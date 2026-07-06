// DataDock – client-side UI helpers

// Fetches a table/view's ready-to-use SQL (Select Top 1000 Rows / Script as
// INSERT / Script as UPDATE, generated server-side per dialect) and opens it
// in the SQL editor, mirroring SSMS's object-explorer quick actions. Used by
// onclick handlers in the sidebar and the table view toolbar.
function openTableScript(tableName, action) {
  fetch('/api/tables/' + encodeURIComponent(tableName) + '/script')
    .then(function (r) {
      if (!r.ok) { throw new Error('Could not load script for "' + tableName + '" (HTTP ' + r.status + ')'); }
      return r.json();
    })
    .then(function (script) {
      var sql = '', title = '', autoRun = false;
      if (action === 'top') {
        sql = script.selectTop; title = 'Top 1000 · ' + tableName; autoRun = true;
      } else if (action === 'insert') {
        sql = script.insertTmpl; title = 'Insert · ' + tableName;
      } else if (action === 'update') {
        sql = script.updateTmpl; title = 'Update · ' + tableName;
      }
      if (!sql) {
        window.alert('No ' + action + ' script is available for "' + tableName + '".');
        return;
      }
      openSQLInEditor(sql, title, autoRun);
    })
    .catch(function (err) {
      window.alert(err.message || String(err));
    });
}

// openSQLInEditor opens arbitrary SQL text in a new SQL Editor tab, reusing
// the same "#s=" shared-query hash the editor already understands. Used by
// openTableScript() above and by the table view's Structure/Definition
// panels ("Open in SQL Editor" buttons on a view's CREATE/ALTER statement).
function openSQLInEditor(sql, title, autoRun) {
  var payload = JSON.stringify({sql: sql, title: title, autoRun: !!autoRun});
  var encoded = encodeURIComponent(btoa(unescape(encodeURIComponent(payload))));
  var url = '/query#s=' + encoded;
  if (window.DataDock && typeof window.DataDock.navigateShellPage === 'function') {
    window.DataDock.navigateShellPage(url, true);
    return;
  }
  window.location.href = url;
}

(function () {
  var path = window.location.pathname;
  var root = document.documentElement;
  var mainContent = document.getElementById('mainContent');
  var tableNavigationSeq = 0;
  var shellPageCache = {};
  var shellActiveKey = '';

  // Track which table/view pages were opened, most-recent-first, so the
  // sidebar can show a "Recent" shortlist above the full Tables/Views tree.
  var recentTableMatch = path.match(/^\/t\/([^/]+)/);
  if (recentTableMatch) {
    recordRecentTable(decodeURIComponent(recentTableMatch[1]));
  }

  var allowedThemes = ['workbench', 'midnight', 'forest', 'contrast', 'solaris', 'xp', 'classic2000', 'kde'];
  var allowedDensity = ['comfortable', 'compact'];

  function storedValue(key, fallback, allowed) {
    try {
      var value = localStorage.getItem(key) || fallback;
      return allowed.indexOf(value) >= 0 ? value : fallback;
    } catch (e) {
      return fallback;
    }
  }

  function applyTheme(theme) {
    root.dataset.theme = theme;
    try { localStorage.setItem('datadock.theme', theme); } catch (e) {}
  }

  function applyDensity(density) {
    root.dataset.density = density;
    try { localStorage.setItem('datadock.density', density); } catch (e) {}
  }

  // The inline head script already resolved and applied the effective
  // theme/density (localStorage override, falling back to the server's
  // configured default), so reuse that instead of re-deriving a default here.
  var theme = storedValue('datadock.theme', root.dataset.theme || 'workbench', allowedThemes);
  var density = storedValue('datadock.density', root.dataset.density || 'comfortable', allowedDensity);
  applyTheme(theme);
  applyDensity(density);

  setActiveTableFromURL(new URL(window.location.href));

  setActiveTopNavFromURL(new URL(window.location.href));

  // Collapsible sidebar groups (SSMS-style object explorer: Database >
  // Schema > Tables/Views/Procedures), remembering each group's expand
  // state across page loads. Reused for both the static tinySQL tree
  // rendered by the server and the dynamic multi-database tree in
  // renderCatalogTree() below.
  document.querySelectorAll('.sidebar-group-header').forEach(wireSidebarGroupCollapse);

  var filter = document.getElementById('sidebarFilter');
  var filterCount = document.getElementById('sidebarFilterCount');
  if (filter) {
    filter.addEventListener('input', function () {
      var term = filter.value.trim();
      var termLower = term.toLowerCase();
      var total = 0, visible = 0;
      document.querySelectorAll('.table-row').forEach(function (row) {
        var link = row.querySelector('.table-link');
        var name = (link && (link.dataset.name || link.textContent)) || '';
        var match = !termLower || name.toLowerCase().indexOf(termLower) !== -1;
        row.hidden = !match;
        total++;
        if (match) visible++;
        var textEl = link && link.querySelector('.tl-text');
        if (textEl) {
          textEl.innerHTML = term ? highlightSidebarMatch(name, term) : escapeHTMLText(name);
        }
      });
      // A group is visible iff it (transitively) contains a visible row;
      // while filtering, force-expand groups with a match so results show
      // even if the user had collapsed that group earlier.
      document.querySelectorAll('.sidebar-group').forEach(function (group) {
        var anyVisible = !!group.querySelector('.table-row:not([hidden])');
        group.hidden = !!term && !anyVisible;
        var body = group.querySelector(':scope > .sidebar-group-body');
        var header = group.querySelector(':scope > .sidebar-group-header');
        if (term && anyVisible && body && header) {
          body.hidden = false;
          header.setAttribute('aria-expanded', 'true');
          header.classList.remove('collapsed');
        }
      });
      if (filterCount) filterCount.textContent = term ? (visible + ' / ' + total) : '';
    });
    filter.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && filter.value) {
        filter.value = '';
        filter.dispatchEvent(new Event('input', {bubbles: true}));
      }
    });
  }

  // "/" focuses the sidebar filter from anywhere on the page (unless the
  // user is already typing somewhere else), mirroring GitHub/Slack-style
  // quick-find shortcuts. Escape (wired above) clears it again.
  document.addEventListener('keydown', function (e) {
    if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
    var tag = (e.target && e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || (e.target && e.target.isContentEditable)) return;
    if (!filter) return;
    e.preventDefault();
    filter.focus();
    filter.select();
  });

  // Table pages are the highest-traffic navigation path in DataDock. Load
  // them into the existing shell so opening tables, sorting columns, and
  // paging rows does not rebuild the sidebar or reload the whole document.
  document.addEventListener('click', function (e) {
    var link = e.target && e.target.closest ? e.target.closest('a.table-link, .nav-primary a.table-nav-tab, #mainContent a') : null;
    if (!shouldHandleTableNavigation(e, link)) return;
    var url = new URL(link.href, window.location.origin);
    if (!isTablePageURL(url)) return;
    e.preventDefault();
    e.stopPropagation();
    navigateContentPage(url, true);
  }, true);

  document.addEventListener('click', function (e) {
    var link = e.target && e.target.closest ? e.target.closest('.nav-primary a.btn-nav, .app-nav .dropdown-item, #mainContent a') : null;
    if (!shouldHandleShellNavigation(e, link)) return;
    var url = new URL(link.href, window.location.origin);
    if (!isShellPageURL(url)) return;
    e.preventDefault();
    navigateContentPage(url, true);
  });

  window.addEventListener('popstate', function () {
    var url = new URL(window.location.href);
    if (isContentPageURL(url)) {
      navigateContentPage(url, false);
      return;
    }
    window.location.reload();
  });

  window.DataDock = window.DataDock || {};
  window.DataDock.navigateTablePage = function (urlLike) {
    var url = new URL(urlLike, window.location.origin);
    if (!isTablePageURL(url)) {
      window.location.href = url.href;
      return false;
    }
    navigateContentPage(url, true);
    return true;
  };
  window.DataDock.navigateShellPage = function (urlLike, pushHistory) {
    var url = new URL(urlLike, window.location.origin);
    if (!isShellPageURL(url)) {
      window.location.href = url.href;
      return false;
    }
    navigateContentPage(url, pushHistory !== false);
    return true;
  };
  window.DataDock.refreshSidebar = function () {
    refreshSidebarTree();
  };

  // Collapse/expand the whole sidebar to reclaim width on smaller screens
  // or when the SQL editor/data grid needs more room.
  var collapseBtn = document.getElementById('sidebarCollapseBtn');
  var appBody = document.querySelector('.app-body');
  if (collapseBtn && appBody) {
    var sidebarHidden = false;
    try { sidebarHidden = localStorage.getItem('datadock.sidebar.hidden') === '1'; } catch (e) {}
    setSidebarHidden(appBody, collapseBtn, sidebarHidden);
    collapseBtn.addEventListener('click', function () {
      var nowHidden = !appBody.classList.contains('sidebar-collapsed');
      setSidebarHidden(appBody, collapseBtn, nowHidden);
      try { localStorage.setItem('datadock.sidebar.hidden', nowHidden ? '1' : '0'); } catch (e) {}
    });
  }

  // Manual refresh: table/view lists are otherwise only current as of the
  // last full page load, so creating/dropping objects elsewhere (e.g. the
  // SQL editor) wouldn't show up in the sidebar without this.
  var refreshBtn = document.getElementById('sidebarRefreshBtn');
  if (refreshBtn) {
    refreshBtn.addEventListener('click', function () { refreshSidebarTree(refreshBtn); });
  }

  renderRecentTables();
  loadCatalogTree();

  var themeSelect = document.getElementById('themeSelect');
  if (themeSelect) {
    themeSelect.value = theme;
    themeSelect.addEventListener('change', function () {
      applyTheme(themeSelect.value);
    });
  }

  var densitySelect = document.getElementById('densitySelect');
  if (densitySelect) {
    densitySelect.value = density;
    densitySelect.addEventListener('change', function () {
      applyDensity(densitySelect.value);
    });
  }

  function shouldHandleTableNavigation(e, link) {
    if (!link || !mainContent) return false;
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return false;
    if (link.target && link.target !== '_self') return false;
    if (link.hasAttribute('download')) return false;
    return true;
  }

  function shouldHandleShellNavigation(e, link) {
    if (!link || !mainContent) return false;
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return false;
    if (link.target && link.target !== '_self') return false;
    if (link.hasAttribute('download')) return false;
    return true;
  }

  function isContentPageURL(url) {
    return isTablePageURL(url) || isShellPageURL(url);
  }

  function isTablePageURL(url) {
    return url.origin === window.location.origin && /^\/t\/[^/]+\/?$/.test(url.pathname);
  }

  function isShellPageURL(url) {
    return url.origin === window.location.origin && shellPageKey(url) !== '';
  }

  function shellPageKey(url) {
    if (['/query', '/history', '/create-table', '/import', '/export', '/connections', '/guide', '/about'].indexOf(url.pathname) === -1) return '';
    return url.pathname + url.search;
  }

  function tableNameFromURL(url) {
    var match = url.pathname.match(/^\/t\/([^/]+)\/?$/);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function navigateContentPage(url, pushHistory) {
    if (!mainContent) {
      window.location.href = url.href;
      return;
    }
    var shellKey = shellPageKey(url);
    if (shellKey && shellPageCache[shellKey]) {
      showCachedShellPage(url, shellKey, pushHistory);
      return;
    }
    var seq = ++tableNavigationSeq;
    cacheCurrentShellPage();
    mainContent.classList.add('is-loading');
    mainContent.setAttribute('aria-busy', 'true');

    fetch(url.href, {headers: {'X-Requested-With': 'fetch'}})
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.text();
      })
      .then(function (html) {
        if (seq !== tableNavigationSeq) return;
        var doc = new DOMParser().parseFromString(html, 'text/html');
        var nextMain = doc.getElementById('mainContent');
        if (!nextMain) throw new Error('No main content in response');

        if (doc.title) document.title = doc.title;
        if (pushHistory && url.href !== window.location.href) history.pushState({datadockContentPage: true}, '', url.href);

        if (shellKey) {
          var pane = document.createElement('div');
          pane.className = 'main-pane';
          pane.dataset.pageKey = shellKey;
          pane.innerHTML = nextMain.innerHTML;
          shellPageCache[shellKey] = {pane: pane, title: doc.title || document.title, url: url.href};
          shellActiveKey = shellKey;
          mainContent.replaceChildren(pane);
          executeScripts(pane);
        } else {
          shellActiveKey = '';
          mainContent.innerHTML = nextMain.innerHTML;
          executeScripts(mainContent);
        }

        var tableName = tableNameFromURL(url);
        if (tableName) recordRecentTable(tableName);
        setActiveTableFromURL(url);
        setActiveTopNavFromURL(url);
        renderRecentTables();
        if (tableName && !sidebarHasTable(tableName)) refreshSidebarTree();
        runPageActivation(url);

        mainContent.scrollTop = 0;
        mainContent.focus({preventScroll: true});
      })
      .catch(function () {
        if (seq !== tableNavigationSeq) return;
        window.location.href = url.href;
      })
      .finally(function () {
        if (seq !== tableNavigationSeq) return;
        mainContent.classList.remove('is-loading');
        mainContent.removeAttribute('aria-busy');
      });
  }

  function cacheCurrentShellPage() {
    var currentURL = new URL(window.location.href);
    var key = shellActiveKey || shellPageKey(currentURL);
    if (!key || !mainContent) return;
    if (shellPageCache[key] && shellPageCache[key].pane.parentNode === mainContent) return;

    var pane = document.createElement('div');
    pane.className = 'main-pane';
    pane.dataset.pageKey = key;
    while (mainContent.firstChild) {
      pane.appendChild(mainContent.firstChild);
    }
    shellPageCache[key] = {pane: pane, title: document.title, url: currentURL.href};
    shellActiveKey = key;
    mainContent.appendChild(pane);
  }

  function showCachedShellPage(url, key, pushHistory) {
    cacheCurrentShellPage();
    var cached = shellPageCache[key];
    if (!cached) return;
    if (pushHistory && url.href !== window.location.href) history.pushState({datadockShellPage: true}, '', url.href);
    shellActiveKey = key;
    document.title = cached.title || document.title;
    mainContent.replaceChildren(cached.pane);
    setActiveTableFromURL(url);
    setActiveTopNavFromURL(url);
    runPageActivation(url);
    mainContent.scrollTop = 0;
    mainContent.focus({preventScroll: true});
  }

  function runPageActivation(url) {
    if (url.pathname === '/query') {
      setTimeout(function () {
        if (typeof restoreQueryFromHash === 'function' && url.hash) restoreQueryFromHash();
        if (typeof sqlEditor !== 'undefined' && sqlEditor && typeof sqlEditor.layout === 'function') {
          sqlEditor.layout();
        }
      }, 0);
    } else if (url.pathname === '/history') {
      setTimeout(function () {
        if (typeof renderLocalQueryHistory === 'function') renderLocalQueryHistory();
      }, 0);
    }
  }

  function sidebarHasTable(name) {
    var wanted = String(name || '').toLowerCase();
    if (!wanted) return false;
    var found = false;
    document.querySelectorAll('.sidebar .table-link').forEach(function (a) {
      var linkURL;
      try {
        linkURL = new URL(a.getAttribute('href') || '', window.location.origin);
      } catch (e) {
        linkURL = null;
      }
      var linkMatch = linkURL && linkURL.pathname.match(/^\/t\/([^/]+)\/?$/);
      var linkName = linkMatch ? decodeURIComponent(linkMatch[1]) : '';
      if (linkName.toLowerCase() === wanted) found = true;
    });
    return found;
  }

  function executeScripts(container) {
    var originalAddEventListener = document.addEventListener;
    if (document.readyState !== 'loading') {
      document.addEventListener = function (type, listener, options) {
        if (type === 'DOMContentLoaded' && typeof listener === 'function') {
          setTimeout(function () { listener.call(document, new Event('DOMContentLoaded')); }, 0);
          return;
        }
        return originalAddEventListener.call(document, type, listener, options);
      };
    }
    try {
      container.querySelectorAll('script').forEach(function (oldScript) {
        var script = document.createElement('script');
        Array.prototype.forEach.call(oldScript.attributes, function (attr) {
          script.setAttribute(attr.name, attr.value);
        });
        script.text = oldScript.textContent || '';
        oldScript.replaceWith(script);
      });
    } finally {
      document.addEventListener = originalAddEventListener;
    }
  }
})();

function setActiveTableFromURL(url) {
  var activeName = '';
  var match = url.pathname.match(/^\/t\/([^/]+)\/?$/);
  if (match) activeName = decodeURIComponent(match[1]);

  document.querySelectorAll('#catalogTreeRoot, #sidebarStaticRoot').forEach(function (root) {
    root.dataset.activeTable = activeName;
  });

  document.querySelectorAll('.sidebar .table-link').forEach(function (a) {
    var linkURL;
    try {
      linkURL = new URL(a.getAttribute('href') || '', window.location.origin);
    } catch (e) {
      linkURL = null;
    }
    var linkMatch = linkURL && linkURL.pathname.match(/^\/t\/([^/]+)\/?$/);
    var linkName = linkMatch ? decodeURIComponent(linkMatch[1]) : '';
    a.classList.toggle('active', !!activeName && linkName.toLowerCase() === activeName.toLowerCase());
  });
}

function setActiveTopNavFromURL(url) {
  var path = url.pathname;
  var activeSection = navSectionForPath(path);
  updateTableNavTab(url);
  document.querySelectorAll('.app-nav .btn-nav').forEach(function (a) {
    a.classList.remove('active');
    if (a.hasAttribute('aria-selected')) a.setAttribute('aria-selected', 'false');
  });
  document.querySelectorAll('.app-nav .dropdown-item').forEach(function (a) {
    a.classList.remove('active');
  });

  document.querySelectorAll('.app-nav .btn-nav').forEach(function (a) {
    var href = a.getAttribute('href');
    var section = a.dataset.navSection || '';
    if ((section && section === activeSection) || (!section && href && href !== '/' && (path === href || path.indexOf(href + '/') === 0))) {
      a.classList.add('active');
      if (a.hasAttribute('aria-selected')) a.setAttribute('aria-selected', 'true');
    }
  });
  document.querySelectorAll('.app-nav .dropdown-item').forEach(function (a) {
    var href = a.getAttribute('href');
    if (href && href !== '/' && (path === href || path.indexOf(href + '/') === 0)) {
      a.classList.add('active');
      var menu = a.closest('.dropdown');
      if (menu) {
        var button = menu.querySelector('.btn-nav');
        if (button) {
          button.classList.add('active');
          if (button.hasAttribute('aria-selected')) button.setAttribute('aria-selected', 'true');
        }
      }
    }
  });
}

function navSectionForPath(path) {
  if (path === '/query' || path.indexOf('/query/') === 0) return 'query';
  if (path === '/history' || path.indexOf('/history/') === 0) return 'history';
  if (path === '/create-table' || path.indexOf('/create-table/') === 0) return 'manage';
  if (path === '/import' || path === '/export' || path.indexOf('/import/') === 0 || path.indexOf('/export/') === 0) return 'transfer';
  if (path === '/connections' || path.indexOf('/connections/') === 0) return 'connections';
  if (/^\/t\/[^/]+\/?$/.test(path)) return 'table';
  return '';
}

function updateTableNavTab(url) {
  var tab = document.querySelector('.nav-primary .table-nav-tab');
  if (!tab) return;
  var match = url.pathname.match(/^\/t\/([^/]+)\/?$/);
  if (!match) {
    tab.classList.add('d-none');
    tab.setAttribute('href', '#');
    tab.setAttribute('title', 'Current table');
    var emptyLabel = tab.querySelector('span');
    if (emptyLabel) emptyLabel.textContent = 'Preview';
    return;
  }
  var tableName = decodeURIComponent(match[1]);
  tab.classList.remove('d-none');
  tab.setAttribute('href', '/t/' + encodeURIComponent(tableName));
  tab.setAttribute('title', tableName);
  var label = tab.querySelector('span');
  if (label) label.textContent = 'Preview';
}

// wireSidebarGroupCollapse attaches expand/collapse behavior to a
// .sidebar-group-header button, persisting the state in localStorage by
// header.dataset.group (which must be unique across the whole tree — the
// catalog tree builder below includes the database/schema path in it).
function wireSidebarGroupCollapse(header) {
  // The body is always the header's next sibling (both the server-rendered
  // template and makeSidebarGroup() build them that way), so look it up
  // structurally instead of via document.getElementById(). The dynamic
  // catalog tree wires collapse behavior on detached nodes before they're
  // inserted into the page, and getElementById() only ever finds elements
  // that are already attached to the live document — it would silently
  // return null there and this whole function would no-op.
  var body = header.nextElementSibling;
  if (!body || !body.classList.contains('sidebar-group-body')) {
    body = header.dataset.target ? document.getElementById(header.dataset.target) : null;
  }
  if (!body) return;
  var key = 'datadock.sidebar.collapsed.' + (header.dataset.group || header.dataset.target);
  // Folders start collapsed (SSMS-style) until the user has explicitly
  // expanded or collapsed one — only an explicit stored '0' keeps it open.
  var collapsed = true;
  try {
    var stored = localStorage.getItem(key);
    if (stored !== null) collapsed = stored === '1';
  } catch (e) {}
  setSidebarGroupCollapsed(header, body, collapsed);
  header.addEventListener('click', function () {
    var nowCollapsed = !body.hidden;
    setSidebarGroupCollapsed(header, body, nowCollapsed);
    try { localStorage.setItem(key, nowCollapsed ? '1' : '0'); } catch (e) {}
  });
}

// escapeHTMLText/highlightSidebarMatch render a table name back into its
// .tl-text span, wrapping the matched substring in <mark> while filtering —
// using the DOM (not string concatenation) to escape safely.
function escapeHTMLText(s) {
  var div = document.createElement('div');
  div.textContent = s == null ? '' : String(s);
  return div.innerHTML;
}

function highlightSidebarMatch(name, term) {
  var escaped = escapeHTMLText(name);
  var escapedTerm = String(term).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  if (!escapedTerm) return escaped;
  try {
    return escaped.replace(new RegExp('(' + escapedTerm + ')', 'ig'), '<mark>$1</mark>');
  } catch (e) {
    return escaped;
  }
}

function setSidebarHidden(appBody, btn, hidden) {
  appBody.classList.toggle('sidebar-collapsed', hidden);
  btn.setAttribute('aria-expanded', String(!hidden));
  btn.setAttribute('aria-label', hidden ? 'Show sidebar' : 'Hide sidebar');
  btn.title = hidden ? 'Show the Tables & Views sidebar' : 'Hide the Tables & Views sidebar';
}

// refreshSidebarTree re-fetches and re-renders the sidebar tree on demand
// (e.g. after creating/dropping a table elsewhere) without a full page
// reload. The static tinySQL sidebar is promoted to the same dynamic
// catalog-tree container used for managed SQL connections so both paths
// share one refresh implementation.
function refreshSidebarTree(btn) {
  var container = document.getElementById('catalogTreeRoot') || document.getElementById('sidebarStaticRoot');
  if (!container) return;
  if (container.id === 'sidebarStaticRoot') {
    container.id = 'catalogTreeRoot';
  }
  if (btn) btn.classList.add('is-loading');
  var done = function () { if (btn) btn.classList.remove('is-loading'); };
  var result = loadCatalogTree();
  if (result && typeof result.finally === 'function') {
    result.finally(done);
  } else {
    setTimeout(done, 400);
  }
}

// ── Recently viewed tables/views ──────────────────────────────────────────

var recentTablesKey = 'datadock.recentTables';
var maxRecentTables = 8;

function loadRecentTables() {
  try {
    var list = JSON.parse(localStorage.getItem(recentTablesKey) || '[]');
    return Array.isArray(list) ? list : [];
  } catch (e) {
    return [];
  }
}

function recordRecentTable(name) {
  if (!name) return;
  try {
    var list = loadRecentTables().filter(function (n) { return n.toLowerCase() !== name.toLowerCase(); });
    list.unshift(name);
    localStorage.setItem(recentTablesKey, JSON.stringify(list.slice(0, maxRecentTables)));
  } catch (e) {}
}

function removeRecentTable(name) {
  try {
    var list = loadRecentTables().filter(function (n) { return n.toLowerCase() !== name.toLowerCase(); });
    localStorage.setItem(recentTablesKey, JSON.stringify(list));
  } catch (e) {}
  renderRecentTables();
}

// renderRecentTables draws the "Recent" folder above the Tables/Views tree.
// It works the same way regardless of whether the rest of the sidebar is
// the static tinySQL render or the async multi-database catalog tree, since
// it only needs localStorage and reuses makeSidebarGroup() from the catalog
// tree builder below.
function renderRecentTables() {
  var root = document.getElementById('sidebarRecentRoot');
  if (!root) return;
  var list = loadRecentTables();
  root.innerHTML = '';
  if (list.length === 0) return;
  var activeRoot = document.getElementById('catalogTreeRoot') || document.getElementById('sidebarStaticRoot');
  var activeName = activeRoot ? activeRoot.dataset.activeTable : '';

  var g = makeSidebarGroup('bi-clock-history', 'Recent', list.length, 'recent');
  list.forEach(function (name) {
    var row = document.createElement('div');
    row.className = 'table-row';

    var link = document.createElement('a');
    link.className = 'table-link' + (name === activeName ? ' active' : '');
    link.href = '/t/' + encodeURIComponent(name);
    link.dataset.name = name;
    link.appendChild(sidebarTextNode('i', 'bi bi-clock-history'));
    link.appendChild(sidebarTextNode('span', 'tl-text', name));
    row.appendChild(link);

    var actions = document.createElement('span');
    actions.className = 'table-quick-actions';
    var topBtn = document.createElement('button');
    topBtn.type = 'button';
    topBtn.className = 'qa-btn';
    topBtn.title = 'Select Top 1000 Rows';
    topBtn.appendChild(sidebarTextNode('i', 'bi bi-lightning-charge'));
    topBtn.addEventListener('click', function (e) { e.preventDefault(); openTableScript(name, 'top'); });
    actions.appendChild(topBtn);

    var removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'qa-btn remove-recent-btn';
    removeBtn.title = 'Remove from recent';
    removeBtn.appendChild(sidebarTextNode('i', 'bi bi-x-lg'));
    removeBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      removeRecentTable(name);
    });
    actions.appendChild(removeBtn);

    row.appendChild(actions);
    g.body.appendChild(row);
  });
  root.appendChild(g.group);
}

function setSidebarGroupCollapsed(header, body, collapsed) {
  body.hidden = collapsed;
  header.setAttribute('aria-expanded', String(!collapsed));
  header.classList.toggle('collapsed', collapsed);
}

// ── Multi-database catalog tree (Database > Schema > Tables/Views/Procedures) ──
//
// Used for every managed SQL connection (PostgreSQL/MySQL/SQL Server): the
// server can't cheaply compute this on every page render (PostgreSQL needs a
// separate connection per other database, MySQL/SQL Server need extra
// queries), so it's fetched once client-side and rendered here instead of by
// the Go template.

var catalogKindMeta = {
  table: {icon: 'bi-table', label: 'Tables', actions: ['top', 'insert', 'update']},
  view: {icon: 'bi-eye', label: 'Views', actions: ['top']},
  procedure: {icon: 'bi-gear', label: 'Procedures', actions: []},
  function: {icon: 'bi-gear', label: 'Functions', actions: []}
};

function loadCatalogTree() {
  var root = document.getElementById('catalogTreeRoot');
  if (!root) return null;
  var ctx = {
    activeTable: root.dataset.activeTable || '',
    dialect: root.dataset.dialect || ''
  };
  return fetch('/api/catalog')
    .then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(function (databases) {
      root.innerHTML = '';
      if (!databases || databases.length === 0) {
        root.appendChild(sidebarTextNode('div', 'sidebar-empty', 'No tables or views yet.'));
        return;
      }
      var multiDB = databases.length > 1;
      databases.forEach(function (db) {
        root.appendChild(renderCatalogDatabase(db, ctx, multiDB));
      });
    })
    .catch(function (err) {
      root.innerHTML = '';
      root.appendChild(sidebarTextNode('div', 'sidebar-empty text-danger', 'Could not load catalog: ' + (err.message || err)));
    });
}

// qualifiedCatalogName builds the identifier to send to /t/{name} and the
// script/quick-action APIs, matching what the Go backend expects to resolve
// a table/view — which differs by dialect and by whether the object lives in
// the connection's own default database:
//   - PostgreSQL: unqualified in the "public" schema of the CURRENT database,
//     "schema.table" for another schema of the current database. Another
//     database entirely can't be reached at all (PostgreSQL has no
//     cross-database queries), so those are marked not navigable.
//   - MySQL: unqualified within the current database; "otherdb.table" for
//     another database (MySQL can query across databases on one connection).
//   - SQL Server: always "schema.table" within the current database (matches
//     existing behavior), "otherdb.schema.table" for another database
//     (SQL Server supports three-part cross-database names on one connection).
//   - SQLite/tinySQL: always unqualified (single database, no schemas).
function qualifiedCatalogName(ctx, dbInfo, schemaName, itemName) {
  var isCurrentDB = !!dbInfo.current;
  switch (ctx.dialect) {
    case 'postgres':
      if (!isCurrentDB) return {name: itemName, navigable: false};
      return {name: (schemaName && schemaName !== 'public') ? (schemaName + '.' + itemName) : itemName, navigable: true};
    case 'mysql':
    case 'mariadb':
      return {name: isCurrentDB ? itemName : (dbInfo.name + '.' + itemName), navigable: true};
    case 'mssql':
    case 'sqlserver':
      return {name: isCurrentDB ? (schemaName + '.' + itemName) : (dbInfo.name + '.' + schemaName + '.' + itemName), navigable: true};
    default:
      return {name: itemName, navigable: true};
  }
}

function sidebarTextNode(tag, className, text) {
  var el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

var sidebarGroupSeq = 0;

// makeSidebarGroup builds a collapsible <div class="sidebar-group"> with the
// given icon/label/count header, wires its collapse behavior (persisted
// under groupKey), and returns {group, body} so the caller can append
// children into body. Nesting indentation comes from the cascading
// .sidebar-group-body { padding-left } CSS rule, not from any depth math
// here — however many levels get nested just stack automatically.
function makeSidebarGroup(icon, label, count, groupKey) {
  var group = document.createElement('div');
  group.className = 'sidebar-group';
  var header = document.createElement('button');
  header.type = 'button';
  header.className = 'sidebar-group-header';
  header.dataset.group = groupKey;
  var bodyID = 'sg-' + (sidebarGroupSeq++);
  header.dataset.target = bodyID;
  var caret = sidebarTextNode('i', 'bi bi-chevron-down sidebar-group-caret');
  header.appendChild(caret);
  if (icon) header.appendChild(sidebarTextNode('i', 'bi ' + icon + ' me-1'));
  header.appendChild(sidebarTextNode('span', null, label));
  if (count !== undefined && count !== null) {
    header.appendChild(sidebarTextNode('span', 'sidebar-group-count', String(count)));
  }
  var body = document.createElement('div');
  body.className = 'sidebar-group-body';
  body.id = bodyID;
  group.appendChild(header);
  group.appendChild(body);
  wireSidebarGroupCollapse(header);
  return {group: group, header: header, body: body};
}

function renderCatalogDatabase(db, ctx, showDatabaseLevel) {
  if (!showDatabaseLevel) {
    return renderCatalogSchemas(db, ctx);
  }
  var totalCount = (db.schemas || []).reduce(function (n, s) {
    return n + (s.tables || []).length + (s.views || []).length + (s.procedures || []).length;
  }, 0);
  var g = makeSidebarGroup('bi-hdd-stack', db.name || '(default)', db.needsFetch ? '' : totalCount, 'db/' + db.name);
  if (db.current) g.header.classList.add('sidebar-group-current');

  if (db.needsFetch) {
    g.body.appendChild(sidebarTextNode('div', 'sidebar-empty small', 'Expand to load…'));
    var loaded = false, loading = false;
    g.header.addEventListener('click', function () {
      if (loaded || loading || g.body.hidden) return;
      loading = true;
      g.body.innerHTML = '';
      g.body.appendChild(sidebarTextNode('div', 'sidebar-empty small', 'Loading…'));
      fetch('/api/catalog/expand?database=' + encodeURIComponent(db.name))
        .then(function (r) {
          if (!r.ok) throw new Error('HTTP ' + r.status);
          return r.json();
        })
        .then(function (fullDB) {
          loaded = true;
          g.body.innerHTML = '';
          g.body.appendChild(renderCatalogSchemas(fullDB, ctx));
        })
        .catch(function (err) {
          loading = false;
          g.body.innerHTML = '';
          g.body.appendChild(sidebarTextNode('div', 'sidebar-empty small text-danger', err.message || String(err)));
        });
    });
  } else {
    g.body.appendChild(renderCatalogSchemas(db, ctx));
  }
  return g.group;
}

function renderCatalogSchemas(db, ctx) {
  var wrap = document.createElement('div');
  var schemas = db.schemas || [];
  var showSchemaLevel = schemas.length > 1 || (schemas.length === 1 && !!schemas[0].name);
  schemas.forEach(function (schema) {
    if (showSchemaLevel && schema.name) {
      var totalCount = (schema.tables || []).length + (schema.views || []).length + (schema.procedures || []).length;
      var g = makeSidebarGroup('bi-folder2', schema.name, totalCount, 'schema/' + db.name + '/' + schema.name);
      g.body.appendChild(renderCatalogKindFolders(db, schema, ctx));
      wrap.appendChild(g.group);
    } else {
      wrap.appendChild(renderCatalogKindFolders(db, schema, ctx));
    }
  });
  return wrap;
}

function renderCatalogKindFolders(db, schema, ctx) {
  var wrap = document.createElement('div');
  ['table', 'view', 'procedure'].forEach(function (kind) {
    var pluralKey = kind === 'table' ? 'tables' : kind === 'view' ? 'views' : 'procedures';
    var items = schema[pluralKey] || [];
    if (items.length === 0) return;
    var meta = catalogKindMeta[kind];
    var key = 'kind/' + db.name + '/' + schema.name + '/' + kind;
    var g = makeSidebarGroup(meta.icon, meta.label, items.length, key);
    items.forEach(function (item) {
      g.body.appendChild(renderCatalogRow(item, db, schema.name, ctx));
    });
    wrap.appendChild(g.group);
  });
  return wrap;
}

function renderCatalogRow(item, db, schemaName, ctx) {
  var meta = catalogKindMeta[item.kind] || catalogKindMeta.table;
  var qualified = qualifiedCatalogName(ctx, db, schemaName, item.name);
  var row = document.createElement('div');
  row.className = 'table-row';
  var link = document.createElement('a');
  link.className = 'table-link' + (qualified.name === ctx.activeTable ? ' active' : '');
  link.dataset.name = item.name;
  link.appendChild(sidebarTextNode('i', 'bi ' + meta.icon));
  link.appendChild(sidebarTextNode('span', 'tl-text', item.name));
  if (qualified.navigable) {
    link.href = '/t/' + encodeURIComponent(qualified.name);
  } else {
    link.href = '#';
    link.title = 'PostgreSQL can\'t query across databases on one connection. Add "' + db.name + '" as a separate connection to browse it.';
    link.addEventListener('click', function (e) {
      e.preventDefault();
      window.alert(link.title);
    });
  }
  row.appendChild(link);

  var actions = qualified.navigable ? (meta.actions || []) : [];
  if (actions.length > 0) {
    var actionsWrap = document.createElement('span');
    actionsWrap.className = 'table-quick-actions';
    var actionMeta = {
      top: {icon: 'bi-lightning-charge', title: 'Select Top 1000 Rows'},
      insert: {icon: 'bi-plus-square', title: 'Script as INSERT'},
      update: {icon: 'bi-pencil-square', title: 'Script as UPDATE'}
    };
    actions.forEach(function (action) {
      var am = actionMeta[action];
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'qa-btn';
      btn.title = am.title;
      btn.appendChild(sidebarTextNode('i', 'bi ' + am.icon));
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        openTableScript(qualified.name, action);
      });
      actionsWrap.appendChild(btn);
    });
    row.appendChild(actionsWrap);
  }
  return row;
}
