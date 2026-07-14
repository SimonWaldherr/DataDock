// DataDock – client-side UI helpers

// escHtml escapes a value for safe interpolation into innerHTML-built markup
// (table cells, dependency lists, query results, ...). Shared by every page
// that builds HTML strings client-side instead of using textContent.
function escHtml(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

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
  if (path === '/import' || path === '/export' || path.indexOf('/import/') === 0 || path.indexOf('/export/') === 0) return 'manage';
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
  var isRoutine = item.kind === 'procedure' || item.kind === 'function';
  if (qualified.navigable) {
    link.href = isRoutine
      ? '/r/' + encodeURIComponent(qualified.name) + '?kind=' + encodeURIComponent(item.kind)
      : '/t/' + encodeURIComponent(qualified.name);
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

// ── SQL Editor (/query) ──────────────────────────────────────────────────
//
// defaultResultWindowLimit is the one piece of this page's state that's
// server-rendered (the configured page size) and so stays declared inline
// in query.html itself; everything else below is plain client state.

var lastResultData = null;
var lastErrorMessage = '';
var queryHistoryKey = 'datadock.queryHistory.v1';
var queryTabsKey = 'datadock.queryTabs.v1';
var queryTabsActiveKey = 'datadock.queryTabs.active.v1';
var maxLocalQueryHistory = 20;
var sqlEditor = null;
var monacoBaseURL = 'https://cdnjs.cloudflare.com/ajax/libs/monaco-editor/0.40.0/min';
var queryTabs = [];
var activeQueryTabID = '';
var suppressTabSave = false;
var queryTabRuntime = {};
var queryRequestSeq = 0;
var queryLiveTimers = {};
var defaultLiveIntervalMs = 5000;
var activeResultMap = null;

// Ready-to-run examples against the built-in demo dataset, so the editor and
// the LLM assistant can be tried out immediately without inventing a query or
// prompt, and without manually preparing any data first: sample queries load
// the demo dataset automatically (once) if it isn't present yet.
var sampleQueries = [
  { label: 'Top customers by revenue', sql:
    'SELECT c.name, SUM(o.total_amount) AS revenue\n' +
    'FROM datadock_demo_customers c\n' +
    'JOIN datadock_demo_orders o ON o.customer_id = c.id\n' +
    'GROUP BY c.name\n' +
    'ORDER BY revenue DESC' },
  { label: 'Orders by status', sql:
    'SELECT status, COUNT(*) AS order_count, SUM(total_amount) AS total_revenue\n' +
    'FROM datadock_demo_orders\n' +
    'GROUP BY status' },
  { label: 'Daily revenue trend (30 days)', sql:
    "SELECT metric_date, value\nFROM datadock_demo_metrics\nWHERE metric = 'daily_revenue'\nORDER BY metric_date" },
  { label: 'Best-selling products', sql:
    'SELECT p.name, SUM(oi.quantity) AS units_sold, SUM(oi.quantity * oi.unit_price) AS revenue\n' +
    'FROM datadock_demo_order_items oi\n' +
    'JOIN datadock_demo_products p ON p.id = oi.product_id\n' +
    'GROUP BY p.name\n' +
    'ORDER BY revenue DESC' },
  { label: 'Open tickets by priority', sql:
    "SELECT priority, COUNT(*) AS open_tickets\nFROM datadock_demo_tickets\nWHERE status <> 'closed'\nGROUP BY priority\nORDER BY open_tickets DESC" },
  { label: 'Department budgets', sql:
    'SELECT name, region, annual_budget\nFROM datadock_demo_departments\nORDER BY annual_budget DESC' },
  { label: 'Project status overview', sql:
    'SELECT status, COUNT(*) AS projects, SUM(budget) AS total_budget\nFROM datadock_demo_projects\nGROUP BY status' },
  { label: 'Locations for Map view', viewMode: 'map', sql:
    'SELECT name, category, lon, lat\nFROM datadock_demo_locations\nORDER BY name' },
  { label: 'GeoJSON geometry export sample', viewMode: 'map', sql:
    'SELECT name, category, geometry\nFROM datadock_demo_locations\nORDER BY name' },
  { label: 'tinySQL geo functions', viewMode: 'map', sql:
    'SELECT name, category, ST_X(geometry) AS lon, ST_Y(geometry) AS lat,\n' +
    "       ST_DISTANCE(geometry, ST_MakePoint(11.5755, 48.1372)) AS meters_from_munich\n" +
    'FROM datadock_demo_locations\n' +
    'WHERE GEO_WITHIN_BBOX(geometry, 5.5, 47.0, 15.5, 55.5)\n' +
    'ORDER BY meters_from_munich' },
  { label: 'JSON/XML payload tree', viewMode: 'json', sql:
    'SELECT source, external_id, imported_at, payload_json, payload_xml\nFROM datadock_demo_payloads\nORDER BY imported_at' },
  { label: 'XML payload tree', viewMode: 'xml', sql:
    'SELECT source, payload_xml\nFROM datadock_demo_payloads\nORDER BY imported_at' },
  { label: 'tinySQL agent context', viewMode: 'json', sql:
    'CALL datadock_agent_context(12, 6000)' },
  { label: 'tinySQL table info PRAGMA', sql:
    'PRAGMA table_info(datadock_demo_people)' },
  { label: 'tinySQL index catalog', sql:
    'SELECT name, table_name, columns, is_unique, created_at\nFROM sys.indexes\nORDER BY table_name, name' },
  { label: 'Excel CSV edge cases', sql:
    'SELECT external_id, imported_at, source\nFROM datadock_demo_payloads\nORDER BY external_id' }
];

var samplePrompts = [
  'Which customers generated the most revenue?',
  'Show the daily revenue trend for the last 30 days',
  'What are the best-selling products?',
  'List all open support tickets sorted by priority',
  'Which projects are blocked or over budget?',
  'Summarize orders by sales channel',
  'Show demo locations on a map',
  'Inspect the JSON payloads as a tree',
  'Prepare an Excel-safe export for external IDs and timestamps'
];


// initQueryPage is registered from query.html's inline DOMContentLoaded
// call, kept there (not just the function) so DataDock's AJAX shell
// navigation re-runs it every time this page loads into the shell, not
// just on a full browser navigation — see executeScripts() in this file.
function initQueryPage() {
  var el = document.querySelector('noscript');
  // noscript is not rendered in DOM so nothing to hide.

  var ta = document.getElementById('sqlInput');
  initQueryTabs(ta.value || '');
  restoreQueryFromHash();
  ta.addEventListener('keydown', function (e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      runQuery();
    }
    if (e.key === 'F5') {
      e.preventDefault();
      runQuery();
    }
  });
  ta.addEventListener('input', saveActiveQueryTabFromEditor);
  ta.addEventListener('input', function () {
    updateLLMButtonState();
    refreshLLMPreviewIfOpen();
  });
  loadMonacoEditor();
  renderSampleMenus();
  setLLMMode(loadLLMMode(), {skipPreview: true});

  var llmDebounce = null;
  var llmPromptEl = document.getElementById('llmPrompt');
  if (llmPromptEl) {
    llmPromptEl.addEventListener('keydown', function (e) {
      if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
        e.preventDefault();
        runLLMAction();
      }
    });
    llmPromptEl.addEventListener('input', function () {
      updateLLMButtonState();
      clearTimeout(llmDebounce);
      llmDebounce = setTimeout(refreshLLMPreviewIfOpen, 400);
    });
  }
}

function initQueryTabs(initialSQL) {
  queryTabs = loadQueryTabs();
  if (queryTabs.length === 0) {
    queryTabs = [{
      id: newQueryTabID(),
      title: 'Query 1',
      sql: initialSQL || '',
      manualTitle: false,
      liveEnabled: false,
      liveIntervalMs: defaultLiveIntervalMs,
      viewMode: 'table',
      logFilter: '',
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString()
    }];
  } else if (initialSQL && queryTabs.length === 1 && !queryTabs[0].sql) {
    queryTabs[0].sql = initialSQL;
  }
  activeQueryTabID = loadActiveQueryTabID();
  if (!getQueryTab(activeQueryTabID)) {
    activeQueryTabID = queryTabs[0].id;
  }
  persistQueryTabs();
  var active = getActiveQueryTab();
  suppressTabSave = true;
  setEditorValue(active ? active.sql : '');
  suppressTabSave = false;
  renderQueryTabs();
  syncActiveTabControls();
  scheduleAllLiveTabs();
}

function loadQueryTabs() {
  try {
    var raw = localStorage.getItem(queryTabsKey);
    var parsed = raw ? JSON.parse(raw) : [];
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(function(tab) {
      return tab && typeof tab.id === 'string';
    }).map(function(tab, idx) {
      return {
        id: tab.id,
        title: String(tab.title || ('Query ' + (idx + 1))),
        sql: String(tab.sql || ''),
        manualTitle: !!tab.manualTitle,
        liveEnabled: !!tab.liveEnabled,
        liveIntervalMs: normalizeLiveInterval(tab.liveIntervalMs),
        viewMode: normalizeViewMode(tab.viewMode),
        logFilter: String(tab.logFilter || ''),
        createdAt: tab.createdAt || new Date().toISOString(),
        updatedAt: tab.updatedAt || new Date().toISOString()
      };
    }).slice(0, 24);
  } catch (e) {
    return [];
  }
}

function normalizeViewMode(value) {
  var allowed = ['table', 'log', 'cards', 'json', 'xml', 'map', 'pivot', 'profile', 'schema', 'notebook'];
  value = String(value || 'table');
  return allowed.indexOf(value) >= 0 ? value : 'table';
}

function normalizeLiveInterval(value) {
  value = Number(value || defaultLiveIntervalMs);
  var allowed = [1000, 2000, 5000, 15000, 30000, 60000];
  return allowed.indexOf(value) >= 0 ? value : defaultLiveIntervalMs;
}

function loadActiveQueryTabID() {
  try {
    return localStorage.getItem(queryTabsActiveKey) || '';
  } catch (e) {
    return '';
  }
}

function persistQueryTabs() {
  try {
    localStorage.setItem(queryTabsKey, JSON.stringify(queryTabs));
    localStorage.setItem(queryTabsActiveKey, activeQueryTabID || '');
  } catch (e) {}
}

function newQueryTabID() {
  return 'tab-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 8);
}

function getQueryTab(id) {
  for (var i = 0; i < queryTabs.length; i++) {
    if (queryTabs[i].id === id) return queryTabs[i];
  }
  return null;
}

function getActiveQueryTab() {
  return getQueryTab(activeQueryTabID);
}

function syncActiveTabControls() {
  var tab = getActiveQueryTab();
  if (!tab) return;
  var liveBtn = document.getElementById('liveToggleBtn');
  var intervalSelect = document.getElementById('liveIntervalSelect');
  var viewSelect = document.getElementById('viewModeSelect');
  var filterInput = document.getElementById('logFilterInput');
  if (liveBtn) {
    liveBtn.classList.toggle('btn-success', !!tab.liveEnabled);
    liveBtn.classList.toggle('btn-outline-secondary', !tab.liveEnabled);
    liveBtn.innerHTML = tab.liveEnabled ?
      '<i class="bi bi-broadcast-pin me-1"></i>Live' :
      '<i class="bi bi-broadcast me-1"></i>Live';
  }
  if (intervalSelect) {
    intervalSelect.value = String(normalizeLiveInterval(tab.liveIntervalMs));
  }
  if (viewSelect) {
    viewSelect.value = normalizeViewMode(tab.viewMode);
  }
  if (filterInput) {
    filterInput.classList.toggle('d-none', tab.viewMode !== 'log');
    filterInput.value = tab.logFilter || '';
  }
}

function toggleLiveQuery() {
  saveActiveQueryTabFromEditor();
  var tab = getActiveQueryTab();
  if (!tab) return;
  tab.liveEnabled = !tab.liveEnabled;
  tab.liveIntervalMs = normalizeLiveInterval(tab.liveIntervalMs);
  tab.updatedAt = new Date().toISOString();
  persistQueryTabs();
  syncActiveTabControls();
  renderQueryTabs();
  if (tab.liveEnabled) {
    runLiveQueryTab(tab.id);
  } else {
    stopLiveTab(tab.id);
    setQueryTabStatus(tab.id, '');
  }
}

function setLiveInterval(value) {
  var tab = getActiveQueryTab();
  if (!tab) return;
  tab.liveIntervalMs = normalizeLiveInterval(value);
  tab.updatedAt = new Date().toISOString();
  persistQueryTabs();
  if (tab.liveEnabled) {
    scheduleLiveTab(tab.id);
  }
}

function setQueryViewMode(value) {
  var tab = getActiveQueryTab();
  if (!tab) return;
  tab.viewMode = normalizeViewMode(value);
  tab.updatedAt = new Date().toISOString();
  persistQueryTabs();
  syncActiveTabControls();
  renderActiveTabOutput();
}

function setLogFilter(value) {
  var tab = getActiveQueryTab();
  if (!tab) return;
  tab.logFilter = value || '';
  tab.updatedAt = new Date().toISOString();
  persistQueryTabs();
  renderActiveTabOutput();
}

function scheduleAllLiveTabs() {
  queryTabs.forEach(function(tab) {
    if (tab.liveEnabled) {
      scheduleLiveTab(tab.id);
    }
  });
}

function scheduleLiveTab(tabID) {
  stopLiveTab(tabID);
  var tab = getQueryTab(tabID);
  if (!tab || !tab.liveEnabled) return;
  queryLiveTimers[tabID] = window.setTimeout(function() {
    runLiveQueryTab(tabID);
  }, normalizeLiveInterval(tab.liveIntervalMs));
}

function stopLiveTab(tabID) {
  if (queryLiveTimers[tabID]) {
    window.clearTimeout(queryLiveTimers[tabID]);
    delete queryLiveTimers[tabID];
  }
}

function runLiveQueryTab(tabID) {
  var tab = getQueryTab(tabID);
  if (!tab || !tab.liveEnabled) return;
  var runtime = getQueryTabRuntime(tabID);
  if (runtime.running) {
    scheduleLiveTab(tabID);
    return;
  }
  var sql = (tab.sql || '').trim();
  if (!sql) {
    setQueryTabStatus(tabID, 'Live waiting for SQL');
    scheduleLiveTab(tabID);
    return;
  }
  executeQueryForTab(tabID, sql, {live: true});
}

function getQueryTabRuntime(id) {
  if (!queryTabRuntime[id]) {
    queryTabRuntime[id] = {
      running: false,
      requestID: 0,
      statusText: '',
      resultData: null,
      errorMessage: '',
      chartChoice: null,
      chartSQL: null,
      controller: null
    };
  }
  return queryTabRuntime[id];
}

function renderQueryTabs() {
  var target = document.getElementById('queryTabs');
  if (!target) return;
  target.innerHTML = '';
  queryTabs.forEach(function(tab) {
    var runtime = getQueryTabRuntime(tab.id);
    var wrap = document.createElement('div');
    wrap.className = 'nav-item d-flex align-items-center query-tab-item' +
      (tab.id === activeQueryTabID ? ' active' : '') +
      (runtime.running ? ' running' : '') +
      (runtime.errorMessage ? ' has-error' : '');

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'nav-link query-tab-button' + (tab.id === activeQueryTabID ? ' active' : '');
    btn.id = 'query-tab-' + tab.id;
    btn.setAttribute('role', 'tab');
    btn.setAttribute('aria-selected', tab.id === activeQueryTabID ? 'true' : 'false');
    btn.setAttribute('aria-controls', 'resultArea');
    btn.setAttribute('tabindex', tab.id === activeQueryTabID ? '0' : '-1');
    btn.title = runtime.statusText ? tab.title + ' - ' + runtime.statusText : tab.title;
    btn.textContent = (runtime.running ? '● ' : '') + tab.title;
    btn.onclick = function() { switchQueryTab(tab.id); };
    btn.onkeydown = function(e) { handleQueryTabKeydown(e, tab.id); };
    btn.ondblclick = function(e) {
      e.preventDefault();
      renameQueryTab(tab.id);
    };
    wrap.appendChild(btn);

    if (queryTabs.length > 1) {
      var close = document.createElement('button');
      close.type = 'button';
      close.className = 'btn btn-link btn-sm query-tab-close';
      close.title = 'Close tab';
      close.setAttribute('aria-label', 'Close ' + tab.title);
      close.innerHTML = '<i class="bi bi-x"></i>';
      close.onclick = function(e) {
        e.preventDefault();
        e.stopPropagation();
        closeQueryTab(tab.id);
      };
      wrap.appendChild(close);
    }
    target.appendChild(wrap);
  });
}

function handleQueryTabKeydown(e, tabID) {
  var idx = queryTabs.findIndex(function(tab) { return tab.id === tabID; });
  if (idx < 0) return;
  var nextIdx = idx;
  if (e.key === 'ArrowRight') nextIdx = (idx + 1) % queryTabs.length;
  if (e.key === 'ArrowLeft') nextIdx = (idx - 1 + queryTabs.length) % queryTabs.length;
  if (e.key === 'Home') nextIdx = 0;
  if (e.key === 'End') nextIdx = queryTabs.length - 1;
  if (nextIdx !== idx) {
    e.preventDefault();
    switchQueryTab(queryTabs[nextIdx].id);
    window.setTimeout(function() {
      var btn = document.getElementById('query-tab-' + queryTabs[nextIdx].id);
      if (btn) btn.focus();
    }, 0);
  }
}

function newQueryTab(sql, title) {
  saveActiveQueryTabFromEditor();
  var count = queryTabs.length + 1;
  var now = new Date().toISOString();
  var tab = {
    id: newQueryTabID(),
    title: title || ('Query ' + count),
    sql: sql || '',
    manualTitle: !!title,
    liveEnabled: false,
    liveIntervalMs: defaultLiveIntervalMs,
    viewMode: 'table',
    logFilter: '',
    createdAt: now,
    updatedAt: now
  };
  queryTabs.push(tab);
  activeQueryTabID = tab.id;
  setEditorValue(tab.sql);
  persistQueryTabs();
  renderQueryTabs();
  syncActiveTabControls();
  clearQueryOutput();
  focusEditor();
}

function switchQueryTab(id) {
  if (id === activeQueryTabID) return;
  saveActiveQueryTabFromEditor();
  var tab = getQueryTab(id);
  if (!tab) return;
  activeQueryTabID = id;
  suppressTabSave = true;
  setEditorValue(tab.sql || '');
  suppressTabSave = false;
  persistQueryTabs();
  renderQueryTabs();
  syncActiveTabControls();
  renderActiveTabOutput();
  focusEditor();
}

