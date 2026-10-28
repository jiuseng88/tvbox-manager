/* tvbox-manager 管理后台（Vue3 内嵌版，无构建步骤） */
(function () {
  "use strict";

  const { createApp, ref, reactive, computed, onMounted, watch } = Vue;

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
          loadSources();
          loadFlats();
        }, 2000);
      }
      function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }
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
      const filters = reactive({ keyword: "", type: "", status: "", group: "", enabled: "", page: 1, size: 50, sort: "", sort_by: "", sort_dir: "" });
      const groups = ref([]);
      const checked = ref([]);
      let searchDebounce = null;
      let sourceReqSeq = 0; // 请求序号：翻页/轮询并发时只采纳最新响应，防止旧响应覆盖新页内容
      let sourceInflight = null; // 参数相同的并发请求合并成一次（{qs, p}）

      async function loadSources() {
        const q = new URLSearchParams();
        Object.keys(filters).forEach(k => { if (filters[k] !== "" && filters[k] != null) q.set(k, filters[k]); });
        const qs = q.toString();
        // 同一次操作可能同时触发「筛选变化」和「切换视图」两个 watch，参数一样就等前一个的结果
        if (sourceInflight && sourceInflight.qs === qs) return sourceInflight.p;
        const p = (async () => {
          const seq = ++sourceReqSeq;
          try {
            const data = await api("GET", "/api/sources?" + qs);
            if (seq !== sourceReqSeq) return; // 已有更新的请求在途，丢弃本次旧响应
            list.value = data.list || [];
            total.value = data.total || 0;
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
          setTimeout(loadSources, 2500);
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
      function openEdit(s) { editSrc.value = { ...s, _id: s.id }; }
      async function saveEdit() {
        const e0 = editSrc.value;
        try {
          await api("PUT", "/api/sources/" + e0._id, {
            name: e0.name, url: e0.url, group: e0.group, remark: e0.remark, sort: Number(e0.sort) || 0,
            timeoutSeconds: Math.max(0, Math.min(120, Number(e0.timeoutSeconds) || 0)),
          });
          toast("已保存", "ok"); editSrc.value = null; loadSources(); loadGroups();
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
        setTimeout(loadSources, 3000);
      }
      const siteNamesExpanded = ref(false);
      const rawExpanded = ref(false);

      function copyText(text) {
        navigator.clipboard.writeText(text).then(
          () => toast("已复制", "ok"),
          () => {
            const ta = document.createElement("textarea");
            ta.value = text; document.body.appendChild(ta); ta.select();
            document.execCommand("copy"); ta.remove(); toast("已复制", "ok");
          });
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

      async function loadFlats() {
        const q = new URLSearchParams();
        Object.keys(flatFilters).forEach(k => { if (flatFilters[k] !== "" && flatFilters[k] != null) q.set(k, flatFilters[k]); });
        const qs = q.toString();
        // 同一次操作可能同时触发「筛选变化」和「切换视图」两个 watch，
        // 参数一样就直接等前一个的结果，不再发一遍。
        if (flatInflight && flatInflight.qs === qs) return flatInflight.p;
        const p = (async () => {
          const seq = ++flatReqSeq;
          try {
            const data = await api("GET", "/api/flats?" + qs);
            if (seq !== flatReqSeq) return; // 已有更新的请求在途，丢弃本次旧响应
            flatList.value = data.list || [];
            flatsTotal.value = data.total || 0;
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
          toast(`已导入 ${jsonResult.value.added} 条，正在后台探活（确认是单仓/多仓后才进订阅）`, "ok");
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
        try { await navigator.clipboard.writeText(backupJson.value || ""); toast("已复制到剪贴板", "ok"); }
        catch (e) {
          // 剪贴板 API 不可用时降级：选中文本提示手动复制
          const ta = document.querySelector(".modal textarea");
          if (ta) { ta.focus(); ta.select(); toast("已全选，请按 Ctrl+C 复制", "info"); }
          else toast("复制失败", "error");
        }
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
        sortArrow, toggleSort,
        toggleSource, checkOne, delSource,
        flatList, flatsTotal, flatFilters, flatGroups, flatChecked, flatPages, loadFlats, loadFlatGroups, applyFlatFilter, gotoSource, gotoFlat,
        flatSortArrow, toggleFlatSort,
        flatStatusText, flatStatusClass, toggleFlat, checkFlat, delFlat, flatBatchAction, batchCheckFlats,
        editFlat, openFlatEdit, saveFlatEdit, openFlatPreview,
        batchBar, batchAction, editSrc, openEdit, saveEdit,
        preview, openPreview, retryPreview, siteNamesExpanded, rawExpanded, batchCheck,
        addTab, singleForm, singlePreview, singlePreviewing, previewUrl, addSingle,
        batchText, batchResult, batchParsed, submitBatch,
        jsonText, jsonParsed, jsonParsing, jsonResult, jsonRows, submitJsonImport,
        showAdd, openAddModal, knownUrls,
        fileRows, fileResult, handleFile, submitFileRows,
        config, subUrls, loadConfig, saveConfig, newToken, changeToken, authRequired,
        backupJson, openBackupModal, copyBackup, downloadBackup, importBackup,
        login, logout,
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
