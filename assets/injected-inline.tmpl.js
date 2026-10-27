<script>
  if (!window.siteproxy_injected_flag) {
    var proxy_url_prefix = '__PP__';
    var proxy_real_protocol = '__PROTO__';
    var proxy_real_host = '__HOST__';
    var config_proxy_url = '__CURL__';
    var config_token_prefix = '__TOKEN__';
    function proxyRewrite(url) {
      if (typeof url !== "string") return url;
      if (url.startsWith(proxy_url_prefix)) return url;
      if (url.startsWith("http://") || url.startsWith("https://")) {
        var protocol = url.split("://")[0];
        var rest = url.substring(protocol.length + 3);
        return proxy_url_prefix + protocol + "/" + rest;
      }
      if (url.startsWith("//")) {
        return proxy_url_prefix + "https/" + url.substring(2);
      }
      if (url.startsWith("/")) {
        return proxy_url_prefix + proxy_real_protocol + "/" + proxy_real_host + url;
      }
      // relative path without leading slash
      return proxy_url_prefix + proxy_real_protocol + "/" + proxy_real_host + "/" + url;
    }
    window.___location = (function() {
      var target = window.location;
      return {
        get href() { return target.href; },
        set href(v) { target.href = proxyRewrite(v); },
        replace(v) { target.replace(proxyRewrite(v)); },
        assign(v) { target.assign(proxyRewrite(v)); },
        reload: target.reload.bind(target)
      };
    })();
    var originalReplaceState = window.history.replaceState;
    var originalPushState = window.history.pushState;
    window.history.___replaceState = function(state, title, url) {
      var rewritten = url !== undefined ? proxyRewrite(url) : url;
      return originalReplaceState.call(this, state, title, rewritten);
    };
    window.history.___pushState = function(state, title, url) {
      var rewritten = url !== undefined ? proxyRewrite(url) : url;
      return originalPushState.call(this, state, title, rewritten);
    };
  } else {
    proxy_url_prefix = '__PP__';
    proxy_real_protocol = '__PROTO__';
    proxy_real_host = '__HOST__';
    config_proxy_url = '__CURL__';
    config_token_prefix = '__TOKEN__';
    window.siteproxy_injected_flag = false; // reset flag to allow script re-execution
    if (window.proxy_worker_registration && window.proxy_worker_registration.active) {
      window.proxy_worker_registration.active.postMessage({
        type: 'PROXY_CUR_LOCATION',
        data: { protocol: proxy_real_protocol, host: proxy_real_host }
      });
    }
  }
</script>