function closeQueryTab(id) {
  if (queryTabs.length <= 1) return;
  saveActiveQueryTabFromEditor();
  var idx = queryTabs.findIndex(function(tab) { return tab.id === id; });
  if (idx < 0) return;
  stopLiveTab(id);
  delete queryTabRuntime[id];
  queryTabs.splice(idx, 1);
  if (activeQueryTabID === id) {
    activeQueryTabID = queryTabs[Math.max(0, idx - 1)].id;
    var active = getActiveQueryTab();
    suppressTabSave = true;
    setEditorValue(active ? active.sql : '');
    suppressTabSave = false;
  }
  persistQueryTabs();
  renderQueryTabs();
  syncActiveTabControls();
  renderActiveTabOutput();
  focusEditor();
}

function renameQueryTab(id) {
  var tab = getQueryTab(id);
  if (!tab) return;
  var title = window.prompt('Tab name', tab.title);
  if (title === null) return;
  title = title.trim();
  if (!title) return;
  tab.title = title.slice(0, 40);
  tab.manualTitle = true;
  tab.updatedAt = new Date().toISOString();
  persistQueryTabs();
  renderQueryTabs();
}

function saveActiveQueryTabFromEditor() {
  if (suppressTabSave) return;
  var tab = getActiveQueryTab();
  if (!tab) return;
  tab.sql = getEditorValue();
  tab.updatedAt = new Date().toISOString();
  var titleChanged = false;
  if (!tab.manualTitle) {
    var nextTitle = titleForSQL(tab.sql, queryTabs.indexOf(tab) + 1);
    titleChanged = nextTitle !== tab.title;
    tab.title = nextTitle;
  }
  persistQueryTabs();
  if (titleChanged) {
    renderQueryTabs();
  }
}

function titleForSQL(sql, idx) {
  var compact = (sql || '').replace(/\s+/g, ' ').trim();
  if (!compact) return 'Query ' + idx;
  compact = compact.replace(/;$/, '');
  if (compact.length > 32) compact = compact.slice(0, 32) + '...';
  return compact;
}

function renderSampleMenus() {
  var queryMenu = document.getElementById('sampleQueryMenu');
  if (queryMenu) {
    sampleQueries.forEach(function (item, idx) {
      var li = document.createElement('li');
      var a = document.createElement('a');
      a.className = 'dropdown-item';
      a.href = '#';
      a.textContent = item.label;
      a.addEventListener('click', function (e) {
        e.preventDefault();
        runSampleQuery(idx);
      });
      li.appendChild(a);
      queryMenu.appendChild(li);
    });
  }

  var promptMenu = document.getElementById('samplePromptMenu');
  if (promptMenu) {
    samplePrompts.forEach(function (prompt) {
      var li = document.createElement('li');
      var a = document.createElement('a');
      a.className = 'dropdown-item';
      a.href = '#';
      a.textContent = prompt;
      a.addEventListener('click', function (e) {
        e.preventDefault();
        document.getElementById('llmPrompt').value = prompt;
        document.getElementById('llmPrompt').focus();
      });
      li.appendChild(a);
      promptMenu.appendChild(li);
    });
  }
}

// runSampleQuery loads an example query into the editor and runs it. If the
// demo tables it depends on don't exist yet, the demo dataset is imported
// automatically once and the query is retried, so trying DataDock out never
// requires any manual setup.
function runSampleQuery(idx) {
  var item = sampleQueries[idx];
  if (!item) return;
  setEditorValue(item.sql);
  var tab = getActiveQueryTab();
  if (tab && item.viewMode) {
    tab.viewMode = normalizeViewMode(item.viewMode);
    persistQueryTabs();
    syncActiveTabControls();
  }
  focusEditor();
  runQueryWithDemoDataFallback(activeQueryTabID, item.sql);
}

function runQueryWithDemoDataFallback(tabID, sql) {
  executeQueryForTab(tabID, sql, {demoFallback: true});
}

function executeQueryForTab(tabID, sql, options) {
  options = options || {};
  var runtime = beginQueryRun(tabID, options.live ? 'Live refresh…' : (options.append ? 'Loading more…' : 'Running…'), !!options.append);
  var controller = new AbortController();
  runtime.controller = controller;
  var requestID = runtime.requestID;
  var offset = Math.max(0, Number(options.offset || 0));
  var limit = Math.max(1, Number(options.limit || defaultResultWindowLimit));
  fetch('/api/query', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({sql: sql, offset: offset, limit: limit}),
    signal: controller.signal
  })
    .then(function (r) { return r.json(); })
    .then(function (data) {
      if (!isCurrentQueryRequest(tabID, requestID)) return null;
      if (options.demoFallback && data.error && isMissingObjectError(data.error)) {
        setQueryTabStatus(tabID, 'Loading demo data…');
        return fetch('/demo-data', {method: 'POST', signal: controller.signal})
          .then(function () {
            if (window.DataDock && typeof window.DataDock.refreshSidebar === 'function') {
              window.DataDock.refreshSidebar();
            }
            return fetch('/api/query', {
              method: 'POST',
              headers: {'Content-Type': 'application/json'},
              body: JSON.stringify({sql: sql, offset: offset, limit: limit}),
              signal: controller.signal
            }).then(function (r2) { return r2.json(); });
          });
      }
      return data;
    })
    .then(function(data) {
      if (!data || !isCurrentQueryRequest(tabID, requestID)) return;
      finishQueryRun(tabID, data, sql, options);
      if (options.live) {
        scheduleLiveTab(tabID);
      }
    })
    .catch(function (err) {
      if (!isCurrentQueryRequest(tabID, requestID)) return;
      if (err && err.name === 'AbortError') {
        finishQueryStop(tabID);
        return;
      }
      finishQueryRun(tabID, {error: err.message || err.toString()}, sql, options);
      if (options.live) {
        scheduleLiveTab(tabID);
      }
    });
}

// stopQuery cancels the in-flight request for a tab (default: the active
// tab) via AbortController. The fetch abort also cancels the request's
// context on the server, so a genuinely long-running query is actually
// interrupted there too, not just hidden in the UI. Stopping a live tab
// also turns Live off, since "stop" while auto-refreshing means "stop
// refreshing", not "skip just this one tick".
function stopQuery(tabID) {
  tabID = tabID || activeQueryTabID;
  var runtime = getQueryTabRuntime(tabID);
  if (runtime.controller) {
    runtime.controller.abort();
  }
  stopLiveTab(tabID);
  var tab = getQueryTab(tabID);
  if (tab && tab.liveEnabled) {
    tab.liveEnabled = false;
    tab.updatedAt = new Date().toISOString();
    persistQueryTabs();
    if (tabID === activeQueryTabID) {
      syncActiveTabControls();
    }
  }
}

function finishQueryStop(tabID) {
  var runtime = getQueryTabRuntime(tabID);
  runtime.running = false;
  runtime.controller = null;
  runtime.statusText = 'Stopped';
  if (tabID === activeQueryTabID) {
    document.getElementById('statusMsg').textContent = 'Stopped';
    syncRunButton();
  }
  renderQueryTabs();
}

// The Run button doubles as Stop while a query is in flight, instead of
// adding yet another permanent toolbar button — matches the Run/Stop toggle
// pattern most SQL IDEs use.
function handleRunButtonClick() {
  var runtime = getQueryTabRuntime(activeQueryTabID);
  if (runtime.running) {
    stopQuery(activeQueryTabID);
  } else {
    runQuery();
  }
}

function syncRunButton() {
  var runtime = getQueryTabRuntime(activeQueryTabID);
  var btn = document.getElementById('runBtn');
  var icon = document.getElementById('runBtnIcon');
  var label = document.getElementById('runBtnLabel');
  var kbd = document.getElementById('runBtnKbd');
  if (!btn) return;
  btn.disabled = false;
  if (runtime.running) {
    btn.classList.remove('btn-primary');
    btn.classList.add('btn-danger');
    if (icon) icon.className = 'bi bi-stop-fill me-1';
    if (label) label.textContent = 'Stop';
    if (kbd) kbd.classList.add('d-none');
  } else {
    btn.classList.remove('btn-danger');
    btn.classList.add('btn-primary');
    if (icon) icon.className = 'bi bi-play-fill me-1';
    if (label) label.textContent = 'Run';
    if (kbd) kbd.classList.remove('d-none');
  }
}

function isMissingObjectError(msg) {
  msg = (msg || '').toLowerCase();
  return msg.indexOf('not found') >= 0 ||
    msg.indexOf('no such table') >= 0 ||
    msg.indexOf("doesn't exist") >= 0 ||
    msg.indexOf('does not exist') >= 0 ||
    msg.indexOf('unknown table') >= 0 ||
    msg.indexOf('invalid object name') >= 0;
}

function llmPayloadForAction(action) {
  var payload = {
    action: action,
    prompt: document.getElementById('llmPrompt').value.trim(),
    sql: getEditorValue().trim()
  };
  if (action === 'explain_error' || action === 'fix_sql') {
    payload.error = lastErrorMessage || '';
  }
  if ((action === 'explain_results' || action === 'create_chart' || action === 'suggest_questions' || action === 'analyze_quality') && lastResultData) {
    payload.columns = lastResultData.columns || [];
    payload.rows = lastResultData.rows || [];
  }
  return payload;
}

function validateLLMAction(action, payload) {
  if (action === 'generate_sql' && !payload.prompt && !payload.sql) {
    return {message: 'Describe the query you want, or keep a SQL draft in the editor to refine.', focus: 'prompt'};
  }
  if (action === 'ask_and_run' && !payload.prompt) {
    return {message: 'Describe the read-only query to generate and run.', focus: 'prompt'};
  }
  if (action === 'fix_sql' && (!payload.sql || !payload.error)) {
    return {message: 'Run a failing SQL query first, then let AI prepare a correction.', focus: 'editor'};
  }
  if (action === 'optimize_sql' && !payload.sql) {
    return {message: 'Write or select SQL in the editor before asking AI to optimize it.', focus: 'editor'};
  }
  if (action === 'review_sql' && !payload.sql) {
    return {message: 'Write or select SQL in the editor before requesting a review.', focus: 'editor'};
  }
  if (action === 'explain_results' && (!lastResultData || !(lastResultData.rows || []).length)) {
    return {message: 'Run a query first, then explain its result.', focus: 'editor'};
  }
  if (action === 'create_chart' && (!lastResultData || !(lastResultData.rows || []).length)) {
    return {message: 'Run a query first, then ask AI for a chart.', focus: 'editor'};
  }
  if (action === 'suggest_questions' && (!lastResultData || !(lastResultData.rows || []).length)) {
    return {message: 'Run a query first, then ask AI for follow-up questions.', focus: 'editor'};
  }
  if (action === 'analyze_quality' && (!lastResultData || !(lastResultData.rows || []).length)) {
    return {message: 'Run a query first, then ask AI to inspect its data-quality signals.', focus: 'editor'};
  }
  if (action === 'explain_error' && !payload.error) {
    return {message: 'Run a failing query first, then explain the error.', focus: 'editor'};
  }
  return null;
}

function showLLMAlert(kind, message) {
  var llmAnswer = document.getElementById('llmAnswer');
  llmAnswer.innerHTML = '<div class="alert alert-' + kind + ' py-2 mb-0" style="white-space:pre-wrap">' + escHtml(message || '') + '</div>';
}

function focusLLMTarget(target) {
  if (target === 'prompt') {
    document.getElementById('llmPrompt').focus();
    return;
  }
  focusEditor();
}

function askLLM(action) {
  var payload = llmPayloadForAction(action);
  var validation = validateLLMAction(action, payload);
  if (validation) {
    showLLMAlert('warning', validation.message);
    focusLLMTarget(validation.focus);
    updateLLMButtonState();
    return;
  }

  var llmStatus = document.getElementById('llmStatus');
  var meta = llmModeMeta[action] || llmModeMeta.generate_sql;
  llmStatus.textContent = meta.status;
  document.getElementById('llmAnswer').innerHTML = '';
  setLLMBusy(true);

  fetch('/api/llm', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(payload)
  })
  .then(function(r){ return r.json().then(function(data){ return {ok: r.ok, data: data}; }); })
  .then(function(resp) {
    setLLMBusy(false);
    llmStatus.textContent = '';
    refreshLLMPreviewIfOpen();
    if (!resp.ok || resp.data.error) {
      showLLMAlert('warning', resp.data.error || 'LLM request failed');
      return;
    }
    if (action === 'generate_sql' || action === 'fix_sql' || action === 'optimize_sql') {
      if (resp.data.action === 'clarify') {
        handleLLMClarify(resp.data);
        return;
      }
      var generatedSQL = (resp.data.sql || '').trim();
      if (!generatedSQL) {
        showLLMAlert('warning', 'The LLM response did not contain executable SQL.');
        return;
      }
      setEditorValue(generatedSQL);
      focusEditor();
      document.getElementById('llmAnswer').innerHTML = renderLLMSQLMetadata(resp.data, action);
      return;
    }
    if (action === 'suggest_questions') {
      if (resp.data.action === 'clarify') {
        handleLLMClarify(resp.data);
        return;
      }
      renderLLMSuggestions(resp.data);
      return;
    }
    if (action === 'create_chart') {
      if (resp.data.action === 'clarify') {
        handleLLMClarify(resp.data);
        return;
      }
      if (!renderAIChart(lastResultData, resp.data.chart, resp.data.explanation)) {
        showLLMAlert('warning', 'The AI response did not contain a usable chart specification.');
        return;
      }
      showLLMAlert('info', resp.data.explanation || 'Chart created.');
      return;
    }
    showLLMAlert('info', resp.data.text || '');
  })
  .catch(function(err) {
    setLLMBusy(false);
    llmStatus.textContent = '';
    showLLMAlert('warning', err.message || err.toString());
  });
}

function renderLLMSQLMetadata(data, action) {
  var headline = 'SQL generated in the editor. Review it before running.';
  if (action === 'fix_sql') {
    headline = 'Corrected SQL is in the editor. Review it before running.';
  } else if (action === 'optimize_sql') {
    headline = 'Optimized SQL is in the editor. Review it before running.';
  }
  var parts = ['<div class="alert alert-info py-2 mb-0">' + headline];
  if (data.explanation) {
    parts.push('<div class="mt-2" style="white-space:pre-wrap">' + escHtml(data.explanation) + '</div>');
  }
  if (data.review) {
    parts.push('<div class="mt-2 pt-2 border-top"><span class="fw-semibold">AI review:</span><div style="white-space:pre-wrap">' + escHtml(data.review) + '</div></div>');
  }
  if (data.follow_up) {
    parts.push('<div class="mt-2"><span class="fw-semibold">Follow-up:</span> ' + escHtml(data.follow_up) + '</div>');
  }
  if (data.action && data.action !== 'sql') {
    parts.push('<div class="mt-2 text-muted">Action: ' + escHtml(data.action) + '</div>');
  }
  parts.push('</div>');
  return parts.join('');
}

function renderLLMSuggestions(data) {
  var answer = document.getElementById('llmAnswer');
  var suggestions = Array.isArray(data.suggestions) ? data.suggestions.filter(function (suggestion) {
    return typeof suggestion === 'string' && suggestion.trim();
  }) : [];
  answer.innerHTML = '';
  if (!suggestions.length) {
    showLLMAlert('warning', data.explanation || 'The AI did not return usable follow-up questions.');
    return;
  }

  var panel = document.createElement('div');
  panel.className = 'alert alert-info py-2 mb-0';
  var intro = document.createElement('div');
  intro.className = 'mb-2';
  intro.textContent = data.explanation || 'Choose a follow-up question to draft SQL. Nothing runs automatically.';
  panel.appendChild(intro);

  suggestions.forEach(function (suggestion) {
    var button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn btn-outline-primary btn-sm d-block text-start w-100 mb-1';
    button.textContent = suggestion;
    button.addEventListener('click', function () {
      var prompt = document.getElementById('llmPrompt');
      prompt.value = suggestion;
      setLLMMode('generate_sql');
      prompt.focus();
      updateLLMButtonState();
    });
    panel.appendChild(button);
  });
  answer.appendChild(panel);
}

function askAndRunLLM() {
  var payload = llmPayloadForAction('ask_and_run');
  var validation = validateLLMAction('ask_and_run', payload);
  if (validation) {
    showLLMAlert('warning', validation.message);
    focusLLMTarget(validation.focus);
    updateLLMButtonState();
    return;
  }

  var llmStatus = document.getElementById('llmStatus');
  llmStatus.textContent = llmModeMeta.ask_and_run.status;
  document.getElementById('llmAnswer').innerHTML = '';
  setLLMBusy(true);

  fetch('/api/llm/run', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({prompt: payload.prompt})
  })
  .then(function(r){ return r.json().then(function(data){ return {ok: r.ok, data: data}; }); })
  .then(function(resp) {
    setLLMBusy(false);
    llmStatus.textContent = '';
    refreshLLMPreviewIfOpen();
    if (!resp.ok || resp.data.error && resp.data.action !== 'error' && resp.data.action !== 'blocked') {
      showLLMAlert('warning', resp.data.error || 'LLM request failed');
      return;
    }
    if (resp.data.action === 'clarify') {
      handleLLMClarify(resp.data);
      return;
    }
    if (resp.data.sql) {
      setEditorValue(resp.data.sql);
    }
    if (resp.data.action === 'blocked' || resp.data.action === 'error') {
      renderError(resp.data.error || 'LLM-generated SQL could not be executed.');
      showLLMAlert('warning', combineLLMExplanationAndReview(resp.data.explanation || resp.data.error || '', resp.data.review || ''));
      return;
    }
    renderResult(resp.data);
    if (resp.data.explanation || resp.data.review) {
      showLLMAlert('info', combineLLMExplanationAndReview(resp.data.explanation || '', resp.data.review || ''));
    }
  })
  .catch(function(err) {
    setLLMBusy(false);
    llmStatus.textContent = '';
    showLLMAlert('warning', err.message || err.toString());
  });
}

function handleLLMClarify(data) {
  if (data.follow_up) {
    var prompt = document.getElementById('llmPrompt');
    prompt.value = data.follow_up;
    prompt.focus();
    prompt.select();
  }
  showLLMAlert('info', data.explanation || data.follow_up || 'Clarification needed.');
  updateLLMButtonState();
}

function combineLLMExplanationAndReview(explanation, review) {
  var parts = [];
  if (explanation) parts.push(explanation);
  if (review) parts.push('AI review:\n' + review);
  return parts.join('\n\n');
}

function setLLMBusy(busy) {
  llmBusy = busy;
  ['llmAskBtn', 'llmModeBtn', 'llmHealthBtn', 'llmDetailsBtn'].forEach(function(id) {
    var el = document.getElementById(id);
    if (el) el.disabled = busy;
  });
  if (!busy) updateLLMButtonState();
}

