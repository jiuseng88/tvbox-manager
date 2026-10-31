/* 登录态客户端兜底
 *
 * 服务端已经对 /tv 与 /tvapi/* 做了鉴权：未登录时首屏 HTML 会被 302 到后台登录页，
 * 所以正常情况下本文件不会触发。
 *
 * 这里只处理一种残余情况：页面已经打开，之后 Cookie 过期或被清掉，
 * 此时接口开始返回 401。把用户弹回登录页，并带上当前地址，登录完成后原路返回，
 * 否则页面会静默变成「一个源都没有」的空壳，用户以为源挂了。
 */
(function () {
  "use strict";

  var target = "/?login=1&next=" + encodeURIComponent(location.pathname + location.search);
  var jumped = false;

  function goLogin() {
    if (jumped) return;
    jumped = true;
    location.replace(target);
  }

  // fetch：tv 页拉源列表、直播表、代理接口都走这里
  var origFetch = window.fetch;
  if (origFetch) {
    window.fetch = function () {
      return origFetch.apply(this, arguments).then(function (r) {
        if (r && r.status === 401) goLogin();
        return r;
      });
    };
  }

  // XHR：hls.js / mpegts.js 拉分片走 XHR，不经 fetch
  var origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function () {
    var xhr = this;
    xhr.addEventListener("loadend", function () {
      if (xhr.status === 401) goLogin();
    });
    return origOpen.apply(this, arguments);
  };
})();
