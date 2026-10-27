/*!
 * siteproxy_service_worker.js — service worker for siteproxy-go.
 *
 * Clean re-implementation of the upstream v2.7.11 service worker. It:
 *   1. intercepts every request from controlled pages (all methods):
 *        - static.cloudflareinsights.com is answered with an empty 200
 *          (beacon neutralisation, mirrors upstream);
 *        - in-scope requests are fetched through with the real-site origin
 *          attached as siteproxy-newreferer (consumed server-side);
 *        - out-of-scope same-origin requests are repaired into
 *          {token_prefix}{proto}/{host}{path}{search} and fetched directly
 *          (no redirect round-trip), with the query string un-rewritten;
 *        - cross-origin requests are routed through the proxy using the
 *          request's own host and the current site's protocol (upstream
 *          behaviour — keeps third-party API/font calls inside the proxy);
 *   2. rewrites text-ish POST/PUT/PATCH bodies: proxied absolute URLs and
 *      ___-prefixed identifiers embedded in JSON/text/form payloads are
 *      mapped back to their real values (upstream `_0x13e402` unrewrite);
 *   3. delays 404 responses by 3 seconds before returning them (upstream
 *      behaviour — the response is NOT refetched);
 *   4. keeps the current site context in sync via PROXY_CUR_LOCATION.
 *
 * The upstream `x-siteproxy-body-headers` body-header smuggling unpacker is
 * intentionally not implemented: it only exists to work around Cloudflare
 * worker header size limits, which do not apply to this Go server.
 */
'use strict';

const CONFIG = __SITEPROXY_CONFIG_JSON__;
const TOKEN_PREFIX = CONFIG.token_prefix;
const PROXY_ORIGIN = self.location.origin;
const PROXY_URL_PREFIX = (CONFIG.proxy_url || PROXY_ORIGIN).replace(/\/+$/, '') + TOKEN_PREFIX;
const PROXY_BASE_PATH = (() => {
  try { return new URL(CONFIG.proxy_url || PROXY_ORIGIN).pathname.replace(/\/+$/, ''); }
  catch (e) { return ''; }
})();
const PROXY_HOST = (() => {
  try { return new URL(CONFIG.proxy_url || PROXY_ORIGIN).host; }
  catch (e) { return ''; }
})();

// Current proxied site context, updated by pages via postMessage.
let curLocation = {
  protocol: CONFIG.proxy_real_protocol || 'https',
  host: CONFIG.proxy_real_host || ''
};

self.addEventListener('install', (event) => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'PROXY_CUR_LOCATION' && event.data.data) {
    const d = event.data.data;
    if (d.protocol !== undefined && d.host !== undefined &&
        (d.protocol !== self.proxy_target_protocol || d.host !== self.proxy_target_host)) {
      self.proxy_target_protocol = d.protocol;
      self.proxy_target_host = d.host;
    }
  }
});

// Removes the configured proxy base path and token prefix from a pathname.
// Returns null when the path is not inside the proxy scope.
function pathInScope(pathname) {
  let p = pathname;
  if (PROXY_BASE_PATH && p.startsWith(PROXY_BASE_PATH)) p = p.substring(PROXY_BASE_PATH.length);
  if (TOKEN_PREFIX === '/') return true;
  return p.startsWith(TOKEN_PREFIX);
}

function isSelfScriptPath(pathname) {
  return pathname.endsWith('/siteproxy-response-injected.js') ||
         pathname.endsWith('/siteproxy_service_worker.js');
}

// Extracts {protocol, host} from a proxied URL (referer / client URL).
function siteFromProxiedURL(u) {
  if (!u || !u.startsWith(PROXY_URL_PREFIX)) return null;
  const rest = u.substring(PROXY_URL_PREFIX.length).split('/');
  if (rest.length >= 2) return { protocol: rest[0], host: rest[1] };
  return null;
}