// ── AI panel: single action button with a mode dropdown, plus a
// "Details" toggle that previews the exact system/user messages that would
// be sent (via /api/llm/preview, built from the same code the real request
// uses), so it's always accurate instead of a best-effort client guess.
//
// Default is Write SQL (generate_sql: writes the query, doesn't run it) rather
// than "Ask & Run" — auto-executing LLM-generated SQL is the riskier action
// (it could touch the wrong table or run something destructive-looking
// unreviewed), so it must be an explicit choice, not the one-click default.

var llmMode = 'generate_sql';
var llmBusy = false;
var llmModeMeta = {
  generate_sql: {
    icon: 'bi-stars',
    label: 'Ask AI',
    title: 'Ask AI to write SQL into the editor',
    status: 'Drafting and reviewing SQL...',
    placeholder: 'Ask AI to write or improve SQL'
  },
  fix_sql: {
    icon: 'bi-wrench-adjustable',
    label: 'Fix query',
    title: 'Ask AI to correct the last failing SQL without running it',
    status: 'Preparing a safe SQL correction...',
    placeholder: 'Optionally describe the intended result'
  },
  optimize_sql: {
    icon: 'bi-speedometer2',
    label: 'Optimize SQL',
    title: 'Ask AI to improve the current SQL without running it',
    status: 'Optimizing and reviewing SQL...',
    placeholder: 'Optionally describe the constraint or concern'
  },
  review_sql: {
    icon: 'bi-shield-check',
    label: 'Review SQL',
    title: 'Ask AI to review the current SQL for intent, safety, and risks',
    status: 'Reviewing SQL...',
    placeholder: 'Optionally describe the intended result'
  },
  ask_and_run: {
    icon: 'bi-stars',
    label: 'Ask AI',
    title: 'Ask AI to generate and run a read-only query',
    status: 'Generating, reviewing, and running...',
    placeholder: 'Ask AI to run a read-only query'
  },
  create_chart: {
    icon: 'bi-bar-chart',
    label: 'Ask AI',
    title: 'Ask AI to chart the current result',
    status: 'Creating chart...',
    placeholder: 'Ask AI how to chart the current result'
  },
  explain_results: {
    icon: 'bi-stars',
    label: 'Ask AI',
    title: 'Ask AI to explain the current result',
    status: 'Explaining result...',
    placeholder: 'Ask AI what to focus on in the result'
  },
  analyze_quality: {
    icon: 'bi-clipboard2-data',
    label: 'Analyze quality',
    title: 'Ask AI to inspect the current result for data-quality signals',
    status: 'Analyzing result quality...',
    placeholder: 'Optionally say which quality concern to investigate'
  },
  suggest_questions: {
    icon: 'bi-signpost-split',
    label: 'Next questions',
    title: 'Ask AI for follow-up questions without running them',
    status: 'Finding useful follow-up questions...',
    placeholder: 'Optionally say what you want to investigate next'
  },
  explain_error: {
    icon: 'bi-stars',
    label: 'Ask AI',
    title: 'Ask AI to explain the last SQL error',
    status: 'Explaining error...',
    placeholder: 'Ask AI what you were trying to do'
  }
};

function loadLLMMode() {
  try {
    var stored = localStorage.getItem('datadock.llm.mode') || '';
    if (stored && stored !== 'ask_and_run' && llmModeMeta[stored]) return stored;
  } catch (e) {}
  return 'generate_sql';
}

function setLLMMode(mode, options) {
  llmMode = llmModeMeta[mode] ? mode : 'generate_sql';
  var meta = llmModeMeta[llmMode];
  document.getElementById('llmAskBtnLabel').textContent = meta.label;
  document.getElementById('llmAskBtnIcon').className = 'bi ' + meta.icon + ' me-1';
  var prompt = document.getElementById('llmPrompt');
  if (prompt) prompt.setAttribute('placeholder', meta.placeholder);
  var btn = document.getElementById('llmAskBtn');
  if (btn) btn.setAttribute('title', meta.title);
  document.querySelectorAll('#llmModeMenu [data-llm-mode]').forEach(function(item) {
    item.classList.toggle('active', item.getAttribute('data-llm-mode') === llmMode);
  });
  try { localStorage.setItem('datadock.llm.mode', llmMode); } catch (e) {}
  updateLLMButtonState();
  if (!options || !options.skipPreview) refreshLLMPreviewIfOpen();
}

function updateLLMButtonState() {
  if (llmBusy) return;
  var btn = document.getElementById('llmAskBtn');
  if (!btn) return;
  var payload = llmPayloadForAction(llmMode);
  var validation = validateLLMAction(llmMode, payload);
  var meta = llmModeMeta[llmMode] || llmModeMeta.generate_sql;
  btn.disabled = false;
  btn.setAttribute('title', validation ? validation.message : meta.title);
}

function runLLMAction() {
  if (llmMode === 'ask_and_run') {
    askAndRunLLM();
  } else {
    askLLM(llmMode);
  }
}

function toggleLLMDetails() {
  var panel = document.getElementById('llmDetails');
  if (panel.classList.contains('d-none')) {
    panel.classList.remove('d-none');
    refreshLLMPreview();
  } else {
    panel.classList.add('d-none');
  }
}

function refreshLLMPreviewIfOpen() {
  var panel = document.getElementById('llmDetails');
  if (panel && !panel.classList.contains('d-none')) {
    refreshLLMPreview();
  }
}

function currentLLMRequestPayload() {
  var previewAction = llmMode === 'ask_and_run' ? 'generate_sql' : llmMode;
  var payload = llmPayloadForAction(previewAction);
  if (llmMode === 'ask_and_run') payload.sql = '';
  return payload;
}

function refreshLLMPreview() {
  var body = document.getElementById('llmDetailsBody');
  body.textContent = 'Loading preview…';
  fetch('/api/llm/preview', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(currentLLMRequestPayload())
  })
    .then(function (r) { return r.json().then(function (data) { return {ok: r.ok, data: data}; }); })
    .then(function (resp) {
      if (!resp.ok) {
        body.innerHTML = '<div class="text-danger">' + escHtml(resp.data.error || 'Could not build a preview') + '</div>';
        return;
      }
      var d = resp.data;
      var statusLine = d.configured
        ? ('Would be sent to <code>' + escHtml(d.baseURL || '') + '</code> using model <code>' + escHtml(d.model || '') + '</code> — ' + d.charCount + ' characters.')
        : '<span class="text-warning">No LLM is configured yet (see Admin) — this is what would be sent once one is.</span>';
      body.innerHTML = '<div class="mb-2">' + statusLine + '</div>' +
        '<div class="fw-semibold small text-muted">System message</div>' +
        '<pre class="p-2 rounded small" style="background:var(--code-bg);color:var(--code-ink);white-space:pre-wrap;max-height:160px;overflow:auto">' + escHtml(d.system || '') + '</pre>' +
        '<div class="fw-semibold small text-muted">User message</div>' +
        '<pre class="p-2 rounded small" style="background:var(--code-bg);color:var(--code-ink);white-space:pre-wrap;max-height:280px;overflow:auto">' + escHtml(d.userDisplay || d.user || '') + '</pre>';
    })
    .catch(function (err) {
      body.innerHTML = '<div class="text-danger">' + escHtml(err.message || err) + '</div>';
    });
}

function testLLMHealth() {
  var llmStatus = document.getElementById('llmStatus');
  var llmAnswer = document.getElementById('llmAnswer');
  llmStatus.textContent = 'Testing LLM...';
  llmAnswer.innerHTML = '';
  setLLMBusy(true);
  fetch('/api/llm/health')
    .then(function(r){ return r.json().then(function(data){ return {ok: r.ok, data: data}; }); })
    .then(function(resp) {
      setLLMBusy(false);
      llmStatus.textContent = '';
      if (!resp.ok || resp.data.error) {
        llmAnswer.innerHTML = '<div class="alert alert-warning py-2 mb-0">' + escHtml(resp.data.error || 'LLM health check failed') + '</div>';
        return;
      }
      llmAnswer.innerHTML = '<div class="alert alert-success py-2 mb-0">LLM reachable: ' + escHtml(resp.data.response || 'OK') + '</div>';
    })
    .catch(function(err) {
      setLLMBusy(false);
      llmStatus.textContent = '';
      llmAnswer.innerHTML = '<div class="alert alert-warning py-2 mb-0">' + escHtml(err.message || err.toString()) + '</div>';
    });
}

function runQuery() {
  var sql = currentSQL().trim();
  if (!sql) return;
  executeQueryForTab(activeQueryTabID, sql, {});
}

function clearEditor() {
  setEditorValue('');
  clearQueryOutput();
  focusEditor();
}

function beginQueryRun(tabID, statusText, preserveOutput) {
  var runtime = getQueryTabRuntime(tabID);
  runtime.running = true;
  runtime.requestID = ++queryRequestSeq;
  runtime.statusText = statusText || 'Running…';
  if (!preserveOutput) {
    runtime.resultData = null;
    runtime.errorMessage = '';
  }
  if (tabID === activeQueryTabID) {
    if (!preserveOutput) {
      lastResultData = null;
      lastErrorMessage = '';
    }
    document.getElementById('statusMsg').textContent = runtime.statusText;
    syncRunButton();
    if (!preserveOutput) {
      document.getElementById('resultArea').innerHTML = '';
      document.getElementById('chartArea').innerHTML = '';
    }
  }
  renderQueryTabs();
  return runtime;
}

function setQueryTabStatus(tabID, statusText) {
  var runtime = getQueryTabRuntime(tabID);
  runtime.statusText = statusText || '';
  if (tabID === activeQueryTabID) {
    document.getElementById('statusMsg').textContent = runtime.statusText;
  }
  renderQueryTabs();
}

function isCurrentQueryRequest(tabID, requestID) {
  return getQueryTabRuntime(tabID).requestID === requestID;
}

function finishQueryRun(tabID, data, sql, options) {
  options = options || {};
  var runtime = getQueryTabRuntime(tabID);
  var previous = runtime.resultData;
  runtime.running = false;
  runtime.resultData = mergeQueryWindow(previous, data, options);
  runtime.errorMessage = data && data.error ? data.error : '';
  runtime.statusText = queryStatusText(runtime.resultData || data);
  // A genuinely new query invalidates any manual "Group by"/"Measure" chart
  // choice from before (columns may differ); paging/live-refreshing the
  // same query keeps it, so the user doesn't have to redo it every time.
  if (runtime.chartSQL !== sql) {
    runtime.chartChoice = null;
    runtime.chartSQL = sql;
  }
  saveQueryResultHistory(sql, data);
  if (data && !data.error && (data.statement_class === 'ddl' || data.statement_class === 'script')) {
    if (window.DataDock && typeof window.DataDock.refreshSidebar === 'function') {
      window.DataDock.refreshSidebar();
    }
  }
  if (tabID === activeQueryTabID) {
    renderActiveTabOutput();
  }
  renderQueryTabs();
}

function mergeQueryWindow(previous, data, options) {
  if (!data || data.error) return null;
  if (!options.append || !previous || !previous.rows || !data.rows) return data;
  var merged = {};
  Object.keys(data).forEach(function(key) { merged[key] = data[key]; });
  merged.columns = previous.columns || data.columns || [];
  merged.rows = (previous.rows || []).concat(data.rows || []);
  merged.offset = previous.offset || 0;
  merged.limit = data.limit || previous.limit || defaultResultWindowLimit;
  merged.has_more = !!data.has_more;
  merged.elapsed_ms = data.elapsed_ms || previous.elapsed_ms || 0;
  return merged;
}

function queryStatusText(data) {
  if (!data) return '';
  if (data.error) return 'Error';
  var ms = data.elapsed_ms || 0;
  if (data.columns && data.columns.length > 0) {
    var rows = data.rows || [];
    return rows.length + ' row(s) shown' + (data.has_more ? ' · more available' : '') + ' · ' + ms + ' ms';
  }
  return 'OK · ' + (data.affected || 0) + ' row(s) · ' + ms + ' ms';
}

function saveQueryResultHistory(sql, data) {
  if (!sql || !data) return;
  if (data.error) {
    saveLocalQueryHistory(sql, 0, 0, data.error);
    return;
  }
  var ms = data.elapsed_ms || 0;
  if (data.columns && data.columns.length > 0) {
    saveLocalQueryHistory(sql, (data.rows || []).length, ms, '');
  } else {
    saveLocalQueryHistory(sql, data.affected || 0, ms, '');
  }
}

function renderActiveTabOutput() {
  var runtime = getQueryTabRuntime(activeQueryTabID);
  var tab = getActiveQueryTab() || {};
  syncRunButton();
  if (runtime.running) {
    document.getElementById('statusMsg').textContent = runtime.statusText || 'Running…';
    if (runtime.resultData) {
      renderResultToPage(runtime.resultData);
      document.getElementById('statusMsg').textContent = runtime.statusText || 'Running…';
    } else {
      lastResultData = null;
      lastErrorMessage = '';
      disposeActiveResultMap();
      document.getElementById('resultArea').innerHTML = '';
      document.getElementById('chartArea').innerHTML = '';
    }
    return;
  }
  if (runtime.errorMessage) {
    renderErrorToPage(runtime.errorMessage);
    return;
  }
  if (runtime.resultData) {
    renderResultToPage(runtime.resultData);
    return;
  }
  if (tab.viewMode === 'schema') {
    document.getElementById('statusMsg').textContent = '';
    document.getElementById('resultArea').innerHTML = renderSchemaGraphShell();
    document.getElementById('chartArea').innerHTML = '';
    loadSchemaGraph();
    return;
  }
  lastResultData = null;
  lastErrorMessage = '';
  document.getElementById('statusMsg').textContent = runtime.statusText || '';
  document.getElementById('resultArea').innerHTML = '';
  document.getElementById('chartArea').innerHTML = '';
}

function clearQueryOutput(tabID) {
  tabID = tabID || activeQueryTabID;
  var runtime = getQueryTabRuntime(tabID);
  if (runtime.controller) {
    runtime.controller.abort();
    runtime.controller = null;
  }
  runtime.running = false;
  runtime.requestID = ++queryRequestSeq;
  runtime.statusText = '';
  runtime.resultData = null;
  runtime.errorMessage = '';
  if (tabID !== activeQueryTabID) {
    renderQueryTabs();
    return;
  }
  lastResultData = null;
  lastErrorMessage = '';
  disposeActiveResultMap();
  syncRunButton();
  document.getElementById('resultArea').innerHTML = '';
  document.getElementById('chartArea').innerHTML = '';
  document.getElementById('statusMsg').textContent = '';
  renderQueryTabs();
}

function exportQuery(format) {
  var sql = currentSQL().trim();
  if (!sql) return;
  var status = document.getElementById('statusMsg');
  status.textContent = 'Exporting…';

  fetch('/api/export', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({sql: sql, format: format})
  })
  .then(function(r) {
    if (!r.ok) {
      return r.text().then(function(text) { throw new Error(text || r.statusText); });
    }
    return r.blob();
  })
  .then(function(blob) {
    status.textContent = '';
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'query.' + exportFileExtension(format);
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  })
  .catch(function(err) {
    status.textContent = '';
    renderError(err.message || err.toString());
  });
}

function exportFileExtension(format) {
  switch (format) {
    case 'csv-excel':
    case 'excel-csv':
      return 'excel.csv';
    case 'geojson':
      return 'geojson';
    case 'geojson-summary':
    case 'geojson-stats':
      return 'geojson-summary.json';
    case 'shp':
    case 'shapefile':
    case 'shpzip':
      return 'shp.zip';
    case 'sqlite3':
    case 'db':
      return 'sqlite';
    case 'htm':
      return 'html';
    default:
      return format || 'csv';
  }
}

function currentSQL() {
  if (sqlEditor) {
    var selection = sqlEditor.getSelection();
    var selectedText = selection ? sqlEditor.getModel().getValueInRange(selection) : '';
    return selectedText || sqlEditor.getValue();
  }
  var ta = document.getElementById('sqlInput');
  var selected = '';
  if (typeof ta.selectionStart === 'number' && ta.selectionEnd > ta.selectionStart) {
    selected = ta.value.slice(ta.selectionStart, ta.selectionEnd);
  }
  return selected || ta.value;
}

function shareQuery() {
  saveActiveQueryTabFromEditor();
  var tab = getActiveQueryTab();
  if (!tab || !String(tab.sql || '').trim()) return;
  var shareState = {
    v: 2,
    title: tab.title,
    sql: tab.sql,
    viewMode: normalizeViewMode(tab.viewMode),
    liveEnabled: !!tab.liveEnabled,
    liveIntervalMs: normalizeLiveInterval(tab.liveIntervalMs),
    logFilter: tab.logFilter || ''
  };
  var encoded = utf8ToB64(JSON.stringify(shareState));
  var url = window.location.origin + window.location.pathname + '#s=' + encodeURIComponent(encoded);
  window.location.hash = 's=' + encodeURIComponent(encoded);
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).catch(function(){});
  }
  setQueryTabStatus(activeQueryTabID, 'Share link copied');
}

function restoreQueryFromHash() {
  var hash = window.location.hash || '';
  try {
    if (hash.indexOf('#s=') === 0) {
      var state = JSON.parse(b64ToUtf8(decodeURIComponent(hash.slice(3))));
      if (state && String(state.sql || '').trim()) {
        var tab = getActiveQueryTab();
        if (tab && String(tab.sql || '').trim()) {
          newQueryTab(state.sql, state.title || 'Shared Query');
          tab = getActiveQueryTab();
        } else if (tab) {
          tab.sql = state.sql;
          tab.title = state.title || titleForSQL(state.sql, queryTabs.indexOf(tab) + 1);
        }
        if (tab) {
          tab.manualTitle = !!state.title;
          tab.viewMode = normalizeViewMode(state.viewMode);
          tab.liveEnabled = !!state.liveEnabled;
          tab.liveIntervalMs = normalizeLiveInterval(state.liveIntervalMs);
          tab.logFilter = String(state.logFilter || '');
          suppressTabSave = true;
          setEditorValue(tab.sql);
          suppressTabSave = false;
          persistQueryTabs();
          syncActiveTabControls();
          renderQueryTabs();
          if (tab.liveEnabled) runLiveQueryTab(tab.id);
          else if (state.autoRun) runQuery();
        }
      }
      history.replaceState(null, '', window.location.pathname + window.location.search);
      return;
    }
    if (hash.indexOf('#q=') === 0) {
      var encoded = decodeURIComponent(hash.slice(3));
      var sql = b64ToUtf8(encoded);
      if (sql.trim()) {
        setEditorValue(sql);
      }
    }
  } catch (e) {}
}

