// 全局常量配置（tvbox-manager 内嵌版）
// 前端底座基于 LBTV（Apache-2.0），源列表由 /tvapi/sources 动态下发，
// 所有上游 CMS 请求统一走 Go 服务端代理 /tvapi/proxy。

const PROXY_URL = '/tvapi/proxy?url=';
const SEARCH_HISTORY_KEY = 'videoSearchHistory';
const MAX_HISTORY_ITEMS = 5;

// 网站信息配置
const SITE_CONFIG = {
    name: '影视搜索',
    description: 'tvbox-manager 平铺源聚合搜索与观看',
    version: '1.0.0'
};

// API站点配置：由 js/sources.js 启动时从 /tvapi/sources 拉取后填充，
// 键名形如 s0/s1/...，与 selectedAPIs 的持久化格式保持一致。
const API_SITES = {};

// 添加聚合搜索的配置选项
const AGGREGATED_SEARCH_CONFIG = {
    enabled: true,
    timeout: 8000,
    maxResults: 10000,
    parallelRequests: true,
    showSourceBadges: true
};

// 抽象API请求配置
const API_CONFIG = {
    search: {
        path: '/api.php/provide/vod/?ac=videolist&wd=',
        headers: {
            'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36',
            'Accept': 'application/json'
        }
    },
    detail: {
        path: '/api.php/provide/vod/?ac=videolist&ids=',
        headers: {
            'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36',
            'Accept': 'application/json'
        }
    }
};

// 优化后的正则表达式模式
const M3U8_PATTERN = /\$https?:\/\/[^"'\s]+?\.m3u8/g;

// 添加自定义播放器URL
const CUSTOM_PLAYER_URL = 'player.html';

// 增加视频播放相关配置
const PLAYER_CONFIG = {
    autoplay: true,
    allowFullscreen: true,
    width: '100%',
    height: '600',
    timeout: 15000,
    filterAds: true,
    autoPlayNext: true,
    adFilteringEnabled: true,
    adFilteringStorage: 'adFilteringEnabled'
};

// 添加错误信息本地化
const ERROR_MESSAGES = {
    NETWORK_ERROR: '网络连接错误，请检查网络设置',
    TIMEOUT_ERROR: '请求超时，服务器响应时间过长',
    API_ERROR: 'API接口返回错误，请尝试更换数据源',
    PLAYER_ERROR: '播放器加载失败，请尝试其他视频源',
    UNKNOWN_ERROR: '发生未知错误，请刷新页面重试'
};

// 添加多个自定义API源的配置
const CUSTOM_API_CONFIG = {
    separator: ',',
    maxSources: 20,
    testTimeout: 5000,
    namePrefix: '自定义-',
    validateUrl: true,
    cacheResults: true,
    cacheExpiry: 5184000000,
    adultPropName: 'isAdult'
};

// 首次访问时默认勾选的源数量上限（避免上百源并发拖垮搜索）
const DEFAULT_SELECTED_LIMIT = 20;

// 成人源分组开关（动态源不带 adult 标记，恒为 true 即只显示普通资源组）
const HIDE_BUILTIN_ADULT_APIS = true;
