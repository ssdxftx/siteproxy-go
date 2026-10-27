/*!
 * siteproxy-response-injected.js — client runtime for siteproxy-go.
 *
 * Clean-room re-implementation of the client script the original
 * netptop/siteproxy (v2.7.x) injects into every proxied HTML page. It:
 *   1. defines proxyRewrite()/unRewriteURL() with the same URL rules as the
 *      upstream urlMeta runtime (relative URLs resolve against the current
 *      REAL page URL, default ports are normalised away);
 *   2. derives the current site context (protocol/host) from the proxied
 *      path, so no per-page state is required;
 *   3. exposes a full window.___location Location wrapper (href, origin,
 *      protocol, pathname, host, hostname, port, search, hash — all mapped
 *      onto the REAL site space) — page JS reaches it because the server
 *      rewrites every `location` identifier to `___location`;
 *   4. overrides history.pushState/replaceState / window.open / fetch /
 *      XMLHttpRequest / WebSocket / EventSource / sendBeacon;
 *   5. traps document.cookie (per-site path isolation, 30min cap);
 *   6. blocks navigator.serviceWorker / requestStorageAccessFor for page
 *      code (server rewrites their call sites to the ___-prefixed names)
 *      while registering our own service worker;
 *   7. neutralises Cloudflare Insights beacons and rewrites DOM nodes
 *      inserted at runtime (MutationObserver);
 *   8. shows the "accessed via siteproxy" top bar every 24 hours.
 */