function getEditorValue() {
  return sqlEditor ? sqlEditor.getValue() : document.getElementById('sqlInput').value;
}

function setEditorValue(value) {
  if (sqlEditor) {
    sqlEditor.setValue(value || '');
    saveActiveQueryTabFromEditor();
    return;
  }
  document.getElementById('sqlInput').value = value || '';
  saveActiveQueryTabFromEditor();
}

function focusEditor() {
  if (sqlEditor) {
    sqlEditor.focus();
    return;
  }
  document.getElementById('sqlInput').focus();
}

function loadMonacoEditor() {
  if (window.require) {
    initMonacoEditor();
    return;
  }
  var script = document.createElement('script');
  script.src = monacoBaseURL + '/vs/loader.min.js';
  script.async = true;
  script.onload = initMonacoEditor;
  script.onerror = function() { sqlEditor = null; };
  document.head.appendChild(script);
}

function initMonacoEditor() {
  if (!window.require || !document.getElementById('monacoEditor')) return;
  try {
    window.MonacoEnvironment = {
      getWorkerUrl: function() {
        var code = "self.MonacoEnvironment={baseUrl:'" + monacoBaseURL + "/'};importScripts('" + monacoBaseURL + "/vs/base/worker/workerMain.js');";
        return 'data:text/javascript;charset=utf-8,' + encodeURIComponent(code);
      }
    };
    require.config({paths: {'vs': monacoBaseURL + '/vs'}});
    require(['vs/editor/editor.main'], function() {
      var textarea = document.getElementById('sqlInput');
      var container = document.getElementById('monacoEditor');
      sqlEditor = monaco.editor.create(container, {
        value: textarea.value,
        language: 'sql',
        theme: 'vs-dark',
        automaticLayout: true,
        minimap: {enabled: false},
        scrollBeyondLastLine: false,
        fontSize: 13,
        wordWrap: 'on'
      });
      textarea.classList.add('d-none');
      container.classList.remove('d-none');
      sqlEditor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.Enter, runQuery);
      sqlEditor.addCommand(monaco.KeyCode.F5, runQuery);
      sqlEditor.onDidChangeModelContent(saveActiveQueryTabFromEditor);
    });
  } catch (e) {
    sqlEditor = null;
  }
}

function utf8ToB64(str) {
  return btoa(unescape(encodeURIComponent(str)));
}

function b64ToUtf8(str) {
  return decodeURIComponent(escape(atob(str)));
}

function toggleSchemaPreview() {
  var box = document.getElementById('schemaPreview');
  var text = document.getElementById('schemaText');
  if (!box.classList.contains('d-none')) {
    box.classList.add('d-none');
    return;
  }
  box.classList.remove('d-none');
  text.textContent = 'Loading schema context...';
  fetch('/api/schema')
    .then(function(r) { return r.text(); })
    .then(function(body) {
      try {
        text.textContent = JSON.stringify(JSON.parse(body), null, 2);
      } catch (e) {
        text.textContent = body;
      }
    })
    .catch(function(err) {
      text.textContent = err.message || String(err);
    });
}

function copySchemaPreview() {
  var value = document.getElementById('schemaText').textContent || '';
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(value).catch(function(){});
  }
}

function renderError(msg) {
  finishQueryRun(activeQueryTabID, {error: msg}, currentSQL().trim());
}

function renderErrorToPage(msg) {
  lastResultData = null;
  lastErrorMessage = msg;
  disposeActiveResultMap();
  document.getElementById('statusMsg').textContent = '';
  syncRunButton();
  document.getElementById('chartArea').innerHTML = '';
  document.getElementById('resultArea').innerHTML =
    '<div class="alert alert-danger mt-1 py-2"><i class="bi bi-exclamation-triangle-fill me-2"></i>' +
    escHtml(msg) + '</div>';
}

function renderResult(data) {
  finishQueryRun(activeQueryTabID, data, currentSQL().trim());
}