// Un-rewrite text: proxied absolute URLs -> real URLs, and the ___-prefixed
// identifiers the server injected back to their original names
// (upstream `_0x13e402`).
function unrewriteText(text) {
  if (typeof text !== 'string') return text;
  text = text.replace(
    new RegExp(PROXY_URL_PREFIX.replace(/[-/\\^$*+?.()|[\]{}]/g, '\\$&') + '(http[s]?)/([^/]+)', 'g'),
    '$1://$2');
  text = text.replace(/___location/g, 'location');
  text = text.replace(/___domain/g, 'domain');
  text = text.replace(/navigator.___serviceWorker/g, 'navigator.serviceWorker');
  text = text.replace(/document.___requestStorageAccessFor/g, 'document.requestStorageAccessFor');
  text = text.replace(/___pushState/g, 'pushState');
  text = text.replace(/___replaceState/g, 'replaceState');
  text = text.replace(/___URL/g, 'URL');
  return text;
}

self.addEventListener('fetch', (event) => {
  event.respondWith(handleFetch(event));
});

async function handleFetch(event) {
  const req = event.request;
  let url;
  try {
    url = new URL(req.url);
  } catch (e) {
    return fetch(req);
  }

  // Cloudflare Insights beacon: answer directly (upstream behaviour).
  if (url.hostname === 'static.cloudflareinsights.com') {
    return new Response('', { status: 200, headers: { 'content-type': 'text/javascript' } });
  }

  // Determine the current site context: client window URL first, then the
  // request referer, then the last PROXY_CUR_LOCATION message.
  let siteProto = self.proxy_target_protocol || curLocation.protocol;
  let siteHost = self.proxy_target_host || curLocation.host;
  let client = null;
  try {
    client = event.clientId ? await self.clients.get(event.clientId) : null;
  } catch (e) { /* ignore */ }
  const fromClient = client && client.url && client.url.startsWith(PROXY_URL_PREFIX)
    ? siteFromProxiedURL(client.url) : null;
  if (fromClient) {
    siteProto = fromClient.protocol;
    siteHost = fromClient.host;
  } else {
    const fromRef = siteFromProxiedURL(req.referrer || '');
    if (fromRef) {
      siteProto = fromRef.protocol;
      siteHost = fromRef.host;
    }
  }

  const headers = new Headers(req.headers);
  headers.set('siteproxy-sw', '1');

  // The referer must stay the current page's real origin, even when the
  // request itself targets a cross-origin host.
  const siteOrigin = siteProto + '://' + siteHost;
  let fetchURL = req.url;
  const isSelfScripts = url.host === PROXY_HOST && isSelfScriptPath(url.pathname);

  if (siteProto !== undefined && !isSelfScripts &&
      (url.host !== PROXY_HOST || !pathInScope(url.pathname))) {
    // Out-of-scope request (or any cross-origin request): repair it into
    // the proxy scope.
    if (siteHost !== url.host && url.host !== PROXY_HOST) {
      siteHost = url.host; // cross-origin request: use its own host
    }
    if (siteHost !== PROXY_HOST) {
      const search = unrewriteText(url.search);
      fetchURL = PROXY_URL_PREFIX + siteProto + '/' + siteHost + url.pathname + search;
    }
  }

  // Only send a real referer when the site context is known; otherwise an
  // empty host would produce an invalid "https://" Referer upstream.
  if (siteHost) headers.set('siteproxy-newreferer', siteOrigin);

  const init = {
    method: req.method,
    headers: headers,
    mode: 'cors',
    credentials: 'include',
    redirect: req.redirect,
  };

  // Text-ish request bodies: un-rewrite embedded proxied URLs / identifiers.
  const method = req.method.toUpperCase();
  if (method === 'POST' || method === 'PUT' || method === 'PATCH') {
    const clone = req.clone();
    const ct = clone.headers.get('Content-Type') || '';
    const enc = clone.headers.get('Content-Encoding');
    if (!enc && (ct.includes('json') || ct.includes('text') || ct.includes('form'))) {
      let bodyText = await clone.text();
      bodyText = unrewriteText(bodyText);
      init.body = bodyText;
    } else {
      init.body = await clone.arrayBuffer();
    }
  }

  let resp;
  try {
    resp = await fetch(fetchURL, init);
  } catch (err) {
    return new Response('siteproxy service worker fetch error: ' + err, {
      status: 502,
      headers: { 'Content-Type': 'text/plain; charset=UTF-8' },
    });
  }
  if (resp && resp.status === 404) {
    // Upstream behaviour: delay the 404 by 3s, then return it unchanged.
    await new Promise((r) => setTimeout(r, 3000));
  }
  return resp;
}
