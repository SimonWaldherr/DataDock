// DataDock – client-side UI helpers

(function () {
  var path = window.location.pathname;
  var root = document.documentElement;
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

  // Highlight active table link in the sidebar based on the current URL path.
  document.querySelectorAll('.sidebar .table-link').forEach(function (a) {
    if (a.getAttribute('href') === path) {
      a.classList.add('active');
    }
  });

  // Highlight active top-level navigation without needing per-template state.
  document.querySelectorAll('.app-nav .btn-nav').forEach(function (a) {
    var href = a.getAttribute('href');
    if (href && href !== '/' && (path === href || path.indexOf(href + '/') === 0)) {
      a.classList.add('active');
    }
  });
  document.querySelectorAll('.app-nav .dropdown-item').forEach(function (a) {
    var href = a.getAttribute('href');
    if (href && href !== '/' && (path === href || path.indexOf(href + '/') === 0)) {
      a.classList.add('active');
      var menu = a.closest('.dropdown');
      if (menu) {
        var button = menu.querySelector('.btn-nav');
        if (button) button.classList.add('active');
      }
    }
  });

  var filter = document.getElementById('sidebarFilter');
  if (filter) {
    filter.addEventListener('input', function () {
      var term = filter.value.trim().toLowerCase();
      document.querySelectorAll('.sidebar .table-link').forEach(function (a) {
        a.hidden = term && a.textContent.toLowerCase().indexOf(term) === -1;
      });
    });
  }

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
})();