function renderResultToPage(data) {
  var status = document.getElementById('statusMsg');
  var area = document.getElementById('resultArea');
  syncRunButton();
  disposeActiveResultMap();

  if (data.error) {
    renderErrorToPage(data.error);
    return;
  }

  var ms = data.elapsed_ms || 0;
  if (data.columns && data.columns.length > 0) {
    lastResultData = data;
    lastErrorMessage = '';
    var rows = data.rows || [];
    status.textContent = queryStatusText(data);
    var tab = getActiveQueryTab() || {};
    if (tab.viewMode === 'log') {
      area.innerHTML = renderResultWindowBar(data) + renderLogResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'cards') {
      area.innerHTML = renderResultWindowBar(data) + renderCardResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'json') {
      area.innerHTML = renderResultWindowBar(data) + renderStructuredResult(data, 'json');
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'xml') {
      area.innerHTML = renderResultWindowBar(data) + renderStructuredResult(data, 'xml');
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'map') {
      area.innerHTML = renderResultWindowBar(data) + renderMapResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'pivot') {
      area.innerHTML = renderResultWindowBar(data) + renderPivotResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'profile') {
      area.innerHTML = renderResultWindowBar(data) + renderProfileResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    if (tab.viewMode === 'schema') {
      area.innerHTML = renderSchemaGraphShell();
      document.getElementById('chartArea').innerHTML = '';
      loadSchemaGraph();
      return;
    }
    if (tab.viewMode === 'notebook') {
      area.innerHTML = renderResultWindowBar(data) + renderNotebookResult(data);
      document.getElementById('chartArea').innerHTML = '';
      return;
    }
    var html = renderResultWindowBar(data) + '<div class="card mt-1"><div class="table-responsive">' +
      '<table class="table table-sm table-hover result-table mb-0"><thead><tr>';
    data.columns.forEach(function(c){ html += '<th>' + escHtml(c) + '</th>'; });
    html += '</tr></thead><tbody>';
    rows.forEach(function(row){
      html += '<tr>';
      row.forEach(function(cell){
        html += '<td>' + (cell === '' ? '<span class="null-cell">null</span>' : escHtml(cell)) + '</td>';
      });
      html += '</tr>';
    });
    html += '</tbody></table></div></div>';
    area.innerHTML = html;
    renderQuickChart(data, getQueryTabRuntime(activeQueryTabID).chartChoice);
  } else {
    lastResultData = null;
    lastErrorMessage = '';
    document.getElementById('chartArea').innerHTML = '';
    var affected = data.affected || 0;
    status.textContent = '';
    area.innerHTML = '<div class="alert alert-success mt-1 py-2"><i class="bi bi-check-circle-fill me-2"></i>' +
      'OK — ' + affected + ' row(s) affected · ' + ms + ' ms</div>';
  }
}

// Same page-size choices, and the same Prev/Page N/Next markup (.pagination-bar),
// as the plain table view — so paging through a query result feels identical
// to paging through a table.
var queryPageSizeOptions = [25, 50, 100, 250, 500, 1000];

function renderResultWindowBar(data) {
  if (!data || !data.columns || !data.columns.length) return '';
  var rows = data.rows || [];
  var limit = data.limit || defaultResultWindowLimit;
  var offset = data.offset || 0;
  var page = Math.floor(offset / limit) + 1;
  var statementClass = data.statement_class ? ' · ' + data.statement_class.replace('_', ' ') : '';
  var rangeText = rows.length ? ('rows ' + (offset + 1) + '–' + (offset + rows.length)) : '0 rows';

  var html = '<nav class="pagination-bar mt-1">';
  if (offset > 0) {
    html += '<button type="button" class="btn btn-sm btn-outline-secondary" onclick="goToQueryResultPage(-1)">' +
      '<i class="bi bi-chevron-left"></i> Prev</button>';
  }
  html += '<span class="text-muted small">Page ' + page + ' · ' + rangeText + statementClass + '</span>';
  if (data.has_more) {
    html += '<button type="button" class="btn btn-sm btn-outline-secondary" onclick="goToQueryResultPage(1)">' +
      'Next <i class="bi bi-chevron-right"></i></button>';
  }
  html += '<label class="d-flex align-items-center gap-1 small text-muted ms-auto mb-0">Rows per page' +
    renderQueryPageSizeSelect(limit) + '</label>';
  html += '</nav>';
  return html;
}

function renderQueryPageSizeSelect(current) {
  var html = '<select class="form-select form-select-sm" style="width:auto" onchange="changeQueryPageSize(this.value)">';
  queryPageSizeOptions.forEach(function (n) {
    html += '<option value="' + n + '"' + (n === current ? ' selected' : '') + '>' + n + '</option>';
  });
  html += '</select>';
  return html;
}

// Replaces the current result window with the previous/next page, mirroring
// the table view's Prev/Next pager instead of accumulating rows.
function goToQueryResultPage(direction) {
  var tab = getActiveQueryTab();
  var runtime = getQueryTabRuntime(activeQueryTabID);
  if (!tab || !runtime.resultData || runtime.running) return;
  var limit = runtime.resultData.limit || defaultResultWindowLimit;
  var offset = Math.max(0, (runtime.resultData.offset || 0) + direction * limit);
  executeQueryForTab(activeQueryTabID, tab.sql || currentSQL().trim(), {offset: offset, limit: limit});
}

function changeQueryPageSize(size) {
  var tab = getActiveQueryTab();
  if (!tab || runtimeIsBusy()) return;
  executeQueryForTab(activeQueryTabID, tab.sql || currentSQL().trim(), {offset: 0, limit: Number(size)});
}

function runtimeIsBusy() {
  return !!getQueryTabRuntime(activeQueryTabID).running;
}

function disposeActiveResultMap() {
  if (activeResultMap && activeResultMap.remove) {
    try { activeResultMap.remove(); } catch (e) {}
  }
  activeResultMap = null;
}

function renderCardResult(data) {
  var columns = data.columns || [];
  var rows = data.rows || [];
  var html = '<div class="result-card-grid mt-1">';
  rows.forEach(function(row, idx) {
    html += '<div class="result-record-card"><div class="d-flex justify-content-between align-items-center mb-2">' +
      '<span class="fw-semibold">Record ' + (idx + 1) + '</span><span class="text-muted small">' + columns.length + ' field(s)</span></div>';
    columns.forEach(function(col, cidx) {
      html += '<div class="record-field"><div class="record-key">' + escHtml(col) + '</div><div class="record-value">' +
        escHtml(row[cidx] || '') + '</div></div>';
    });
    html += '</div>';
  });
  html += '</div>';
  return html;
}

function renderStructuredResult(data, mode) {
  var values = [];
  (data.rows || []).forEach(function(row) {
    row.forEach(function(cell) {
      var text = String(cell || '').trim();
      if (!text) return;
      if (mode === 'json' && looksLikeJSON(text)) {
        try { values.push({label: 'JSON cell', value: JSON.parse(text)}); } catch (e) {}
      }
      if (mode === 'xml' && looksLikeXML(text)) {
        values.push({label: 'XML cell', value: parseXMLTree(text)});
      }
    });
  });
  if (values.length === 0 && mode === 'json') {
    values.push({label: 'Rows as JSON', value: rowsAsObjects(data)});
  }
  if (values.length === 0) {
    return '<div class="alert alert-info mt-1 py-2">No ' + mode.toUpperCase() + ' content detected in this result.</div>';
  }
  var html = '<div class="card mt-1"><div class="card-body py-2"><h2 class="h6 mb-2">' +
    '<i class="bi bi-braces text-primary"></i> ' + mode.toUpperCase() + ' Tree</h2>';
  values.slice(0, 20).forEach(function(item, idx) {
    html += '<details class="tree-node" open><summary>' + escHtml(item.label + ' ' + (idx + 1)) + '</summary>' +
      renderTreeValue(item.value) + '</details>';
  });
  html += '</div></div>';
  return html;
}

function rowsAsObjects(data) {
  return (data.rows || []).map(function(row) {
    var obj = {};
    (data.columns || []).forEach(function(col, idx) { obj[col] = row[idx]; });
    return obj;
  });
}

function looksLikeJSON(text) {
  return /^[\[{]/.test(text);
}

function looksLikeXML(text) {
  return /^<[^>]+>/.test(text);
}

function parseXMLTree(text) {
  try {
    var doc = new DOMParser().parseFromString(text, 'application/xml');
    if (doc.querySelector('parsererror')) return text;
    return xmlNodeToObject(doc.documentElement);
  } catch (e) {
    return text;
  }
}

function xmlNodeToObject(node) {
  var out = {name: node.nodeName};
  if (node.attributes && node.attributes.length) {
    out.attributes = {};
    Array.prototype.forEach.call(node.attributes, function(attr) { out.attributes[attr.name] = attr.value; });
  }
  var children = Array.prototype.filter.call(node.childNodes || [], function(child) {
    return child.nodeType === 1 || child.nodeType === 3 && child.nodeValue.trim();
  });
  if (children.length === 1 && children[0].nodeType === 3) {
    out.text = children[0].nodeValue.trim();
  } else if (children.length) {
    out.children = children.map(function(child) {
      return child.nodeType === 3 ? child.nodeValue.trim() : xmlNodeToObject(child);
    });
  }
  return out;
}

function renderTreeValue(value) {
  if (value === null || value === undefined) {
    return '<div class="tree-leaf null-cell">null</div>';
  }
  if (typeof value !== 'object') {
    return '<div class="tree-leaf">' + escHtml(String(value)) + '</div>';
  }
  if (Array.isArray(value)) {
    return '<div class="tree-children">' + value.map(function(item, idx) {
      return '<details class="tree-node"><summary>[' + idx + ']</summary>' + renderTreeValue(item) + '</details>';
    }).join('') + '</div>';
  }
  return '<div class="tree-children">' + Object.keys(value).map(function(key) {
    var child = value[key];
    if (child !== null && typeof child === 'object') {
      return '<details class="tree-node"><summary>' + escHtml(key) + '</summary>' + renderTreeValue(child) + '</details>';
    }
    return '<div class="tree-leaf"><span class="tree-key">' + escHtml(key) + ':</span> ' + escHtml(String(child)) + '</div>';
  }).join('') + '</div>';
}

function renderMapResult(data) {
  var fc = buildGeoFeatureCollectionFromRows(data);
  if (!fc.features.length) {
    return '<div class="alert alert-info mt-1 py-2">No GeoJSON geometry or latitude/longitude columns detected.</div>';
  }
  window.setTimeout(function() { initResultMap(fc); }, 0);
  return '<div class="card mt-1"><div class="card-body py-2">' +
    '<div class="d-flex justify-content-between align-items-center mb-2">' +
    '<h2 class="h6 mb-0"><i class="bi bi-geo-alt text-primary"></i> Map</h2>' +
    '<span class="small text-muted">' + fc.features.length + ' feature(s)</span></div>' +
    '<div id="resultMapCanvas" class="query-map-canvas"></div></div></div>';
}

function buildGeoFeatureCollectionFromRows(data) {
  var columns = data.columns || [];
  var rows = data.rows || [];
  var features = [];
  var geometryIdx = detectGeoJSONColumn(columns, rows);
  var lonLat = detectLonLatIndexes(columns);
  rows.forEach(function(row) {
    var props = rowPropertiesForMap(columns, row, geometryIdx, lonLat.lon, lonLat.lat);
    if (geometryIdx >= 0 && geometryIdx < row.length) {
      features = features.concat(featuresFromGeoJSONCell(row[geometryIdx], props));
      return;
    }
    if (lonLat.lon >= 0 && lonLat.lat >= 0 && lonLat.lon < row.length && lonLat.lat < row.length) {
      var lon = Number(row[lonLat.lon]);
      var lat = Number(row[lonLat.lat]);
      if (isFinite(lon) && isFinite(lat) && lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90) {
        features.push({type: 'Feature', geometry: {type: 'Point', coordinates: [lon, lat]}, properties: props});
      }
    }
  });
  return {type: 'FeatureCollection', features: features};
}

function detectGeoJSONColumn(columns, rows) {
  var bestIdx = -1;
  var bestScore = 0;
  columns.forEach(function(col, idx) {
    var score = /geojson|geometry|geom|shape/i.test(col || '') ? 2 : 0;
    rows.forEach(function(row) {
      if (idx < row.length && featuresFromGeoJSONCell(row[idx], {}).length) score += 3;
    });
    if (score > bestScore) {
      bestScore = score;
      bestIdx = idx;
    }
  });
  return bestScore >= 3 ? bestIdx : -1;
}

function detectLonLatIndexes(columns) {
  var out = {lon: -1, lat: -1};
  columns.forEach(function(col, idx) {
    var name = String(col || '').trim();
    if (out.lon < 0 && /^(lon|lng|long|longitude|x)$/i.test(name)) out.lon = idx;
    if (out.lat < 0 && /^(lat|latitude|y)$/i.test(name)) out.lat = idx;
  });
  return out;
}

function rowPropertiesForMap(columns, row) {
  var skip = {};
  Array.prototype.slice.call(arguments, 2).forEach(function(idx) {
    if (idx >= 0) skip[idx] = true;
  });
  var props = {};
  columns.forEach(function(col, idx) {
    if (skip[idx]) return;
    props[col] = idx < row.length ? row[idx] : '';
  });
  return props;
}

function featuresFromGeoJSONCell(raw, rowProps) {
  var text = String(raw || '').trim();
  if (!text || text.charAt(0) !== '{') return [];
  try {
    return featuresFromGeoJSONObject(JSON.parse(text), rowProps || {});
  } catch (e) {
    return [];
  }
}

function featuresFromGeoJSONObject(obj, rowProps) {
  if (!obj || !obj.type) return [];
  var type = String(obj.type).toLowerCase();
  if (type === 'featurecollection') {
    var out = [];
    (obj.features || []).forEach(function(feature) {
      out = out.concat(featuresFromGeoJSONObject(feature, rowProps));
    });
    return out;
  }
  if (type === 'feature') {
    if (!isGeoJSONGeometry(obj.geometry)) return [];
    return [{type: 'Feature', geometry: obj.geometry, properties: mergeMapProperties(rowProps, obj.properties || {})}];
  }
  if (isGeoJSONGeometry(obj)) {
    return [{type: 'Feature', geometry: obj, properties: rowProps || {}}];
  }
  return [];
}

function isGeoJSONGeometry(obj) {
  if (!obj || !obj.type) return false;
  return /^(Point|MultiPoint|LineString|MultiLineString|Polygon|MultiPolygon|GeometryCollection)$/i.test(obj.type);
}

function mergeMapProperties(first, second) {
  var out = {};
  Object.keys(first || {}).forEach(function(key) { out[key] = first[key]; });
  Object.keys(second || {}).forEach(function(key) { out[key] = second[key]; });
  return out;
}

function initResultMap(featureCollection) {
  var target = document.getElementById('resultMapCanvas');
  if (!target) return;
  if (activeResultMap && activeResultMap.remove) {
    try { activeResultMap.remove(); } catch (e) {}
  }
  activeResultMap = null;
  if (window.maplibregl && window.maplibregl.Map) {
    try {
      renderMapLibreResult(target, featureCollection);
      return;
    } catch (e) {
      disposeActiveResultMap();
    }
  }
  renderSVGResultMap(target, featureCollection);
}

function initTileLayerMap(elementID) {
  var target = document.getElementById(elementID);
  if (!target || !window.maplibregl || !window.maplibregl.Map) return;
  var tileJSONURL = target.getAttribute('data-tilejson-url');
  if (!tileJSONURL) return;
  fetch(tileJSONURL, {headers: {Accept: 'application/json'}})
    .then(function(response) {
      if (!response.ok) throw new Error('Could not load map layer metadata.');
      return response.json();
    })
    .then(function(tileJSON) { renderTileLayerMap(target, tileJSONURL, tileJSON); })
    .catch(function(error) {
      target.innerHTML = '<div class="alert alert-danger m-3 py-2">' + escHtml(error.message) + '</div>';
    });
}

function initRoutingPage() {
  var workspace = document.querySelector('.routing-workspace');
  var form = document.getElementById('routingForm');
  if (!workspace || !form) return;
  var mode = document.getElementById('routingMode');
  var destination = document.getElementById('routingDestinationGroup');
  var maxCost = document.getElementById('routingMaxCostGroup');
  function refreshMode() {
    var reachable = mode.value === 'reachable';
    destination.classList.toggle('d-none', reachable);
    maxCost.classList.toggle('d-none', !reachable);
    document.getElementById('routingTo').required = !reachable;
    document.getElementById('routingMaxCost').required = reachable;
  }
  mode.addEventListener('change', refreshMode);
  refreshMode();
  form.addEventListener('submit', function(event) {
    event.preventDefault();
    runRoutingRequest(workspace.getAttribute('data-table'), mode.value);
  });
}

function runRoutingRequest(table, mode) {
  var result = document.getElementById('routingResult');
  var payload = {
    from_id: document.getElementById('routingFrom').value.trim(),
    cost_field: document.getElementById('routingCost').value.trim() || 'cost',
    directed: document.getElementById('routingDirected').checked
  };
  if (mode === 'reachable') payload.max_cost = Number(document.getElementById('routingMaxCost').value);
  else payload.to_id = document.getElementById('routingTo').value.trim();
  result.innerHTML = '<div class="small text-muted py-3">Calculating…</div>';
  fetch('/api/routing/' + encodeURIComponent(table) + '/' + (mode === 'reachable' ? 'reachable' : 'route'), {
    method: 'POST', headers: {'Content-Type': 'application/json', Accept: 'application/json'}, body: JSON.stringify(payload)
  }).then(function(response) {
    return response.json().then(function(data) {
      if (!response.ok) throw new Error((data.detail || data.title || 'Routing request failed.'));
      return data;
    });
  }).then(function(data) {
    renderRoutingResult(result, data, mode);
  }).catch(function(error) {
    result.innerHTML = '<div class="alert alert-danger mt-3 py-2">' + escHtml(error.message) + '</div>';
  });
}

function renderRoutingResult(target, data, mode) {
  var geojson = data.geojson || {type: 'FeatureCollection', features: []};
  var fc = geojson.type === 'Feature' ? {type: 'FeatureCollection', features: [geojson]} : geojson;
  var summary = mode === 'reachable'
    ? String(data.reachable_nodes || 0) + ' reachable node(s), max cost ' + escHtml(String(data.max_cost))
    : String(data.edge_count || 0) + ' edge(s), cost ' + escHtml(String(data.cost)) + ', distance ' + escHtml(String(Math.round(data.distance_meters || 0))) + ' m';
  target.innerHTML = '<div class="routing-result-summary">' + summary + '</div><div id="routingMapCanvas" class="query-map-canvas"></div>';
  var mapTarget = document.getElementById('routingMapCanvas');
  if (mapTarget) renderSVGResultMap(mapTarget, fc);
}

function renderTileLayerMap(target, tileJSONURL, tileJSON) {
  var format = String(tileJSON.format || '').toLowerCase();
  var vector = /^(pbf|mvt|vector|application\/x-protobuf)$/.test(format);
  var map = new maplibregl.Map({
    container: target,
    style: {version: 8, sources: {}, layers: [{id: 'background', type: 'background', paint: {'background-color': '#eef2f7'}}]},
    center: [0, 0],
    zoom: 1,
    attributionControl: true
  });
  map.on('load', function() {
    if (vector) {
      map.addSource('datadock-layer', {type: 'vector', url: tileJSONURL});
      var layers = tileJSON.vector_layers || [];
      layers.forEach(function(layer, index) {
        var sourceLayer = String(layer.id || '').trim();
        if (!sourceLayer) return;
        var prefix = 'datadock-' + sourceLayer.replace(/[^a-zA-Z0-9_-]/g, '_') + '-' + index;
        map.addLayer({id: prefix + '-fill', type: 'fill', source: 'datadock-layer', 'source-layer': sourceLayer,
          filter: ['==', ['geometry-type'], 'Polygon'], paint: {'fill-color': '#3b82f6', 'fill-opacity': 0.18}});
        map.addLayer({id: prefix + '-line', type: 'line', source: 'datadock-layer', 'source-layer': sourceLayer,
          filter: ['==', ['geometry-type'], 'LineString'], paint: {'line-color': '#1d4ed8', 'line-width': 1.6}});
        map.addLayer({id: prefix + '-point', type: 'circle', source: 'datadock-layer', 'source-layer': sourceLayer,
          filter: ['==', ['geometry-type'], 'Point'], paint: {'circle-color': '#dc2626', 'circle-radius': 3.5, 'circle-stroke-color': '#ffffff', 'circle-stroke-width': 1}});
      });
      if (!layers.length) {
        target.insertAdjacentHTML('beforeend', '<div class="map-layer-notice">Vector tile metadata has no layer list.</div>');
      }
    } else {
      map.addSource('datadock-layer', {type: 'raster', url: tileJSONURL, tileSize: 256});
      map.addLayer({id: 'datadock-raster', type: 'raster', source: 'datadock-layer'});
    }
    if (Array.isArray(tileJSON.bounds) && tileJSON.bounds.length === 4) {
      map.fitBounds([[tileJSON.bounds[0], tileJSON.bounds[1]], [tileJSON.bounds[2], tileJSON.bounds[3]]], {padding: 36, duration: 0, maxZoom: tileJSON.maxzoom || 16});
    }
  });
}

function renderMapLibreResult(target, featureCollection) {
  target.innerHTML = '';
  var map = new maplibregl.Map({
    container: target,
    style: {
      version: 8,
      sources: {},
      layers: [{id: 'background', type: 'background', paint: {'background-color': '#eef2f7'}}]
    },
    center: [0, 0],
    zoom: 1,
    attributionControl: false
  });
  activeResultMap = map;
  map.on('load', function() {
    map.addSource('result-features', {type: 'geojson', data: featureCollection});
    map.addLayer({id: 'result-fill', type: 'fill', source: 'result-features',
      filter: ['any', ['==', ['geometry-type'], 'Polygon'], ['==', ['geometry-type'], 'MultiPolygon']],
      paint: {'fill-color': '#2563eb', 'fill-opacity': 0.18}});
    map.addLayer({id: 'result-line', type: 'line', source: 'result-features',
      paint: {'line-color': '#1d4ed8', 'line-width': 2}});
    map.addLayer({id: 'result-point', type: 'circle', source: 'result-features',
      filter: ['any', ['==', ['geometry-type'], 'Point'], ['==', ['geometry-type'], 'MultiPoint']],
      paint: {'circle-color': '#dc2626', 'circle-radius': 5, 'circle-stroke-color': '#ffffff', 'circle-stroke-width': 1.5}});
    var bounds = geoFeatureBounds(featureCollection.features);
    if (bounds) {
      map.fitBounds([[bounds.minLon, bounds.minLat], [bounds.maxLon, bounds.maxLat]], {padding: 38, maxZoom: 14, duration: 0});
    }
  });
}

function renderSVGResultMap(target, featureCollection) {
  var bounds = geoFeatureBounds(featureCollection.features);
  if (!bounds) {
    target.innerHTML = '<div class="text-muted small p-3">No renderable coordinates.</div>';
    return;
  }
  var width = 960;
  var height = 420;
  var shapes = featureCollection.features.map(function(feature, idx) {
    return svgForGeometry(feature.geometry, bounds, width, height, idx);
  }).join('');
  target.innerHTML = '<svg class="query-map-svg" viewBox="0 0 ' + width + ' ' + height + '" role="img">' +
    '<rect x="0" y="0" width="' + width + '" height="' + height + '" rx="0" fill="var(--surface-strong)"/>' +
    '<g>' + shapes + '</g></svg>';
}

function svgForGeometry(geometry, bounds, width, height, idx) {
  if (!geometry || !geometry.type) return '';
  var type = String(geometry.type);
  var coords = geometry.coordinates || [];
  if (type === 'Point') {
    return svgPoint(coords, bounds, width, height);
  }
  if (type === 'MultiPoint') {
    return coords.map(function(point) { return svgPoint(point, bounds, width, height); }).join('');
  }
  if (type === 'LineString') {
    return svgLine(coords, bounds, width, height);
  }
  if (type === 'MultiLineString') {
    return coords.map(function(line) { return svgLine(line, bounds, width, height); }).join('');
  }
  if (type === 'Polygon') {
    return svgPolygon(coords, bounds, width, height, idx);
  }
  if (type === 'MultiPolygon') {
    return coords.map(function(poly, pidx) { return svgPolygon(poly, bounds, width, height, idx + pidx); }).join('');
  }
  if (type === 'GeometryCollection') {
    return (geometry.geometries || []).map(function(child) { return svgForGeometry(child, bounds, width, height, idx); }).join('');
  }
  return '';
}

function svgPoint(coord, bounds, width, height) {
  var p = projectGeoCoord(coord, bounds, width, height);
  if (!p) return '';
  return '<circle cx="' + p.x + '" cy="' + p.y + '" r="5" class="query-map-point"/>';
}

function svgLine(coords, bounds, width, height) {
  var points = coords.map(function(coord) { return projectGeoCoord(coord, bounds, width, height); }).filter(Boolean);
  if (points.length < 2) return '';
  return '<polyline points="' + points.map(function(p) { return p.x + ',' + p.y; }).join(' ') + '" class="query-map-line"/>';
}

function svgPolygon(rings, bounds, width, height, idx) {
  var path = rings.map(function(ring) {
    var points = ring.map(function(coord) { return projectGeoCoord(coord, bounds, width, height); }).filter(Boolean);
    if (points.length < 3) return '';
    return 'M ' + points.map(function(p) { return p.x + ' ' + p.y; }).join(' L ') + ' Z';
  }).join(' ');
  if (!path) return '';
  return '<path d="' + path + '" class="query-map-polygon query-map-polygon-' + (idx % 6) + '"/>';
}

function projectGeoCoord(coord, bounds, width, height) {
  if (!coord || coord.length < 2) return null;
  var lon = Number(coord[0]);
  var lat = Number(coord[1]);
  if (!isFinite(lon) || !isFinite(lat)) return null;
  var pad = 26;
  var dx = bounds.maxLon - bounds.minLon || 1;
  var dy = bounds.maxLat - bounds.minLat || 1;
  return {
    x: Math.round((pad + (lon - bounds.minLon) / dx * (width - pad * 2)) * 100) / 100,
    y: Math.round((height - pad - (lat - bounds.minLat) / dy * (height - pad * 2)) * 100) / 100
  };
}

function geoFeatureBounds(features) {
  var bounds = null;
  features.forEach(function(feature) {
    eachGeoCoord(feature.geometry, function(coord) {
      var lon = Number(coord[0]);
      var lat = Number(coord[1]);
      if (!isFinite(lon) || !isFinite(lat)) return;
      if (!bounds) {
        bounds = {minLon: lon, maxLon: lon, minLat: lat, maxLat: lat};
      } else {
        bounds.minLon = Math.min(bounds.minLon, lon);
        bounds.maxLon = Math.max(bounds.maxLon, lon);
        bounds.minLat = Math.min(bounds.minLat, lat);
        bounds.maxLat = Math.max(bounds.maxLat, lat);
      }
    });
  });
  if (bounds && bounds.minLon === bounds.maxLon) {
    bounds.minLon -= 0.01;
    bounds.maxLon += 0.01;
  }
  if (bounds && bounds.minLat === bounds.maxLat) {
    bounds.minLat -= 0.01;
    bounds.maxLat += 0.01;
  }
  return bounds;
}

function eachGeoCoord(geometry, fn) {
  if (!geometry || !geometry.type) return;
  var type = String(geometry.type);
  if (type === 'GeometryCollection') {
    (geometry.geometries || []).forEach(function(child) { eachGeoCoord(child, fn); });
    return;
  }
  walkGeoCoordinates(geometry.coordinates, type, fn);
}

function walkGeoCoordinates(coords, type, fn) {
  if (!coords) return;
  if (type === 'Point') {
    fn(coords);
    return;
  }
  if (type === 'LineString' || type === 'MultiPoint') {
    coords.forEach(fn);
    return;
  }
  if (type === 'Polygon' || type === 'MultiLineString') {
    coords.forEach(function(line) { line.forEach(fn); });
    return;
  }
  if (type === 'MultiPolygon') {
    coords.forEach(function(poly) {
      poly.forEach(function(line) { line.forEach(fn); });
    });
  }
}

function renderPivotResult(data) {
  var columns = data.columns || [];
  var rows = data.rows || [];
  if (columns.length < 2 || rows.length === 0) {
    return '<div class="alert alert-info mt-1 py-2">Pivot needs at least two columns and one row.</div>';
  }
  var groupIdx = firstTextColumn(data);
  var numeric = numericColumnIndexes(data);
  if (groupIdx < 0 || numeric.length === 0) {
    return '<div class="alert alert-info mt-1 py-2">Pivot needs one categorical column and one numeric column.</div>';
  }
  var groups = {};
  rows.forEach(function(row) {
    var key = row[groupIdx] || '(blank)';
    if (!groups[key]) groups[key] = {count: 0, sums: {}};
    groups[key].count++;
    numeric.forEach(function(idx) {
      groups[key].sums[idx] = (groups[key].sums[idx] || 0) + Number(row[idx] || 0);
    });
  });
  var keys = Object.keys(groups).sort(function(a, b) { return groups[b].count - groups[a].count; }).slice(0, 100);
  var html = '<div class="card mt-1"><div class="table-responsive"><table class="table table-sm result-table mb-0">' +
    '<thead><tr><th>' + escHtml(columns[groupIdx]) + '</th><th>count</th>';
  numeric.forEach(function(idx) { html += '<th>sum ' + escHtml(columns[idx]) + '</th>'; });
  html += '</tr></thead><tbody>';
  keys.forEach(function(key) {
    html += '<tr><td>' + escHtml(key) + '</td><td>' + groups[key].count + '</td>';
    numeric.forEach(function(idx) { html += '<td>' + formatNumber(groups[key].sums[idx]) + '</td>'; });
    html += '</tr>';
  });
  html += '</tbody></table></div></div>';
  return html;
}

function renderProfileResult(data) {
  var columns = data.columns || [];
  var rows = data.rows || [];
  var html = '<div class="result-card-grid mt-1">';
  columns.forEach(function(col, idx) {
    var values = rows.map(function(row) { return row[idx] || ''; });
    var nonEmpty = values.filter(function(v) { return String(v).trim() !== ''; });
    var distinct = {};
    values.forEach(function(v) { distinct[v] = true; });
    var nums = nonEmpty.map(Number).filter(function(v) { return !isNaN(v); });
    html += '<div class="result-record-card"><h2 class="h6 mb-2">' + escHtml(col) + '</h2>' +
      '<div class="small text-muted mb-2">' + nonEmpty.length + '/' + rows.length + ' non-empty · ' +
      Object.keys(distinct).length + ' distinct</div>';
    if (nums.length) {
      var sum = nums.reduce(function(a, b) { return a + b; }, 0);
      html += '<div class="record-field"><div class="record-key">min</div><div class="record-value">' + formatNumber(Math.min.apply(null, nums)) + '</div></div>' +
        '<div class="record-field"><div class="record-key">max</div><div class="record-value">' + formatNumber(Math.max.apply(null, nums)) + '</div></div>' +
        '<div class="record-field"><div class="record-key">avg</div><div class="record-value">' + formatNumber(sum / nums.length) + '</div></div>';
    }
    html += '<div class="record-field"><div class="record-key">examples</div><div class="record-value">' +
      escHtml(nonEmpty.slice(0, 5).join(', ')) + '</div></div></div>';
  });
  html += '</div>';
  return html;
}

function renderNotebookResult(data) {
  return '<div class="notebook-view mt-1">' +
    '<section class="notebook-cell"><div class="notebook-cell-title">SQL</div><pre>' + escHtml(getEditorValue()) + '</pre></section>' +
    '<section class="notebook-cell"><div class="notebook-cell-title">Result Summary</div><p>' +
    escHtml(queryStatusText(data)) + '</p></section>' +
    '<section class="notebook-cell"><div class="notebook-cell-title">Data</div>' + renderTableFragment(data) + '</section>' +
    '</div>';
}

function renderTableFragment(data) {
  if (!data || !data.columns || data.columns.length === 0) return '<div class="text-muted small">No tabular result.</div>';
  var html = '<div class="table-responsive"><table class="table table-sm result-table mb-0"><thead><tr>';
  data.columns.forEach(function(c) { html += '<th>' + escHtml(c) + '</th>'; });
  html += '</tr></thead><tbody>';
  (data.rows || []).slice(0, 50).forEach(function(row) {
    html += '<tr>';
    row.forEach(function(cell) { html += '<td>' + escHtml(cell || '') + '</td>'; });
    html += '</tr>';
  });
  html += '</tbody></table></div>';
  return html;
}

function renderSchemaGraphShell() {
  return '<div class="card mt-1"><div class="card-body py-2"><div class="d-flex justify-content-between align-items-center mb-2">' +
    '<h2 class="h6 mb-0"><i class="bi bi-diagram-3 text-primary"></i> Schema Graph</h2>' +
    '<span class="small text-muted">tables, columns, relationships</span></div><div id="schemaGraph" class="schema-graph">Loading...</div></div></div>' +
    '<div class="modal fade" id="schemaObjectModal" tabindex="-1" aria-labelledby="schemaObjectModalTitle" aria-hidden="true">' +
      '<div class="modal-dialog modal-lg modal-dialog-scrollable">' +
        '<div class="modal-content">' +
          '<div class="modal-header">' +
            '<div><h2 class="modal-title h5" id="schemaObjectModalTitle">Object</h2><div class="small text-muted" id="schemaObjectModalKind"></div></div>' +
            '<button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close"></button>' +
          '</div>' +
          '<div class="modal-body" id="schemaObjectModalBody"></div>' +
          '<div class="modal-footer">' +
            '<button type="button" class="btn btn-outline-secondary btn-sm" data-bs-dismiss="modal">Close</button>' +
            '<a class="btn btn-primary btn-sm" id="schemaObjectOpenLink" href="#"><i class="bi bi-box-arrow-up-right me-1"></i>Open</a>' +
          '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
}

var schemaGraphTablesByName = {};

function loadSchemaGraph() {
  fetch('/api/schema')
    .then(function(r) { return r.json(); })
    .then(function(schema) {
      var target = document.getElementById('schemaGraph');
      if (!target) return;
      var tables = schema.tables || [];
      var relationships = schema.relationships || [];
      if (!tables.length) {
        target.innerHTML = '<div class="text-muted small">No schema context available.</div>';
        return;
      }
      schemaGraphTablesByName = {};
      var html = '<div id="sg-tables" class="schema-graph-grid">';
      tables.forEach(function(table) {
        schemaGraphTablesByName[table.name] = table;
        html += '<button type="button" class="schema-node" data-schema-object="' + escHtml(table.name) + '" onclick="openSchemaObjectModal(this.dataset.schemaObject)">' +
          '<div class="schema-node-title">' + escHtml(table.name) +
          '<span class="text-muted small ms-1">' + escHtml(table.kind || 'table') + '</span></div>';
        (table.columns || []).forEach(function(col) {
          html += '<div class="schema-column">' + escHtml(col.name) +
            '<span class="text-muted"> ' + escHtml(col.type || '') + '</span></div>';
        });
        html += '</button>';
      });
      html += '</div>';
      if (relationships.length) {
        html += '<div class="mt-3 small"><div class="fw-semibold mb-1">Relationships</div>';
        relationships.forEach(function(rel) {
          html += '<div class="schema-edge">' + escHtml(rel.from || '') + ' → ' + escHtml(rel.to || '') + '</div>';
        });
        html += '</div>';
      }
      target.innerHTML = html;
    })
    .catch(function(err) {
      var target = document.getElementById('schemaGraph');
      if (target) target.textContent = err.message || String(err);
    });
}

function openSchemaObjectModal(name) {
  var table = schemaGraphTablesByName[name];
  if (!table) return;
  var title = document.getElementById('schemaObjectModalTitle');
  var kind = document.getElementById('schemaObjectModalKind');
  var body = document.getElementById('schemaObjectModalBody');
  var openLink = document.getElementById('schemaObjectOpenLink');
  if (!title || !kind || !body || !openLink) return;

  title.textContent = table.name || 'Object';
  kind.textContent = table.kind || 'table';
  openLink.href = '/t/' + encodeURIComponent(table.name || '');

  var html = '<div class="table-responsive"><table class="table table-sm mb-0">' +
    '<thead><tr><th>Column</th><th>Type</th></tr></thead><tbody>';
  var columns = table.columns || [];
  if (!columns.length) {
    html += '<tr><td colspan="2" class="text-muted">No columns available.</td></tr>';
  } else {
    columns.forEach(function(col) {
      html += '<tr><td><code>' + escHtml(col.name || '') + '</code></td><td>' + escHtml(col.type || '') + '</td></tr>';
    });
  }
  html += '</tbody></table></div>';
  body.innerHTML = html;

  if (window.bootstrap && window.bootstrap.Modal) {
    window.bootstrap.Modal.getOrCreateInstance(document.getElementById('schemaObjectModal')).show();
  }
}

function firstTextColumn(data) {
  for (var i = 0; i < (data.columns || []).length; i++) {
    var numericCount = 0;
    (data.rows || []).forEach(function(row) {
      if (row[i] !== '' && !isNaN(Number(row[i]))) numericCount++;
    });
    if (numericCount < Math.max(1, Math.floor((data.rows || []).length * 0.5))) return i;
  }
  return -1;
}

function numericColumnIndexes(data) {
  var out = [];
  (data.columns || []).forEach(function(_, idx) {
    var numericCount = 0;
    (data.rows || []).forEach(function(row) {
      if (row[idx] !== '' && !isNaN(Number(row[idx]))) numericCount++;
    });
    if (numericCount > 0 && numericCount >= Math.ceil((data.rows || []).length * 0.6)) out.push(idx);
  });
  return out;
}

function formatNumber(value) {
  if (!isFinite(value)) return '';
  return Math.round(value * 1000) / 1000;
}

function renderLogResult(data) {
  var tab = getActiveQueryTab() || {};
  var filter = (tab.logFilter || '').toLowerCase();
  var columns = data.columns || [];
  var rows = (data.rows || []).slice(-1000);
  var rendered = rows.map(function(row) {
    return logLineFromRow(columns, row);
  }).filter(function(line) {
    return !filter || line.text.toLowerCase().indexOf(filter) >= 0;
  });
  var html = '<div class="card mt-1"><div class="card-body py-2">' +
    '<div class="d-flex justify-content-between align-items-center mb-2">' +
    '<h2 class="h6 mb-0"><i class="bi bi-file-text text-primary"></i> Logs</h2>' +
    '<span class="small text-muted">' + rendered.length + ' visible / ' + rows.length + ' row(s)</span>' +
    '</div><div id="logViewer" class="log-viewer">';
  if (rendered.length === 0) {
    html += '<div class="text-muted small px-2 py-2">No log rows match the current filter.</div>';
  } else {
    rendered.forEach(function(line) {
      html += '<div class="log-line log-' + escHtml(line.level) + '">' + escHtml(line.text) + '</div>';
    });
  }
  html += '</div></div></div>';
  window.setTimeout(function() {
    var viewer = document.getElementById('logViewer');
    if (viewer) viewer.scrollTop = viewer.scrollHeight;
  }, 0);
  return html;
}

function logLineFromRow(columns, row) {
  var time = '';
  var level = '';
  var message = '';
  var parts = [];
  columns.forEach(function(col, idx) {
    var name = String(col || '');
    var value = String(row[idx] || '');
    if (!time && /time|date|ts|timestamp|created|updated/i.test(name)) {
      time = value;
      return;
    }
    if (!level && /level|severity|status|priority/i.test(name)) {
      level = normalizeLogLevel(value);
      parts.push(name + '=' + value);
      return;
    }
    if (!message && /msg|message|log|event|error|detail|text/i.test(name)) {
      message = value;
      return;
    }
    parts.push(name + '=' + value);
  });
  if (!level) {
    level = normalizeLogLevel(row.join(' '));
  }
  var textParts = [];
  if (time) textParts.push(time);
  textParts.push('[' + level.toUpperCase() + ']');
  if (message) textParts.push(message);
  if (parts.length > 0) textParts.push(parts.join(' '));
  return {level: level, text: textParts.join(' ')};
}

function normalizeLogLevel(value) {
  value = String(value || '').toLowerCase();
  if (/fatal|panic|critical|crit/.test(value)) return 'fatal';
  if (/error|err|fail|failed/.test(value)) return 'error';
  if (/warn|warning/.test(value)) return 'warn';
  if (/debug|trace/.test(value)) return 'debug';
  if (/info|notice|ok|success|succeeded/.test(value)) return 'info';
  return 'default';
}

function saveLocalQueryHistory(sql, rowCount, elapsedMs, errorText) {
  if (!sql) return;
  var items = loadLocalQueryHistory().filter(function(item) { return item.sql !== sql; });
  items.unshift({
    sql: sql,
    rowCount: rowCount || 0,
    elapsedMs: elapsedMs || 0,
    error: errorText || '',
    at: new Date().toISOString()
  });
  if (items.length > maxLocalQueryHistory) {
    items = items.slice(0, maxLocalQueryHistory);
  }
  try {
    localStorage.setItem(queryHistoryKey, JSON.stringify(items));
  } catch (e) {
    return;
  }
}

function loadLocalQueryHistory() {
  try {
    var raw = localStorage.getItem(queryHistoryKey);
    return raw ? JSON.parse(raw) || [] : [];
  } catch (e) {
    return [];
  }
}

// Quick Chart picks a sensible dimension/measure automatically, but keeps
// the raw result data and the current choice around so the "Group by" /
// "Measure" controls can re-aggregate and redraw without re-running the query.
var quickChartMaxCategories = 20;
var quickChartState = {data: null, choice: null};

function renderAIChart(data, spec, explanation) {
  var area = document.getElementById('chartArea');
  if (!window.d3 || !data || !data.columns || !data.rows || !data.rows.length || !spec) return false;
  var xIdx = data.columns.indexOf(spec.x || '');
  var yIdx = data.columns.indexOf(spec.y || '');
  if (xIdx < 0) return false;
  var aggregation = (spec.aggregation || (yIdx >= 0 ? 'sum' : 'count')).toLowerCase();
  if (aggregation !== 'count' && yIdx < 0) return false;
  var type = (spec.type || 'bar').toLowerCase();
  if (type !== 'bar' && type !== 'line' && type !== 'area') type = 'bar';
  var points = aggregateAIChart(data, xIdx, yIdx, aggregation, type !== 'bar');
  if (!points.length) return false;
  var xName = data.columns[xIdx];
  var yName = yIdx >= 0 ? data.columns[yIdx] : 'rows';
  var title = spec.title || ((aggregation === 'count' ? 'Count' : aggregation.toUpperCase() + ' ' + yName) + ' by ' + xName);
  area.innerHTML = '<div class="card h-100"><div class="card-body py-2">' +
    '<div class="d-flex justify-content-between align-items-start mb-2 gap-2 flex-wrap">' +
    '<div><h2 class="h6 mb-0"><i class="bi bi-stars text-primary"></i> AI Chart</h2>' +
    '<span class="small text-muted">' + escHtml(title) + '</span></div>' +
    '<span class="badge text-bg-light">' + escHtml(type) + '</span></div>' +
    (explanation ? '<div class="small text-muted mb-2">' + escHtml(explanation) + '</div>' : '') +
    '<div id="aiChart"></div></div></div>';
  drawAIChart(points, type);
  return true;
}

function aggregateAIChart(data, xIdx, yIdx, aggregation, preferTime) {
  var groups = {}, order = [];
  (data.rows || []).forEach(function(row, i) {
    var raw = xIdx >= 0 ? row[xIdx] : 'Row ' + (i + 1);
    var label = (raw === '' || raw === null || raw === undefined) ? '(empty)' : String(raw);
    if (!groups[label]) {
      groups[label] = {label: label, value: 0, count: 0, date: preferTime ? parseChartDate(raw) : null};
      order.push(label);
    }
    groups[label].count++;
    var n = yIdx >= 0 ? Number(row[yIdx]) : 1;
    if (aggregation === 'count') {
      groups[label].value = groups[label].count;
    } else if (!isNaN(n)) {
      groups[label].value += n;
    }
  });
  var points = order.map(function(label) {
    var p = groups[label];
    if (aggregation === 'avg' && p.count > 0) p.value = p.value / p.count;
    return p;
  });
  if (preferTime) {
    points = points.filter(function(p) { return p.date; }).sort(function(a, b) { return a.date - b.date; });
  } else {
    points.sort(function(a, b) { return b.value - a.value; });
    points = points.slice(0, quickChartMaxCategories);
  }
  return points;
}

function drawAIChart(points, type) {
  var target = document.querySelector('#chartArea .card-body');
  var width = Math.max(320, target ? target.clientWidth - 12 : 640);
  var height = 240;
  var margin = {top: 8, right: 12, bottom: 54, left: 54};
  var svg = d3.select('#aiChart').append('svg').attr('viewBox', '0 0 ' + width + ' ' + height).attr('role', 'img');
  var maxY = d3.max(points, function(d) { return d.value; }) || 0;
  var y = d3.scaleLinear().domain([0, maxY]).nice().range([height - margin.bottom, margin.top]);
  svg.append('g').attr('transform', 'translate(' + margin.left + ',0)').call(d3.axisLeft(y).ticks(5));
  var allDates = points.length >= 2 && points.every(function(p) { return !!p.date; });
  if ((type === 'line' || type === 'area') && allDates) {
    var xTime = d3.scaleTime().domain(d3.extent(points, function(d) { return d.date; })).range([margin.left, width - margin.right]);
    svg.append('g').attr('transform', 'translate(0,' + (height - margin.bottom) + ')').call(d3.axisBottom(xTime).ticks(5));
    if (type === 'area') {
      svg.append('path').datum(points).attr('fill', 'rgba(21,101,192,.18)').attr('d', d3.area().x(function(d) { return xTime(d.date); }).y0(y(0)).y1(function(d) { return y(d.value); }));
    }
    svg.append('path').datum(points).attr('fill', 'none').attr('stroke', '#1565c0').attr('stroke-width', 2).attr('d', d3.line().x(function(d) { return xTime(d.date); }).y(function(d) { return y(d.value); }));
    svg.append('g').selectAll('circle').data(points).join('circle').attr('cx', function(d) { return xTime(d.date); }).attr('cy', function(d) { return y(d.value); }).attr('r', 3).attr('fill', '#1565c0').append('title').text(function(d) { return d.label + ': ' + d.value; });
    return;
  }
  var x = d3.scaleBand().domain(points.map(function(d) { return d.label; })).range([margin.left, width - margin.right]).padding(0.18);
  svg.append('g').attr('transform', 'translate(0,' + (height - margin.bottom) + ')').call(d3.axisBottom(x).tickFormat(function(d) { return String(d).slice(0, 12); })).selectAll('text').attr('transform', 'rotate(-35)').style('text-anchor', 'end');
  svg.append('g').selectAll('rect').data(points).join('rect').attr('x', function(d) { return x(d.label); }).attr('y', function(d) { return y(Math.max(0, d.value)); }).attr('width', x.bandwidth()).attr('height', function(d) { return Math.max(0, y(0) - y(Math.max(0, d.value))); }).attr('fill', '#1565c0').append('title').text(function(d) { return d.label + ': ' + d.value; });
}

function renderQuickChart(data, choiceOverride) {
  var area = document.getElementById('chartArea');
  if (!window.d3 || !data || !data.columns || data.columns.length < 1 || !data.rows || data.rows.length === 0) {
    area.innerHTML = '';
    quickChartState = {data: null, choice: null};
    return;
  }
  var profiles = profileQuickChartColumns(data);
  var choice = choiceOverride || pickQuickChartDefaults(profiles, data.rows.length);
  if (!choice) {
    area.innerHTML = '';
    quickChartState = {data: null, choice: null};
    return;
  }
  quickChartState = {data: data, choice: choice};

  var agg = aggregateQuickChart(data, choice);
  var points = agg.points;
  if (points.length === 0) {
    area.innerHTML = '';
    return;
  }

  var dimName = choice.dimIdx >= 0 ? data.columns[choice.dimIdx] : 'Row';
  var measureName = choice.metricIdx >= 0 ? data.columns[choice.metricIdx] : 'Count of rows';
  var subtitle = (choice.metricIdx >= 0 ? 'Sum of ' + measureName : measureName) + ' by ' + dimName;
  if (agg.truncated > 0) {
    subtitle += ' · top ' + points.length + ' of ' + (points.length + agg.truncated) + ' shown';
  }

  area.innerHTML = '<div class="card h-100"><div class="card-body py-2">' +
    '<div class="d-flex justify-content-between align-items-start mb-2 gap-2 flex-wrap">' +
    '<div><h2 class="h6 mb-0"><i class="bi bi-bar-chart text-primary"></i> Quick Chart</h2>' +
    '<span class="small text-muted">' + escHtml(subtitle) + '</span></div>' +
    renderQuickChartControls(profiles, choice) +
    '</div><div id="quickChart"></div></div></div>';
  wireQuickChartControls();
  drawQuickChart(points, choice.isTimeDim);
}

// profileQuickChartColumns inspects every column once to learn how numeric,
// date-like, distinct, and id-like it is, so the picker can reason about
// which column is a useful grouping dimension vs. a measure vs. noise
// (e.g. a primary key) without hardcoding "column 0 is always the label".
function profileQuickChartColumns(data) {
  var rows = data.rows || [];
  return data.columns.map(function (name, idx) {
    var seen = {};
    var distinct = 0, numericCount = 0, dateCount = 0, nonEmpty = 0;
    rows.forEach(function (row) {
      var v = row[idx];
      if (v === '' || v === null || v === undefined) return;
      nonEmpty++;
      if (!seen[v]) { seen[v] = true; distinct++; }
      if (!isNaN(Number(v))) numericCount++;
      if (parseChartDate(v)) dateCount++;
    });
    var numericRatio = nonEmpty ? numericCount / nonEmpty : 0;
    var dateRatio = nonEmpty ? dateCount / nonEmpty : 0;
    return {
      idx: idx,
      name: name,
      distinct: distinct,
      nonEmpty: nonEmpty,
      numericRatio: numericRatio,
      isIdLike: /(^|_)(id|uuid|guid)$/i.test(name || ''),
      isDate: dateRatio >= 0.6 && /date|time|year|month|jahr|monat|tag|datum/i.test(name || ''),
      isNumeric: nonEmpty > 0 && numericRatio >= 0.9,
      isCategorical: nonEmpty > 0 && numericRatio < 0.9
    };
  });
}

// pickQuickChartDefaults chooses the most useful dimension/measure pair:
// a time column beats a categorical one; among categorical columns the one
// with the most repetition (lowest distinct/row ratio) wins, since that's
// the column worth grouping by (e.g. a tenant/client/status column) rather
// than a free-text column that's different on every row. When no numeric
// measure exists, it falls back to counting rows per group — so a table
// with only a repeated "Mandant" column still produces a meaningful chart
// (rows per tenant) instead of nothing.
function pickQuickChartDefaults(profiles, rowCount) {
  var dateCol = profiles.filter(function (p) { return p.isDate; })[0];
  var textCandidates = profiles.filter(function (p) { return p.isCategorical && p.distinct > 1; });
  textCandidates.sort(function (a, b) {
    if (a.isIdLike !== b.isIdLike) return a.isIdLike ? 1 : -1;
    var ra = a.distinct / Math.max(1, rowCount), rb = b.distinct / Math.max(1, rowCount);
    if (ra !== rb) return ra - rb;
    return a.idx - b.idx;
  });
  var dimension = dateCol || textCandidates[0] || profiles[0];
  if (!dimension) return null;

  var numericCandidates = profiles.filter(function (p) {
    return p.isNumeric && p.idx !== dimension.idx && !p.isIdLike;
  });
  numericCandidates.sort(function (a, b) { return b.numericRatio - a.numericRatio || a.idx - b.idx; });
  var metric = numericCandidates[0] || null;

  return {dimIdx: dimension.idx, metricIdx: metric ? metric.idx : -1, isTimeDim: !!dateCol && dimension === dateCol};
}

// aggregateQuickChart groups rows by the dimension column, summing the
// measure column (or counting rows when there's no measure). This handles
// both raw, ungrouped tables (many rows share the same dimension value) and
// already-aggregated query results (one row per group) with the same code
// path — grouping a table that's already unique per key is a no-op.
function aggregateQuickChart(data, choice) {
  var rows = data.rows || [];
  var groups = {}, order = [];
  rows.forEach(function (row, i) {
    var raw = choice.dimIdx >= 0 ? row[choice.dimIdx] : 'Row ' + (i + 1);
    var label = (raw === '' || raw === null || raw === undefined) ? '(empty)' : String(raw);
    if (!groups[label]) {
      groups[label] = {label: label, value: 0, count: 0, date: choice.isTimeDim ? parseChartDate(raw) : null};
      order.push(label);
    }
    groups[label].count++;
    if (choice.metricIdx >= 0) {
      var v = Number(row[choice.metricIdx]);
      if (!isNaN(v)) groups[label].value += v;
    } else {
      groups[label].value = groups[label].count;
    }
  });
  var points = order.map(function (label) { return groups[label]; });
  var truncated = 0;
  if (choice.isTimeDim) {
    points = points.filter(function (p) { return p.date; }).sort(function (a, b) { return a.date - b.date; });
  } else {
    points.sort(function (a, b) { return b.value - a.value; });
    if (points.length > quickChartMaxCategories) {
      truncated = points.length - quickChartMaxCategories;
      points = points.slice(0, quickChartMaxCategories);
    }
  }
  return {points: points, truncated: truncated};
}

// renderQuickChartControls lets the user override the auto-picked
// dimension/measure without re-running the query (e.g. group by a
// different column, or switch from "count" to summing a numeric column).
function renderQuickChartControls(profiles, choice) {
  var dimOptions = profiles.map(function (p) {
    return '<option value="' + p.idx + '"' + (p.idx === choice.dimIdx ? ' selected' : '') + '>' + escHtml(p.name) + '</option>';
  }).join('');
  var measureOptions = '<option value="-1"' + (choice.metricIdx < 0 ? ' selected' : '') + '>Count of rows</option>' +
    profiles.filter(function (p) { return p.isNumeric; }).map(function (p) {
      return '<option value="' + p.idx + '"' + (p.idx === choice.metricIdx ? ' selected' : '') + '>Sum of ' + escHtml(p.name) + '</option>';
    }).join('');
  return '<div class="d-flex gap-2 flex-wrap small">' +
    '<label class="d-flex align-items-center gap-1 mb-0 text-muted">Group by' +
    '<select class="form-select form-select-sm" id="qcDimSelect" style="width:auto">' + dimOptions + '</select></label>' +
    '<label class="d-flex align-items-center gap-1 mb-0 text-muted">Measure' +
    '<select class="form-select form-select-sm" id="qcMeasureSelect" style="width:auto">' + measureOptions + '</select></label>' +
    '</div>';
}

function wireQuickChartControls() {
  var dimSelect = document.getElementById('qcDimSelect');
  var measureSelect = document.getElementById('qcMeasureSelect');
  if (!dimSelect || !measureSelect) return;
  var onChange = function () {
    if (!quickChartState.data) return;
    var dimIdx = Number(dimSelect.value);
    var profiles = profileQuickChartColumns(quickChartState.data);
    var dimProfile = profiles[dimIdx];
    var choice = {
      dimIdx: dimIdx,
      metricIdx: Number(measureSelect.value),
      isTimeDim: !!(dimProfile && dimProfile.isDate)
    };
    getQueryTabRuntime(activeQueryTabID).chartChoice = choice;
    renderQuickChart(quickChartState.data, choice);
  };
  dimSelect.addEventListener('change', onChange);
  measureSelect.addEventListener('change', onChange);
}

function drawQuickChart(points, isTimeDim) {
  var width = Math.max(320, document.querySelector('#chartArea .card-body').clientWidth - 12);
  var height = 220;
  var margin = {top: 8, right: 12, bottom: 54, left: 54};
  var svg = d3.select('#quickChart').append('svg')
    .attr('viewBox', '0 0 ' + width + ' ' + height)
    .attr('role', 'img');
  var maxY = d3.max(points, function(d) { return d.value; }) || 0;
  var y = d3.scaleLinear()
    .domain([0, maxY]).nice()
    .range([height - margin.bottom, margin.top]);
  svg.append('g')
    .attr('transform', 'translate(' + margin.left + ',0)')
    .call(d3.axisLeft(y).ticks(5));
  if (isTimeDim && points.length >= 2) {
    var xTime = d3.scaleTime()
      .domain(d3.extent(points, function(d) { return d.date; }))
      .range([margin.left, width - margin.right]);
    svg.append('g')
      .attr('transform', 'translate(0,' + (height - margin.bottom) + ')')
      .call(d3.axisBottom(xTime).ticks(5));
    svg.append('path')
      .datum(points)
      .attr('fill', 'none')
      .attr('stroke', '#1565c0')
      .attr('stroke-width', 2)
      .attr('d', d3.line().x(function(d) { return xTime(d.date); }).y(function(d) { return y(d.value); }));
    svg.append('g')
      .selectAll('circle')
      .data(points)
      .join('circle')
      .attr('cx', function(d) { return xTime(d.date); })
      .attr('cy', function(d) { return y(d.value); })
      .attr('r', 3)
      .attr('fill', '#1565c0')
      .append('title').text(function(d) { return d.label + ': ' + d.value; });
  } else {
    var x = d3.scaleBand()
      .domain(points.map(function(d) { return d.label; }))
      .range([margin.left, width - margin.right])
      .padding(0.18);
    svg.append('g')
      .attr('transform', 'translate(0,' + (height - margin.bottom) + ')')
      .call(d3.axisBottom(x).tickFormat(function(d) { return String(d).slice(0, 12); }))
      .selectAll('text')
      .attr('transform', 'rotate(-35)')
      .style('text-anchor', 'end');
    svg.append('g')
      .selectAll('rect')
      .data(points)
      .join('rect')
      .attr('x', function(d) { return x(d.label); })
      .attr('y', function(d) { return y(Math.max(0, d.value)); })
      .attr('width', x.bandwidth())
      .attr('height', function(d) { return Math.max(0, y(0) - y(Math.max(0, d.value))); })
      .attr('fill', '#1565c0')
      .append('title').text(function(d) { return d.label + ': ' + d.value; });
  }
}

function parseChartDate(value) {
  if (value === null || value === undefined || value === '') return null;
  var s = String(value);
  if (/^\d{4}$/.test(s)) return new Date(Number(s), 0, 1);
  if (/^\d{4}-\d{2}$/.test(s)) return new Date(Number(s.slice(0, 4)), Number(s.slice(5, 7)) - 1, 1);
  var d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}


// ── Connections page (/connections) ─────────────────────────────────────
//
// Both functions are called directly from an inline <script> in
// connections.html (not just declared here) so DataDock's AJAX shell
// navigation re-wires this page's forms every time it loads into the
// shell, not just on a full browser navigation — see executeScripts().

function initConnectionForm() {
  var kind = document.getElementById('connectionKind');
  var quickFields = document.getElementById('quickConnectFields');
  var advancedField = document.getElementById('advancedDSNField');
  var dsn = document.getElementById('connectionDSN');
  var modeQuickBtn = document.getElementById('modeQuickBtn');
  var modeAdvancedBtn = document.getElementById('modeAdvancedBtn');
  var qcPort = document.getElementById('qcPort');
  var qcInstanceWrap = document.getElementById('qcInstanceWrap');
  var qcMssqlOptions = document.getElementById('qcMssqlOptions');
  var qcPostgresOptions = document.getElementById('qcPostgresOptions');
  var qcAuthModeWrap = document.getElementById('qcAuthModeWrap');
  var qcAuthMode = document.getElementById('qcAuthMode');
  var qcUserWrap = document.getElementById('qcUserWrap');
  var qcUserLabel = document.getElementById('qcUserLabel');
  var qcUser = document.getElementById('qcUser');
  var qcPasswordWrap = document.getElementById('qcPasswordWrap');
  var qcWindowsCurrentHint = document.getElementById('qcWindowsCurrentHint');
  if (!kind || !quickFields || !advancedField) return;

  var defaultPorts = { mssql: '1433', postgres: '5432', mysql: '3306', sqlite: '' };

  function setMode(mode) {
    if (mode === 'advanced') {
      quickFields.classList.add('d-none');
      advancedField.classList.remove('d-none');
      if (dsn) dsn.required = true;
      modeAdvancedBtn.classList.add('active');
      modeQuickBtn.classList.remove('active');
    } else {
      quickFields.classList.remove('d-none');
      advancedField.classList.add('d-none');
      if (dsn) dsn.required = false;
      modeQuickBtn.classList.add('active');
      modeAdvancedBtn.classList.remove('active');
    }
  }

  function refreshAuthModeUI() {
    if (!qcAuthMode) return;
    var isMssql = kind.value === 'mssql';
    var mode = isMssql ? qcAuthMode.value : 'sql';
    var isWindowsCurrent = isMssql && mode === 'windows-current';
    if (qcUserWrap) qcUserWrap.classList.toggle('d-none', isWindowsCurrent);
    if (qcPasswordWrap) qcPasswordWrap.classList.toggle('d-none', isWindowsCurrent);
    if (qcWindowsCurrentHint) qcWindowsCurrentHint.classList.toggle('d-none', !isWindowsCurrent);
    if (qcUserLabel && qcUser) {
      if (isMssql && mode === 'windows-account') {
        qcUserLabel.textContent = 'User (DOMAIN\\username)';
        qcUser.placeholder = 'CONTOSO\\svc-datadock';
      } else {
        qcUserLabel.textContent = 'User';
        qcUser.placeholder = 'sa';
      }
    }
  }

  function refreshKindUI() {
    var k = kind.value;
    if (k === 'auto') {
      setMode('advanced');
      return;
    }
    if (qcPort) qcPort.placeholder = defaultPorts[k] || '';
    if (qcInstanceWrap) qcInstanceWrap.classList.toggle('d-none', k !== 'mssql');
    if (qcAuthModeWrap) qcAuthModeWrap.classList.toggle('d-none', k !== 'mssql');
    if (qcMssqlOptions) qcMssqlOptions.classList.toggle('d-none', k !== 'mssql');
    if (qcPostgresOptions) qcPostgresOptions.classList.toggle('d-none', k !== 'postgres');
    refreshAuthModeUI();
  }

  modeQuickBtn.addEventListener('click', function () { setMode('quick'); });
  modeAdvancedBtn.addEventListener('click', function () { setMode('advanced'); });
  kind.addEventListener('change', refreshKindUI);
  if (qcAuthMode) qcAuthMode.addEventListener('change', refreshAuthModeUI);

  // After a failed attempt the server re-renders this form with whatever
  // was submitted; restore the matching input mode instead of always
  // defaulting back to Quick Connect.
  if (dsn && dsn.value.trim() !== '') {
    setMode('advanced');
  } else {
    setMode('quick');
  }
  refreshKindUI();

  // Remember non-secret Quick Connect fields in this browser for next time —
  // never the password, and never sent to the server as a "please save
  // this" setting. This is purely a client-side convenience; whether the
  // connection itself persists on the server is a separate, admin-only
  // decision (see the Storage column above).
  var prefillKey = 'datadock.connections.lastQuickConnect';
  var prefillFields = ['kind', 'host', 'port', 'database', 'user', 'instance', 'authmode', 'encrypt', 'sslmode'];
  var form = document.getElementById('connectionForm');

  function loadPrefill() {
    try {
      return JSON.parse(localStorage.getItem(prefillKey) || '{}');
    } catch (e) {
      return {};
    }
  }

  // Only prefill on a fresh GET-rendered form (no error re-population
  // already happened server-side, i.e. every field is still empty).
  var alreadyPopulated = prefillFields.some(function (name) {
    var el = form.elements[name];
    return el && el.value;
  });
  if (!alreadyPopulated) {
    var saved = loadPrefill();
    prefillFields.forEach(function (name) {
      var el = form.elements[name];
      if (el && saved[name]) el.value = saved[name];
    });
    kind.dispatchEvent(new Event('change'));
  }

  form.addEventListener('submit', function () {
    var toSave = {};
    prefillFields.forEach(function (name) {
      var el = form.elements[name];
      if (el) toSave[name] = el.value;
    });
    try { localStorage.setItem(prefillKey, JSON.stringify(toSave)); } catch (e) {}
  });
}

function initLogicSearchBox() {
  var btn = document.getElementById('logicSearchBtn');
  var connSelect = document.getElementById('logicSearchConn');
  var queryInput = document.getElementById('logicSearchQuery');
  var results = document.getElementById('logicSearchResults');
  if (!btn || !connSelect || !queryInput || !results) return;

  function iconFor(kind) {
    if (kind === 'view') return 'bi-eye';
    if (kind === 'procedure' || kind === 'function') return 'bi-gear';
    return 'bi-table';
  }

  function hrefFor(hit) {
    if (hit.objectKind === 'procedure' || hit.objectKind === 'function') {
      return '/r/' + encodeURIComponent(hit.objectName) + '?kind=' + encodeURIComponent(hit.objectKind);
    }
    return '/t/' + encodeURIComponent(hit.objectName);
  }

  function runSearch() {
    var query = queryInput.value.trim();
    if (!query) return;
    results.innerHTML = '<div class="small text-muted">Searching…</div>';
    fetch('/api/logic-search', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ connection_id: connSelect.value, query: query })
    })
      .then(function (r) {
        return r.json().then(function (data) { return { ok: r.ok, data: data }; });
      })
      .then(function (res) {
        if (!res.ok) {
          results.innerHTML = '<div class="small text-danger">' + escHtml(res.data.detail || res.data.title || 'Search failed') + '</div>';
          return;
        }
        var hits = res.data.hits || [];
        if (hits.length === 0) {
          results.innerHTML = '<div class="small text-muted">No matches. Has this connection been reindexed?</div>';
          return;
        }
        var html = '<ul class="list-unstyled small mb-0">';
        hits.forEach(function (hit) {
          html += '<li class="mb-1"><a href="' + hrefFor(hit) + '"><i class="bi ' + iconFor(hit.objectKind) + ' me-1"></i>' +
            escHtml(hit.objectName) + '</a> <span class="badge bg-light text-secondary border fw-normal">' +
            escHtml(hit.objectKind) + '</span> <span class="text-muted">score ' + hit.score.toFixed(3) + '</span></li>';
        });
        html += '</ul>';
        results.innerHTML = html;
      })
      .catch(function (err) {
        results.innerHTML = '<div class="small text-danger">' + escHtml(err.message || err) + '</div>';
      });
  }

  btn.addEventListener('click', runSearch);
  queryInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') runSearch();
  });
}

