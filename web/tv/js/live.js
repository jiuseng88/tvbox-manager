// /tv 直播页逻辑。
//
// 频道表由 /tvapi/live 一次性下发（服务端已合并去重），这里只负责：
//   分组/搜索过滤 → 渲染列表 → 按协议选播放引擎 → 断流重连 → 换线路。
// 播放地址统一走 /tvapi/media 代理：直连会撞 CORS 与防盗链，且 http 源在
// https 页面下会被浏览器当混合内容拦掉。
(function () {
    'use strict';

    var LIVE_URL = '/tvapi/live';
    var MEDIA_URL = '/tvapi/media?url=';
    var LAST_KEY = 'tvboxLiveLast';
    var MAX_RECONNECT = 3;
    var MAX_RENDER = 400; // 单个分组最多渲染多少个频道，超出靠搜索收敛

    var state = {
        channels: [],
        groups: [],
        sources: [],
        group: '',
        srcFilter: '', // 按直播源筛选：空=全部，其余=来源名
        keyword: '',
        statFlash: '', // 临时状态文案（刷新成功后短暂提示）
        cur: null,
        line: 0,
        hls: null,
        flv: null,
        reconnect: 0,
        mediaRecover: 0,
        timer: null
    };

    var el = {
        stat: document.getElementById('liveStat'),
        search: document.getElementById('chSearch'),
        refreshBtn: document.getElementById('refreshBtn'),
        groupList: document.getElementById('groupList'),
        srcFilter: document.getElementById('srcFilter'),
        chanList: document.getElementById('chanList'),
        chanCount: document.getElementById('chanCount'),
        chanPaneTitle: document.getElementById('chanPaneTitle'),
        video: document.getElementById('liveVideo'),
        tip: document.getElementById('playerTip'),
        nowName: document.getElementById('nowName'),
        nowSrc: document.getElementById('nowSrc'),
        lineGroup: document.getElementById('lineGroup')
    };

    // ---- 工具 ----

    function mediaURL(raw) {
        return MEDIA_URL + encodeURIComponent(raw);
    }

    function esc(s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // kindOf 只按地址后缀判协议。直播源里 m3u8 占绝大多数，其余猜测一律按 HLS 试，
    // 失败后还有换线路/重连兜底，没必要做内容嗅探（那要多发一次请求）。
    function kindOf(raw) {
        var stem = raw.split('|')[0].split('?')[0].toLowerCase();
        if (stem.slice(-4) === '.flv') return 'flv';
        if (/\.(mp4|m4v|webm)$/.test(stem)) return 'direct';
        if (stem.slice(-5) === '.m3u8' || stem.slice(-4) === '.m3u') return 'hls';
        return 'hls';
    }

    function showTip(html, spin) {
        el.tip.innerHTML = (spin ? '<div class="spin"></div>' : '') + '<div>' + html + '</div>';
        el.tip.classList.add('show');
    }

    function hideTip() {
        el.tip.classList.remove('show');
    }

    // ---- 加载频道表 ----

    // hasLines 频道是否有可播线路。后端已过滤一遍，这里再兜一层：
    // 旧版本落盘缓存、或源里写了空地址时，不该把「点开必失败」的频道摆出来。
    function hasLines(ch) {
        var urls = ch && ch.urls;
        if (!Array.isArray(urls)) return false;
        for (var i = 0; i < urls.length; i++) {
            if (urls[i] && String(urls[i]).trim()) return true;
        }
        return false;
    }

    // applyData 应用一次频道表响应。只保留有线路的频道，下游（分组计数、来源筛选、
    // 统计、列表）全部基于过滤后的集合，口径自然一致。
    // keepView=true 用于「刷新频道」：尽量沿用用户当前的分组/来源/搜索。
    function applyData(data, keepView) {
        var list = Array.isArray(data && data.channels) ? data.channels : [];
        state.channels = list.filter(hasLines);
        state.groups = Array.isArray(data && data.groups) ? data.groups : [];
        state.sources = Array.isArray(data && data.sources) ? data.sources : [];

        if (!state.channels.length) {
            state.group = '';
            showTip(emptyHint(), false);
            renderStat();
            renderSourceFilter();
            renderGroups();
            renderChannels();
            return 0;
        }
        hideTip();

        if (!keepView) {
            state.srcFilter = '';
            state.keyword = '';
            if (el.search) el.search.value = '';
            state.group = firstGroupWithChannels();
        } else {
            // 刷新后原来的分组可能已经没有频道（源下线/改名），回退到第一个有内容的分组
            if (state.group && !groupHasChannels(state.group)) {
                state.group = firstGroupWithChannels();
            }
        }

        renderStat();
        renderSourceFilter();
        renderGroups();
        renderChannels();
        return state.channels.length;
    }

    // firstGroupWithChannels 第一个还有频道的分组。后端 groups 已按展示权重排好序，
    // 直接沿用它的顺序，跳过刷新后变空的分组，避免进页面就是空白列表。
    function firstGroupWithChannels() {
        var counts = groupCounts();
        for (var i = 0; i < state.groups.length; i++) {
            if (counts[state.groups[i]]) return state.groups[i];
        }
        return state.groups[0] || '';
    }

    function groupHasChannels(g) {
        for (var i = 0; i < state.channels.length; i++) {
            if (state.channels[i].group === g) return true;
        }
        return false;
    }

    function load() {
        showTip('正在加载频道…', true);
        fetch(LIVE_URL, { cache: 'no-store' })
            .then(function (r) {
                if (!r.ok) throw new Error('HTTP ' + r.status);
                return r.json();
            })
            .then(function (body) {
                applyData((body && body.data) || {}, false);
                restoreLast();
            })
            .catch(function (err) {
                showTip('频道加载失败：' + esc(err.message) + '<br>稍后重试，或检查管理端的源状态。', false);
            });
    }

    function emptyHint() {
        var bad = state.sources.filter(function (s) { return !s.ok; });
        var html = '没有取到任何频道。<br>';
        if (!state.sources.length) {
            html += '平铺源里没有可用的 <code>lives</code> 直播配置。<br>换一个带直播的仓，或在管理端重新检测源后再来。';
        } else if (bad.length === state.sources.length) {
            html += '所有直播源都拉取失败：<br>' + bad.slice(0, 5).map(function (s) {
                return esc(s.name) + '：' + esc(s.error || '未知错误');
            }).join('<br>');
        } else {
            html += '直播源里解析不出可播放频道（可能全是 proxy:// 或 rtmp/rtsp 地址）。';
        }
        return html;
    }

    function renderStat() {
        if (state.statFlash) {
            el.stat.textContent = state.statFlash;
            return;
        }
        var ok = state.sources.filter(function (s) { return s.ok; }).length;
        var txt = '共 ' + state.channels.length + ' 个频道';
        if (state.sources.length) txt += ' · ' + ok + '/' + state.sources.length + ' 个直播源';
        el.stat.textContent = txt;
    }

    // statFlash 短暂替换顶栏统计文案（如刷新成功提示），几秒后自动复原。
    var statFlashTimer = null;
    function flashStat(txt) {
        state.statFlash = txt;
        renderStat();
        clearTimeout(statFlashTimer);
        statFlashTimer = setTimeout(function () {
            state.statFlash = '';
            renderStat();
        }, 2600);
    }

    // ---- 分组 ----

    function groupCounts() {
        var map = Object.create(null);
        state.channels.forEach(function (ch) {
            if (state.srcFilter && chanSources(ch).indexOf(state.srcFilter) < 0) return;
            map[ch.group] = (map[ch.group] || 0) + 1;
        });
        return map;
    }

    function renderGroups() {
        var counts = groupCounts();
        var total = 0;
        state.groups.forEach(function (g) { total += counts[g] || 0; });
        var frag = document.createDocumentFragment();

        frag.appendChild(groupItem('', '全部频道', total));
        state.groups.forEach(function (g) {
            var n = counts[g] || 0;
            if (!n) return; // 没有频道的分组不占位（筛选某个来源后尤其明显）
            frag.appendChild(groupItem(g, g, n));
        });

        el.groupList.textContent = '';
        el.groupList.appendChild(frag);
    }

    function groupItem(value, label, count) {
        var d = document.createElement('div');
        d.className = 'group-item' + (state.group === value ? ' active' : '');
        var n = document.createElement('span');
        n.className = 'gname';
        n.textContent = label;
        var c = document.createElement('span');
        c.className = 'gcount';
        c.textContent = count;
        d.appendChild(n);
        d.appendChild(c);
        d.addEventListener('click', function () {
            state.group = value;
            renderGroups();
            renderChannels();
        });
        return d;
    }

    // ---- 直播源筛选 ----

    // sourceNames 从频道里聚合出实际出现过的来源名（去重后按名称排序）。
    // 用频道聚合而不是 state.sources：筛选项一定能命中，且失败源不会占位。
    function sourceNames() {
        var seen = Object.create(null);
        var out = [];
        state.channels.forEach(function (ch) {
            chanSources(ch).forEach(function (s) {
                if (s && !seen[s]) { seen[s] = true; out.push(s); }
            });
        });
        out.sort(function (a, b) { return a.localeCompare(b); });
        return out;
    }

    function renderSourceFilter() {
        if (!el.srcFilter) return;
        var names = sourceNames();
        if (state.srcFilter && names.indexOf(state.srcFilter) < 0) {
            state.srcFilter = ''; // 源已不存在（删除/改名），回退全部
        }
        el.srcFilter.textContent = '';
        var all = document.createElement('option');
        all.value = '';
        all.textContent = '全部直播源（' + names.length + '）';
        el.srcFilter.appendChild(all);
        names.forEach(function (n) {
            var o = document.createElement('option');
            o.value = n;
            o.textContent = n;
            el.srcFilter.appendChild(o);
        });
        el.srcFilter.value = state.srcFilter;
    }

    // ---- 频道列表 ----

    function renderChannels() {
        var kw = state.keyword.trim().toLowerCase();
        var list = state.channels.filter(function (ch) {
            if (state.group && ch.group !== state.group) return false;
            if (state.srcFilter && chanSources(ch).indexOf(state.srcFilter) < 0) return false;
            if (kw && ch.name.toLowerCase().indexOf(kw) < 0) return false;
            return true;
        });

        var title = state.group || '全部频道';
        if (state.srcFilter) title = '来源 ' + state.srcFilter + ' · ' + title;
        el.chanPaneTitle.textContent = title;
        el.chanCount.textContent = list.length + ' 个';

        if (!list.length) {
            var hint = state.srcFilter
                ? '来源「' + state.srcFilter + '」在「' + (state.group || '全部频道') + '」下没有频道，换个分组或来源试试'
                : '这个分组下没有匹配的频道';
            el.chanList.innerHTML = '<div class="empty">' + esc(hint) + '</div>';
            return;
        }

        var frag = document.createDocumentFragment();
        list.slice(0, MAX_RENDER).forEach(function (ch) {
            frag.appendChild(chanItem(ch));
        });
        if (list.length > MAX_RENDER) {
            var more = document.createElement('div');
            more.className = 'empty';
            more.textContent = '共 ' + list.length + ' 个，仅显示前 ' + MAX_RENDER + ' 个，请用搜索框缩小范围';
            frag.appendChild(more);
        }

        el.chanList.textContent = '';
        el.chanList.appendChild(frag);
    }

    // chanSources 频道的全部来源名：优先用后端合并的 sources[]，回退旧字段 source。
    function chanSources(ch) {
        if (Array.isArray(ch.sources) && ch.sources.length) return ch.sources;
        return ch.source ? [ch.source] : [];
    }

    function chanItem(ch) {
        var d = document.createElement('div');
        var active = state.cur && state.cur.name === ch.name && state.cur.group === ch.group;
        d.className = 'chan-item' + (active ? ' active' : '');
        d.dataset.key = ch.group + '|' + ch.name;

        if (ch.logo) {
            var img = document.createElement('img');
            img.className = 'chan-logo';
            img.loading = 'lazy';
            img.referrerPolicy = 'no-referrer';
            img.src = ch.logo;
            img.alt = '';
            img.onerror = function () { img.replaceWith(placeholderLogo(ch.name)); };
            d.appendChild(img);
        } else {
            d.appendChild(placeholderLogo(ch.name));
        }

        var n = document.createElement('span');
        n.className = 'chan-name';
        n.textContent = ch.name;
        d.appendChild(n);

        // 来源标签：让用户能看出频道来自哪个直播源（改过名的源也能核对）。
        var srcs = chanSources(ch);
        // 正按某源筛选时，把该源排到最前，标签与筛选条件一致
        if (state.srcFilter && srcs.indexOf(state.srcFilter) >= 0) {
            srcs = [state.srcFilter].concat(srcs.filter(function (s) { return s !== state.srcFilter; }));
        }
        if (srcs.length) {
            var s = document.createElement('span');
            s.className = 'chan-src';
            s.textContent = srcs.length > 1 ? srcs[0] + ' +' + (srcs.length - 1) : srcs[0];
            s.title = '来源：' + srcs.join('、');
            d.appendChild(s);
        }

        if (ch.urls.length > 1) {
            var l = document.createElement('span');
            l.className = 'chan-lines';
            l.textContent = ch.urls.length + '线';
            d.appendChild(l);
        }

        d.addEventListener('click', function () {
            play(ch, 0, false);
        });
        return d;
    }

    function placeholderLogo(name) {
        var s = document.createElement('span');
        s.className = 'chan-logo ph';
        s.textContent = (name || '?').trim().charAt(0).toUpperCase();
        return s;
    }

    function markActive() {
        var key = state.cur ? state.cur.group + '|' + state.cur.name : '';
        var items = el.chanList.querySelectorAll('.chan-item');
        for (var i = 0; i < items.length; i++) {
            items[i].classList.toggle('active', items[i].dataset.key === key);
        }
    }

    // ---- 播放 ----

    function play(ch, line, keepRetry) {
        clearTimeout(state.timer);
        if (!keepRetry) {
            state.reconnect = 0;
            state.mediaRecover = 0;
        }
        stopEngine();

        state.cur = ch;
        state.line = line || 0;
        var raw = (ch.urls || [])[state.line];
        if (!raw) {
            showTip('该频道没有可用的播放地址', false);
            return;
        }

        renderNow();
        markActive();
        remember(ch);
        showTip('正在连接…', true);

        var kind = kindOf(raw);
        if (kind === 'flv') startFlv(raw);
        else if (kind === 'direct') startDirect(raw);
        else startHls(raw);
    }

    function startHls(raw) {
        var video = el.video;
        var url = mediaURL(raw);
        video.onerror = null;

        if (window.Hls && window.Hls.isSupported()) {
            var hls = new Hls({
                enableWorker: true,
                lowLatencyMode: false,
                liveSyncDurationCount: 3,
                manifestLoadingTimeOut: 12000,
                manifestLoadingMaxRetry: 2,
                levelLoadingTimeOut: 12000,
                levelLoadingMaxRetry: 3,
                fragLoadingTimeOut: 20000,
                fragLoadingMaxRetry: 4
            });
            state.hls = hls;
            hls.attachMedia(video);
            hls.on(Hls.Events.MEDIA_ATTACHED, function () { hls.loadSource(url); });
            hls.on(Hls.Events.MANIFEST_PARSED, function () {
                video.play().catch(function () { /* 自动播放被拦，等用户点 */ });
            });
            hls.on(Hls.Events.ERROR, onHlsError);
            return;
        }
        if (video.canPlayType('application/vnd.apple.mpegurl')) {
            video.src = url;
            video.play().catch(function () {});
            video.onerror = function () { retry('连接中断'); };
            return;
        }
        showTip('当前浏览器不支持 HLS 播放', false);
    }

    function onHlsError(_, data) {
        if (!data || !data.fatal) return;
        if (data.type === Hls.ErrorTypes.MEDIA_ERROR && state.mediaRecover < 2) {
            state.mediaRecover++;
            try {
                state.hls.recoverMediaError();
                return;
            } catch (e) { /* 落下去走整体重连 */ }
        }
        retry(data.type === Hls.ErrorTypes.MEDIA_ERROR ? '解码异常' : '连接中断');
    }

    function startFlv(raw) {
        if (!window.mpegts || !window.mpegts.isSupported()) {
            showTip('当前浏览器不支持 FLV 播放', false);
            return;
        }
        var player = mpegts.createPlayer({
            type: 'flv',
            isLive: true,
            url: mediaURL(raw)
        }, {
            enableStashBuffer: false,
            stashInitialSize: 128,
            liveBufferLatencyChasing: true,
            lazyLoad: false,
            autoCleanupSourceBuffer: true
        });
        state.flv = player;
        player.attachMediaElement(el.video);
        player.on(mpegts.Events.ERROR, function () { retry('连接中断'); });
        player.load();
        var p = player.play();
        if (p && p.catch) p.catch(function () {});
    }

    function startDirect(raw) {
        var video = el.video;
        video.src = mediaURL(raw);
        video.onerror = function () { retry('连接中断'); };
        video.play().catch(function () {});
    }

    // retry 直播断流是常态（源站波动、CDN 切节点），静默重连比弹错误体验好。
    function retry(reason) {
        if (!state.cur) return;
        if (state.reconnect >= MAX_RECONNECT) {
            nextLine(reason);
            return;
        }
        state.reconnect++;
        showTip(reason + '，正在重连（' + state.reconnect + '/' + MAX_RECONNECT + '）…', true);
        var delay = 1000 * state.reconnect;
        state.timer = setTimeout(function () {
            play(state.cur, state.line, true);
        }, delay);
    }

    function nextLine(reason) {
        var ch = state.cur;
        if (!ch) return;
        var urls = ch.urls || [];
        if (state.line + 1 < urls.length) {
            showTip(reason + '，切换到线路 ' + (state.line + 2) + '…', true);
            state.timer = setTimeout(function () {
                state.reconnect = 0;
                state.mediaRecover = 0;
                play(ch, state.line + 1, true);
            }, 500);
            return;
        }
        showTip(reason + '。该频道的 ' + urls.length + ' 条线路都放不出来，换一个频道试试。', false);
    }

    function stopEngine() {
        if (state.hls) {
            try { state.hls.destroy(); } catch (e) {}
            state.hls = null;
        }
        if (state.flv) {
            try { state.flv.destroy(); } catch (e) {}
            state.flv = null;
        }
        var video = el.video;
        video.onerror = null;
        video.removeAttribute('src');
        try { video.load(); } catch (e) {}
    }

    // ---- 当前频道信息 ----

    function renderNow() {
        var ch = state.cur;
        if (!ch) return;
        el.nowName.textContent = ch.name;
        var bits = [];
        if (ch.group) bits.push(ch.group);
        var srcs = chanSources(ch);
        if (srcs.length) {
            var label = srcs.slice(0, 3).join('、');
            if (srcs.length > 3) label += ' 等 ' + srcs.length + ' 个源';
            bits.push('来源 ' + label);
        }
        bits.push((ch.urls || []).length + ' 条线路');
        el.nowSrc.textContent = bits.join(' · ');

        el.lineGroup.textContent = '';
        (ch.urls || []).forEach(function (_, i) {
            var b = document.createElement('button');
            b.className = 'line-btn' + (i === state.line ? ' active' : '');
            b.textContent = '线路 ' + (i + 1);
            b.addEventListener('click', function () {
                state.reconnect = 0;
                state.mediaRecover = 0;
                play(ch, i, false);
            });
            el.lineGroup.appendChild(b);
        });
    }

    // ---- 记忆上次观看 ----

    function remember(ch) {
        try {
            localStorage.setItem(LAST_KEY, JSON.stringify({ group: ch.group, name: ch.name }));
        } catch (e) {}
    }

    function restoreLast() {
        var last;
        try {
            last = JSON.parse(localStorage.getItem(LAST_KEY) || 'null');
        } catch (e) {
            return;
        }
        if (!last || !last.name) return;
        var hit = state.channels.filter(function (ch) {
            return ch.name === last.name && ch.group === last.group;
        })[0];
        if (!hit) return;
        // 不自动起播（浏览器会拦自动播放），只把上次的频道选中并定位到分组
        state.cur = hit;
        state.line = 0;
        if (state.groups.indexOf(hit.group) >= 0 && state.group !== hit.group) {
            state.group = hit.group;
            renderGroups();
            renderChannels();
        }
        renderNow();
        markActive();
        showTip('上次看到「' + esc(hit.name) + '」，点一下频道即可继续。', false);
    }

    // ---- 事件绑定 ----

    el.video.addEventListener('playing', function () {
        hideTip();
        state.reconnect = 0;
        state.mediaRecover = 0;
    });

    var searchTimer = null;
    el.search.addEventListener('input', function () {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(function () {
            state.keyword = el.search.value || '';
            renderChannels();
        }, 180);
    });

    if (el.srcFilter) {
        el.srcFilter.addEventListener('change', function () {
            state.srcFilter = el.srcFilter.value || '';
            // 切源后当前分组可能一个频道都没有，顺带跳到第一个有内容的分组
            if (state.group && !groupHasChannels(state.group)) {
                var counts = groupCounts();
                if (!counts[state.group]) state.group = firstGroupWithChannels();
            }
            renderGroups();   // 分组计数跟随来源筛选
            renderChannels();
        });
    }

    // ---- 刷新频道 ----

    // refreshing 挡住连点：一次刷新要重拉几十个上游，重复触发只会互相排队。
    var refreshing = false;

    function refreshChannels() {
        if (refreshing) return;
        refreshing = true;
        if (el.refreshBtn) {
            el.refreshBtn.disabled = true;
            el.refreshBtn.classList.add('busy');
        }
        showTip('正在刷新频道…<br>重新拉取所有直播源，通常需要几秒', true);
        fetch(LIVE_URL + '/refresh', { method: 'POST', cache: 'no-store' })
            .then(function (r) {
                if (!r.ok) throw new Error('HTTP ' + r.status);
                return r.json();
            })
            .then(function (body) {
                var n = applyData((body && body.data) || {}, true);
                if (n) flashStat('已刷新 · ' + n + ' 个频道');
            })
            .catch(function (err) {
                showTip('刷新失败：' + esc(err.message) + '<br>稍后重试，或检查管理端的直播源状态。', false);
            })
            .then(function () {
                refreshing = false;
                if (el.refreshBtn) {
                    el.refreshBtn.disabled = false;
                    el.refreshBtn.classList.remove('busy');
                }
            });
    }

    if (el.refreshBtn) {
        el.refreshBtn.addEventListener('click', refreshChannels);
    }

    window.addEventListener('beforeunload', stopEngine);

    load();
})();
