/* tvbox-manager 管理后台（Vue3 内嵌版，无构建步骤） */
(function () {
  "use strict";

  const { createApp, ref, reactive, computed, onMounted, watch, nextTick } = Vue;

  /* ---------- 工具 ---------- */

  const store = {
    get token() { return localStorage.getItem("tm_token") || ""; },
    set token(v) { v ? localStorage.setItem("tm_token", v) : localStorage.removeItem("tm_token"); },
  };

  let loginGate = null; // 组件挂载后指向 needLogin ref

  async function api(method, path, body, isForm) {
    const opt = { method, headers: { "X-Token": store.token } };
    if (body && !isForm) {
      opt.headers["Content-Type"] = "application/json";
      opt.body = JSON.stringify(body);
    } else if (body && isForm) {
      opt.body = body; // FormData
    }
    const resp = await fetch(path, opt);
    let data = {};
    try { data = await resp.json(); } catch (e) { /* 非 JSON */ }
    if (resp.status === 401) {
      if (loginGate) loginGate.value = true;
      throw new Error("未授权，请输入访问令牌");
    }
    if (!resp.ok || data.code !== 0) {
      throw new Error(data.msg || ("HTTP " + resp.status));
    }
    return data.data;
  }

  function relTime(iso) {
    if (!iso) return "从未";
    const d = new Date(iso), diff = (Date.now() - d.getTime()) / 1000;
    if (diff < 5) return "刚刚";
    if (diff < 60) return Math.floor(diff) + " 秒前";
    if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
    if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
    return Math.floor(diff / 86400) + " 天前";
  }
  function absTime(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    const p = (n) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }
  function fmtBytes(n) {
    if (!n) return "0B";
    if (n < 1024) return n + "B";
    if (n < 1048576) return (n / 1024).toFixed(1) + "KB";
    return (n / 1048576).toFixed(1) + "MB";
  }
  // 「名称 URL / 名称,URL / 名称|URL / 纯URL」行解析（与后端一致）；空格/Tab 分隔时 URL 在后
  function parseLine(line) {
    const s = (line || "").trim();
    if (!s || s.startsWith("#")) return null;
    if (s.startsWith("http://") || s.startsWith("https://")) return { name: "", url: s };
    for (const sep of ["|", ","] ) {
      const i = s.indexOf(sep);
      if (i > 0) return { name: s.slice(0, i).trim(), url: s.slice(i + 1).trim() };
    }
    // 「名称 地址」空格/Tab 分隔（地址不含空格，取第一个空白切分）
    const m = s.match(/^(\S+)[\s\t]+(\S+)$/);
    if (m) return { name: m[1], url: m[2] };
    return { name: "", url: s };
  }

  /* ---------- 应用 ---------- */

  const app = createApp({
    setup() {
      const view = ref("dashboard");
      const needLogin = ref(false);
      const tokenInput = ref("");
      const toasts = ref([]);
      const toast = (msg, type) => {
        const id = ++toastSeq;
        toasts.value.push({ id, msg, type: type || "info" });
        setTimeout(() => { toasts.value = toasts.value.filter(t => t.id !== id); }, 3200);
      };

      /* ---- 概览 ---- */
      const stats = ref({
        total: 0, enabled: 0, ok: 0, fail: 0, checking: 0, unknown: 0, single: 0, multi: 0, groups: 0,
        groupDist: [],
        // 站点源（订阅实际下发层）口径
        flatTotal: 0, flatEnabled: 0, flatOk: 0, flatFail: 0, flatChecking: 0, flatUnknown: 0
      });
      const progress = ref({ running: false, total: 0, done: 0, ok: 0, fail: 0 });
      const groupDist = ref([]);
      let pollTimer = null;

      async function loadStats() {
        try {
          stats.value = await api("GET", "/api/stats");
          // 分组分布由后端一次算全（以前是拉 ?size=500 的列表自己数，超过 500 条就不准）
          groupDist.value = (stats.value.groupDist || []).slice(0, 8);
        } catch (e) { toast(e.message, "error"); }
      }
      async function loadProgress() {
        try {
          progress.value = await api("GET", "/api/check/status");
        } catch (e) { /* 静默 */ }
      }
      // 只在「确实有检测在跑」时轮询进度；平时不产生任何周期请求。
      // 以前是只要停在概览页就每 2 秒打一次 /api/check/status，纯浪费。
      function startPolling() {
        stopPolling();
        pollTimer = setInterval(async () => {
          await loadProgress();
          if (progress.value.running) return; // 还在跑，下个周期继续
          stopPolling();                      // 跑完了：停表并刷新一次真实数据
          loadStats();
          loadSources(true);                  // 强制：丢掉检测前在途的旧响应
          loadFlats(true);
        }, 2000);
      }
      function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

      // 单条 / 批量检测是异步的：接口立刻返回，后端要等探测结束才落库状态。
      // 以前只在 3 秒后刷一次列表，慢源（超时 20-60 秒）就一直卡在「检测中」，
      // 用户以为没生效、或者看到的是检测前的旧状态。这里盯住 stats 的 checking
      // 计数（含仓源层），归零就停，最多盯 5 分钟。
      let watchTimer = null;
      function watchChecking() {
        stopWatching();
        let ticks = 0;
        watchTimer = setInterval(async () => {
          ticks++;
          const reload = view.value === "flats" ? loadFlats(true) : loadSources(true);
          await Promise.all([reload, loadStats()]);
          const pending = (stats.value.checking || 0) + (stats.value.flatChecking || 0);
          if (pending === 0 || ticks >= 150) stopWatching();
        }, 2000);
      }
      function stopWatching() { if (watchTimer) { clearInterval(watchTimer); watchTimer = null; } }
      async function runCheckAll() {
        try {
          await api("POST", "/api/check/run");
          toast("已开始全量检测", "ok");
          loadProgress();
          startPolling(); // 有了明确开始时间才起表，跑完自动停
        } catch (e) {
          toast(e.message, "warn"); // 409 = 已在运行，跟上进度即可
          loadProgress().then(() => { if (progress.value.running) startPolling(); });
        }
      }

      /* ---- 源列表 ---- */
      const list = ref([]);
      const total = ref(0);
      const filters = reactive({ keyword: "", type: "", status: "", group: "", enabled: "", notInSub: "", page: 1, size: 50, sort: "", sort_by: "", sort_dir: "" });
      const groups = ref([]);
      const checked = ref([]);
      let searchDebounce = null;
      let sourceReqSeq = 0; // 请求序号：翻页/轮询并发时只采纳最新响应，防止旧响应覆盖新页内容
      let sourceInflight = null; // 参数相同的并发请求合并成一次（{qs, p}）
      const refreshingSources = ref(false);
      const sourcesSyncedAt = ref(0); // 列表最后一次成功同步的时间戳（展示「更新于」）

      // force=true：不吃在途合并、并推进序号让在途旧响应作废。
      // 手动刷新和「检测后自愈」都必须走这条路 —— 否则刚检测完点刷新，
      // 拿回来的还是检测前那次请求的结果，看起来就是「列表状态对不上」。
      async function loadSources(force = false) {
        const q = new URLSearchParams();
        Object.keys(filters).forEach(k => { if (filters[k] !== "" && filters[k] != null) q.set(k, filters[k]); });
        const qs = q.toString();
        // 同一次操作可能同时触发「筛选变化」和「切换视图」两个 watch，参数一样就等前一个的结果
        if (!force && sourceInflight && sourceInflight.qs === qs) return sourceInflight.p;
        const p = (async () => {
          const seq = ++sourceReqSeq;
          try {
            const data = await api("GET", "/api/sources?" + qs);
            if (seq !== sourceReqSeq) return; // 已有更新的请求在途，丢弃本次旧响应
            list.value = data.list || [];
            total.value = data.total || 0;
            sourcesSyncedAt.value = Date.now();
            checked.value = checked.value.filter(id => list.value.some(s => s.id === id));
            // 数据变少导致当前页越界时，自动回到最后一页重载
            const maxPage = Math.max(1, Math.ceil(total.value / filters.size));
            if (!list.value.length && filters.page > maxPage) {
              filters.page = maxPage;
              loadSources();
            }
          } catch (e) { if (seq === sourceReqSeq) toast(e.message, "error"); }
        })();
        sourceInflight = { qs, p };
        try { await p; } finally { if (sourceInflight && sourceInflight.p === p) sourceInflight = null; }
      }
      // 刷新按钮：列表 + 概览数字一起重拉，按钮期间显示「刷新中…」给个反馈
      async function refreshSources() {
        if (refreshingSources.value) return;
        refreshingSources.value = true;
        try { await Promise.all([loadSources(true), loadStats(), loadGroups()]); }
        finally { refreshingSources.value = false; }
      }
      async function loadGroups() {
        try { groups.value = await api("GET", "/api/groups"); } catch (e) { /* 忽略 */ }
      }
      function applyFilter() {
        filters.page = 1;
        // 不在源列表页时不预加载：切过去时 watch(view) 会带着当前条件加载一次，
        // 否则同一次点击会连发两个一模一样的请求。
        if (view.value === "sources") loadSources();
      }
      watch(() => filters.keyword, () => {
        clearTimeout(searchDebounce);
        searchDebounce = setTimeout(applyFilter, 300);
      });
      watch(() => [filters.type, filters.status, filters.group, filters.enabled, filters.sort], applyFilter);
      // 选了下拉排序（失效置顶）时清掉表头排序，避免两者叠加
      watch(() => filters.sort, (v) => { if (v) { filters.sort_by = ""; filters.sort_dir = ""; } });
      // ---- 表头排序：点击升序 -> 再点降序 -> 三点恢复默认 ----
      function sortArrow(key) { return filters.sort_by === key ? (filters.sort_dir === "asc" ? "▲" : "▼") : ""; }
      function toggleSort(key) {
        if (filters.sort_by === key) {
          if (filters.sort_dir === "asc") { filters.sort_dir = "desc"; }
          else { filters.sort_by = ""; filters.sort_dir = ""; }
        } else { filters.sort_by = key; filters.sort_dir = "asc"; }
        applyFilter();
      }

      function summary(s) {
        if (!s.detail) return "—";
        const d = s.detail;
        if (s.type === "multi") return (d.subUrls || []).length + " 个子仓";
        return `${d.sites} 站点 / ${d.lives} 直播`;
      }
      // subHint 说明「这条源为什么进不了订阅」：启用了却一条可用站点源都没有时才有值。
      // flatCount 由后端按「未隐藏且自身启用的站点源」实时回填，判定比前端猜更准。
      function subHint(s) {
        if (!s.enabled) return "";
        if (s.flatCount > 0) return "";
        if (s.type === "unknown") return "未定型，未进订阅";
        if (s.type === "multi" && !((s.detail && s.detail.subUrls) || []).length) return "多仓无子仓，未进订阅";
        return "仓源被删，未进订阅";
      }
      // 恢复：按父源重建站点源 + 复活误删条目（后端 RestoreSourceFlats）
      async function restoreSource(s) {
        try {
          const rep = await api("POST", "/api/sources/" + s.id + "/restore");
          if (rep.hint) {
            toast(rep.hint, "error");
          } else {
            const bits = [];
            if (rep.restored) bits.push(`恢复 ${rep.restored} 条`);
            if (rep.created) bits.push(`新建 ${rep.created} 条`);
            toast(bits.length ? bits.join("，") : `已重建，当前 ${rep.alive} 条`, "ok");
          }
          loadSources(); loadStats();
        } catch (e) { toast(e.message, "error"); }
      }
      // 一键恢复当前列表里所有「未进订阅」的源
      async function restoreAllMissing() {
        const targets = list.value.filter(s => subHint(s));
        if (!targets.length) { toast("没有需要恢复的源", "info"); return; }
        let restored = 0, failed = 0;
        for (const s of targets) {
          try {
            const rep = await api("POST", "/api/sources/" + s.id + "/restore");
            restored += rep.restored + rep.created;
            if (rep.hint) failed++;
          } catch (e) { failed++; }
        }
        toast(`处理 ${targets.length} 条：新增/恢复 ${restored} 条仓源${failed ? `，${failed} 条仍需先检测或定型` : ""}`, failed ? "info" : "ok");
        loadSources(); loadStats();
      }
      function pages() { return Math.max(1, Math.ceil(total.value / filters.size)); }

      async function toggleSource(s) {
        try { await api("POST", "/api/sources/" + s.id + "/toggle"); s.enabled = !s.enabled; }
        catch (e) { toast(e.message, "error"); }
      }
      async function checkOne(s) {
        try {
          await api("POST", "/api/sources/" + s.id + "/check");
          s.status = "checking";
          toast("正在检测 " + s.name, "info");
          watchChecking(); // 盯到后端落库为止，不再靠一次 2.5 秒后的刷新碰运气
        } catch (e) { toast(e.message, "error"); }
      }
      function delSource(s) {
        if (!confirm(`确认删除源「${s.name}」？此操作不可恢复。`)) return;
        api("DELETE", "/api/sources/" + s.id).then(() => {
          toast("已删除", "ok"); loadSources(); loadGroups(); loadStats();
        }).catch(e => toast(e.message, "error"));
      }

      /* 批量 */
      const batchBar = computed(() => checked.value.length > 0);
      async function batchAction(action) {
        let group = "";
        if (action === "group") {
          group = prompt("移入分组名称：", "新分组");
          if (!group) return;
        }
        if (action === "delete" && !confirm(`确认删除选中的 ${checked.value.length} 个源？`)) return;
        const ids = [...checked.value];
        const idSet = new Set(ids);
        try {
          const r = await api("POST", "/api/sources/batch-action", { ids, action, group });
          const label = { enable: "启用", disable: "禁用", group: "移组", delete: "删除" }[action] || action;
          toast(`批量${label}完成：${r.affected} 条`, "ok");
          // 先本地同步刷新开关状态（不等接口回查，避免视觉延迟）
          if (action === "enable" || action === "disable") {
            const want = action === "enable";
            list.value = list.value.map(s => idSet.has(s.id) ? { ...s, enabled: want } : s);
          } else if (action === "group") {
            list.value = list.value.map(s => idSet.has(s.id) ? { ...s, group } : s);
          }
          checked.value = [];
          loadSources(); loadGroups(); loadStats();
        } catch (e) { toast(e.message, "error"); }
      }

      /* 编辑弹窗 */
      const editSrc = ref(null);
      // 只有 JSON 粘贴导入的本地源能改配置原文：内容托管在本服务的 /configs/xxx.json 上，
      // 其它源（普通地址、地址列表导入、多仓）没有可编辑的内容，弹窗里也就不显示该区块。
      const editConfigLoading = ref(false);
      // 读取托管原文失败时把原因留在弹窗里：否则用户面对一个空白 textarea，
      // 改完保存要么发空内容被拒，要么把原文整个覆盖掉。
      const editConfigError = ref("");
      // 内容改动的实时校验结果：{ok, msg}。用导入同一个 dryRun 接口，
      // 让「改完能不能解析成单仓」当场可见，而不是等点保存才失败。
      const editParsed = ref(null);
      const editContentDirty = ref(false);
      let parseTimer = null;
      function isLocalSource(s) { return !!s && s.type === "local"; }

      async function openEdit(s) {
        const local = isLocalSource(s);
        editConfigError.value = "";
        editParsed.value = null;
        editContentDirty.value = false;
        editSrc.value = {
          ...s, _id: s.id, forceType: s.forceType || "",
          _isLocal: local, _origUrl: s.url, _sites: (s.detail && s.detail.sites) || 0,
          content: "",
        };
        if (!local) return;
        await loadEditContent(s.id);
      }

      async function loadEditContent(id) {
        editConfigLoading.value = true;
        editConfigError.value = "";
        try {
          const d = await api("GET", "/api/sources/" + id + "/config");
          if (editSrc.value && editSrc.value._id === id) {
            editSrc.value.content = d.content || "";
            editSrc.value._sites = (d.detail && d.detail.sites) || 0;
            editParsed.value = null;
            editContentDirty.value = false;
          }
        } catch (err) {
          editConfigError.value = err.message;
        } finally {
          editConfigLoading.value = false;
        }
      }
      function retryLoadConfig() {
        if (editSrc.value) loadEditContent(editSrc.value._id);
      }

      function onContentInput() {
        editContentDirty.value = true;
        editParsed.value = null;
        clearTimeout(parseTimer);
        const text = (editSrc.value && editSrc.value.content) || "";
        if (!text.trim()) return;
        parseTimer = setTimeout(async () => {
          const cur = editSrc.value;
          if (!cur) return;
          try {
            const d = await api("POST", "/api/sources/import/json", { text, dryRun: true });
            if (editSrc.value !== cur) return; // 期间换了源，结果作废
            if (d.kind !== "repo-single") {
              editParsed.value = { ok: false, msg: d.kind === "repo-multi"
                ? "识别为多仓配置：本地源只能托管单仓"
                : "不是单仓配置，保存会被拒绝" };
            } else {
              editParsed.value = { ok: true, msg: d.summary };
            }
          } catch (e) {
            editParsed.value = { ok: false, msg: e.message };
          }
        }, 700);
      }

      // 格式化：把压成一行的 JSON 展开成缩进格式，方便手改。
      // 宽松 JSON（注释/尾逗号/缺引号）过不了 JSON.parse，提示走保存让服务端修。
      function formatContent() {
        const t = (editSrc.value && editSrc.value.content) || "";
        if (!t.trim()) return;
        try {
          editSrc.value.content = JSON.stringify(JSON.parse(t), null, 2);
          onContentInput();
          toast("已格式化", "ok");
        } catch (e) {
          toast("不是标准 JSON（可能含注释/尾逗号/缺引号），直接点保存由服务端自动修复", "error");
        }
      }
      // 手动定型：某些地址探活一直失败/内容抓不全，系统分不清单仓多仓就不下发。
      // 用户确认了是什么，就在这里钉住，不必等探活成功。
      function forceTypeHint(e0) {
        if (!e0 || !e0.forceType) return "";
        return e0.forceType === "single"
          ? "已强制按单仓处理：直接用这条地址生成 1 条仓源"
          : "已强制按多仓处理：需要探活成功解析出子仓地址才会展开";
      }
      async function saveEdit() {
        const e0 = editSrc.value;
        const body = {
          name: e0.name, url: e0.url, group: e0.group, remark: e0.remark, sort: Number(e0.sort) || 0,
          timeoutSeconds: Math.max(0, Math.min(120, Number(e0.timeoutSeconds) || 0)),
          forceType: e0.forceType || "",
        };
        let contentChanged = false;
        if (e0._isLocal) {
          if (e0.url !== e0._origUrl) {
            // 换地址 = 脱离本地托管，旧托管文件会被清理，不可恢复
            if (!confirm("换地址后这条源会变成普通地址源，原托管配置将被删除且无法恢复。继续吗？")) return;
          } else if (editContentDirty.value && !editConfigError.value) {
            // 内容没动就不提交，省得白写一次文件
            body.content = e0.content;
            contentChanged = true;
          }
        }
        try {
          const d = await api("PUT", "/api/sources/" + e0._id, body);
          const det = (d && d.detail) || null;
          if (contentChanged) {
            toast(det && det.sites
              ? `配置已更新（${det.sites} 站点 / ${det.lives} 直播），托管地址不变`
              : "配置已更新，托管地址不变", "ok");
          } else {
            toast(det && det.sites ? `已保存（${det.sites} 站点 / ${det.lives} 直播）` : "已保存", "ok");
          }
          editSrc.value = null; loadSources(); loadGroups(); loadStats();
        } catch (err) { toast(err.message, "error"); }
      }

      /* 预览弹窗 */
      const preview = ref(null); // {loading, error, data, id, name}
      async function openPreview(s) {
        preview.value = { loading: true, name: s.name, id: s.id, data: null, error: "" };
        try {
          preview.value.data = await api("GET", `/api/sources/${s.id}/preview`);
          preview.value.loading = false;
        } catch (e) {
          preview.value.loading = false;
          preview.value.error = e.message;
        }
      }
      function retryPreview() {
        if (preview.value && preview.value.id) {
          openPreview({ id: preview.value.id, name: preview.value.name });
        } else if (preview.value && preview.value.url) {
          openFlatPreview({ name: preview.value.name, url: preview.value.url });
        }
      }
      async function batchCheck() {
        const ids = [...checked.value];
        for (const id of ids) {
          try { await api("POST", "/api/sources/" + id + "/check"); } catch (e) { /* 单条失败继续 */ }
        }
        toast(`已发起 ${ids.length} 条检测`, "ok");
        checked.value = [];
        watchChecking();
      }
      const siteNamesExpanded = ref(false);
      const rawExpanded = ref(false);

      // 复制到剪贴板：三步走，任一步成功就提示「已复制」。
      //  1) Clipboard API —— 只在安全上下文（https / localhost）可用
      //  2) execCommand('copy') 兜底 —— 用 http + IP 访问时只能靠它
      //  3) 都失败 → 弹出手动复制框，内容已全选，按 Ctrl+C 即可
      // 老实现的坑：① navigator.clipboard 在 http+IP 下是 undefined，直接抛
      // TypeError 连兜底都不执行（点了完全没反应，也没有提示）；② 兜底分支
      // 不看 execCommand 的返回值，失败照样提示「已复制」（说复制了其实没复制）。
      const manualCopy = ref("");
      function legacyCopy(text) {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        // 留在视口内、透明度 0：iOS/Safari 对 display:none 或移出视口的元素拒绝选中
        ta.style.position = "fixed";
        ta.style.top = "0";
        ta.style.left = "-9999px";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        ta.setSelectionRange(0, text.length);
        let ok = false;
        try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
        ta.remove();
        return ok;
      }
      async function copyText(text) {
        const s = text == null ? "" : String(text);
        if (!s) { toast("没有可复制的内容", "warn"); return; }
        if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
          try {
            await navigator.clipboard.writeText(s);
            toast("已复制", "ok");
            return;
          } catch (e) { /* 权限被拒 / 文档失焦 → 走兜底 */ }
        }
        if (legacyCopy(s)) { toast("已复制", "ok"); return; }
        manualCopy.value = s; // 最后兜底：给个全选的框让用户自己按 Ctrl+C
        toast("浏览器拦住了自动复制，请在弹出的框里按 Ctrl+C", "warn");
      }
      function closeManualCopy() { manualCopy.value = ""; }
      // 手动复制兜底框弹出后自动聚焦并全选，用户按 Ctrl+C / ⌘C 即可
      watch(manualCopy, (v) => {
        if (!v) return;
        nextTick(() => {
          const el = document.getElementById("manual-copy-ta");
          if (el) { el.focus(); el.select(); }
        });
      });
      // 列表「更新于 HH:MM:SS」：让人一眼看出数据是不是新鲜的
      function clockTime(ts) {
        if (!ts) return "";
        const d = new Date(ts);
        const p = n => String(n).padStart(2, "0");
        return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
      }

      /* ---- 站点源（平铺层，订阅数据源） ---- */
      const flatList = ref([]);
      const flatsTotal = ref(0);
      const flatFilters = reactive({ keyword: "", status: "", group: "", enabled: "", page: 1, size: 50, sort: "", sort_by: "", sort_dir: "" });
      const flatGroups = ref([]);
      const flatChecked = ref([]);
      let flatSearchDebounce = null;
      let flatReqSeq = 0; // 请求序号：同 loadSources，防止旧响应覆盖新页内容
      let flatInflight = null; // 参数相同的并发请求合并成一次（{qs, p}）
      const refreshingFlats = ref(false);
      const flatsSyncedAt = ref(0);

      async function loadFlats(force = false) {
        const q = new URLSearchParams();
        Object.keys(flatFilters).forEach(k => { if (flatFilters[k] !== "" && flatFilters[k] != null) q.set(k, flatFilters[k]); });
        const qs = q.toString();
        // 同一次操作可能同时触发「筛选变化」和「切换视图」两个 watch，
        // 参数一样就直接等前一个的结果，不再发一遍（force 时不吃这个合并）。
        if (!force && flatInflight && flatInflight.qs === qs) return flatInflight.p;
        const p = (async () => {
          const seq = ++flatReqSeq;
          try {
            const data = await api("GET", "/api/flats?" + qs);
            if (seq !== flatReqSeq) return; // 已有更新的请求在途，丢弃本次旧响应
            flatList.value = data.list || [];
            flatsTotal.value = data.total || 0;
            flatsSyncedAt.value = Date.now();
            flatChecked.value = flatChecked.value.filter(id => flatList.value.some(f => f.id === id));
            // 数据变少导致当前页越界时，自动回到最后一页重载
            const maxPage = Math.max(1, Math.ceil(flatsTotal.value / flatFilters.size));
            if (!flatList.value.length && flatFilters.page > maxPage) {
              flatFilters.page = maxPage;
              loadFlats();
            }
          } catch (e) { if (seq === flatReqSeq) toast(e.message, "error"); }
        })();
        flatInflight = { qs, p };
        try { await p; } finally { if (flatInflight && flatInflight.p === p) flatInflight = null; }
      }
      async function refreshFlats() {
        if (refreshingFlats.value) return;
        refreshingFlats.value = true;
        try { await Promise.all([loadFlats(true), loadStats(), loadFlatGroups()]); }
        finally { refreshingFlats.value = false; }
      }
      async function loadFlatGroups() {
        try { flatGroups.value = await api("GET", "/api/flats/groups"); } catch (e) { /* 忽略 */ }
      }
      // 概览「订阅输出 → 失效」卡片：切到仓源页并筛出失效。
      // 筛选项先摆好再切视图，watch(view) 加载时直接就是筛选后的结果；
      // 顺手清掉其它条件，避免点进来发现「空列表」其实是被上次的搜索词挡住了。
      // ---- 概览卡片下钻：把筛选条件摆好再切视图，watch(view) 加载时就是筛选后的结果 ----
      // 无关条件一并清掉，避免「点进来是空列表、其实被上次的搜索词挡住」。
      function resetSourceFilters() {
        filters.keyword = "";
        filters.type = "";
        filters.group = "";
        filters.notInSub = "";
        filters.sort = "";
        filters.sort_by = "";
        filters.sort_dir = "";
        filters.page = 1;
      }
      function resetFlatFilters() {
        flatFilters.keyword = "";
        flatFilters.group = "";
        flatFilters.sort = "";
        flatFilters.sort_by = "";
        flatFilters.sort_dir = "";
        flatFilters.page = 1;
      }
      // status="" 表示全部；enabled 传 "true"/"false" 对应「仅启用/仅禁用」
      function gotoSource(status, enabled) {
        resetSourceFilters();
        filters.status = status || "";
        filters.enabled = enabled || "";
        view.value = "sources";
      }
      // 概览「未进订阅」下钻：只看启用了却没有可用仓源的源
      function gotoNotInSub() {
        resetSourceFilters();
        filters.status = "";
        filters.enabled = "true";
        filters.notInSub = "true";
        view.value = "sources";
      }
      function gotoFlat(status, enabled) {
        resetFlatFilters();
        flatFilters.status = status || "";
        flatFilters.enabled = enabled || "";
        view.value = "flats";
      }
      function applyFlatFilter() {
        flatFilters.page = 1;
        if (view.value === "flats") loadFlats();
      }
      watch(() => flatFilters.keyword, () => {
        clearTimeout(flatSearchDebounce);
        flatSearchDebounce = setTimeout(applyFlatFilter, 300);
      });
      watch(() => [flatFilters.status, flatFilters.group, flatFilters.enabled, flatFilters.sort], applyFlatFilter);
      // 选了下拉排序（失效置顶）时清掉表头排序，避免两者叠加
      watch(() => flatFilters.sort, (v) => { if (v) { flatFilters.sort_by = ""; flatFilters.sort_dir = ""; } });
      // ---- 表头排序：点击升序 -> 再点降序 -> 三点恢复默认 ----
      function flatSortArrow(key) { return flatFilters.sort_by === key ? (flatFilters.sort_dir === "asc" ? "▲" : "▼") : ""; }
      function toggleFlatSort(key) {
        if (flatFilters.sort_by === key) {
          if (flatFilters.sort_dir === "asc") { flatFilters.sort_dir = "desc"; }
          else { flatFilters.sort_by = ""; flatFilters.sort_dir = ""; }
        } else { flatFilters.sort_by = key; flatFilters.sort_dir = "asc"; }
        applyFlatFilter();
      }
      function flatPages() { return Math.max(1, Math.ceil(flatsTotal.value / flatFilters.size)); }

      function flatStatusText(f) {
        if (!f.effectiveEnabled) return f.enabled ? "父源禁用" : "已禁用";
        return { ok: "可用", fail: "不可用", checking: "检测中" }[f.status] || "未检测";
      }
      function flatStatusClass(f) {
        if (!f.effectiveEnabled) return "off";
        return f.status || "";
      }
      async function toggleFlat(f) {
        try {
          await api("POST", "/api/flats/" + f.id + "/toggle");
          f.enabled = !f.enabled;
          // 生效状态还取决于父源是否启用，只能回查；否则状态列会停在旧文字
          loadFlats();
        }
        catch (e) { toast(e.message, "error"); }
      }
      async function checkFlat(f) {
        try {
          await api("POST", "/api/flats/" + f.id + "/check");
          f.status = "checking";
          toast("正在检测 " + f.name, "info");
          setTimeout(loadFlats, 2500);
        } catch (e) { toast(e.message, "error"); }
      }
      function delFlat(f) {
        if (!confirm(`确认删除站点源「${f.name}」？\n删除后不会再出现在订阅里，父源再次同步也不会把它加回来（可在数据里手动恢复）。`)) return;
        api("DELETE", "/api/flats/" + f.id).then(() => {
          toast("已删除", "ok"); loadFlats(); loadFlatGroups();
        }).catch(e => toast(e.message, "error"));
      }
      const flatBatchBar = computed(() => flatChecked.value.length > 0);
      async function flatBatchAction(action) {
        let group = "";
        if (action === "group") {
          group = prompt("移入分组名称：", "新分组");
          if (!group) return;
        }
        if (action === "delete" && !confirm(`确认删除选中的 ${flatChecked.value.length} 个站点源？`)) return;
        const ids = [...flatChecked.value];
        const idSet = new Set(ids);
        try {
          const r = await api("POST", "/api/flats/batch-action", { ids, action, group });
          const label = { enable: "启用", disable: "禁用", group: "移组", delete: "删除" }[action] || action;
          toast(`批量${label}完成：${r.affected} 条`, "ok");
          // 先本地同步刷新开关状态（不等接口回查，避免视觉延迟）
          if (action === "enable" || action === "disable") {
            const want = action === "enable";
            flatList.value = flatList.value.map(f => idSet.has(f.id) ? { ...f, enabled: want, effectiveEnabled: want && (f.parentNames || []).length > 0 } : f);
          } else if (action === "group") {
            flatList.value = flatList.value.map(f => idSet.has(f.id) ? { ...f, group } : f);
          }
          flatChecked.value = [];
          loadFlats(); loadFlatGroups();
        } catch (e) { toast(e.message, "error"); }
      }
      async function batchCheckFlats() {
        const ids = [...flatChecked.value];
        if (!ids.length) { toast("先勾选要检测的站点源", "warn"); return; }
        try {
          // 一次请求完成全部检测（后端并发受限 + 攒批落盘），不再逐条打 N 个接口
          const r = await api("POST", "/api/flats/batch-check", { ids });
          toast(`已发起 ${r.count} 条检测`, "ok");
          flatChecked.value = [];
          flatList.value.forEach(f => { if (ids.includes(f.id)) f.status = "checking"; });
          setTimeout(loadFlats, 3000);
        } catch (e) { toast(e.message, "error"); }
      }
      const editFlat = ref(null);
      function openFlatEdit(f) { editFlat.value = { ...f, _id: f.id }; }
      async function saveFlatEdit() {
        const f = editFlat.value;
        try {
          await api("PUT", "/api/flats/" + f._id, {
            name: f.name, group: f.group, remark: f.remark, sort: Number(f.sort) || 0,
            timeoutSeconds: Math.max(0, Math.min(120, Number(f.timeoutSeconds) || 0)),
          });
          toast("已保存", "ok"); editFlat.value = null; loadFlats(); loadFlatGroups();
        } catch (err) { toast(err.message, "error"); }
      }
      function openFlatPreview(f) {
        preview.value = { loading: true, name: f.name, id: "", url: f.url, data: null, error: "" };
        api("GET", "/api/preview?url=" + encodeURIComponent(f.url)).then(data => {
          preview.value.data = data; preview.value.loading = false;
        }).catch(e => {
          preview.value.loading = false; preview.value.error = e.message;
        });
      }

      /* ---- 添加源（弹窗） ---- */
      const showAdd = ref(false);
      const addTab = ref("single");
      // 全量源 URL 集合：去重预检用（列表是分页的，不能用当前页判断重复）
      const knownUrls = ref(new Set());
      async function loadKnownUrls() {
        try {
          const all = await api("GET", "/api/sources?size=500");
          knownUrls.value = new Set((all.list || []).map(s => s.url));
        } catch (e) { /* 静默 */ }
      }
      function openAddModal() {
        addTab.value = "single";
        loadGroups(); loadKnownUrls();
        showAdd.value = true;
      }
      const singleForm = reactive({ name: "", url: "", group: "", remark: "", enabled: true });
      const singlePreview = ref(null);
      const singlePreviewing = ref(false);
      async function previewUrl() {
        if (!singleForm.url.trim()) return;
        singlePreviewing.value = true;
        singlePreview.value = null;
        try {
          singlePreview.value = await api("GET", "/api/preview?url=" + encodeURIComponent(singleForm.url.trim()));
        } catch (e) { singlePreview.value = { errMsg: e.message }; }
        singlePreviewing.value = false;
      }
      async function addSingle() {
        if (!singleForm.url.trim()) { toast("地址不能为空", "warn"); return; }
        try {
          await api("POST", "/api/sources", { ...singleForm });
          toast("已添加，正在后台探活", "ok");
          singleForm.name = ""; singleForm.url = ""; singleForm.remark = "";
          singlePreview.value = null;
          showAdd.value = false;
          loadSources(); loadGroups(); loadKnownUrls();
        } catch (e) { toast(e.message, "error"); }
      }

      const batchText = ref("");
      const batchResult = ref(null);
      const batchParsed = computed(() => {
        const lines = batchText.value.split(/\r?\n/).map(parseLine).filter(Boolean);
        let dup = 0;
        lines.forEach(l => { if (knownUrls.value.has(l.url)) dup++; });
        return { count: lines.length, dup };
      });
      async function submitBatch() {
        const lines = batchText.value.split(/\r?\n/).map(parseLine).filter(Boolean);
        if (!lines.length) { toast("未识别到有效行", "warn"); return; }
        try {
          batchResult.value = await api("POST", "/api/sources/batch", {
            urls: lines.map(l => l.name ? l.name + "," + l.url : l.url),
            group: singleForm.group || "默认",
          });
          toast(`已添加 ${batchResult.value.added} 条，正在后台探活（确认是单仓/多仓后才进订阅）`, "ok");
          batchText.value = "";
          loadSources(); loadGroups(); loadKnownUrls();
        } catch (e) { toast(e.message, "error"); }
      }

      /* 文件导入：前端解析预览 + 勾选提交 */
      const fileRows = ref([]);
      const fileResult = ref(null);
      function handleFile(ev) {
        const f = ev.target.files && ev.target.files[0];
        if (!f) return;
        fileResult.value = null;
        const rd = new FileReader();
        rd.onload = () => {
          const text = String(rd.result || "");
          const rows = [];
          const known = knownUrls.value;
          const addRow = (name, url) => rows.push({ name: name || "", url, dup: known.has(url), checked: !known.has(url) });
          if (/\.json$/i.test(f.name)) {
            try {
              const j = JSON.parse(text);
              const arr = Array.isArray(j) ? j : (j.urls || []);
              arr.forEach(x => {
                const url = typeof x === "string" ? x : x.url;
                if (url) addRow(typeof x === "object" ? x.name : "", url);
              });
              if (!rows.length) toast("JSON 中未找到 url 条目", "warn");
            } catch (e) { toast("JSON 解析失败：" + e.message, "error"); }
          } else {
            text.split(/\r?\n/).forEach(line => {
              const l = parseLine(line);
              if (l) addRow(l.name, l.url);
            });
          }
          fileRows.value = rows;
        };
        rd.readAsText(f);
        ev.target.value = "";
      }
      async function submitFileRows() {
        const sel = fileRows.value.filter(r => r.checked);
        if (!sel.length) { toast("未勾选任何行", "warn"); return; }
        try {
          fileResult.value = await api("POST", "/api/sources/batch", {
            urls: sel.map(r => r.name ? r.name + "," + r.url : r.url),
            group: singleForm.group || "默认",
          });
          toast(`已导入 ${fileResult.value.added} 条，正在后台探活（确认是单仓/多仓后才进订阅）`, "ok");
          fileRows.value = [];
          loadSources(); loadGroups(); loadKnownUrls();
        } catch (e) { toast(e.message, "error"); }
      }

      /* JSON 粘贴导入：解析规则统一放后端（ParseSourceJSON），前端只负责预览与提交，
         避免两边各写一套识别逻辑后越走越偏。 */
      const jsonText = ref("");
      const jsonParsed = ref(null);
      const jsonParsing = ref(false);
      const jsonResult = ref(null);
      let jsonTimer = 0;
      let jsonSeq = 0;
      async function previewJsonImport() {
        const text = jsonText.value;
        if (!text.trim()) { jsonParsed.value = null; jsonParsing.value = false; return; }
        const seq = ++jsonSeq; // 连打时只认最后一次响应
        jsonParsing.value = true;
        try {
          const r = await api("POST", "/api/sources/import/json", { text, dryRun: true });
          if (seq !== jsonSeq) return;
          jsonParsed.value = { ...r, errMsg: "" };
          jsonParsing.value = false;
        } catch (e) {
          if (seq !== jsonSeq) return;
          jsonParsed.value = { errMsg: e.message, entries: [], total: 0, dup: 0, kind: "" };
          jsonParsing.value = false;
        }
      }
      watch(jsonText, (v) => {
        // 只有「用户又粘了新内容」才作废旧结果；导入成功后主动清空输入框不能把结果一起清掉
        if (v.trim()) jsonResult.value = null;
        clearTimeout(jsonTimer);
        if (!v.trim()) { jsonParsed.value = null; jsonParsing.value = false; return; }
        jsonParsing.value = true;
        jsonTimer = setTimeout(previewJsonImport, 400);
      });
      const jsonRows = computed(() => {
        const p = jsonParsed.value;
        if (!p || !p.entries) return [];
        return p.entries.slice(0, 200);
      });
      async function submitJsonImport() {
        const text = jsonText.value;
        if (!text.trim()) { toast("请先粘贴 JSON 内容", "warn"); return; }
        try {
          jsonResult.value = await api("POST", "/api/sources/import/json", {
            text, group: singleForm.group || "默认", enabled: true,
          });
          if (jsonResult.value.selfHosted) {
            toast(`已托管为本地源（${jsonResult.value.summary || "单仓配置"}），正在后台探活`, "ok");
          } else {
            toast(`已导入 ${jsonResult.value.added} 条，正在后台探活（确认是单仓/多仓后才进订阅）`, "ok");
          }
          jsonText.value = "";
          jsonParsed.value = null;
          loadSources(); loadGroups(); loadKnownUrls();
        } catch (e) { toast(e.message, "error"); }
      }

      /* ---- 设置 ---- */
      const config = ref(null);
      const subUrls = computed(() => {
        if (!config.value) return [];
        const base = location.origin;
        const p = config.value.subPath || "/sub";
        return [
          { label: "订阅地址（所有仓源列表）", url: base + p },
          { label: "饭太硬风格", url: base + p + "?format=fan" },
          { label: "纯文本", url: base + p + "/txt" },
          { label: "纯文本（带名称）", url: base + p + "/txt?name=" },
          { label: "带前缀示例", url: base + p + "?name=客厅" },
        ];
      });
      async function loadConfig() {
        try { config.value = (await api("GET", "/api/config")).config; }
        catch (e) { toast(e.message, "error"); }
      }
      async function saveConfig() {
        const c = config.value;
        try {
          await api("PUT", "/api/config", {
            port: Number(c.port), subPath: c.subPath,
            checkIntervalMinutes: Number(c.checkIntervalMinutes),
            checkTimeoutSeconds: Number(c.checkTimeoutSeconds),
            checkConcurrency: Number(c.checkConcurrency),
            startupCheck: !!c.startupCheck,
            autoDisableAfterFails: Number(c.autoDisableAfterFails) || 0,
            insecureSkipVerify: !!c.insecureSkipVerify,
            checkSubUrls: !!c.checkSubUrls,
          });
          toast("配置已保存（探活设置立即生效）", "ok");
        } catch (e) { toast(e.message, "error"); }
      }
      /* ---- 令牌修改：后端鉴权每次请求都读最新配置，改完立即生效，无需重启 ---- */
      const newToken = ref("");
      async function changeToken() {
        const t = newToken.value.trim();
        if (!t) { toast("请输入新令牌", "warn"); return; }
        if (t.length < 6 || t.length > 64) { toast("令牌长度需在 6-64 位之间", "warn"); return; }
        if (t === store.token) { toast("新令牌与当前令牌相同", "warn"); return; }
        if (!confirm("修改令牌后，其他已登录的浏览器需要重新登录。确定修改？")) return;
        try {
          const c = config.value;
          await api("PUT", "/api/config", {
            port: Number(c.port), subPath: c.subPath,
            checkIntervalMinutes: Number(c.checkIntervalMinutes),
            checkTimeoutSeconds: Number(c.checkTimeoutSeconds),
            checkConcurrency: Number(c.checkConcurrency),
            startupCheck: !!c.startupCheck,
            autoDisableAfterFails: Number(c.autoDisableAfterFails) || 0,
            insecureSkipVerify: !!c.insecureSkipVerify,
            checkSubUrls: !!c.checkSubUrls,
            token: t, // 空 / "**" 后端都会保持原令牌，这里一定带新值
          });
          store.token = t; // 当前会话直接切到新令牌，不用重新登录
          authRequired.value = true; // 已设置令牌，鉴权随即开启
          newToken.value = "";
          toast("令牌已修改，立即生效", "ok");
        } catch (e) { toast(e.message, "error"); }
      }
      /* ---- 备份导出（弹窗展示，避免预览环境拦截下载） ---- */
      const backupJson = ref(null);
      async function openBackupModal() {
        try {
          const data = await api("GET", "/api/export");
          backupJson.value = JSON.stringify(data, null, 2);
        } catch (e) { toast(e.message, "error"); }
      }
      async function copyBackup() {
        // 统一走 copyText：http+IP 访问时 navigator.clipboard 根本不存在，只能靠兜底
        await copyText(backupJson.value || "");
      }
      function downloadBackup() {
        const blob = new Blob([backupJson.value || ""], { type: "application/json" });
        const a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = "tvbox-manager-backup-" + new Date().toISOString().slice(0, 10) + ".json";
        document.body.appendChild(a);
        a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(a.href), 3000);
        toast("已发起下载（若无响应请直接复制内容）", "ok");
      }
      function importBackup() {
        const inp = document.createElement("input");
        inp.type = "file"; inp.accept = ".json";
        inp.onchange = () => {
          const f = inp.files[0]; if (!f) return;
          const rd = new FileReader();
          rd.onload = () => {
            let j;
            try { j = JSON.parse(String(rd.result)); } catch (e) { toast("文件不是合法 JSON", "error"); return; }
            const mode = confirm("点「确定」= merge 合并导入\n点「取消」= replace 完全替换") ? "merge" : "replace";
            if (mode === "replace" && !confirm("完全替换将清空现有全部源，确认？")) return;
            api("POST", "/api/import/config", { mode, data: j }).then(r => {
              toast(`导入完成：新增 ${r.added ?? r.replaced} 条`, "ok");
              loadStats(); loadSources(); loadGroups(); loadConfig();
            }).catch(e => toast(e.message, "error"));
          };
          rd.readAsText(f);
        };
        inp.click();
      }

      /* ---- 登录 ---- */
      function login() {
        const t = tokenInput.value.trim();
        if (authRequired.value && !t) { toast("请输入访问令牌（首次启动时打印在服务日志中）", "warn"); return; }
        store.token = t; // 无鉴权模式下允许为空
        needLogin.value = false;
        boot(true);
      }
      function logout() {
        store.token = "";
        needLogin.value = true;
        toast("已退出登录", "info");
      }
      function openTv() {
        window.open("/tv/", "_blank");
      }

      /* ---- 生命周期 ---- */
      // authRequired：后台是否设置了令牌。false = 无鉴权模式（/api/* 不校验），前端提示尽快设置。
      const authRequired = ref(true);
      async function checkAuthMode() {
        try {
          const h = await fetch("/healthz").then(r => r.json());
          authRequired.value = !!h.tokenRequired;
        } catch (e) { authRequired.value = true; } // 探测失败按需要鉴权处理，宁可多要一次令牌
      }
      async function boot(force) {
        await checkAuthMode();
        if (authRequired.value && !store.token) { needLogin.value = true; return; }
        needLogin.value = false;
        // 落地页是概览，看不到仓源列表：等真正切过去时 watch(view) 再加载，
        // 省掉每次进后台都白拉一页站点源。
        loadStats(); loadSources(); loadGroups(); loadConfig();
        // 只在检测真的在跑时续上进度轮询，否则一次都不打
        loadProgress().then(() => { if (progress.value.running) startPolling(); });
      }
      onMounted(boot);
      watch(view, (v) => {
        if (v === "dashboard") {
          loadStats();
          loadProgress().then(() => { if (progress.value.running) startPolling(); });
        }
        if (v === "sources") { loadSources(); loadGroups(); }
        if (v === "flats") { loadFlats(); loadFlatGroups(); }
        if (v === "settings") loadConfig();
      });
      // api() 的 401 门控指向本组件
      loginGate = needLogin;
      return {
        view, needLogin, tokenInput, toasts, toast, relTime, absTime, fmtBytes, copyText,
        stats, progress, groupDist, runCheckAll, loadStats,
        list, total, filters, groups, checked, summary, pages, loadSources, loadGroups, applyFilter,
        refreshSources, refreshingSources, sourcesSyncedAt,
        sortArrow, toggleSort,
        toggleSource, checkOne, delSource,
        flatList, flatsTotal, flatFilters, flatGroups, flatChecked, flatPages, loadFlats, loadFlatGroups, applyFlatFilter, gotoSource, gotoFlat,
        refreshFlats, refreshingFlats, flatsSyncedAt,
        manualCopy, closeManualCopy, clockTime,
        gotoNotInSub, subHint, restoreSource, restoreAllMissing, forceTypeHint,
        flatSortArrow, toggleFlatSort,
        flatStatusText, flatStatusClass, toggleFlat, checkFlat, delFlat, flatBatchAction, batchCheckFlats,
        editFlat, openFlatEdit, saveFlatEdit, openFlatPreview,
        batchBar, batchAction, editSrc, openEdit, saveEdit, editConfigLoading,
        editConfigError, editParsed, editContentDirty, onContentInput, formatContent, retryLoadConfig,
        preview, openPreview, retryPreview, siteNamesExpanded, rawExpanded, batchCheck,
        addTab, singleForm, singlePreview, singlePreviewing, previewUrl, addSingle,
        batchText, batchResult, batchParsed, submitBatch,
        jsonText, jsonParsed, jsonParsing, jsonResult, jsonRows, submitJsonImport,
        showAdd, openAddModal, knownUrls,
        fileRows, fileResult, handleFile, submitFileRows,
        config, subUrls, loadConfig, saveConfig, newToken, changeToken, authRequired,
        backupJson, openBackupModal, copyBackup, downloadBackup, importBackup,
        login, logout, openTv,
      };
    },
  });

  let toastSeq = 0;

  // 开关组件
  app.component("switch-box", {
    props: { on: { type: Boolean, default: false } },
    emits: ["toggle"],
    template: `<span class="switch" :class="{on: on}" @click="$emit('toggle')"><span class="knob"></span></span>`,
  });

  app.mount("#app");
})();