// ── Routine view (/r/{name}) ─────────────────────────────────────────────
//
// ROUTINE_DEFINITION/ROUTINE_TITLE are server-rendered globals declared
// inline in routine_view.html.

function copyRoutineDefinition(btn) {
  navigator.clipboard.writeText(ROUTINE_DEFINITION).then(function () {
    var original = btn.innerHTML;
    btn.innerHTML = '<i class="bi bi-check2 me-1"></i>Copied';
    setTimeout(function () { btn.innerHTML = original; }, 1500);
  });
}

// ── Table view (/t/{name}) ───────────────────────────────────────────────

function confirmDrop(name) {
  if (confirm('Drop table "' + name + '"? This cannot be undone.')) {
    document.getElementById('dropForm').submit();
  }
}

function changeTablePageSize(size) {
  var url = new URL(window.location.href);
  url.searchParams.set('pagesize', size);
  url.searchParams.set('page', '1');
  if (!window.DataDock || typeof window.DataDock.navigateTablePage !== 'function' || !window.DataDock.navigateTablePage(url)) {
    window.location.href = url.toString();
  }
}

var tableScriptCache = null;

function loadTableScript(tableName) {
  if (tableScriptCache) return Promise.resolve(tableScriptCache);
  return fetch('/api/tables/' + encodeURIComponent(tableName) + '/script')
    .then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(function (script) {
      tableScriptCache = script;
      return script;
    });
}