(function () {
  'use strict';
  if (window.siteproxy_injected_flag) return; // idempotent
  window.siteproxy_injected_flag = true;

  var CONFIG = __SITEPROXY_CONFIG_JSON__;

  var token_prefix = CONFIG.token_prefix;
  var config_proxy_url = CONFIG.proxy_url;
  var proxy_url_prefix = location.origin + token_prefix;

  // Proxy host without scheme/port, used as the Domain for JS-set cookies.
  var proxyHostNoScheme = config_proxy_url.substring(config_proxy_url.indexOf('//') + 2);
  if (proxyHostNoScheme.indexOf('/') !== -1) {
    proxyHostNoScheme = proxyHostNoScheme.substring(0, proxyHostNoScheme.indexOf('/'));
  }

  // ------------------------------------------------------------------
  // 1. Current site context — derived from the proxied path.
  // ------------------------------------------------------------------
  function parseCurrentLocation() {
    var path = location.pathname;
    if (token_prefix !== '/' && path.indexOf(token_prefix) === 0) {
      path = path.substring(token_prefix.length);
    }
    var m = path.match(/^(https?)\/([^/]+)(?:\/|$)/);
    if (m) return { protocol: m[1], host: m[2] };
    return { protocol: CONFIG.proxy_real_protocol || 'https',
             host: CONFIG.proxy_real_host || '' };
  }
  var curLoc = parseCurrentLocation();
  var proxy_real_protocol = curLoc.protocol;
  var proxy_real_host = curLoc.host;

  function refreshCurrentLocation() {
    var c = parseCurrentLocation();
    proxy_real_protocol = c.protocol;
    proxy_real_host = c.host;
    notifyServiceWorker();
  }

  // ------------------------------------------------------------------
  // 2. URL rewrite / unrewrite (upstream urlMeta semantics).
  // ------------------------------------------------------------------
  var realBaseCache = null;
  function realBaseURL() {
    if (realBaseCache === null) {
      try { realBaseCache = unRewriteURL(location.href); } catch (e) { realBaseCache = ''; }
    }
    return realBaseCache;
  }

  function isSpecialScheme(url) {
    return /^(javascript|data|blob|mailto|tel|about|ws|wss|ftp|file|sms|view-source|webcal|content|ssh|vbscript|chrome):/i.test(url);
  }

  function proxyRewrite(url) {
    if (typeof url !== 'string') return url;
    if (url.startsWith(proxy_url_prefix)) return url;
    if (token_prefix !== '/' && url.startsWith(token_prefix)) return url;

    // Generic scheme check: only http(s) URLs are proxied.
    var sm = url.match(/^([a-z][a-z0-9+.-]*):/i);
    if (sm && !/^https?$/i.test(sm[1])) return url;

    if (url.startsWith('http://') || url.startsWith('https://')) {
      if (config_proxy_url && url.startsWith(config_proxy_url)) return url; // no self-wrap
      var protocol = url.split('://')[0];
      var rest = url.substring(protocol.length + 3);
      return proxy_url_prefix + protocol + '/' + rest;
    }
    if (url.startsWith('//')) {
      if (url.toLowerCase().startsWith('//' + proxyHostNoScheme.toLowerCase())) return url;
      return proxy_url_prefix + 'https/' + url.substring(2);
    }
    if (url.startsWith('/')) {
      if (!proxy_real_host) return url;
      return proxy_url_prefix + proxy_real_protocol + '/' + proxy_real_host + url;
    }

    // Bare relative path: resolve against the current REAL page URL so that
    // "img/logo.png" on /dir/page lands in /dir/ (upstream uses new URL()).
    if (!proxy_real_host) return url;
    if (url.indexOf(' ') >= 0) {
      // Contains spaces — not a single URL; fall back to root anchoring.
      return proxy_url_prefix + proxy_real_protocol + '/' + proxy_real_host + '/' + url;
    }
    try {
      var base = realBaseURL() || (proxy_real_protocol + '://' + proxy_real_host + '/');
      var resolved = new URL(url, base);
      if (resolved.origin === location.origin ||
          resolved.host === proxyHostNoScheme) {
        // Resolved onto the proxy origin: re-anchor into the site scope.
        if (token_prefix !== '/' && resolved.pathname.startsWith(token_prefix)) return url;
        return proxy_url_prefix + proxy_real_protocol + '/' + proxy_real_host +
               resolved.pathname + resolved.search + resolved.hash;
      }
      var scheme = resolved.protocol.replace(':', '');
      var hostport = resolved.hostname;
      var defaultPort = scheme === 'http' ? '80' : '443';
      if (resolved.port && resolved.port !== defaultPort) hostport += ':' + resolved.port;
      return proxy_url_prefix + scheme + '/' + hostport +
             resolved.pathname + resolved.search + resolved.hash;
    } catch (e) {
      return proxy_url_prefix + proxy_real_protocol + '/' + proxy_real_host + '/' + url;
    }
  }

  // Inverse: proxied URL -> real URL (document.URL, referrers, ___location).
  function unRewriteURL(url) {
    if (typeof url !== 'string') return url;
    var path = null;
    if (url.startsWith(proxy_url_prefix)) {
      path = url.substring(proxy_url_prefix.length);
    } else if (url.startsWith('/') && token_prefix !== '/' &&
               url.startsWith(token_prefix)) {
      path = url.substring(token_prefix.length);
    } else {
      // Same-origin (proxy) URL without the token scope: map through the
      // current site context (upstream unrewrite fallback).
      try {
        var u = new URL(url);
        if (u.origin === location.origin) {
          return proxy_real_protocol + '://' + proxy_real_host +
                 u.pathname + u.search + u.hash;
        }
      } catch (e) { /* not a URL */ }
      return url;
    }
    var m = path.match(/^(https?)\/([^/]+)(?:\/(.*))?$/);
    if (!m) return url;
    var real = m[1] + '://' + m[2];
    if (m[3]) real += '/' + m[3];
    return real;
  }

  // ------------------------------------------------------------------
  // 3. window.___location — full Location wrapper on the REAL site space.
  //    (The server rewrites `location` -> `___location` in page JS, so this
  //    object receives every read AND write of page location objects.)
  // ------------------------------------------------------------------
  (function () {
    var realLocation = window.location;

    // Upstream guard: reject non-URL values (null, plain objects, ...)
    function isNotURL(v) {
      if (v == null) return true;
      if (typeof v === 'object' && !(v instanceof URL)) {
        try { return String(v) === '[object Object]'; } catch (e) { return true; }
      }
      return false;
    }

    // Parse the proxied URL into its real parts.
    function siteParts() {
      var path = realLocation.pathname;
      if (token_prefix !== '/' && path.startsWith(token_prefix)) {
        path = path.substring(token_prefix.length);
      }
      var m = path.match(/^(https?)\/([^/]+)(\/.*)?$/);
      if (m) return { protocol: m[1], host: m[2], pathname: m[3] || '/' };
      return { protocol: proxy_real_protocol, host: proxy_real_host, pathname: path };
    }

    function Wrapper(target) {
      this.originalLocation = target;
    }
    Wrapper.prototype = {
      toString: function () { return unRewriteURL(this.originalLocation.href); },
      assign: function (v) {
        if (isNotURL(v)) { console.warn('[LocationWrapper.assign] rejecting non-URL value:', v); return; }
        var rw = proxyRewrite(String(v));
        if (rw === this.originalLocation.href) return;
        this.originalLocation.assign(rw);
      },
      replace: function (v) {
        if (isNotURL(v)) { console.warn('[LocationWrapper.replace] rejecting non-URL value:', v); return; }
        var rw = proxyRewrite(String(v));
        if (rw === this.originalLocation.href) return;
        this.originalLocation.replace(rw);
      },
      reload: function (force) { this.originalLocation.reload(force); }
    };
    Object.defineProperties(Wrapper.prototype, {
      href: {
        get: function () { return unRewriteURL(this.originalLocation.href); },
        set: function (v) {
          if (isNotURL(v)) { console.warn('[LocationWrapper.set href] rejecting non-URL value:', v); return; }
          var rw = proxyRewrite(String(v));
          if (rw === this.originalLocation.href) return;
          this.originalLocation.href = rw;
        }
      },
      origin: {
        get: function () { return proxy_real_protocol + '://' + proxy_real_host; }
      },
      protocol: {
        get: function () { return siteParts().protocol + ':'; },
        set: function (v) {
          try {
            var p = String(v).replace(/:$/, '');
            if (p !== 'http' && p !== 'https') return;
            var parts = siteParts();
            if (p === parts.protocol) return;
            this.originalLocation.href =
              proxy_url_prefix + p + '/' + parts.host + parts.pathname +
              this.originalLocation.search + this.originalLocation.hash;
          } catch (e) { console.warn('setProtocol error:', e); }
        }
      },
      pathname: {
        get: function () { return siteParts().pathname; },
        set: function () { /* upstream: no-op */ }
      },
      host: {
        get: function () { return siteParts().host; },
        set: function () { /* upstream: no-op */ }
      },
      hostname: {
        get: function () {
          var h = siteParts().host;
          var i = h.indexOf(':');
          return i >= 0 ? h.substring(0, i) : h;
        },
        set: function () { /* upstream: no-op */ }
      },
      port: {
        get: function () {
          var h = siteParts().host;
          var i = h.indexOf(':');
          return i >= 0 ? h.substring(i + 1) : '';
        },
        set: function () { /* upstream: no-op */ }
      },
      search: {
        get: function () { return this.originalLocation.search; },
        set: function () { /* upstream: no-op */ }
      },
      hash: {
        get: function () { return this.originalLocation.hash; },
        set: function (v) { this.originalLocation.hash = v; }
      }
    });

    var wrapper = new Wrapper(realLocation);
    try { delete window.___location; } catch (e) { /* ignore */ }
    window.___location = wrapper;
    try { delete document.___location; } catch (e) { /* ignore */ }
    document.___location = wrapper;
    // window.___location = "<url>" navigates (mirrors upstream setter).
    try {
      Object.defineProperty(window, '___location', {
        get: function () { return wrapper; },
        set: function (v) {
          if (isNotURL(v)) { console.warn('[window.___location setter] rejecting non-URL value:', v); return; }
          var rw = proxyRewrite(String(v));
          if (rw === realLocation.href) return;
          realLocation.href = rw;
        },
        configurable: true
      });
      Object.defineProperty(document, '___location', {
        get: function () { return wrapper; },
        set: function (v) {
          if (isNotURL(v)) return;
          var rw = proxyRewrite(String(v));
          if (rw === realLocation.href) return;
          realLocation.href = rw;
        },
        configurable: true
      });
    } catch (e) { /* keep plain wrapper */ }

    // window.___URL / document.___domain receive rewritten page references.
    try { window.___URL = window.URL; } catch (e) { /* ignore */ }
    try {
      Object.defineProperty(document, '___domain', {
        get: function () {
          var h = unRewriteURL(document.URL);
          var m = String(h).match(/^https?:\/\/([^/?#]+)/i);
          return m ? m[1] : '';
        },
        set: function () { /* upstream: no-op */ },
        configurable: true
      });
    } catch (e) { /* ignore */ }
  })();

  // ------------------------------------------------------------------
  // 4. history / window.open / fetch / XHR / WebSocket / EventSource /
  //    sendBeacon overrides.
  // ------------------------------------------------------------------
  var originalPushState = history.pushState.bind(history);
  var originalReplaceState = history.replaceState.bind(history);
  // Upstream only patches the top frame (iframes keep native history).
  if (window.self === window.top) {
    history.pushState = function (state, title, url) {
      var rewritten = (url !== undefined && url !== null) ? proxyRewrite(String(url)) : url;
      var out = originalPushState(state, title, rewritten);
      refreshCurrentLocation();
      return out;
    };
    history.replaceState = function (state, title, url) {
      var rewritten = (url !== undefined && url !== null) ? proxyRewrite(String(url)) : url;
      var out = originalReplaceState(state, title, rewritten);
      refreshCurrentLocation();
      return out;
    };
  }
  // The server rewrites history.pushState/replaceState call sites to these
  // ___-prefixed aliases; keep them rewriting like the upstream node script.
  history.___pushState = function (state, title, url) {
    var rewritten = (url !== undefined && url !== null) ? proxyRewrite(String(url)) : url;
    return originalPushState.call(this, state, title, rewritten);
  };
  history.___replaceState = function (state, title, url) {
    var rewritten = (url !== undefined && url !== null) ? proxyRewrite(String(url)) : url;
    return originalReplaceState.call(this, state, title, rewritten);
  };

  var originalOpen = window.open ? window.open.bind(window) : null;
  if (originalOpen) {
    window.open = function (url, name, specs) {
      if (typeof url === 'string') url = proxyRewrite(url);
      return originalOpen(url, name, specs);
    };
  }

  // --- fetch ---
  var originalFetch = window.fetch ? window.fetch.bind(window) : null;
  if (originalFetch) {
    window.fetch = function (input, init) {
      try {
        if (typeof input === 'string' || input instanceof URL) {
          input = proxyRewrite(String(input));
        } else if (input && typeof input.url === 'string') {
          var newUrl = proxyRewrite(input.url);
          if (newUrl !== input.url) {
            var cloned = new Request(newUrl, { method: input.method, headers: input.headers,
                                               body: input.body, mode: 'same-origin',
                                               credentials: input.credentials,
                                               cache: input.cache, redirect: input.redirect,
                                               referrer: input.referrer, integrity: '' });
            input = cloned;
          }
        }
        init = init || {};
        init.headers = new Headers(init.headers || (input && input.headers) || undefined);
        // Pass the *real* referer to the server via a side header.
        if (!init.headers.has('siteproxy-newreferer')) {
          var real = unRewriteURL(location.href);
          if (real !== location.href) init.headers.set('siteproxy-newreferer', real);
        }
      } catch (e) { /* fallthrough: fetch directly */ }
      return originalFetch(input, init);
    };
  }

  // --- XMLHttpRequest ---
  (function () {
    if (!window.XMLHttpRequest) return;
    var OriginalOpen = XMLHttpRequest.prototype.open;
    var OriginalSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (method, url) {
      this.__siteproxy_real_url = url;
      var args = Array.prototype.slice.call(arguments);
      if (typeof url === 'string') args[1] = proxyRewrite(url);
      return OriginalOpen.apply(this, args);
    };
    XMLHttpRequest.prototype.send = function (body) {
      try {
        var real = unRewriteURL(location.href);
        if (real !== location.href) {
          this.setRequestHeader('siteproxy-newreferer', real);
        }
      } catch (e) { /* headers may be locked */ }
      return OriginalSend.call(this, body);
    };
  })();

  // --- WebSocket ---
  (function () {
    if (!window.WebSocket) return;
    var OriginalWebSocket = window.WebSocket;
    function SiteProxyWebSocket(url, protocols) {
      if (typeof url === 'string') {
        var httpUrl = url.replace(/^wss:/i, 'https:').replace(/^ws:/i, 'http:');
        url = proxyRewrite(httpUrl);
      } else if (url instanceof OriginalWebSocket) {
        url = url.url; // best effort
      }
      if (protocols === undefined) return new OriginalWebSocket(url);
      return new OriginalWebSocket(url, protocols);
    }
    SiteProxyWebSocket.prototype = OriginalWebSocket.prototype;
    SiteProxyWebSocket.CONNECTING = OriginalWebSocket.CONNECTING;
    SiteProxyWebSocket.OPEN = OriginalWebSocket.OPEN;
    SiteProxyWebSocket.CLOSING = OriginalWebSocket.CLOSING;
    SiteProxyWebSocket.CLOSED = OriginalWebSocket.CLOSED;
    try {
      Object.defineProperty(SiteProxyWebSocket, 'name', { value: 'WebSocket' });
    } catch (e) { /* ignore */ }
    window.WebSocket = SiteProxyWebSocket;
  })();

  // --- EventSource (SSE) ---
  (function () {
    if (!window.EventSource) return;
    var OriginalEventSource = window.EventSource;
    function SiteProxyEventSource(url, cfg) {
      if (typeof url === 'string') url = proxyRewrite(url);
      if (cfg === undefined) return new OriginalEventSource(url);
      return new OriginalEventSource(url, cfg);
    }
    SiteProxyEventSource.prototype = OriginalEventSource.prototype;
    try {
      Object.defineProperty(SiteProxyEventSource, 'name', { value: 'EventSource' });
    } catch (e) { /* ignore */ }
    window.EventSource = SiteProxyEventSource;
  })();

  // --- navigator.sendBeacon (v2.7.6) ---
  (function () {
    if (!navigator.sendBeacon) return;
    var originalSendBeacon = navigator.sendBeacon.bind(navigator);
    navigator.sendBeacon = function (url, data) {
      if (typeof url === 'string') url = proxyRewrite(url);
      return originalSendBeacon(url, data);
    };
  })();

  // --- document.URL / documentURI / document.domain overrides (v2.7.0) ---
  (function () {
    try {
      var realURL = unRewriteURL(location.href);
      if (realURL !== location.href) {
        Object.defineProperty(document, 'URL', { get: function () { return realURL; }, configurable: true });
        Object.defineProperty(document, 'documentURI', { get: function () { return realURL; }, configurable: true });
        var realHost = realURL.split('://')[1] || '';
        if (realHost.indexOf('/') >= 0) realHost = realHost.split('/')[0];
        Object.defineProperty(document, 'domain', { get: function () { return realHost; }, configurable: true });
      }
    } catch (e) { /* ignore */ }
  })();

  // ------------------------------------------------------------------
  // 5. Blocked platform APIs for page code (the server rewrites the call
  //    sites to the ___-prefixed names; both spellings are blocked).
  // ------------------------------------------------------------------
  (function () {
    function blockedStorageAccess() {
      return Promise.reject(new DOMException('requestStorageAccessFor blocked by proxy', 'SecurityError'));
    }
    try {
      if (typeof document.requestStorageAccessFor === 'function') {
        document.requestStorageAccessFor = blockedStorageAccess;
      }
    } catch (e) { /* ignore */ }
    try {
      delete document.___requestStorageAccessFor;
      Object.defineProperty(document, '___requestStorageAccessFor', {
        value: blockedStorageAccess, writable: false, configurable: true
      });
    } catch (e) { /* ignore */ }
  })();

  // ------------------------------------------------------------------
  // 6. document.cookie trap (clientCookieModify, ported from upstream).
  // ------------------------------------------------------------------
  (function () {
    var cookiePath = token_prefix;
    if (cookiePath.charAt(cookiePath.length - 1) !== '/') cookiePath += '/';
    cookiePath += proxy_real_protocol + '/' + proxy_real_host + '/';
    if (cookiePath.charAt(0) !== '/') cookiePath = '/' + cookiePath;

    var cookieDomain = proxyHostNoScheme;

    var seenNames = {};
    function isPast(dateStr) {
      try { return new Date(dateStr) < new Date(); } catch (e) { return false; }
    }

    function modifyCookie(value) {
      var m = value.match(/^([^=]+)=/);
      var name = m ? m[1] : '';
      var isControl = name === 'proxy_real_host' || name === 'proxy_real_protocol';
      if (!isControl) seenNames[name] = true;

      var hadExpires = /Expires\s*=/i.test(value);
      var hadMaxAge = /Max-Age\s*=/i.test(value);

      // Domain -> proxy host (added when missing), like the original.
      var out = value.replace(/Domain\s*=\s*[^;]*?(;|$)/ig, 'Domain=' + cookieDomain + ';');

      // Path -> isolated per-site path.
      var pm = value.match(/Path\s*=\s*([^;]*)(;|$)/i);
      var origPath = pm ? pm[1].trim() : '/';
      if (origPath.charAt(0) !== '/') origPath = '/' + origPath;
      var newPath;
      if (isControl) {
        newPath = '/';
      } else if (origPath === '/') {
        newPath = cookiePath;
      } else {
        var base = cookiePath.slice(0, -1); // without trailing slash
        if (origPath.indexOf(base) === 0 &&
            (origPath.length === base.length || origPath.charAt(base.length) === '/')) {
          newPath = origPath; // already isolated
        } else {
          newPath = base + origPath;
        }
      }
      out = /Path\s*=/i.test(out)
        ? out.replace(/Path\s*=\s*([^;]*)(;|$)/ig, 'Path=' + newPath + ';')
        : out + '; Path=' + newPath + ';';
      if (!/Domain\s*=/i.test(out)) out += '; Domain=' + cookieDomain + ';';

      // Lifetime cap: non-control cookies live at most 30 minutes. A deletion
      // (Expires in the past) is preserved so removals still work.
      if (!isControl) {
        out = out.replace(/Max-Age\s*=\s*[^;]*?(;|$)/ig, '');
        var em = out.match(/Expires\s*=\s*([^;]*?)(;|$)/i);
        if (em) {
          if (!isPast(em[1])) {
            out = out.replace(/Expires\s*=\s*([^;]*?)(;|$)/ig, '') + '; Max-Age=1800';
          }
        } else if (!hadExpires && !hadMaxAge) {
          out += '; Max-Age=1800';
        }
      }
      return out.replace(/; ;|;;/g, ';');
    }

    var desc = Object.getOwnPropertyDescriptor(Document.prototype, 'cookie');
    if (!desc || !desc.set) return;
    var nativeGet = desc.get, nativeSet = desc.set;

    // Expire stray root-path copies of cookies that were not isolated (they
    // would otherwise be sent to every proxied site).
    function removeRootPathCookies() {
      try {
        var all = nativeGet.call(document);
        if (!all) return;
        var pairs = all.split('; ');
        for (var i = 0; i < pairs.length; i++) {
          var idx = pairs[i].indexOf('=');
          if (idx === -1) continue;
          var name = pairs[i].substring(0, idx);
          if (name === 'proxy_real_host' || name === 'proxy_real_protocol') continue;
          if (seenNames[name]) continue;
          nativeSet.call(document,
            name + '=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/; domain=' + cookieDomain);
        }
      } catch (e) { /* ignore */ }
    }

    try {
      Object.defineProperty(document, 'cookie', {
        get: function () { return nativeGet.call(document); },
        set: function (value) {
          var modified = modifyCookie(String(value));
          try {
            var r = nativeSet.call(document, modified);
            removeRootPathCookies();
            return r;
          } catch (e) {
            console.error('[siteproxy] cookie setter error:', e);
            throw e;
          }
        },
        configurable: true,
        enumerable: desc.enumerable
      });
    } catch (e) {
      console.warn('[siteproxy] failed to trap document.cookie:', e);
      return;
    }

    // Control cookies (non-HttpOnly copies so page scripts / diagnostics can
    // read the current site context; the server filters them before
    // forwarding upstream).
    document.cookie = 'proxy_real_host=' + proxy_real_host + '; path=/; max-age=3600';
    document.cookie = 'proxy_real_protocol=' + proxy_real_protocol + '; path=/; max-age=3600';
  })();

  // ------------------------------------------------------------------
  // 7. Cloudflare Insights beacon neutralisation (v2.7.6).
  // ------------------------------------------------------------------
  (function () {
    function isBeacon(el) {
      return el && el.nodeType === 1 && el.tagName === 'SCRIPT' &&
        (el.hasAttribute('data-cf-beacon') ||
         (el.getAttribute('src') || '').indexOf('static.cloudflareinsights.com') !== -1);
    }
    function drop(el) {
      try { el.remove(); } catch (e) { /* ignore */ }
    }
    try {
      document.querySelectorAll('script[data-cf-beacon], script[src*="cloudflareinsights"]')
        .forEach(drop);
      new MutationObserver(function (muts) {
        for (var i = 0; i < muts.length; i++) {
          var added = muts[i].addedNodes;
          for (var j = 0; j < added.length; j++) {
            if (isBeacon(added[j])) drop(added[j]);
          }
        }
      }).observe(document.documentElement || document, { childList: true, subtree: true });
    } catch (e) { /* ignore */ }
  })();

  // ------------------------------------------------------------------
  // 8. Service worker: hide ours from page scripts (and from the ___ alias
  //    the server rewrites call sites to), register our own via the real
  //    handle saved before the overrides.
  // ------------------------------------------------------------------
  (function () {
    if (!('serviceWorker' in navigator)) return;
    var realSW = navigator.serviceWorker;
    try {
      // Private handle kept for our own postMessage channel (not enumerable
      // via the page-visible aliases).
      Object.defineProperty(window, '___siteproxyRealSW', {
        value: realSW, writable: false, configurable: true
      });
      // navigator.serviceWorker -> undefined for page code.
      Object.defineProperty(navigator, 'serviceWorker', {
        get: function () { return undefined; }, configurable: true
      });
      // navigator.___serviceWorker (rewritten call sites) -> blocking stub,
      // exactly like the upstream fake registration object.
      var never = new Promise(function () {}); never.catch(function () {});
      var stub = {
        controller: null,
        ready: never,
        onmessage: null,
        oncontrollerchange: null,
        register: function () {
          return Promise.reject(new DOMException('serviceWorker blocked by proxy', 'SecurityError'));
        },
        getRegistrations: function () { return Promise.resolve([]); },
        addEventListener: function () {},
        removeEventListener: function () {},
        dispatchEvent: function () { return true; }
      };
      Object.defineProperty(navigator, '___serviceWorker', {
        value: stub, writable: false, configurable: true
      });
    } catch (e) {
      console.warn('[siteproxy] Failed to define navigator service worker shims:', e);
      return;
    }
    realSW.register('/siteproxy_service_worker.js', { scope: '/' }).catch(function (err) {
      console.warn('[siteproxy] service worker registration failed:', err);
    });
  })();

  function notifyServiceWorker() {
    try {
      var sw = window.___siteproxyRealSW;
      if (sw && sw.controller) {
        sw.controller.postMessage({
          type: 'PROXY_CUR_LOCATION',
          data: { protocol: proxy_real_protocol, host: proxy_real_host }
        });
      }
    } catch (e) { /* ignore */ }
  }
  notifyServiceWorker();
  window.addEventListener('popstate', refreshCurrentLocation);

  // ------------------------------------------------------------------
  // 9. Runtime DOM rewriting (belt & braces with the server rewriter).
  // ------------------------------------------------------------------
  var URL_ATTRS = { href: 1, src: 1, action: 1, poster: 1, formaction: 1, cite: 1, manifest: 1, 'data-url': 1, 'data-src': 1 };
  var SRCSET_ATTRS = { srcset: 1, imagesrcset: 1 };

  function rewriteElement(el) {
    if (!el || el.__siteproxy_traversed) return;
    el.__siteproxy_traversed = true;
    try {
      if (el.tagName === 'IFRAME' || el.tagName === 'SCRIPT' || el.tagName === 'LINK' ||
          el.tagName === 'IMG' || el.tagName === 'A' || el.tagName === 'FORM' ||
          el.tagName === 'SOURCE' || el.tagName === 'VIDEO' || el.tagName === 'AUDIO' ||
          el.tagName === 'EMBED' || el.tagName === 'BLOCKQUOTE') {
        for (var attr in URL_ATTRS) {
          if (el.hasAttribute(attr)) {
            var v = el.getAttribute(attr);
            var nv = proxyRewrite(v);
            if (nv !== v) el.setAttribute(attr, nv);
          }
        }
        for (var sattr in SRCSET_ATTRS) {
          if (el.hasAttribute(sattr)) {
            var sv = el.getAttribute(sattr);
            var parts = sv.split(',').map(function (p) {
              var f = p.trim().split(/\s+/);
              if (f.length && f[0]) f[0] = proxyRewrite(f[0]);
              return f.join(' ');
            });
            el.setAttribute(sattr, parts.join(', '));
          }
        }
        if (el.hasAttribute('integrity')) el.removeAttribute('integrity');
        if (el.hasAttribute('crossorigin')) el.removeAttribute('crossorigin');
      }
      if (el.tagName === 'STYLE' && el.textContent && el.textContent.indexOf('url(') >= 0) {
        el.textContent = rewriteCSSUrls(el.textContent);
      }
      if (el.hasAttribute && el.hasAttribute('style') &&
          el.getAttribute('style').indexOf('url(') >= 0) {
        el.setAttribute('style', rewriteCSSUrls(el.getAttribute('style')));
      }
    } catch (e) { /* ignore single-node failures */ }
  }

  function rewriteCSSUrls(css) {
    return css.replace(/url\(\s*(['"]?)([^)'"]+)(['"]?)\s*\)/gi, function (m, q, u, q2) {
      if (/^(data:|blob:|#)/i.test(u)) return m;
      return 'url(' + q + proxyRewrite(u) + q2 + ')';
    });
  }

  function traverse(root) {
    try {
      var all = (root || document).querySelectorAll(
        'a[href],script[src],link[href],img,iframe,form,source,video,audio,embed,style,[style*="url("]');
      for (var i = 0; i < all.length; i++) rewriteElement(all[i]);
    } catch (e) { /* ignore */ }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { traverse(document); });
  } else {
    traverse(document);
  }

  var mo = new MutationObserver(function (mutations) {
    for (var i = 0; i < mutations.length; i++) {
      var added = mutations[i].addedNodes;
      for (var j = 0; j < added.length; j++) {
        var node = added[j];
        if (node.nodeType !== 1) continue;
        rewriteElement(node);
        if (node.querySelectorAll) traverse(node);
      }
      if (mutations[i].type === 'attributes') {
        rewriteElement(mutations[i].target);
      }
    }
  });
  mo.observe(document.documentElement || document, {
    childList: true, subtree: true, attributes: true,
    attributeFilter: ['href', 'src', 'action', 'srcset', 'imagesrcset', 'poster', 'style']
  });

  // Form submit hook: rewrite action at submit time.
  document.addEventListener('submit', function (ev) {
    try {
      var form = ev.target;
      if (form && form.tagName === 'FORM') {
        if (form.hasAttribute('action')) {
          form.setAttribute('action', proxyRewrite(form.getAttribute('action')));
        } else {
          form.setAttribute('action', proxyRewrite(location.href));
        }
      }
    } catch (e) { /* ignore */ }
  }, true);

  // Element.setAttribute trap (upstream behaviour): URL attributes and srcset
  // candidates are rewritten SYNCHRONOUSLY at set time, so an <img>.src or
  // lazy-loaded data-src assignment never escapes the proxy even before the
  // MutationObserver microtask runs. Subresource integrity is removed from
  // script/link/iframe elements (rewritten bodies never match SRI hashes).
  (function () {
    try {
      var originalSetAttribute = Element.prototype.setAttribute;
      Element.prototype.setAttribute = function (name, value) {
        try {
          var ln = String(name).toLowerCase();
          if (typeof value === 'string' && (URL_ATTRS[ln] || SRCSET_ATTRS[ln])) {
            var tag = this.tagName;
            if (tag === 'SCRIPT' || tag === 'LINK' || tag === 'IFRAME') {
              try { this.removeAttribute('integrity'); } catch (e) { /* ignore */ }
            }
          }
          if (typeof value === 'string' && URL_ATTRS[ln]) {
            var nv = proxyRewrite(value);
            if (nv !== value) value = nv;
          } else if (typeof value === 'string' && SRCSET_ATTRS[ln]) {
            var parts = value.split(',').map(function (p) {
              var f = p.trim().split(/\s+/);
              if (f.length && f[0]) f[0] = proxyRewrite(f[0]);
              return f.join(' ');
            });
            value = parts.join(', ');
          }
          if (ln === 'integrity' &&
              (this.tagName === 'SCRIPT' || this.tagName === 'LINK' || this.tagName === 'IFRAME')) {
            return undefined; // never install SRI on proxied (rewritten) bodies
          }
        } catch (e) { /* fallthrough to native */ }
        return originalSetAttribute.call(this, name, value);
      };
    } catch (e) { /* prototype not writable */ }
  })();

  // ------------------------------------------------------------------
  // 10. Warning top bar (24h interval), ported from upstream v2.3.0 with the
  //     original warning text and styling.
  // ------------------------------------------------------------------
  (function () {
    var KEY = 'siteproxy_topbar_last_shown';
    var INTERVAL = 24 * 60 * 60 * 1000;
    function show() {
      try {
        var last = parseInt(localStorage.getItem(KEY) || '0', 10);
        if (Date.now() - last < INTERVAL) return;
      } catch (e) { /* private mode */ }
      var bar = document.createElement('div');
      bar.setAttribute('data-siteproxy-topbar', '1');
      bar.style.cssText = 'position:fixed;top:0;left:0;width:100%;z-index:10000;' +
        'background:#ff0000;color:#ffffff;text-align:center;font:bold 13px sans-serif;' +
        'padding:10px 40px 10px 12px;box-shadow:0 1px 4px rgba(0,0,0,.4)';
      bar.innerHTML = '!!! This is a SiteProxy proxied website, do not enter your personal information. ' +
        'Refer to: <a style="color:#ffffff" href="https://github.com/netptop/siteproxy">' +
        'https://github.com/netptop/siteproxy</a>' +
        '<span style="position:absolute;right:12px;top:50%;transform:translateY(-50%);' +
        'cursor:pointer;font-size:16px;padding:0 6px;">&times;</span>';
      var closer = bar.querySelector('span');
      closer.addEventListener('click', function () {
        bar.remove();
        try { localStorage.setItem(KEY, String(Date.now())); } catch (e) { /* ignore */ }
      });
      (document.body || document.documentElement).appendChild(bar);
      try { localStorage.setItem(KEY, String(Date.now())); } catch (e) { /* ignore */ }
    }
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', show);
    } else {
      show();
    }
  })();

  // Expose minimal internals for debugging.
  window.___siteproxy = {
    proxyRewrite: proxyRewrite,
    unRewriteURL: unRewriteURL,
    get realLocation() { return { protocol: proxy_real_protocol, host: proxy_real_host }; }
  };
})();
