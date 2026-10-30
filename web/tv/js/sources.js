// 动态源加载：从 tvbox-manager 后端拉取平铺层聚合出的 CMS 站点接口，
// 填充 API_SITES 并维护 selectedAPIs 的有效性。
// 必须在 app.js 的 DOMContentLoaded 初始化之前完成（app.js 会 await 本模块）。

const TV_SOURCES_URL = '/tvapi/sources';

async function loadDynamicSources() {
    let list = [];
    try {
        const resp = await fetch(TV_SOURCES_URL, { cache: 'no-store' });
        if (resp.ok) {
            const body = await resp.json();
            const data = (body && body.data) || {};
            list = Array.isArray(data.sources) ? data.sources : [];
        }
    } catch (err) {
        console.warn('源列表加载失败:', err);
    }

    list.forEach((s, i) => {
        if (!s || !s.api) return;
        API_SITES['s' + i] = {
            api: s.api,
            name: s.name || s.api,
            detail: s.detail || ''
        };
    });

    // 清理 localStorage 里已失效的源引用
    const validKeys = Object.keys(API_SITES);
    const saved = JSON.parse(localStorage.getItem('selectedAPIs') || '[]');
    const kept = saved.filter(k => validKeys.includes(k));

    if (kept.length === 0) {
        // 首次访问（或全部失效）：默认勾选前 N 个
        selectedAPIs = validKeys.slice(0, typeof DEFAULT_SELECTED_LIMIT === 'number' ? DEFAULT_SELECTED_LIMIT : 20);
    } else {
        selectedAPIs = kept;
    }
    localStorage.setItem('selectedAPIs', JSON.stringify(selectedAPIs));

    window.tvSourceCount = validKeys.length;
    return validKeys.length;
}