function toggleTableStructure(tableName) {
  var panel = document.getElementById('structurePanel');
  if (!panel.classList.contains('d-none')) {
    panel.classList.add('d-none');
    return;
  }
  panel.classList.remove('d-none');
  var body = panel.querySelector('.card-body');
  body.innerHTML = '<div class="small text-muted">Loading structure…</div>';
  loadTableScript(tableName).then(function (script) {
    var cols = script.structure || [];
    if (cols.length === 0) {
      body.innerHTML = '<div class="small text-muted">No column details available.</div>';
      return;
    }
    var html = '<div class="table-responsive"><table class="table table-sm mb-0">' +
      '<thead><tr><th>Column</th><th>Type</th><th>Nullable</th><th>Default</th><th>Key</th></tr></thead><tbody>';
    cols.forEach(function (c) {
      var nullableLabel = c.nullable === 'yes' ? 'YES' : (c.nullable === 'no' ? 'NO' : '—');
      html += '<tr><td>' + escHtml(c.name) + '</td><td>' + escHtml(c.typeName) + '</td>' +
        '<td>' + nullableLabel + '</td>' +
        '<td>' + (c.default ? '<code>' + escHtml(c.default) + '</code>' : '<span class="text-muted">—</span>') + '</td>' +
        '<td>' + (c.primaryKey ? '<span class="badge text-bg-primary">PK</span>' : '') + '</td></tr>';
    });
    html += '</tbody></table></div>';
    body.innerHTML = html;
  }).catch(function (err) {
    body.innerHTML = '<div class="small text-danger">Could not load structure: ' + escHtml(err.message || err) + '</div>';
  });
}

function toggleViewDefinition(tableName) {
  var panel = document.getElementById('definitionPanel');
  if (!panel.classList.contains('d-none')) {
    panel.classList.add('d-none');
    return;
  }
  panel.classList.remove('d-none');
  var body = panel.querySelector('.card-body');
  body.innerHTML = '<div class="small text-muted">Loading definition…</div>';
  loadTableScript(tableName).then(function (script) {
    if (script.ddlError) {
      body.innerHTML = '<div class="small text-muted">' + escHtml(script.ddlError) + '</div>';
      return;
    }
    var html = '<div class="small text-muted mb-1">CREATE VIEW</div>' +
      '<pre class="p-2 rounded" style="background:var(--code-bg);color:var(--code-ink);white-space:pre-wrap">' + escHtml(script.createSQL) + '</pre>' +
      '<button type="button" class="btn btn-sm btn-outline-secondary mb-2" onclick="openSQLInEditor(' + JSON.stringify(script.createSQL) + ', ' + JSON.stringify('Definition · ' + tableName) + ', false)">' +
      '<i class="bi bi-box-arrow-up-right me-1"></i>Open in SQL Editor</button>';
    if (script.alterSQL) {
      html += '<div class="small text-muted mb-1 mt-2">ALTER VIEW</div>' +
        '<pre class="p-2 rounded" style="background:var(--code-bg);color:var(--code-ink);white-space:pre-wrap">' + escHtml(script.alterSQL) + '</pre>' +
        '<button type="button" class="btn btn-sm btn-outline-secondary" onclick="openSQLInEditor(' + JSON.stringify(script.alterSQL) + ', ' + JSON.stringify('Alter · ' + tableName) + ', false)">' +
        '<i class="bi bi-box-arrow-up-right me-1"></i>Open in SQL Editor</button>';
    }
    body.innerHTML = html;
  }).catch(function (err) {
    body.innerHTML = '<div class="small text-danger">Could not load definition: ' + escHtml(err.message || err) + '</div>';
  });
}

function renderDependencyList(title, items, emptyText) {
  var html = '<div class="small text-muted mb-1">' + escHtml(title) + '</div>';
  if (!items || items.length === 0) {
    return html + '<div class="small text-muted mb-2">' + escHtml(emptyText) + '</div>';
  }
  html += '<ul class="list-unstyled small mb-2">';
  items.forEach(function (dep) {
    var isRoutine = dep.kind === 'procedure' || dep.kind === 'function';
    var href = isRoutine
      ? '/r/' + encodeURIComponent(dep.name) + '?kind=' + encodeURIComponent(dep.kind)
      : '/t/' + encodeURIComponent(dep.name);
    html += '<li><a href="' + href + '"><i class="bi ' +
      (dep.kind === 'view' ? 'bi-eye' : isRoutine ? 'bi-gear' : 'bi-table') +
      ' me-1"></i>' + escHtml(dep.name) + '</a> <span class="badge bg-light text-secondary border fw-normal">' + escHtml(dep.kind) + '</span></li>';
  });
  html += '</ul>';
  return html;
}

function toggleTableDependencies(tableName) {
  var panel = document.getElementById('dependenciesPanel');
  if (!panel.classList.contains('d-none')) {
    panel.classList.add('d-none');
    return;
  }
  panel.classList.remove('d-none');
  var body = panel.querySelector('.card-body');
  body.innerHTML = '<div class="small text-muted">Loading dependencies…</div>';
  loadTableScript(tableName).then(function (script) {
    if (script.dependenciesError) {
      body.innerHTML = '<div class="small text-muted">' + escHtml(script.dependenciesError) + '</div>';
      return;
    }
    var html = renderDependencyList('Depends on', script.dependsOn, 'Nothing else on this connection.');
    html += renderDependencyList('Depended on by', script.dependents, 'Nothing else on this connection references it.');
    body.innerHTML = html;
  }).catch(function (err) {
    body.innerHTML = '<div class="small text-danger">Could not load dependencies: ' + escHtml(err.message || err) + '</div>';
  });
}

// ── Local query history (/history) ───────────────────────────────────────
//
// queryHistoryKey and loadLocalQueryHistory() are already declared above
// (the SQL editor on /query reads and writes the same localStorage-backed
// history list). renderLocalQueryHistory() is called from an inline
// DOMContentLoaded call kept in history.html itself so DataDock's AJAX
// shell navigation re-runs it every time this page loads into the shell —
// see executeScripts().

function renderLocalQueryHistory() {
  var target = document.getElementById('queryHistory');
  if (!target) return;
  var items = loadLocalQueryHistory();
  if (items.length === 0) {
    target.innerHTML = '<div class="text-muted">No local queries yet.</div>';
    return;
  }
  var html = '<div class="list-group list-group-flush">';
  items.forEach(function(item, idx) {
    var label = item.sql.replace(/\s+/g, ' ').trim();
    if (label.length > 160) label = label.slice(0, 160) + '...';
    html += '<button type="button" class="list-group-item list-group-item-action px-0 py-2" onclick="restoreLocalQueryHistory(' + idx + ')">' +
      '<span class="d-block text-truncate">' + escHtml(label) + '</span>' +
      '<span class="text-muted">' + escHtml(formatHistoryTime(item.at)) + ' · ' +
      (item.error ? '<span class="text-danger">' + escHtml(item.error) + '</span>' : escHtml(item.rowCount || 0) + ' row(s) · ' + escHtml(item.elapsedMs || 0) + ' ms') +
      '</span>' +
      '</button>';
  });
  html += '</div>';
  target.innerHTML = html;
}

function restoreLocalQueryHistory(idx) {
  var item = loadLocalQueryHistory()[idx];
  if (!item) return;
  openSQLInEditor(item.sql, '', false);
}

function clearLocalQueryHistory() {
  try { localStorage.removeItem(queryHistoryKey); } catch (e) {}
  renderLocalQueryHistory();
}

function exportLocalQueryHistory(format) {
  var items = loadLocalQueryHistory();
  if (items.length === 0) {
    window.alert('No local query history to export.');
    return;
  }
  var stamp = new Date().toISOString().replace(/[:.]/g, '-');
  if (format === 'csv') {
    var header = ['at', 'rowCount', 'elapsedMs', 'error', 'sql'];
    var lines = [header.map(csvCell).join(',')];
    items.forEach(function (item) {
      lines.push([
        item.at || '',
        item.rowCount || 0,
        item.elapsedMs || 0,
        item.error || '',
        item.sql || ''
      ].map(csvCell).join(','));
    });
    downloadTextFile('datadock-query-history-' + stamp + '.csv', 'text/csv;charset=utf-8', lines.join('\n') + '\n');
    return;
  }
  downloadTextFile('datadock-query-history-' + stamp + '.json', 'application/json;charset=utf-8', JSON.stringify(items, null, 2) + '\n');
}

function downloadTextFile(filename, type, content) {
  var blob = new Blob([content], {type: type});
  var url = URL.createObjectURL(blob);
  var a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(function () { URL.revokeObjectURL(url); }, 0);
}

function csvCell(value) {
  return '"' + String(value == null ? '' : value).replace(/"/g, '""') + '"';
}

function formatHistoryTime(value) {
  var d = new Date(value);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleString();
}

// ── Jobs (/jobs) ─────────────────────────────────────────────────────────

function setJobStatus(message, isError) {
  var el = document.getElementById('jobStatus');
  el.textContent = message;
  el.setAttribute('role', isError ? 'alert' : 'status');
  el.classList.toggle('text-danger', !!isError);
  el.classList.toggle('text-success', !isError && !!message);
}

function createJob(e) {
  e.preventDefault();
  var f = e.target;
  var btn = document.getElementById('jobSaveBtn');
  var payload = {
    name: f.elements['name'].value,
    sql: f.elements['sql'].value,
    schedule_type: f.elements['schedule_type'].value,
    interval_ms: parseInt(f.elements['interval_ms'].value || '0', 10),
    cron_expr: f.elements['cron_expr'].value,
    enabled: true,
    no_overlap: true
  };
  btn.disabled = true;
  setJobStatus('Saving…', false);
  fetch('/api/jobs', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)})
    .then(function(r){ return r.json().then(function(data){ return {ok:r.ok,data:data}; }); })
    .then(function(resp){
      if (resp.ok) {
        setJobStatus('Saved.', false);
        location.reload();
        return;
      }
      btn.disabled = false;
      setJobStatus(resp.data.error || 'Could not save the job.', true);
    })
    .catch(function(err) {
      btn.disabled = false;
      setJobStatus(err.message || String(err), true);
    });
}

function runJob(name, btn) {
  if (btn) btn.disabled = true;
  setJobStatus('Running "' + name + '"…', false);
  fetch('/api/jobs/run', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({name:name})})
    .then(function(r){ return r.json(); })
    .then(function(data){
      if (btn) btn.disabled = false;
      setJobStatus(data.error || ('Run finished: ' + data.status), !!data.error);
    })
    .catch(function(err) {
      if (btn) btn.disabled = false;
      setJobStatus(err.message || String(err), true);
    });
}

// ── Manage Tables (/create-table, /import, /export) ──────────────────────
//
// switchManageTab() is called from an inline DOMContentLoaded call kept in
// manage_table.html itself (it needs the server-rendered ActiveTab value,
// and must stay inline so DataDock's AJAX shell navigation re-runs it every
// time this page loads into the shell — see executeScripts()).

function switchManageTab(tab) {
  ['create', 'import', 'export'].forEach(function(name) {
    var panel = document.getElementById('manageTab' + name.charAt(0).toUpperCase() + name.slice(1));
    if (panel) panel.classList.toggle('d-none', name !== tab);
  });
  document.querySelectorAll('#manageTableTabs .nav-link').forEach(function(btn) {
    btn.classList.toggle('active', btn.getAttribute('data-tab') === tab);
  });
  try { localStorage.setItem('datadock.manageTable.activeTab', tab); } catch (e) {}
}

function addField() {
  var list = document.getElementById('fieldList');
  var row = document.createElement('div');
  row.className = 'field-row mb-2';
  row.innerHTML =
    '<input type="text" class="form-control form-control-sm" name="col_name" placeholder="column name" required>' +
    '<select class="form-select form-select-sm type-sel" name="col_type">' +
    '<option value="TEXT" selected>TEXT</option><option value="INT">INT</option>' +
    '<option value="FLOAT">FLOAT</option><option value="BOOL">BOOL</option>' +
    '</select>' +
    '<button type="button" class="btn btn-outline-danger btn-sm btn-remove" onclick="removeField(this)" title="Remove column">' +
    '<i class="bi bi-dash"></i></button>';
  list.appendChild(row);
  row.querySelector('input').focus();
}

function removeField(btn) {
  var rows = document.querySelectorAll('#fieldList .field-row');
  if (rows.length <= 1) return; // keep at least one
  btn.closest('.field-row').remove();
}

function exportSelectedTable(format) {
  var select = document.getElementById('exportTableSelect');
  var table = select.value;
  var status = document.getElementById('exportStatus');
  if (!table) {
    status.textContent = 'Choose a table or view first.';
    select.focus();
    return;
  }
  status.textContent = '';
  window.location.href = '/t/' + encodeURIComponent(table) + '/export?format=' + encodeURIComponent(format);
}

// ── Admin (/admin) ────────────────────────────────────────────────────────

function toggleStatusJSON() {
  var body = document.getElementById('statusJSONBody');
  var caret = document.getElementById('statusJSONCaret');
  var copyBtn = document.getElementById('copyStatusBtn');
  var hidden = !body.classList.contains('d-none');
  body.classList.toggle('d-none', hidden);
  copyBtn.classList.toggle('d-none', hidden);
  caret.className = hidden ? 'bi bi-chevron-right me-1' : 'bi bi-chevron-down me-1';
}

function copyAdminStatus(btn) {
  var text = document.getElementById('adminStatus').innerText;
  var restore = function () {
    setTimeout(function () {
      btn.innerHTML = '<i class="bi bi-clipboard me-1"></i>Copy';
    }, 1500);
  };
  navigator.clipboard.writeText(text).then(function () {
    btn.innerHTML = '<i class="bi bi-check2 me-1"></i>Copied!';
    restore();
  }, function () {
    btn.innerHTML = '<i class="bi bi-x-lg me-1"></i>Could not copy';
    restore();
  });
}

var llmDetectedServers = [];

function setLLMAutoStatus(text, kind) {
  var el = document.getElementById('llmAutoStatus');
  if (!el) return;
  el.className = 'small ' + (kind === 'error' ? 'text-danger' : kind === 'ok' ? 'text-success' : 'text-muted');
  el.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  el.textContent = text || '';
}

function refreshLLMAutoModels() {
  var serverSelect = document.getElementById('llmAutoServer');
  var modelSelect = document.getElementById('llmAutoModel');
  if (!serverSelect || !modelSelect) return;
  var server = llmDetectedServers[Number(serverSelect.value || 0)];
  modelSelect.innerHTML = '';
  if (!server || !server.models || !server.models.length) {
    modelSelect.add(new Option('No models', ''));
    return;
  }
  server.models.forEach(function(model) {
    modelSelect.add(new Option(model, model));
  });
}

function renderLLMDetection(data) {
  llmDetectedServers = data.servers || [];
  var serverSelect = document.getElementById('llmAutoServer');
  if (!serverSelect) return;
  serverSelect.innerHTML = '';
  if (!llmDetectedServers.length) {
    serverSelect.add(new Option('No server found', ''));
    refreshLLMAutoModels();
    setLLMAutoStatus('No local LLM server found', 'error');
    return;
  }
  llmDetectedServers.forEach(function(server, idx) {
    var label = server.name + ' · ' + server.base_url + ' · ' + (server.models || []).length + ' models';
    serverSelect.add(new Option(label, String(idx)));
  });
  serverSelect.onchange = refreshLLMAutoModels;
  refreshLLMAutoModels();
  setLLMAutoStatus('Detected ' + llmDetectedServers.length + ' server(s)', 'ok');
}

function detectLLMServers() {
  var btn = document.getElementById('llmDetectBtn');
  if (btn.disabled) return;
  var host = document.getElementById('llmAutoHost').value.trim();
  var port = document.getElementById('llmAutoPort').value.trim();
  var params = new URLSearchParams();
  if (host) params.set('host', host);
  if (port) params.set('port', port);
  btn.disabled = true;
  setLLMAutoStatus('Detecting...', '');
  fetch('/api/llm/discover?' + params.toString())
    .then(function(resp) { return resp.json().then(function(data) { return { ok: resp.ok, data: data }; }); })
    .then(function(resp) {
      if (!resp.ok) throw new Error(resp.data.error || 'Discovery failed');
      renderLLMDetection(resp.data);
    })
    .catch(function(err) {
      llmDetectedServers = [];
      renderLLMDetection({ servers: [] });
      setLLMAutoStatus(err.message || err.toString(), 'error');
    })
    .finally(function() { btn.disabled = false; });
}

function applyLLMAutoConfig() {
  var serverSelect = document.getElementById('llmAutoServer');
  var modelSelect = document.getElementById('llmAutoModel');
  var server = llmDetectedServers[Number((serverSelect && serverSelect.value) || 0)];
  var payload = {
    host: document.getElementById('llmAutoHost').value.trim(),
    port: document.getElementById('llmAutoPort').value.trim(),
    base_url: server ? server.base_url : '',
    model: modelSelect ? modelSelect.value : ''
  };
  setLLMAutoStatus('Applying...', '');
  fetch('/api/llm/autoconfig', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  })
    .then(function(resp) { return resp.json().then(function(data) { return { ok: resp.ok, data: data }; }); })
    .then(function(resp) {
      if (!resp.ok) throw new Error(resp.data.error || 'Auto config failed');
      document.querySelector('[name="llm_base_url"]').value = resp.data.llm_base_url || '';
      document.querySelector('[name="llm_model"]').value = resp.data.llm_model || '';
      document.querySelector('[name="llm_timeout"]').value = resp.data.llm_timeout || '45s';
      setLLMAutoStatus('LLM settings applied', 'ok');
    })
    .catch(function(err) {
      setLLMAutoStatus(err.message || err.toString(), 'error');
    });
}

// ── Matching wizard (/match) ──────────────────────────────────────────────
//
// SOURCE_COLUMNS/TARGET_COLUMNS/MATCH_METHODS/INITIAL_FIELD_ROWS are
// server-rendered globals declared inline in match.html, along with the
// DOMContentLoaded call that seeds the field-rule editor from them.

function mescape(s) {
  return String(s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}
function columnOptionsHTML(list, selected) {
  return list.map(function (name) {
    return '<option value="' + mescape(name) + '"' + (name === selected ? ' selected' : '') + '>' + mescape(name) + '</option>';
  }).join('');
}
function methodOptionsHTML(selected) {
  return MATCH_METHODS.map(function (m) {
    return '<option value="' + m.value + '" title="' + mescape(m.hint) + '"' + (m.value === selected ? ' selected' : '') + '>' + mescape(m.label) + '</option>';
  }).join('');
}
function addFieldRow(row) {
  row = row || {};
  var tbody = document.getElementById('fieldRowsBody');
  var tr = document.createElement('tr');
  tr.innerHTML =
    '<td><select class="form-select form-select-sm" name="field_source">' + columnOptionsHTML(SOURCE_COLUMNS, row.source) + '</select></td>' +
    '<td><select class="form-select form-select-sm" name="field_target">' + columnOptionsHTML(TARGET_COLUMNS, row.target) + '</select></td>' +
    '<td><select class="form-select form-select-sm" name="field_method">' + methodOptionsHTML(row.method || 'token_set') + '</select></td>' +
    '<td><input type="number" step="0.1" min="0.1" class="form-control form-control-sm" name="field_weight" value="' + (row.weight || 1) + '"></td>' +
    '<td><input type="number" step="1" min="0" max="100" class="form-control form-control-sm" name="field_tolerance" value="' + Math.round((row.tolerance || 0) * 100) + '"></td>' +
    '<td><input type="text" class="form-control form-control-sm" name="field_group" placeholder="optional" value="' + mescape(row.group || '') + '"></td>' +
    '<td><button type="button" class="btn btn-outline-danger btn-sm" onclick="removeFieldRow(this)" title="Remove field"><i class="bi bi-dash"></i></button></td>';
  tbody.appendChild(tr);
}
function removeFieldRow(btn) {
  var rows = document.querySelectorAll('#fieldRowsBody tr');
  if (rows.length <= 1) return;
  btn.closest('tr').remove();
}
function loadSavedConfig() {
  var sel = document.getElementById('savedConfigSelect');
  if (!sel || !sel.value) return;
  window.location.href = '/match?config=' + encodeURIComponent(sel.value);
}
function prepareDeleteConfig() {
  var sel = document.getElementById('savedConfigSelect');
  if (!sel || !sel.value) {
    alert('Choose a configuration to delete first.');
    return false;
  }
  document.getElementById('deleteConfigName').value = sel.value;
  return true;
}
function confirmDeleteConfig(form) {
  var sel = document.getElementById('savedConfigSelect');
  var name = sel ? sel.value : '';
  return name !== '' && window.confirm('Delete the saved configuration "' + name + '"? This cannot be undone.');
}
