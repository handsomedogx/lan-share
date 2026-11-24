/* =========================================================================
   LAN Share · 前端逻辑
   纯原生 JavaScript，无框架、无构建步骤。

   结构：
     1. 小工具函数
     2. 全局状态
     3. 网络状态指示
     4. 登录态
     5. 实时传输区（WebSocket）
     6. 文件仓库（HTTP）
     7. 事件绑定与初始化

   关于 WebSocket 地址：一律根据 location 推导，不使用硬编码 host。
   这样才能同时适配「nginx 反代到 10.0.0.1」和「本地直接开 18080」两种情况。
   ========================================================================= */
'use strict';

/* ------------------------------------------------------------------ 1. 工具 */

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

/** 去掉首尾空白。 */
const trim = (s) => (s == null ? '' : String(s).trim());

/** 把字符串里的 HTML 特殊字符转义，所有用户内容都必须过一遍。 */
function escapeHTML(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/** 只在绝对必要的地方用 innerHTML 时，插入点的安全做法。 */
function setText(el, text) {
  if (el) el.textContent = text == null ? '' : String(text);
}

/** 格式化时间：今天只显示时分，其余显示月-日 时:分。 */
function fmtTime(ms) {
  if (!ms) return '—';
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return '—';
  const now = new Date();
  const hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  const sameDay = d.getFullYear() === now.getFullYear()
    && d.getMonth() === now.getMonth()
    && d.getDate() === now.getDate();
  if (sameDay) return hm;
  const md = String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
  const ymd = d.getFullYear() === now.getFullYear() ? md : d.getFullYear() + '-' + md;
  return ymd + ' ' + hm;
}

/** 剩余时间的人话描述。 */
function fmtRemain(ms) {
  if (!ms) return '';
  const diff = ms - Date.now();
  if (diff <= 0) return '已过期';
  const min = Math.floor(diff / 60000);
  if (min < 60) return min + ' 分钟后过期';
  const h = Math.floor(min / 60);
  if (h < 24) return h + ' 小时后过期';
  return Math.floor(h / 24) + ' 天后过期';
}

/** 字节数格式化（前端侧保留一份，避免多一次请求）。 */
function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 ? 0 : 1) + ' ' + units[i];
}

/** 判断字符串是否为纯链接（与后端同一条规则）。 */
function isLink(text) {
  return /^https?:\/\/[^\s<>"]+$/i.test(text);
}

/** 复制到剪贴板，兼容局域网 HTTP 下 navigator.clipboard 不可用的情况。 */
async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) { /* 落到下面的兜底 */ }

  // 兜底：execCommand 虽然已废弃，但在 http:// 页面里是唯一可靠的方式。
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, ta.value.length);
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return ok;
  } catch (_) {
    return false;
  }
}

/**
 * 「谁在滚」—— 桌面端是 .messages 自己（它是 overflow:auto 的滚动盒），
 * 窄屏（≤1000px）CSS 把它改成 overflow:visible，滚的是整个文档。
 *
 * 这里刻意用**实测**而不是拿 window.innerWidth 去比断点：断点写在 CSS 里，
 * 两边各写一份迟早对不上；而且即使桌面端，消息少到不溢出时 .messages
 * 也不是滚动容器，这时该滚的同样是文档。
 */
function scrollHost() {
  const box = $('#messages');
  const doc = document.scrollingElement || document.documentElement;
  if (!box) return doc;
  const oy = getComputedStyle(box).overflowY;
  if ((oy === 'auto' || oy === 'scroll') && box.scrollHeight > box.clientHeight) return box;
  return doc;
}

/** 当前滚动位置离「消息的底」还有多少 px（负数=已经越过）。 */
function distanceFromBottom() {
  const host = scrollHost();
  if (host === (document.scrollingElement || document.documentElement)) {
    // 整页滚动时，要盯的不是文档底部（那是文件仓库），而是实时面板的底 ——
    // 也就是输入框那一带；滚过头会把最新消息顶出屏幕。
    const pane = $('#livePane');
    return pane ? pane.getBoundingClientRect().bottom - window.innerHeight : 0;
  }
  return host.scrollHeight - host.scrollTop - host.clientHeight;
}

/** 节流的 requestAnimationFrame 包装，用于滚动到底部。 */
let scrollScheduled = false;
function scheduleScrollBottom() {
  if (scrollScheduled) return;
  scrollScheduled = true;
  requestAnimationFrame(() => {
    scrollScheduled = false;
    const host = scrollHost();
    if (host === (document.scrollingElement || document.documentElement)) {
      const pane = $('#livePane');
      if (!pane) return;
      const edge = pane.getBoundingClientRect().bottom + window.scrollY;
      window.scrollTo(0, Math.max(0, edge - window.innerHeight));
      return;
    }
    host.scrollTop = host.scrollHeight;
  });
}

/** 判断消息区是否已经在底部附近（用于决定要不要自动跟随滚动）。 */
function nearBottom(slack) {
  return distanceFromBottom() <= (slack == null ? 80 : slack);
}

/** 取名字首字符作为头像文字。 */
function initialOf(name) {
  const s = trim(name);
  if (!s) return '?';
  // 中文取第一个字，英文取首字母大写。
  const first = Array.from(s)[0];
  return /[a-zA-Z]/.test(first) ? first.toUpperCase() : first;
}

/** 依据文件名猜测扩展名，用于列表图标配色。 */
function extOf(name) {
  const m = /\.([a-zA-Z0-9]{1,8})$/.exec(name || '');
  return m ? m[1].toLowerCase() : '';
}

/* ------------------------------------------------------------------ 2. 状态 */

const state = {
  user: null,            // 当前登录用户，null 表示未登录
  files: [],             // 文件列表缓存（仓库只有永久文件，没有分类）
  folders: [],           // 文件夹列表缓存
  filesTotalSize: 0,     // 服务端给的总占用字节数
  filesTotalText: '',    // 服务端给的总体积文案，未搜索时直接用它
  fileQuery: '',         // 文件仓库的搜索词（纯前端过滤，见 visibleFiles）
  currentFolderId: null, // 当前所在文件夹 ID，null 表示根目录
  fileView: 'list',      // 文件仓库视图：'list' | 'card'
  theme: 'auto',         // 界面主题：'auto' | 'light' | 'dark'

  authMode: 'login',     // login | register
  status: null,          // /api/status 最近一次结果

  room: null,            // 当前会话码
  ws: null,              // WebSocket 实例
  wsReady: false,
  myName: '',            // 本连接的显示名 —— 服务端分配（真实客户端 IP），见 hello.self
  myId: '',              // 本连接的唯一标识 —— 判定「这条是不是我发的」只能靠它，见 hello.selfId
  reconnectTimer: null,
  reconnectDelay: 1000,
  manualLeave: false,    // 主动退出时不要自动重连
  members: [],           // 在线成员
  pendingSelf: new Set(), // 本地已乐观渲染、等服务端确认的 cid 集合
  // expiresAt = 0 表示「还没从服务端拿到存活信息」或「房间没有到期时间」。
  // 房间现在必定有时长（没有不限时选项），所以正常路径上它会很快被 hello 填上。
  expiresAt: 0,          // 房间到期时刻（Unix 毫秒）
  ttlMinutes: 0,         // 房间总存活时长（分钟），用于算剩余比例
  // 粘贴进输入框、还没点发送的图片。元素形如 { key, file, url }。
  // 放在前端而不是「粘上就传」：剪贴板里常常是误复制的内容，
  // 给一次看见缩略图再决定发不发的机会。
  attachments: []
};

const LS_CODE = 'lanshare.code';
const LS_FILE_VIEW = 'lanshare.fileView';
const LS_THEME = 'lanshare.theme';

function syncThemeButtons() {
  $$('#themeSwitch [data-theme-opt]').forEach((btn) => {
    const active = btn.dataset.themeOpt === state.theme;
    btn.classList.toggle('is-active', active);
    btn.setAttribute('aria-pressed', active ? 'true' : 'false');
  });
}

function applyTheme(mode, persist = true) {
  const next = (mode === 'light' || mode === 'dark') ? mode : 'auto';
  state.theme = next;
  const root = document.documentElement;
  if (root) {
    if (next === 'auto') {
      root.removeAttribute('data-theme');
    } else {
      root.setAttribute('data-theme', next);
    }
  }
  if (persist) {
    try {
      if (next === 'auto') localStorage.removeItem(LS_THEME);
      else localStorage.setItem(LS_THEME, next);
    } catch (_) {}
  }
  syncThemeButtons();
}

/* ------------------------------------------------------------------ 3. 提示条 */

const ICONS = {
  ok: '<svg viewBox="0 0 20 20" width="16" height="16"><path d="M4.5 10.5l3.6 3.6L15.5 6.4" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  err: '<svg viewBox="0 0 20 20" width="16" height="16"><circle cx="10" cy="10" r="7.2" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M10 6.4v4.4M10 13.4h.01" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
  info: '<svg viewBox="0 0 20 20" width="16" height="16"><circle cx="10" cy="10" r="7.2" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M10 9v4.6M10 6.6h.01" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>'
};

function toast(msg, kind) {
  const type = kind || 'info';
  const wrap = $('#toastWrap');
  if (!wrap) return;

  const el = document.createElement('div');
  el.className = 'toast is-' + type;
  el.innerHTML = '<span class="toast-icon">' + (ICONS[type] || ICONS.info) + '</span>'
    + '<span class="toast-text"></span>';
  setText($('.toast-text', el), msg);
  wrap.appendChild(el);

  setTimeout(() => {
    el.classList.add('is-out');
    setTimeout(() => el.remove(), 220);
  }, type === 'err' ? 3600 : 2400);
}

/** 底栏状态文字。 */
function setStatus(left, right) {
  if (left != null) setText($('#statusLeft'), left);
  if (right != null) setText($('#statusRight'), right);
}

/* ------------------------------------------------------------------ 4. 登录态 */

/** 渲染顶栏用户区域。 */
function renderUser() {
  const area = $('#userArea');
  if (!area) return;
  area.innerHTML = '';

  if (!state.user) {
    const btn = document.createElement('button');
    btn.className = 'btn btn-primary btn-sm';
    btn.textContent = '登录';
    btn.addEventListener('click', () => openAuth('login'));
    area.appendChild(btn);
  } else {
    const chip = document.createElement('div');
    chip.className = 'user-chip';
    chip.innerHTML = '<span class="user-avatar"></span><span class="user-name"></span>';
    setText($('.user-avatar', chip), initialOf(state.user.username));
    setText($('.user-name', chip), state.user.username);
    // 管理员在名字后面挂个小标记，一眼能看出权限不同。
    if (state.user.isAdmin) {
      const badge = document.createElement('span');
      badge.className = 'role-badge';
      badge.textContent = '管理员';
      chip.appendChild(badge);
    }
    area.appendChild(chip);

    const out = document.createElement('button');
    out.className = 'btn btn-ghost btn-sm';
    out.textContent = '登出';
    out.addEventListener('click', doLogout);
    area.appendChild(out);
  }

  // 管理入口只有管理员可见。
  const adminBtn = $('#btnAdmin');
  if (adminBtn) adminBtn.hidden = !(state.user && state.user.isAdmin);

  // 文件仓库的登录遮罩与列表都要跟着变。
  renderLock();
  loadFiles();
}

/** 打开登录/注册弹窗。mode 为 'login' | 'register'。 */
function openAuth(mode) {
  const modal = $('#loginModal');
  if (!modal) return;

  setAuthMode(mode || 'login');
  modal.hidden = false;
  setText($('#loginMsg'), '');

  const u = $('#loginUser');
  if (u) { u.value = ''; setTimeout(() => u.focus(), 30); }
  const p = $('#loginPass');
  if (p) p.value = '';
}

/** 切换登录/注册两种表单状态。 */
function setAuthMode(mode) {
  state.authMode = mode === 'register' ? 'register' : 'login';
  const isReg = state.authMode === 'register';

  $$('#authTabs .tab').forEach((t) => {
    const on = t.dataset.tab === state.authMode;
    t.classList.toggle('is-active', on);
    t.setAttribute('aria-selected', on ? 'true' : 'false');
  });

  setText($('#loginTitle'), isReg ? '注册账号' : '登录');
  setText($('#btnDoLogin'), isReg ? '注册' : '登录');
  setText($('#loginMsg'), '');

  // 注册时提示密码长度要求，并把 autocomplete 切成新密码。
  const hint = $('#passHint');
  if (hint) hint.hidden = !isReg;
  const pass = $('#loginPass');
  if (pass) pass.setAttribute('autocomplete', isReg ? 'new-password' : 'current-password');

  // 首次部署时注册文案要换，让用户知道自己即将成为管理员。
  if (isReg) {
    const first = state.status && state.status.needSetup;
    setText($('#loginSub'), first
      ? '这是第一个账号，注册后你将成为管理员'
      : '注册后即可使用文件仓库');
  } else {
    setText($('#loginSub'), '登录后即可使用长期文件仓库');
  }
}

function closeLogin() {
  const modal = $('#loginModal');
  if (modal) modal.hidden = true;
}

/** 提交登录或注册。 */
async function doLogin(e) {
  if (e) e.preventDefault();

  const username = trim($('#loginUser').value);
  const password = $('#loginPass').value;
  if (!username || !password) {
    setText($('#loginMsg'), '请输入用户名和密码');
    return;
  }

  const isReg = state.authMode === 'register';
  if (isReg && password.length < 6) {
    setText($('#loginMsg'), '密码长度至少 6 位');
    return;
  }

  const btn = $('#btnDoLogin');
  const oldLabel = btn.textContent;
  btn.disabled = true;
  btn.textContent = isReg ? '注册中…' : '登录中…';
  setText($('#loginMsg'), '');

  try {
    const res = await fetch(isReg ? '/api/auth/register' : '/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    const data = await res.json().catch(() => ({}));

    if (!res.ok) {
      setText($('#loginMsg'), data.error || (isReg ? '注册失败' : '登录失败'));
      return;
    }

    state.user = data;
    closeLogin();
    toast(isReg
      ? (data.isAdmin ? '已创建管理员账号：' + data.username : '注册成功：' + data.username)
      : '已登录：' + data.username, 'ok');
    renderUser();
    refreshStatus();
  } catch (err) {
    setText($('#loginMsg'), '网络错误，请重试');
  } finally {
    btn.disabled = false;
    btn.textContent = oldLabel;
  }
}

async function doLogout() {
  try {
    await fetch('/api/auth/logout', { method: 'POST' });
  } catch (_) { /* 登出失败也照样清本地状态 */ }
  state.user = null;
  toast('已登出', 'info');
  renderUser();
  refreshStatus();
}

/** 启动时拉一次当前登录态。 */
async function loadMe() {
  try {
    const res = await fetch('/api/auth/me');
    if (res.ok) {
      state.user = await res.json();
    } else {
      state.user = null;
    }
  } catch (_) {
    state.user = null;
  }
  renderUser();
}

/* ------------------------------------------------------------------ 5b. 部署引导 */

/** 依据 /api/status 决定是否显示「创建管理员账号」引导。 */
function renderSetup() {
  const s = state.status;
  if (!s) return;

  const needSetup = !!s.needSetup;
  const banner = $('#setupBanner');
  if (banner) banner.hidden = !needSetup;

  // 未初始化时，登录弹窗只该给注册入口（没账号可登）。
  const tabs = $('#authTabs');
  if (tabs) tabs.hidden = !(needSetup || s.registrationOpen);

  // 提示条的文案随注册开关变化。
  // 注意用词：提示条只说明「不能上传」，浏览和下载对所有人开放 ——
  // 文案不能说成「仓库需要登录」。
  // needSetup 时不需要写文案：那种情况下提示条本身是隐藏的，
  // 引导由上面的 setup-banner 承担（同一个入口不重复给两次）。
  if (!needSetup) {
    if (!s.registrationOpen) {
      setText($('#lockTitle'), '登录后可上传');
      setText($('#lockText'), '浏览和下载无需登录。登录后可以上传、管理自己的文件。注册已关闭，请联系管理员开通。');
    } else {
      setText($('#lockTitle'), '登录后可上传');
      setText($('#lockText'), '浏览和下载无需登录。登录或注册后即可上传、管理自己的文件。');
    }
    setText($('#btnLoginFromLock'), '立即登录');
  }

  // needSetup 会决定提示条显不显示，所以状态刷新后要重算一次。
  renderLock();
  renderUploadLimits();
  renderUsage();
}

/** 格式化单文件大小限制文案：0 或负数表示不限。 */
function formatLimitText(bytes) {
  const n = Number(bytes) || 0;
  return n > 0 ? ('单文件 ≤ ' + humanSize(n)) : '单文件不限';
}

/** 展示房间与文件仓库的单文件上传大小限制。 */
function renderUploadLimits() {
  const st = state.status;
  if (!st) return;

  if (typeof st.chatMaxUpload === 'number') {
    const chatText = formatLimitText(st.chatMaxUpload);
    const chatEl = $('#chatLimitHint');
    if (chatEl) {
      setText(chatEl, chatText);
      chatEl.hidden = false;
    }
    const btnChatFile = $('#btnChatFile');
    if (btnChatFile) {
      btnChatFile.title = '发送文件（' + chatText + '，也可以直接把文件拖进左侧面板）';
    }
  }

  if (typeof st.maxUpload === 'number') {
    const fileText = formatLimitText(st.maxUpload);
    const fileEl = $('#fileLimitHint');
    if (fileEl) {
      setText(fileEl, fileText);
      fileEl.hidden = false;
    }
    const btnUpload = $('#btnUpload');
    if (btnUpload) {
      btnUpload.title = '上传文件（' + fileText + '）';
    }
  }
}

/** 刷新运行状态（版本、用量、注册开关）。 */
async function refreshStatus() {
  try {
    const res = await fetch('/api/status');
    if (!res.ok) return;
    state.status = await res.json();
    renderSetup();
  } catch (_) { /* 静默：状态拉不到不影响主流程 */ }
}

/* ------------------------------------------------------------------ 5c. 管理员面板 */

function openAdmin() {
  const modal = $('#adminModal');
  if (!modal || !(state.user && state.user.isAdmin)) return;
  modal.hidden = false;
  loadAdminData();
}

function closeAdmin() {
  const modal = $('#adminModal');
  if (modal) modal.hidden = true;
}

/** 拉取注册开关与用户列表。 */
async function loadAdminData() {
  const toggle = $('#regToggle');
  const list = $('#userList');

  try {
    const [setRes, usersRes] = await Promise.all([
      fetch('/api/admin/settings'),
      fetch('/api/admin/users')
    ]);

    if (setRes.ok) {
      const s = await setRes.json();
      if (toggle) toggle.checked = !!s.registrationOpen;
    }
    if (!usersRes.ok) return;

    const data = await usersRes.json();
    const users = data.users || [];
    setText($('#userCountText'), users.length + ' 个账号');

    if (!list) return;
    list.innerHTML = '';

    users.forEach((u) => {
      const row = document.createElement('div');
      row.className = 'user-row';

      const av = document.createElement('span');
      av.className = 'mini-avatar';
      setText(av, initialOf(u.username));

      const name = document.createElement('span');
      name.className = 'uname';
      name.title = u.username;
      setText(name, u.username);

      const badge = document.createElement('span');
      badge.className = 'role-badge' + (u.isAdmin ? '' : ' is-user');
      badge.textContent = u.isAdmin ? '管理员' : '普通用户';

      row.append(av, name, badge);

      // 不允许对自己降级（避免误操作把自己踢成普通用户）。
      const isSelf = state.user && u.id === state.user.id;
      const btn = document.createElement('button');
      btn.className = 'role-btn';
      btn.disabled = isSelf;
      btn.title = isSelf ? '不能修改自己的角色' : '';
      btn.textContent = u.isAdmin ? '设为普通' : '设为管理员';
      btn.addEventListener('click', () => setRole(u, btn));
      row.appendChild(btn);

      list.appendChild(row);
    });
  } catch (_) {
    toast('读取管理员数据失败', 'err');
  }
}

async function setRole(u, btn) {
  const next = u.isAdmin ? 'user' : 'admin';
  btn.disabled = true;
  try {
    const res = await fetch('/api/admin/users/' + u.id + '/role', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ role: next })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '修改角色失败', 'err');
      return;
    }
    toast(u.username + ' 现在是' + (next === 'admin' ? '管理员' : '普通用户'), 'ok');
    loadAdminData();
  } catch (_) {
    toast('网络错误，修改角色失败', 'err');
  } finally {
    btn.disabled = false;
  }
}

async function toggleRegistration(checked) {
  try {
    const res = await fetch('/api/admin/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ registrationOpen: checked })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '保存失败', 'err');
      $('#regToggle').checked = !checked;   // 回滚
      return;
    }
    toast(checked ? '已开放注册' : '已关闭注册', 'ok');
    refreshStatus();
  } catch (_) {
    toast('网络错误，保存失败', 'err');
    $('#regToggle').checked = !checked;
  }
}

/* ------------------------------------------------------------------ 6. 实时传输区 */

/** 依据 location 推导 WebSocket 地址。 */
function wsURL(code) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return proto + '//' + location.host + '/ws/session/' + encodeURIComponent(code);
}

/** 打开「创建房间」弹窗。 */
function openCreate() {
  const modal = $('#createModal');
  if (!modal) return;
  setText($('#createMsg'), '');
  modal.hidden = false;
  const input = $('#createCode');
  if (input) {
    input.value = '';
    setTimeout(() => input.focus(), 30);
  }
}

function closeCreate() {
  const modal = $('#createModal');
  if (modal) modal.hidden = true;
}

/** 选中某个存活时长档位。 */
function pickTTL(btn) {
  $$('#ttlOptions .ttl-item').forEach((b) => {
    const on = b === btn;
    b.classList.toggle('is-active', on);
    b.setAttribute('aria-checked', on ? 'true' : 'false');
  });
}

/**
 * 读取当前选中的存活时长（分钟）。
 *
 * 兜底值刻意是 60 而不是 0：0 曾经表示「不限时」，服务端白名单现已移除它，
 * 传过去会被拒。这里必须给一个合法档位，否则「DOM 里选中的按钮意外丢失」
 * 会变成创建房间直接失败。
 */
const FALLBACK_TTL = 60;

function currentTTL() {
  const active = $('#ttlOptions .ttl-item.is-active');
  if (!active) return FALLBACK_TTL;
  const n = Number(active.dataset.ttl);
  return Number.isFinite(n) && n > 0 ? n : FALLBACK_TTL;
}

/** 提交创建房间表单。 */
async function doCreate() {
  if ($('#btnDoCreate').disabled) return;
  const btn = $('#btnDoCreate');
  const code = trim($('#createCode').value).toUpperCase();

  // 前端先做一遍格式校验，省一次往返；服务端仍会再校验一次。
  if (code && !/^[A-Z0-9]{4,8}$/.test(code)) {
    setText($('#createMsg'), '房间号需为 4 - 8 位字母或数字');
    return;
  }

  const data = await createSession(code, currentTTL(), btn, $('#createMsg'));
  if (!data) return;
  closeCreate();
  toast('房间已创建：' + data.code, 'ok');
  enterRoom(data.code, data);
}

/** 两个创建入口共用请求、错误提示和防重复提交。 */
async function createSession(code, ttlMinutes, btn, hint) {
  btn.disabled = true;
  setText(hint, '');
  try {
    const res = await fetch('/api/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code, ttlMinutes })
    });
    const data = await res.json().catch(() => ({}));
    if (res.status === 409) {
      setText(hint, '该房间号已被占用，请直接加入或换一个');
      return;
    }
    if (!res.ok) {
      setText(hint, data.error || '创建房间失败');
      return;
    }
    return data;
  } catch (_) {
    setText(hint, '网络错误，无法创建房间');
  } finally {
    btn.disabled = false;
  }
}

let joinRequest = 0;

function resetJoinFeedback() {
  joinRequest++;
  setText($('#joinHint'), '');
  const btn = $('#btnCreateMissing');
  btn.hidden = true;
  delete btn.dataset.code;
}

async function createMissingSession() {
  const btn = $('#btnCreateMissing');
  const code = btn.dataset.code;
  if (btn.hidden || btn.disabled || !code) return;
  const input = $('#joinCode');
  const joinBtn = $('#btnJoin');
  input.disabled = true;
  joinBtn.disabled = true;
  try {
    const data = await createSession(code, FALLBACK_TTL, btn, $('#joinHint'));
    if (!data) return;
    resetJoinFeedback();
    toast('房间已创建：' + data.code, 'ok');
    enterRoom(data.code, data);
  } finally {
    input.disabled = false;
    joinBtn.disabled = false;
  }
}

/** 加入会话（先查一次是否存在，给出更友好的错误）。 */
async function joinSession() {
  const btn = $('#btnJoin');
  if (btn.disabled) return;
  const code = trim($('#joinCode').value).toUpperCase();
  const hint = $('#joinHint');
  resetJoinFeedback();
  const request = joinRequest;

  if (!/^[A-Z0-9]{4,8}$/.test(code)) {
    setText(hint, '房间号为 4 - 8 位字母或数字');
    return;
  }

  btn.disabled = true;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(code));
    if (request !== joinRequest) return;
    if (res.status === 404 || res.status === 410) {
      setText(hint, '房间不存在、已过期或已结束');
      const createBtn = $('#btnCreateMissing');
      createBtn.dataset.code = code;
      createBtn.hidden = false;
      return;
    }
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      if (request !== joinRequest) return;
      setText(hint, data.error || '无法加入该房间');
      return;
    }
    enterRoom(code);
  } catch (_) {
    if (request === joinRequest) setText(hint, '网络错误，请重试');
  } finally {
    btn.disabled = false;
  }
}

/** 进入房间：更新 UI 并建立 WebSocket。 */
function enterRoom(code, info) {
  resetJoinFeedback();
  state.room = code;
  state.manualLeave = false;
  // 显示名完全由服务端分配（用客户端 IP 兜底），本地不再收集昵称。
  // 权威值在 hello.self 里回来，见 handleWSEvent。
  try { localStorage.setItem(LS_CODE, code); } catch (_) {}

  // info 只有「自己创建」时才有；从 /api/sessions 直接进来的拿到完整信息。
  // 加入别人的房间时 info 为空，等 hello 事件回来再补上倒计时。
  state.expiresAt = info && info.expiresAt ? info.expiresAt : 0;
  state.ttlMinutes = info ? (info.ttlMinutes || 0) : 0;

  // UI 切换
  $('#liveEmpty').hidden = true;
  $('#messages').hidden = false;
  $('#composer').hidden = false;
  $('#sessionBar').hidden = false;
  $('#btnLeave').hidden = false;
  $('#btnCreate').hidden = true;
  setText($('#sessionCode'), code);
  setText($('#brandSub'), '房间 ' + code);
  $('#messages').innerHTML = '';
  // 灯箱里可能是上一个房间的图，切房间时必须先关掉，
  // 否则会残留在屏幕上指向一个已经不存在的文件。
  closeLightbox();
  state.members = [];
  state.pendingSelf.clear();
  // 换房间后旧身份一律作废：显示名与连接标识都以新连接的 hello 为准。
  // 不清的话，新房间的第一帧历史回放会拿上一个房间的身份去判归属。
  state.myName = '';
  state.myId = '';
  renderMembers();
  renderTTL();

  connect();
  setTimeout(() => { const i = $('#msgInput'); if (i) i.focus(); }, 60);
}

/** 渲染存活倒计时标签。 */
function renderTTL() {
  const tag = $('#ttlTag');
  if (!tag) return;

  // 没有到期时间：正常路径不会出现（房间必定有时长），
  // 只有服务端兜底回收的无期限房间才会走到这里 —— 那时没有东西可倒计时。
  if (!state.expiresAt) {
    tag.hidden = true;
    return;
  }

  const ms = state.expiresAt - Date.now();
  if (ms <= 0) {
    setText(tag, '已过期');
    tag.className = 'ttl-tag is-danger';
    tag.hidden = false;
    return;
  }

  tag.hidden = false;
  setText(tag, '剩 ' + fmtRemain(state.expiresAt));

  // 剩余不到 10% 或不到 5 分钟时转为警示色。
  const total = state.ttlMinutes * 60000;
  const ratio = total > 0 ? ms / total : 1;
  tag.className = 'ttl-tag'
    + (ms < 5 * 60000 ? ' is-danger' : (ratio < 0.1 ? ' is-warn' : ''));
}

/** 倒计时到点：房间已被服务端销毁，本地收拾干净并告诉用户。 */
function handleRoomExpired() {
  if (!state.room) return;
  leaveRoom(true);
  toast('房间已到期，自动销毁', 'err');
  setStatus('房间已到期', '消息已清除，可重新创建');
}

/** 退出房间。 */
function leaveRoom(silent) {
  state.manualLeave = true;
  clearTimeout(state.reconnectTimer);
  if (state.ws) {
    try { state.ws.close(); } catch (_) {}
    state.ws = null;
  }
  state.wsReady = false;
  state.room = null;
  state.members = [];
  state.pendingSelf.clear();
  state.myName = '';
  state.myId = '';
  state.expiresAt = 0;
  state.ttlMinutes = 0;
  // 退出房间时丢掉待发送的图：它们只对当前房间有意义，
  // 而且对象 URL 不释放会一直占着那份内存。
  clearAttachments();
  try { localStorage.removeItem(LS_CODE); } catch (_) {}

  $('#liveEmpty').hidden = false;
  $('#messages').hidden = true;
  $('#composer').hidden = true;
  $('#sessionBar').hidden = true;
  $('#btnLeave').hidden = true;
  $('#btnCreate').hidden = false;
  const tag = $('#ttlTag');
  if (tag) tag.hidden = true;
  setText($('#brandSub'), '局域网内容传输');
  setStatus('已退出房间', '');

  if (!silent) toast('已退出房间', 'info');
}

/** 建立 WebSocket 连接。 */
function connect() {
  if (!state.room) return;

  let ws;
  try {
    ws = new WebSocket(wsURL(state.room));
  } catch (_) {
    scheduleReconnect();
    return;
  }
  state.ws = ws;

  ws.addEventListener('open', () => {
    state.wsReady = true;
    state.reconnectDelay = 1000;
    setStatus('会话 ' + state.room + ' · 已连接', '');
    syncSendButton();
  });

  ws.addEventListener('message', (ev) => {
    let data;
    try { data = JSON.parse(ev.data); } catch (_) { return; }
    handleWSEvent(data);
  });

  ws.addEventListener('close', () => {
    state.wsReady = false;
    state.ws = null;
    if (state.manualLeave || !state.room) return;
    scheduleReconnect();
  });

  ws.addEventListener('error', () => {
    // 具体错误交给 close 处理。
  });
}

function scheduleReconnect() {
  if (state.manualLeave || !state.room) return;
  clearTimeout(state.reconnectTimer);

  const delay = state.reconnectDelay;
  setStatus('连接中断，' + Math.round(delay / 1000) + ' 秒后重连…', '');
  state.reconnectTimer = setTimeout(() => {
    if (state.room && !state.manualLeave) connect();
  }, delay);

  // 指数退避，最多退到 15 秒，避免路由器重启期间疯狂重连。
  state.reconnectDelay = Math.min(state.reconnectDelay * 2, 15000);
}

/** 处理服务端下行事件。 */
function handleWSEvent(data) {
  switch (data.event) {
    case 'hello':
      state.members = data.members || [];
      // 服务端告知本连接的显示名与唯一标识。
      // myName 只用于展示与跨版本兜底；myId 才是判定消息归属的依据 ——
      // 显示名在同一 IP 的多个连接之间会重名（服务端只保证房间内不重名，
      // 会追加 " #2" 之类后缀），id 才是真正唯一的。
      if (data.self) state.myName = data.self;
      if (data.selfId) state.myId = data.selfId;
      // 房间存活信息以服务端为准：加入别人的房间时本地并不知道剩余时间。
      if (typeof data.expiresAt === 'number') state.expiresAt = data.expiresAt;
      if (typeof data.remainingSeconds === 'number') {
        const exp = data.remainingSeconds < 0
          ? 0
          : Date.now() + data.remainingSeconds * 1000;
        state.expiresAt = exp;
        state.ttlMinutes = Math.max(0, Math.round(data.remainingSeconds / 60));
      }
      renderMembers();
      renderTTL();
      setText($('#onlineCount'), data.online || 0);
      // 历史消息：清空重放，保证刷新后内容与服务端一致。
      const box = $('#messages');
      box.innerHTML = '';
      (data.history || []).forEach((m) => appendMessage(m, false));
      scheduleScrollBottom();
      setStatus('房间 ' + state.room + ' · 在线 ' + (data.online || 0) + ' 人', '');
      break;

    case 'join':
      state.members = data.members || [];
      renderMembers();
      setText($('#onlineCount'), data.online || 0);
      break;

    case 'leave':
      state.members = data.members || [];
      renderMembers();
      setText($('#onlineCount'), data.online || 0);
      setStatus('会话 ' + state.room + ' · 在线 ' + (data.online || 0) + ' 人', '');
      break;

    case 'message':
      if (data.message) {
        // 自己发的消息：本地已经乐观渲染过，服务端回显只用来「确认」——
        // 把本地那条换成服务端版本（拿到真实 id / 时间），不新增一条。
        const m = data.message;
        if (m.cid && state.pendingSelf.has(m.cid)) {
          state.pendingSelf.delete(m.cid);
          replaceOptimistic(m.cid, m);
          break;
        }
        appendMessage(m, true);
      }
      break;
    case 'error':
      toast(data.error || '发生错误', 'err');
      break;

    default:
      break;
  }
}

/**
 * 用服务端回显替换掉本地乐观渲染的那条。
 *
 * 只更新「服务端才知道」的字段（真实 id、时间），保留 DOM 位置，
 * 避免整条重建引起的闪烁和滚动跳动。
 */
function replaceOptimistic(cid, m) {
  const el = document.getElementById('msg-local-' + cid);
  if (!el) {
    // 本地那条已经不在了（例如期间刷新过），退回普通追加，至少别丢消息。
    // 传 mine=true：这条确实是本机发的，不能靠昵称去猜。
    appendMessage(m, true, false, true);
    return;
  }
  el.id = 'msg-' + m.id;
  el.dataset.sentAt = String(m.sentAt);

  const time = $('.msg-time', el);
  if (time) setText(time, fmtTime(m.sentAt));

  // 服务端是消息类型的权威（它会自己识别 URL、也是文件卡片的唯一来源）。
  // 若本地乐观渲染时的类型与回显不一致，就得重建主体，否则气泡样式会错。
  // 重建时要显式传 mineHint，因为此刻 cid 已经不在 pendingSelf 里了。
  const wantType = m.type || 'text';
  let haveType = 'text';
  if (el.classList.contains('is-link')) haveType = 'link';
  else if (el.classList.contains('is-file')) haveType = 'file';
  if (wantType !== haveType) {
    el.remove();
    appendMessage(m, false, false, true);
  }
}

/** 生成消息内容的指纹，仅用于调试与日志。 */
function contentKey(m) {
  return m.sender + '\u0000' + m.content;
}

/** 发送消息。 */
function sendMessage() {
  const input = $('#msgInput');
  const content = trim(input.value);
  // 有图没字也算一条要发的消息 —— 截图往往就是不想配文。
  const hasAttach = state.attachments.length > 0;
  if (!content && !hasAttach) return;

  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
    toast('尚未连接，消息未发出', 'err');
    return;
  }
  // 进房间前 composer 是隐藏的，正常不会走到这儿；
  // 但重连空档里状态可能已清而 DOM 还没隐藏，兜一下免得图被静默吞掉。
  if (!state.room) {
    toast('已离开房间，图片未发送', 'err');
    return;
  }

  // 先取走队列：sendFilesToRoom 是异步的，若不清空，
  // 用户在上传途中再按一次 Enter 会把同一批图重复发一遍。
  const pending = state.attachments.slice();
  state.attachments = [];
  renderAttachments();

  // 文字照旧走 WS；图片另走 HTTP 上传 + WS 卡片广播 ——
  // 两条链路本来就各自独立，这里只负责同时触发。
  if (pending.length) {
    sendFilesToRoom(pending.map((a) => a.file));
    pending.forEach((a) => URL.revokeObjectURL(a.url));
  }

  if (!content) {
    // 纯图片：输入框本来就没内容，不需要清。
    syncSendButton();
    input.focus();
    return;
  }

  const type = isLink(content) ? 'link' : 'text';
  // cid 是本次发送的临时标识，服务端会原样回传。
  // 靠它认领回显，比「昵称 + 内容」可靠：昵称可能为空、内容可能重复。
  const cid = 'c' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
  const payload = { type, content, cid };

  // 乐观渲染：先本地插入，服务端回显到达时原地替换。
  // 这样在网络稍慢的局域网上感知延迟几乎为零。
  const optimistic = {
    id: 'local-' + cid,
    cid,
    type,
    content,
    sender: state.myName || '我',
    senderId: state.myId,
    sentAt: Date.now(),
    optimistic: true
  };
  state.pendingSelf.add(cid);
  appendMessage(optimistic, true, true);

  try {
    state.ws.send(JSON.stringify(payload));
  } catch (_) {
    toast('发送失败，请重试', 'err');
    state.pendingSelf.delete(cid);
    const el = document.getElementById('msg-' + optimistic.id);
    if (el) el.remove();
    return;
  }

  if ($('#autoClear').checked) {
    input.value = '';
    autoGrow(input);
  }
  syncSendButton();
  input.focus();
}

/* ------------------------------------------- 剪贴板贴图（待发送队列） */

/**
 * 同步「发送」按钮的文案与可用状态。
 *
 * 唯一入口 —— 输入框打字、粘贴图片、删掉图片、发完清空，全都走这里，
 * 免得散在五处的 setText 又一次走偏（历史上这个按钮就曾因为
 * HTML 里写死了 disabled、JS 从没解开而一直是灰的）。
 *
 * 判定：有文字 或 有图片 = 可发。两者都没有才禁用。
 */
function syncSendButton() {
  const btn = $('#btnSend');
  if (!btn) return;
  const input = $('#msgInput');
  const n = state.attachments.length;

  btn.disabled = !(n > 0 || trim(input.value) !== '');
  setText(btn, n ? '发送 ' + n + ' 张' : '发送');
}

/**
 * 从粘贴事件里挑出图片文件。
 *
 * 两条来源都要看：
 *   - `clipboardData.files`：Chrome / Edge 截图工具、「复制图片」走这条，
 *     带完整 MIME（`image/png`）。
 *   - `clipboardData.items`：Safari / 部分 Firefox 只给 item，
 *     得用 `getAsFile()` 取。
 * 两处都拿不到才是「粘的是纯文字」，那属于 textarea 的正常行为，不该拦。
 *
 * **不用 `kind === 'file'` 过滤 items**：某些浏览器把截图报成 `kind: 'string'`
 * 但 `getAsFile()` 仍能返回图片 File —— 按 kind 过滤会漏掉这批。
 * 统一以「拿到 File 且 type 以 image/ 开头」为准。
 */
function pickClipboardImages(dt) {
  if (!dt) return [];
  const out = [];

  const push = (f) => {
    if (!f) return;
    // 只收 image/*。剪贴板里同时有截图和文件路径（Word/Excel 复制）时，
    // 那些非图片的 File 会被这里挡掉，不会混进预览条。
    if (f.type && f.type.indexOf('image/') === 0) out.push(f);
  };

  if (dt.files && dt.files.length) {
    for (let i = 0; i < dt.files.length; i++) push(dt.files[i]);
  }
  if (!out.length && dt.items && dt.items.length) {
    for (let i = 0; i < dt.items.length; i++) {
      const it = dt.items[i];
      if (it.getAsFile) push(it.getAsFile());
    }
  }
  // getAsFile() 可能对同一张图返回两个条目（files 与 items 各一份），
  // 用 name+size+lastModified 去重。
  const seen = new Set();
  return out.filter((f) => {
    const k = f.name + '|' + f.size + '|' + f.lastModified;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

/**
 * 给剪贴板图片起个文件名。
 *
 * 剪贴板里的 File 往往叫 `image.png` / 空字符串 / `blob`，还有的直接叫
 * `image`。而服务端的图片白名单**只看扩展名**（见 internal/files/image.go），
 * 名字不对就永远拿不到 isImage，图片会被当普通文件渲染成一张卡片 ——
 * 所以这里必须补出一个带正确扩展名的名字。
 */
function clipboardImageName(file, index) {
  // 从 MIME 反推扩展名。image/jpeg → jpg（不是 jpeg）——
  // 与 internal/files/image.go 白名单里的写法对齐。
  const mimeExt = (function () {
    const t = (file.type || '').toLowerCase();
    const sub = t.indexOf('/') >= 0 ? t.slice(t.indexOf('/') + 1) : '';
    switch (sub) {
      case 'jpeg': return 'jpg';
      case 'svg+xml': return '';   // 服务端刻意不内联 SVG，别给假希望
      case '': return '';
      default: return /^[a-z0-9]{2,5}$/.test(sub) ? sub : '';
    }
  })();

  const origin = file.name || '';

  // 原文件名本来就带对扩展名 → 原样沿用，用户看到的名字最自然。
  if (origin && mimeExt && extOf(origin) === mimeExt) return origin;

  // 否则：剥掉原有扩展名当主名，没有主名就用「截图」。
  const base = origin ? origin.replace(/\.[a-zA-Z0-9]{1,8}$/, '') : '';

  // 时间戳既方便自己和别人认，也顺手规避了同名歧义；
  // 同一秒粘多张时靠序号区分。
  const d = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  const stamp = d.getFullYear() + pad(d.getMonth() + 1) + pad(d.getDate())
    + '-' + pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds());
  const suffix = index > 0 ? '-' + (index + 1) : '';

  // mimeExt 为空 = 类型未知或 SVG。仍然给个 .png 的名字让它能当文件发出去，
  // 只是服务端不会内联它、会渲染成普通文件卡片 —— 这是有意的降级。
  return (base || '截图') + '-' + stamp + suffix + '.' + (mimeExt || 'png');
}

/** 把一批剪贴板图片放进待发送队列并刷新预览条。 */
function attachClipboardImages(files) {
  if (!files.length) return;

  const MAX = 12;   // 一次粘贴几十张多半是误操作，截断并告知
  let list = files;
  if (list.length > MAX) {
    toast('一次最多粘贴 ' + MAX + ' 张图片，已保留前 ' + MAX + ' 张', 'err');
    list = list.slice(0, MAX);
  }

  list.forEach((f, i) => {
    const named = new File([f], clipboardImageName(f, i), {
      type: f.type || 'image/png',
      lastModified: f.lastModified || Date.now()
    });
    state.attachments.push({
      key: 'a' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
      file: named,
      // 本地预览用 objectURL，不发请求；发送前会 revoke 掉。
      url: URL.createObjectURL(named)
    });
  });

  renderAttachments();
  syncSendButton();
}

/** 重画待发送图片预览条。 */
function renderAttachments() {
  const box = $('#composerAttach');
  if (!box) return;

  box.textContent = '';
  if (!state.attachments.length) {
    box.hidden = true;
    return;
  }
  box.hidden = false;

  state.attachments.forEach((a) => {
    const item = document.createElement('div');
    item.className = 'attach-item';

    const img = document.createElement('img');
    img.src = a.url;
    img.alt = a.file.name;
    img.title = a.file.name + ' · ' + humanSize(a.file.size);
    item.appendChild(img);

    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'attach-del';
    del.title = '移除这张图片';
    del.setAttribute('aria-label', '移除 ' + a.file.name);
    setText(del, '×');
    del.addEventListener('click', () => removeAttachment(a.key));
    item.appendChild(del);

    box.appendChild(item);
  });
}

/** 从待发送队列里去掉一张（并释放 objectURL）。 */
function removeAttachment(key) {
  const i = state.attachments.findIndex((a) => a.key === key);
  if (i < 0) return;
  URL.revokeObjectURL(state.attachments[i].url);
  state.attachments.splice(i, 1);
  renderAttachments();
  syncSendButton();
}

/** 清空待发送队列（离开房间、发完后收尾用）。 */
function clearAttachments() {
  state.attachments.forEach((a) => URL.revokeObjectURL(a.url));
  state.attachments = [];
  renderAttachments();
}

/**
 * 构造一张文件卡片。
 *
 * 卡片本身就是一个 <a>，点哪儿都能下载 —— 文件消息里「下载」是唯一主操作，
 * 没必要再放一个需要精确点击的按钮。
 */
const FOLDER_SVG = '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
  + '<path d="M5 10.5A3.5 3.5 0 018.5 7h6.4a2.8 2.8 0 012.1.96L19.1 10H32a3 3 0 013 3v16.5a3.5 3.5 0 01-3.5 3.5h-23A3.5 3.5 0 015 29.5V10.5z" fill="#D97706" stroke="#B45309" stroke-width="1.2" stroke-linejoin="round"/>'
  + '<rect x="9" y="11" width="22" height="10" rx="1.6" fill="#FFFFFF" stroke="#E2E8F0" stroke-width="0.9"/>'
  + '<path d="M12.5 14h11" stroke="#60A5FA" stroke-width="1.6" stroke-linecap="round"/>'
  + '<rect x="7.5" y="13.2" width="25" height="9" rx="1.6" fill="#FEF3C7"/>'
  + '<path d="M4.5 17.6A2.6 2.6 0 017.1 15h25.8a2.6 2.6 0 012.6 2.6l-.8 12.2a3.2 3.2 0 01-3.2 3H8.5a3.2 3.2 0 01-3.2-3l-.8-12.2z" fill="#FBBF24" stroke="#B45309" stroke-width="1.2" stroke-linejoin="round"/>'
  + '<path d="M6.2 16.5h27.6" stroke="#FEF9C3" stroke-width="1.4" stroke-linecap="round"/>'
  + '<path d="M5.6 26.8l.4 3A2.7 2.7 0 008.7 32.2h22.6a2.7 2.7 0 002.7-2.4l.4-3H5.6z" fill="#F59E0B" opacity=".55"/>'
  + '</svg>';

const UP_SVG = '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
  + '<path d="M5.5 12A3.2 3.2 0 018.7 8.8h5.8a2.4 2.4 0 011.9.9L18.2 11.8H30a2.8 2.8 0 012.8 2.8V28a3.2 3.2 0 01-3.2 3.2H8.7A3.2 3.2 0 015.5 28V12z" fill="#93C5FD" stroke="#3B82F6" stroke-width="1.2" stroke-linejoin="round"/>'
  + '<path d="M5 17.8A2.4 2.4 0 017.4 15.4h23.8a2.4 2.4 0 012.4 2.4l-.6 10.6a2.8 2.8 0 01-2.8 2.8H8.4a2.8 2.8 0 01-2.8-2.8L5 17.8z" fill="#DBEAFE" stroke="#3B82F6" stroke-width="1.2" stroke-linejoin="round"/>'
  + '<circle cx="26.5" cy="24.5" r="8.5" fill="#4F46E5" stroke="#FFFFFF" stroke-width="1.6"/>'
  + '<path d="M26.5 28.2v-7.4m0 0l-3.2 3.2m3.2-3.2l3.2 3.2" fill="none" stroke="#FFFFFF" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>'
  + '</svg>';

function docShellSVG(foldColor, strokeColor, body) {
  return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
    + '<path d="M8 5.8A2.8 2.8 0 0110.8 3h13.7L32 10.5V34.2A2.8 2.8 0 0129.2 37H10.8A2.8 2.8 0 018 34.2V5.8z" fill="#FFFFFF" stroke="' + strokeColor + '" stroke-width="1.35" stroke-linejoin="round"/>'
    + '<path d="M24.5 3v5.4a2.1 2.1 0 002.1 2.1H32L24.5 3z" fill="' + foldColor + '" stroke="' + strokeColor + '" stroke-width="1.15" stroke-linejoin="round"/>'
    + body + '</svg>';
}

function fileIconSVG(name) {
  const ext = extOf(name || '');
  if (['png', 'jpg', 'jpeg', 'jfif', 'gif', 'webp', 'bmp', 'svg', 'avif', 'ico'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<rect x="5" y="6.5" width="30" height="27" rx="4.2" fill="#FFFFFF" stroke="#8B5CF6" stroke-width="1.4"/>'
      + '<rect x="7.5" y="9" width="25" height="22" rx="2.6" fill="#DBEAFE"/>'
      + '<path d="M7.5 11.6A2.6 2.6 0 0110.1 9h19.8a2.6 2.6 0 012.6 2.6V19H7.5v-7.4z" fill="#C7D2FE"/>'
      + '<circle cx="14.5" cy="14.6" r="2.9" fill="#FBBF24"/>'
      + '<path d="M8.2 31l7.6-8.6a1.4 1.4 0 012.1 0l4.3 4.8 3.2-3.4a1.4 1.4 0 012 0l5.1 5.6V31H8.2z" fill="#10B981"/>'
      + '<path d="M18.5 31l6.9-7.4a1.4 1.4 0 012 0l5.1 5.6V31h-14z" fill="#047857" opacity=".82"/>'
      + '</svg>';
  }
  if (['txt', 'md', 'log'].includes(ext)) {
    const tag = ext === 'md' ? 'MD' : (ext === 'log' ? 'LOG' : 'TXT');
    return docShellSVG('#93C5FD', '#3B82F6',
      '<rect x="11.5" y="8" width="11" height="4.8" rx="1.5" fill="#2563EB"/>'
      + '<text x="17" y="11.6" text-anchor="middle" font-family="system-ui,sans-serif" font-size="3.9" font-weight="800" fill="#FFFFFF">' + tag + '</text>'
      + '<path d="M12 17.5h15.5M12 22h16M12 26.5h12.5" stroke="#64748B" stroke-width="2" stroke-linecap="round"/>'
      + '<path d="M12 31h8.5" stroke="#3B82F6" stroke-width="2" stroke-linecap="round"/>'
      + '<path d="M26.5 29.8l4.2-4.2 1.8 1.8-4.2 4.2-2.4.6.6-2.4z" fill="#F59E0B" stroke="#D97706" stroke-width="0.7" stroke-linejoin="round"/>');
  }
  if (ext === 'pdf') {
    return docShellSVG('#FCA5A5', '#DC2626',
      '<rect x="10.5" y="13.5" width="19" height="8.2" rx="2.2" fill="#EF4444"/>'
      + '<text x="20" y="19.3" text-anchor="middle" font-family="system-ui,sans-serif" font-size="5.6" font-weight="800" fill="#FFFFFF">PDF</text>'
      + '<path d="M13 29.5c2.2-3.5 4.2-6 5.2-7.8.9 2.2 2.9 5 6.8 6.4-2.8.2-5.4-.1-8.2.8-1.5.5-2.8 1.2-3.8.6z" fill="none" stroke="#DC2626" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>');
  }
  if (['zip', 'rar', '7z', 'tar', 'gz', 'bz2', 'xz'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<rect x="6" y="7" width="28" height="27" rx="4.2" fill="#FDE68A" stroke="#D97706" stroke-width="1.35"/>'
      + '<path d="M6 12.2A5.2 5.2 0 0111.2 7h17.6A5.2 5.2 0 0134 12.2V14H6v-1.8z" fill="#F59E0B" stroke="#D97706" stroke-width="1.2"/>'
      + '<rect x="17.5" y="7" width="5" height="27" fill="#78350F" opacity=".2"/>'
      + '<path d="M18.2 10h2v2h-2zm2 3h2v2h-2zm-2 3h2v2h-2zm2 3h2v2h-2z" fill="#FFFBEB"/>'
      + '<rect x="16.4" y="21.5" width="7.2" height="8.2" rx="2" fill="#EA580C" stroke="#9A3412" stroke-width="1"/>'
      + '<circle cx="20" cy="26.2" r="1.4" fill="#FEF3C7"/>'
      + '</svg>';
  }
  if (ext === 'apk') {
    return docShellSVG('#6EE7B7', '#059669',
      '<path d="M13.5 22.5a6.5 6.5 0 0113 0H13.5z" fill="#10B981"/>'
      + '<path d="M16 16.5l-1.6-2.4M24 16.5l1.6-2.4" stroke="#059669" stroke-width="1.6" stroke-linecap="round"/>'
      + '<circle cx="17.4" cy="19.6" r="1" fill="#FFFFFF"/><circle cx="22.6" cy="19.6" r="1" fill="#FFFFFF"/>'
      + '<rect x="11.5" y="25.2" width="17" height="7" rx="2" fill="#059669"/>'
      + '<text x="20" y="30.2" text-anchor="middle" font-family="system-ui,sans-serif" font-size="4.8" font-weight="800" fill="#FFFFFF">APK</text>');
  }
  if (['exe', 'msi', 'bat', 'cmd', 'dmg', 'deb', 'rpm'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<rect x="5" y="6.5" width="30" height="27" rx="4" fill="#F8FAFC" stroke="#4F46E5" stroke-width="1.35"/>'
      + '<path d="M5 10.5A4 4 0 019 6.5h22a4 4 0 014 4v3.8H5v-3.8z" fill="#312E81"/>'
      + '<circle cx="9.2" cy="10.4" r="1.3" fill="#FB7185"/><circle cx="13.2" cy="10.4" r="1.3" fill="#FBBF24"/><circle cx="17.2" cy="10.4" r="1.3" fill="#34D399"/>'
      + '<circle cx="20" cy="24" r="5.2" fill="none" stroke="#0EA5E9" stroke-width="2.6" stroke-dasharray="2.4 1.6"/>'
      + '<circle cx="20" cy="24" r="2.4" fill="#4F46E5"/>'
      + '</svg>';
  }
  if (['mp4', 'mkv', 'avi', 'mov', 'webm', 'flv', 'wmv'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<rect x="5" y="6.5" width="30" height="27" rx="4.2" fill="#FDF2F8" stroke="#DB2777" stroke-width="1.35"/>'
      + '<path d="M5 10.7A4.2 4.2 0 019.2 6.5h21.6a4.2 4.2 0 014.2 4.2v3.6H5v-3.6z" fill="#BE185D"/>'
      + '<path d="M9.5 6.5l2.5 7.8M17.5 6.5l2.5 7.8M25.5 6.5l2.5 7.8" stroke="#FBCFE8" stroke-width="1.8"/>'
      + '<circle cx="20" cy="23.8" r="6.8" fill="#EC4899"/>'
      + '<path d="M18 20.6l5.8 3.2-5.8 3.2v-6.4z" fill="#FFFFFF"/>'
      + '</svg>';
  }
  if (['mp3', 'wav', 'flac', 'm4a', 'ogg', 'aac'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<rect x="5.5" y="5.5" width="29" height="29" rx="5" fill="#ECFEFF" stroke="#0D9488" stroke-width="1.35"/>'
      + '<path d="M16.5 25.5V14.2l11-2.2v11.5" fill="none" stroke="#0F766E" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round"/>'
      + '<path d="M16.5 14.2l11-2.2v3l-11 2.2z" fill="#14B8A6"/>'
      + '<circle cx="13.8" cy="26.2" r="3.1" fill="#06B6D4"/><circle cx="24.8" cy="24" r="3.1" fill="#0D9488"/>'
      + '</svg>';
  }
  if (['doc', 'docx', 'rtf', 'odt'].includes(ext)) {
    return docShellSVG('#93C5FD', '#2563EB',
      '<rect x="11.5" y="13.5" width="9.5" height="9.5" rx="2" fill="#2563EB"/>'
      + '<text x="16.2" y="20.4" text-anchor="middle" font-family="system-ui,sans-serif" font-size="6.4" font-weight="800" fill="#FFFFFF">W</text>'
      + '<path d="M23.5 15.5h5M23.5 19.5h5M12 27h16.5M12 31.2h12" stroke="#3B82F6" stroke-width="1.9" stroke-linecap="round"/>');
  }
  if (['xls', 'xlsx', 'csv', 'ods'].includes(ext)) {
    return docShellSVG('#86EFAC', '#16A34A',
      '<rect x="11.5" y="14.5" width="17" height="16.5" rx="2" fill="#DCFCE7" stroke="#22C55E" stroke-width="1.1"/>'
      + '<path d="M11.5 19.8h17M11.5 25.2h17M17.4 14.5V31" stroke="#22C55E" stroke-width="1.1"/>'
      + '<rect x="11.5" y="14.5" width="17" height="5.3" rx="1.4" fill="#16A34A"/>'
      + '<text x="20" y="18.4" text-anchor="middle" font-family="system-ui,sans-serif" font-size="4.1" font-weight="800" fill="#FFFFFF">' + (ext === 'csv' ? 'CSV' : 'XLS') + '</text>');
  }
  if (['ppt', 'pptx', 'key', 'odp'].includes(ext)) {
    return docShellSVG('#FDBA74', '#EA580C',
      '<rect x="11.5" y="14" width="17" height="12.5" rx="2" fill="#FFF7ED" stroke="#F97316" stroke-width="1.2"/>'
      + '<circle cx="17.5" cy="20.2" r="3.8" fill="#F97316"/>'
      + '<path d="M17.5 20.2V16.4a3.8 3.8 0 013.8 3.8h-3.8z" fill="#FACC15"/>'
      + '<path d="M23 18.2h3.5M23 21.6h2.8M15 30.5h10" stroke="#EA580C" stroke-width="1.8" stroke-linecap="round"/>');
  }
  if (['go', 'js', 'ts', 'jsx', 'tsx', 'py', 'rs', 'java', 'c', 'cpp', 'h', 'hpp', 'json', 'html', 'css', 'scss', 'sh', 'ps1', 'yml', 'yaml', 'xml', 'sql', 'toml'].includes(ext)) {
    return '<svg viewBox="0 0 40 40" width="40" height="40" aria-hidden="true">'
      + '<path d="M8 5.8A2.8 2.8 0 0110.8 3h13.7L32 10.5V34.2A2.8 2.8 0 0129.2 37H10.8A2.8 2.8 0 018 34.2V5.8z" fill="#1E293B" stroke="#475569" stroke-width="1.35" stroke-linejoin="round"/>'
      + '<path d="M24.5 3v5.4a2.1 2.1 0 002.1 2.1H32L24.5 3z" fill="#475569" stroke="#64748B" stroke-width="1.1" stroke-linejoin="round"/>'
      + '<circle cx="12.2" cy="8.2" r="1.1" fill="#F87171"/><circle cx="15.5" cy="8.2" r="1.1" fill="#FBBF24"/><circle cx="18.8" cy="8.2" r="1.1" fill="#34D399"/>'
      + '<path d="M15.8 18.2l-3.6 3.5 3.6 3.5" fill="none" stroke="#38BDF8" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>'
      + '<path d="M24.2 18.2l3.6 3.5-3.6 3.5" fill="none" stroke="#34D399" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>'
      + '<path d="M21.4 16.8l-2.8 9.8" stroke="#FBBF24" stroke-width="2" stroke-linecap="round"/>'
      + '<path d="M12.5 30.2h11" stroke="#64748B" stroke-width="1.8" stroke-linecap="round"/>'
      + '</svg>';
  }
  const label = escapeHTML((ext ? ext.slice(0, 4) : 'FILE').toUpperCase());
  const fSize = label.length >= 4 ? '4.8' : '5.4';
  return docShellSVG('#CBD5E1', '#64748B',
    '<rect x="11" y="14" width="18" height="7.6" rx="2" fill="#6366F1"/>'
    + '<text x="20" y="19.5" text-anchor="middle" font-family="system-ui,sans-serif" font-size="' + fSize + '" font-weight="800" fill="#FFFFFF">' + label + '</text>'
    + '<path d="M12.5 26.2h15M12.5 30.4h10.5" stroke="#94A3B8" stroke-width="1.9" stroke-linecap="round"/>');
}

function buildFileCard(m) {
  const a = document.createElement('a');
  a.className = 'msg-file';
  a.href = m.fileUrl || ('/api/chat-files/' + m.fileId);
  a.setAttribute('download', m.fileName || '');
  a.title = '点击下载 ' + (m.fileName || '');

  const icon = document.createElement('span');
  icon.className = 'msg-file-icon';
  icon.innerHTML = fileIconSVG(m.fileName || '');

  const meta = document.createElement('div');
  meta.className = 'msg-file-meta';
  const nm = document.createElement('div');
  nm.className = 'msg-file-name';
  nm.title = m.fileName || '';
  setText(nm, m.fileName || '未命名文件');
  const sz = document.createElement('div');
  sz.className = 'msg-file-size';
  setText(sz, m.fileText || humanSize(m.fileSize || 0));
  meta.append(nm, sz);

  a.append(icon, meta);
  return a;
}

/**
 * 本地已知的图片扩展名。
 *
 * 与服务端 files/image.go 的白名单保持同一份清单 —— 用来给**乐观渲染**兜底：
 * 文件是我自己刚传上去的，此刻服务端卡片还没回来，但 name 已经在手上，
 * 靠它就能先把缩略图渲染出来，不用等一个来回（否则图片消息会先闪一下
 * 文件卡片再变成图）。服务端返回的 isImage 始终是权威值。
 */
const IMAGE_EXTS = ['png', 'jpg', 'jpeg', 'jfif', 'gif', 'webp', 'bmp', 'avif', 'ico'];

/** 判断一条文件消息是不是图片（服务端标记优先，本地扩展名兜底）。 */
function isImageMsg(m) {
  if (m.isImage === true) return true;
  if (m.isImage === false) return false;
  return IMAGE_EXTS.indexOf(extOf(m.fileName || '')) >= 0;
}

/**
 * 构造一张内联图片消息。
 *
 * 用 <img> 直接指向下载接口的 inline 变体（`?inline=1`）：
 * 浏览器自己负责解码与缩放，服务端不需要装任何图片库 ——
 * 在 ARM64 路由器上跑缩略图生成是纯负担，而局域网带宽本来就是富余的。
 *
 * 尺寸交给 CSS 约束（max-width / max-height），这样不同分辨率的截图
 * 进到消息流里宽度一致，不会一条撑满、一条只有指甲盖大。
 *
 * 卡片自带一枚「下载」按钮（右下角，悬停浮现）。
 * 图片消息的内容就是那张图，没有可复制的文字 —— 通用复制按钮对它没意义，
 * 内容区右上角只留一个真正能用的动作。
 */
function buildImageCard(m) {
  const wrap = document.createElement('div');
  wrap.className = 'msg-image';

  const url = m.fileUrl || ('/api/chat-files/' + m.fileId);
  const img = document.createElement('img');
  // 空格分隔的路径参数：原 URL 上可能已经带了查询串。
  img.src = url + (url.indexOf('?') >= 0 ? '&' : '?') + 'inline=1';
  img.alt = m.fileName || '图片';
  img.loading = 'lazy';       // 历史回放时不要一次性把几十张图全拉下来
  img.decoding = 'async';
  img.draggable = false;

  // 加载失败（文件已被清理、格式其实是坏的、SVG 被服务端拒绝内联……）
  // 必须降级成原来的文件卡片，而不是留一个破图图标。
  // 服务端只看扩展名，这种「名字像图片但内容不是」的情况确实会出现。
  img.addEventListener('error', () => {
    const card = buildFileCard(m);
    card.classList.add('is-fallback');
    wrap.replaceWith(card);
  });

  wrap.appendChild(img);

  // 下载按钮：右下角，悬停时浮现（和 .msg-tools 同一套显隐节奏）。
  // 链接指向**不带** inline 的地址，与灯箱里的「下载原图」同源 ——
  // inline 那个是给 <img> 内联显示用的，拿它做下载语义上不干净。
  const dl = document.createElement('a');
  dl.className = 'btn btn-icon msg-image-dl';
  dl.href = url;
  dl.setAttribute('download', m.fileName || '');
  dl.title = '下载' + (m.fileName ? ' ' + m.fileName : '');
  dl.setAttribute('aria-label', '下载' + (m.fileName || '图片'));
  dl.innerHTML = '<svg viewBox="0 0 20 20" width="15" height="15">'
    + '<path d="M10 3v8.5m0 0L6.8 8.3M10 11.5l3.2-3.2" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>'
    + '<path d="M4 14.5V15a2 2 0 002 2h8a2 2 0 002-2v-.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>';
  // 卡片整体是「点击看大图」，按钮上要掐掉冒泡，否则点下载会顺带弹灯箱。
  // 无 href 时（极端兜底）降级成复制地址，别给一个点了没反应的按钮。
  dl.addEventListener('click', (e) => {
    e.stopPropagation();
    if (!dl.getAttribute('href')) {
      e.preventDefault();
      copyText(url).then((ok) => toast(ok ? '地址已复制' : '复制失败', ok ? 'ok' : 'err'));
    }
  });
  wrap.appendChild(dl);

  // 点图看大图。用 button 语义包一层会破坏 .msg-image 的圆角裁切，
  // 所以直接给 img 挂 click + 键盘可达性。
  img.tabIndex = 0;
  img.title = '点击查看大图';
  img.addEventListener('click', () => openLightbox(m, img.src));
  img.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      openLightbox(m, img.src);
    }
  });

  return wrap;
}

/* --------------------------------------------------------------- 灯箱 */

const LIGHTBOX_MAX_SCALE = 8;

const lightboxView = {
  scale: 1,
  fit: true,
  tx: 0,
  ty: 0,
  dragging: false,
  pointerId: null,
  startX: 0,
  startY: 0,
  startTx: 0,
  startTy: 0,
  moved: false,
  suppressClick: false,
  returnFocus: null
};

function lightboxFitScale() {
  const stage = $('#lightboxStage');
  const img = $('#lightboxImg');
  if (!stage || !img || !img.naturalWidth || !img.naturalHeight) return 1;
  const style = getComputedStyle(stage);
  const width = stage.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
  const height = stage.clientHeight - parseFloat(style.paddingTop) - parseFloat(style.paddingBottom);
  return Math.min(width / img.naturalWidth, height / img.naturalHeight, 1);
}

function clampLightboxOffset() {
  const stage = $('#lightboxStage');
  const img = $('#lightboxImg');
  if (!stage || !img || !img.naturalWidth || !img.naturalHeight) return;
  const imgW = img.naturalWidth * lightboxView.scale;
  const imgH = img.naturalHeight * lightboxView.scale;
  const marginX = Math.min(80, imgW * 0.5, stage.clientWidth * 0.25);
  const marginY = Math.min(80, imgH * 0.5, stage.clientHeight * 0.25);
  const maxX = Math.max(0, (stage.clientWidth + imgW) / 2 - marginX);
  const maxY = Math.max(0, (stage.clientHeight + imgH) / 2 - marginY);
  lightboxView.tx = Math.max(-maxX, Math.min(maxX, lightboxView.tx));
  lightboxView.ty = Math.max(-maxY, Math.min(maxY, lightboxView.ty));
}

function applyLightboxTransform() {
  const img = $('#lightboxImg');
  if (!img) return;
  img.style.transform = 'translate3d(' + lightboxView.tx + 'px, ' + lightboxView.ty + 'px, 0) translate(-50%, -50%)';
}

/** 用实际像素尺寸缩放，支持围绕光标定点缩放及左键拖拽平移。 */
function setLightboxScale(scale, fit = false, anchor = null) {
  const box = $('#lightbox');
  const stage = $('#lightboxStage');
  const img = $('#lightboxImg');
  if (!box || box.hidden || !stage || !img || !img.naturalWidth || !img.naturalHeight) return;
  const fitScale = lightboxFitScale();
  if (!(fitScale > 0)) return;
  const minScale = Math.min(fitScale, 0.1);
  const prev = lightboxView.scale || 1;
  const next = fit ? fitScale : Math.max(minScale, Math.min(LIGHTBOX_MAX_SCALE, scale));

  if (fit || (anchor && anchor.reset)) {
    lightboxView.tx = 0;
    lightboxView.ty = 0;
  } else {
    const viewport = stage.getBoundingClientRect();
    const cx = viewport.left + stage.clientWidth / 2;
    const cy = viewport.top + stage.clientHeight / 2;
    const px = anchor && anchor.x != null ? anchor.x : cx;
    const py = anchor && anchor.y != null ? anchor.y : cy;
    const ratio = next / prev;
    lightboxView.tx = (px - cx) - (px - cx - lightboxView.tx) * ratio;
    lightboxView.ty = (py - cy) - (py - cy - lightboxView.ty) * ratio;
  }

  lightboxView.scale = next;
  lightboxView.fit = fit;
  img.style.width = img.naturalWidth * next + 'px';
  img.style.height = img.naturalHeight * next + 'px';
  img.style.visibility = '';
  clampLightboxOffset();
  applyLightboxTransform();
  setText($('#lightboxScale'), (next < 0.1 ? (next * 100).toFixed(1) : Math.round(next * 100)) + '%');
  $('#btnZoomOut').disabled = next <= minScale + 1e-4;
  $('#btnZoomIn').disabled = next >= LIGHTBOX_MAX_SCALE - 1e-4;
}

/**
 * 打开大图灯箱。
 *
 * 刻意不复用 .modal-backdrop：那个类带着卡片式的内边距和边框，
 * 套在图上会变成「图外面又围了一圈白框」。灯箱要的是纯黑底 + 居中图。
 */
function openLightbox(m, src) {
  const box = $('#lightbox');
  const stage = $('#lightboxStage');
  const img = $('#lightboxImg');
  if (!box || !img) return;

  setText($('#lightboxName'), m.fileName || '');
  setText($('#lightboxMeta'), m.fileText || humanSize(m.fileSize || 0));
  const dl = $('#lightboxDownload');
  if (dl) {
    // 下载链接指向**不带** inline 的那个地址 —— 保证拿到的是附件而不是页面。
    dl.href = m.fileUrl || ('/api/chat-files/' + m.fileId);
    dl.setAttribute('download', m.fileName || '');
  }
  if (box.hidden) lightboxView.returnFocus = document.activeElement;
  lightboxView.fit = true;
  lightboxView.scale = 1;
  lightboxView.tx = 0;
  lightboxView.ty = 0;
  lightboxView.dragging = false;
  lightboxView.pointerId = null;
  lightboxView.moved = false;
  lightboxView.suppressClick = false;
  if (stage) stage.classList.remove('is-dragging');
  img.style.visibility = 'hidden';
  img.style.width = '';
  img.style.height = '';
  img.style.transform = '';
  img.onload = () => setLightboxScale(1, true);
  img.onerror = () => {
    closeLightbox();
    toast('图片加载失败，请尝试下载原图', 'err');
  };
  img.alt = m.fileName || '图片';
  box.hidden = false;
  img.src = src;
  if (img.complete && img.naturalWidth) setLightboxScale(1, true);
  $('#btnCloseLightbox').focus();
}

function closeLightbox() {
  const box = $('#lightbox');
  const stage = $('#lightboxStage');
  const img = $('#lightboxImg');
  if (!box || box.hidden) return;
  box.hidden = true;
  lightboxView.dragging = false;
  lightboxView.pointerId = null;
  lightboxView.moved = false;
  lightboxView.suppressClick = false;
  lightboxView.tx = 0;
  lightboxView.ty = 0;
  if (stage) stage.classList.remove('is-dragging');
  // 清空 src 再关，避免大图继续占着内存（尤其是一次翻过十几张之后）。
  if (img) {
    img.onload = null;
    img.onerror = null;
    img.style.transform = '';
    img.removeAttribute('src');
  }
  if (lightboxView.returnFocus && lightboxView.returnFocus.isConnected) lightboxView.returnFocus.focus();
  lightboxView.returnFocus = null;
}

/**
 * 渲染一条消息。
 *
 * @param m         消息体
 * @param animate   是否播放入场动画
 * @param optimistic 是否是本地乐观渲染（此刻还没有服务端 id）
 * @param mineHint  显式指定「是我发的」。服务端回显替换乐观节点时会用到：
 *                  那一刻 cid 已从 pendingSelf 移除，直接给出结论最省事
 *                  （回显带着本连接的 senderId，其实也判得出来）。
 *
 * mine 的判定顺序：显式提示 > 乐观渲染 > cid 命中 > senderId 比较 > 名字兜底。
 *
 * 权威依据是 senderId（服务端给**连接**分配的唯一标识）：
 * 显示名不足以判定归属 —— 同一台机器开两个标签页、或所有人经由同一个
 * 反向代理进来时，服务端算出的显示名会完全重名，按名字比会把别人的
 * 消息全认成自己的（历史上实时区「消息全显示成我」就是这么来的）。
 */
function appendMessage(m, animate, optimistic, mineHint) {
  const box = $('#messages');
  if (!box) return;

  const mine = mineHint === true
    || optimistic === true
    || (!!m.cid && state.pendingSelf.has(m.cid))
    || (!!state.myId && m.senderId === state.myId)
    // 兜底：老服务端不下发 senderId 时退回按名字比。
    // 它在重名场景下必定误判，只作为跨版本兼容保留。
    || (!m.senderId && !!state.myName && m.sender === state.myName);
  const follow = nearBottom();

  const el = document.createElement('div');
  el.className = 'msg'
    + (mine ? ' is-mine' : '')
    + (m.type === 'link' ? ' is-link' : '')
    + (m.type === 'file' ? ' is-file' : '')
    // 图片是 file 的一个「渲染变体」而不是新的消息类型：
    // 上传、归属校验、随房间销毁的清理三者完全共用，只有画法不同。
    + (m.type === 'file' && isImageMsg(m) ? ' is-image' : '');
  // 带上 id，方便服务端回显到达时原地替换掉乐观渲染的那条。
  if (m.id) el.id = 'msg-' + m.id;
  el.dataset.sentAt = String(m.sentAt);
  if (!animate) el.style.animation = 'none';

  // 头部：发送者 + 时间
  const head = document.createElement('div');
  head.className = 'msg-head';
  const sender = document.createElement('span');
  sender.className = 'msg-sender';
  setText(sender, mine ? '我' : (m.sender || '匿名设备'));
  const sep = document.createElement('span');
  sep.className = 'msg-sep';
  sep.textContent = '·';
  const time = document.createElement('span');
  time.className = 'msg-time';
  setText(time, fmtTime(m.sentAt));
  head.append(sender, sep, time);

  // 主体
  const body = document.createElement('div');
  body.className = 'msg-body';

  if (m.type === 'link') {
    const a = document.createElement('a');
    a.className = 'msg-link';
    a.href = m.content;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.innerHTML = '<svg viewBox="0 0 20 20" width="15" height="15">'
      + '<path d="M8.2 11.8a3.6 3.6 0 010-5.1l2.2-2.2a3.6 3.6 0 015.1 5.1l-1 1" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>'
      + '<path d="M11.8 8.2a3.6 3.6 0 010 5.1l-2.2 2.2a3.6 3.6 0 01-5.1-5.1l1-1" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>'
      + '</svg><span class="msg-link-text"></span>';
    setText($('.msg-link-text', a), m.content);
    body.appendChild(a);
  } else if (m.type === 'file') {
    // 图片走缩略图，其余仍是文件名卡片。判定统一交给 isImageMsg，
    // 与服务端下发的 isImage 保持一个出口，避免两处各判各的。
    body.appendChild(isImageMsg(m) ? buildImageCard(m) : buildFileCard(m));
  } else {
    // 用 textContent 而不是 innerHTML：用户内容永不参与 HTML 解析。
    body.textContent = m.content;
  }

  // 工具栏（复制 / 打开）
  // 图片消息没有可复制的文本（m.content 是空的），复制按钮放这儿只会点了没反应 ——
  // 它的下载按钮已经贴在图片右下角了，这里整体让位。
  const tools = document.createElement('div');
  tools.className = 'msg-tools';

  if (!(m.type === 'file' && isImageMsg(m))) {
    const copyBtn = document.createElement('button');
    copyBtn.className = 'btn btn-icon';
    copyBtn.title = '复制内容';
    copyBtn.setAttribute('aria-label', '复制内容');
    copyBtn.innerHTML = '<svg viewBox="0 0 20 20" width="15" height="15">'
      + '<rect x="7" y="7" width="9" height="10" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/>'
      + '<path d="M13 5.5V5a2 2 0 00-2-2H6a2 2 0 00-2 2v7a2 2 0 002 2h.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>';
    copyBtn.addEventListener('click', async () => {
      const ok = await copyText(m.content);
      toast(ok ? '已复制' : '复制失败，请手动选择', ok ? 'ok' : 'err');
    });
    tools.appendChild(copyBtn);
  }

  if (m.type === 'link') {
    const openBtn = document.createElement('button');
    openBtn.className = 'btn btn-icon';
    openBtn.title = '在新标签页打开';
    openBtn.setAttribute('aria-label', '在新标签页打开');
    openBtn.innerHTML = '<svg viewBox="0 0 20 20" width="15" height="15">'
      + '<path d="M11 4h5v5M15.5 4.5L9.5 10.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>'
      + '<path d="M14 12v3.5A1.5 1.5 0 0112.5 17h-8A1.5 1.5 0 013 15.5v-8A1.5 1.5 0 014.5 6H8" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>';
    openBtn.addEventListener('click', () => window.open(m.content, '_blank', 'noopener'));
    tools.appendChild(openBtn);
  }

  el.append(head, body, tools);
  box.appendChild(el);

  // 只在自己已经贴着底部时才自动滚动，避免打断向上翻阅历史。
  if (follow) scheduleScrollBottom();
}

/** 渲染在线成员头像。 */
function renderMembers() {
  setText($('#onlineCount'), state.members.length);
  const stack = $('#avatarStack');
  if (!stack) return;
  stack.innerHTML = '';
  const colors = ['#3b6ff5', '#17a673', '#d9880c', '#a855c7', '#d1537c', '#2f9e8f'];
  state.members.slice(0, 6).forEach((name, i) => {
    const el = document.createElement('span');
    el.className = 'mini';
    el.style.background = colors[i % colors.length];
    el.title = name;
    setText(el, initialOf(name));
    stack.appendChild(el);
  });
  if (state.members.length > 6) {
    const more = document.createElement('span');
    more.className = 'mini';
    more.style.background = 'var(--text-3)';
    more.textContent = '+' + (state.members.length - 6);
    stack.appendChild(more);
  }
}

/** 输入框自适应高度。 */
function autoGrow(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 168) + 'px';
}

/* ------------------------------------------------------------------ 7. 文件仓库 */

/** 渲染未登录提示条。
 *
 * 提示条只解释「上传按钮为什么是灰的」，**不覆盖列表**：
 * 它是面板里的一行文档流（`.lock-banner`），列表整体下移，
 * 每一行的下载按钮都照常可见可点。
 * （曾经是右上角往下悬垂的绝对定位浮卡，把「上传者 / 下载」两列压住了。）
 *
 * 首次部署（needSetup）时不显示：那时 setup-banner 已经给了同一个入口，
 * 两条提示并排只会重复。
 */
function renderLock() {
  const need = !state.user && !(state.status && state.status.needSetup);
  const overlay = $('#lockOverlay');
  if (overlay) overlay.hidden = !need;
  const btn = $('#btnUpload');
  if (btn) btn.disabled = !state.user;
  const newFolderBtn = $('#btnNewFolder');
  if (newFolderBtn) newFolderBtn.disabled = !state.user;
}

/** 拉取文件与文件夹列表。 */
async function loadFiles() {
  renderLock();

  try {
    const res = await fetch('/api/files?kind=permanent');
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      // 列表已对所有人开放，401 理论上不会出现；真出现了就当空列表处理，
      // 别把「未登录」渲染成错误提示。
      state.files = [];
      state.folders = [];
      state.filesTotalSize = 0;
      state.filesTotalText = '';
      renderFiles();
      toast(data.error || '读取文件列表失败', 'err');
      return;
    }
    state.files = data.files || [];
    state.folders = data.folders || [];
    state.filesTotalSize = Number(data.totalSize) || 0;
    state.filesTotalText = data.totalText || '';
    if (state.currentFolderId && !folderById(state.currentFolderId)) {
      state.currentFolderId = null;
    }
    // 搜索词不清：刷新、上传完成、置顶后都会走到这里，
    // 每次都把用户刚敲的词抹掉会很难用。过滤在 renderFiles 里重新应用。
    renderFiles();
  } catch (_) {
    setStatus('文件列表读取失败', '');
  }
}

/* ------------------------------------------------- 文件搜索（纯前端过滤） */

/**
 * 把搜索词拆成小写关键词。
 *
 * 按空白切分后**要求全部命中**，而不是拿整串做子串匹配：
 * 「截图 2024」这种写法在文件名搜索里很自然，整串匹配会一个都搜不到
 * （真实文件名里「截图」和「2024」之间一定隔着别的东西）。
 */
function searchTokens() {
  const q = trim(state.fileQuery).toLowerCase();
  return q ? q.split(/\s+/) : [];
}

/** 文件名是否命中全部关键词（大小写不敏感）。 */
function nameMatches(name, tokens) {
  const n = String(name || '').toLowerCase();
  return tokens.every((t) => n.indexOf(t) >= 0);
}

/**
 * 构造带高亮片段的名字节点；没命中任何关键词时返回 null，调用方退回纯文本。
 *
 * 用 DocumentFragment 而不是拼 innerHTML：文件名是用户内容，
 * 拼字符串就得自己保证转义不出错，交给 createTextNode 就没有这个面。
 */
function highlightedName(name, tokens) {
  const src = String(name || '');
  if (!tokens.length) return null;
  const lower = src.toLowerCase();

  // 先把每个关键词的全部出现位置收集成区间，再合并重叠部分 ——
  // 「aab」搜「aa ab」会得到两段重叠区间，不合并就会切出嵌套的节点。
  const ranges = [];
  tokens.forEach((t) => {
    let from = 0;
    for (;;) {
      const i = lower.indexOf(t, from);
      if (i < 0) break;
      ranges.push([i, i + t.length]);
      from = i + t.length;   // 关键词非空，这样必然前进，不会死循环
    }
  });
  if (!ranges.length) return null;

  ranges.sort((a, b) => a[0] - b[0]);
  const merged = [];
  ranges.forEach((r) => {
    const last = merged[merged.length - 1];
    if (last && r[0] <= last[1]) last[1] = Math.max(last[1], r[1]);
    else merged.push([r[0], r[1]]);
  });

  const frag = document.createDocumentFragment();
  let pos = 0;
  merged.forEach(([s, e]) => {
    if (s > pos) frag.appendChild(document.createTextNode(src.slice(pos, s)));
    const mark = document.createElement('mark');
    mark.textContent = src.slice(s, e);
    frag.appendChild(mark);
    pos = e;
  });
  if (pos < src.length) frag.appendChild(document.createTextNode(src.slice(pos)));
  return frag;
}

/** 读取搜索框内容并重绘列表。 */
function applySearch() {
  const input = $('#fileSearch');
  state.fileQuery = input ? input.value : '';
  const clear = $('#btnClearSearch');
  if (clear) clear.hidden = trim(state.fileQuery) === '';
  renderFiles();
}

/** 清空搜索（× 按钮与 Esc 共用）。 */
function clearSearch() {
  const input = $('#fileSearch');
  if (input) {
    input.value = '';
    // 清空后把光标留在框里，方便直接敲下一个词；
    // 点 × 时焦点本来在按钮上，不抢回来就得再点一次输入框。
    input.focus();
  }
  applySearch();
}

/**
 * 空态文案。
 *
 * 「仓库是空的」「当前文件夹是空的」和「搜索没命中」是三件事，分别给出明确指引。
 */
function renderFilesEmpty(allCount, shownCount, searching, hasUpRow) {
  const box = $('#fileEmpty');
  if (!box) return;
  box.hidden = shownCount > 0 || !!hasUpRow;
  if (!box.hidden) return;

  if (searching && allCount > 0) {
    setText($('#fileEmptyTitle'), '没有匹配的文件或文件夹');
    setText($('#fileEmptyHint'),
      '全部条目里没有名称含「' + trim(state.fileQuery) + '」的，换个关键词或清空搜索');
  } else if (state.currentFolderId) {
    setText($('#fileEmptyTitle'), '这个文件夹还是空的');
    setText($('#fileEmptyHint'), '可以上传文件到这里，或返回上级目录拖入文件');
  } else {
    setText($('#fileEmptyTitle'), '文件仓库还是空的');
    setText($('#fileEmptyHint'), '上传过的文件会长期保存在文件仓库');
  }
}

/** 用量条文案与容量进度条。搜索时按命中结果统计，并显式写出「命中 / 总数」。 */
function renderUsage(total, list, searching) {
  const all = state.files || [];
  const totalCount = total != null ? total : all.length;
  const hasArgs = list != null;
  const tokens = hasArgs ? [] : searchTokens();
  const isSearch = hasArgs ? !!searching : tokens.length > 0;
  const shown = hasArgs ? list : (isSearch ? all.filter((f) => nameMatches(f.name, tokens)) : all);

  const shownSum = shown.reduce((n, f) => n + (Number(f.size) || 0), 0);
  const totalSum = state.filesTotalSize || all.reduce((n, f) => n + (Number(f.size) || 0), 0);
  const maxStorage = state.status && Number(state.status.maxStorage) ? Number(state.status.maxStorage) : 0;

  if (isSearch) {
    setText($('#usageText'), shown.length + ' / ' + totalCount + ' 个文件 · 共 ' + humanSize(shownSum));
  } else if (maxStorage > 0) {
    const pct = Math.min(100, Math.round((totalSum / maxStorage) * 100));
    setText($('#usageText'),
      totalCount + ' 个文件 · 已用 ' + (state.filesTotalText || humanSize(totalSum))
      + ' / ' + humanSize(maxStorage) + ' (' + pct + '%)');
  } else {
    setText($('#usageText'), totalCount + ' 个文件 · 共 ' + (state.filesTotalText || humanSize(totalSum)));
  }

  const track = $('#usageTrack');
  const fill = $('#usageFill');
  if (track && fill) {
    if (maxStorage > 0 && !isSearch) {
      const ratio = Math.min(100, (totalSum / maxStorage) * 100);
      track.hidden = false;
      fill.style.width = (totalSum > 0 ? Math.max(2, ratio) : 0) + '%';
      fill.classList.toggle('is-warn', ratio >= 80 && ratio < 95);
      fill.classList.toggle('is-danger', ratio >= 95);
    } else {
      track.hidden = true;
    }
  }

  setText($('#usageHint'), state.user ? '文件长期保存在本机仓库' : '浏览与下载无需登录');
}

/* ------------------------------------------------- 视图、文件夹、拖拽与预览 */

const PIN_SVG = '<svg viewBox="0 0 20 20" width="16" height="16">'
  + '<path d="M12.6 3.2a1 1 0 011.5.1l2.6 2.6a1 1 0 01-.1 1.5l-1.9 1.5-.4 3.1a.8.8 0 01-1.3.6L10 9.9l-3.4 3.4a.6.6 0 01-.9-.9L9.1 9 6.4 6.3a.8.8 0 01.6-1.3l3.1-.4 1.5-1.9z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/>'
  + '<path d="M5.6 14.4l-1.9 1.9" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>';


const DL_SVG = '<svg viewBox="0 0 20 20" width="15" height="15" aria-hidden="true">'
  + '<path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>'
  + '<path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>';

const EDIT_SVG = '<svg viewBox="0 0 20 20" width="14" height="14" aria-hidden="true">'
  + '<path d="M13.6 3.6a1.7 1.7 0 012.4 2.4L7.2 14.8l-3.3.8.8-3.3 8.9-8.7z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/></svg>';

const DEL_SVG = '<svg viewBox="0 0 20 20" width="15" height="15" aria-hidden="true">'
  + '<path d="M4.5 6h11M8 6V4.6a1 1 0 011-1h2a1 1 0 011 1V6M6.5 6l.7 9.2a1.4 1.4 0 001.4 1.3h2.8a1.4 1.4 0 001.4-1.3L13.5 6" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';

function isRepoImage(f) {
  if (!f) return false;
  if (f.isImage === true) return true;
  if (f.isImage === false) return false;
  return IMAGE_EXTS.indexOf(extOf(f.name)) >= 0;
}

function isRepoText(f) {
  if (!f) return false;
  if (f.isText === true) return true;
  if (f.isText === false) return false;
  return extOf(f.name) === 'txt';
}

function isRepoPreviewable(f) {
  return isRepoImage(f) || isRepoText(f);
}

function openRepoPreview(f) {
  if (isRepoImage(f)) {
    openLightbox({
      fileName: f.name,
      fileText: f.sizeText || humanSize(f.size),
      fileSize: f.size,
      fileUrl: f.downloadUrl
    }, f.downloadUrl + '?inline=1');
    return;
  }
  if (isRepoText(f)) {
    openTextPreview(f);
  }
}

function folderById(id) {
  if (!id) return null;
  return (state.folders || []).find((x) => x.id === id) || null;
}

function folderPath(id) {
  const chain = [];
  const seen = new Set();
  let cur = folderById(id);
  while (cur && !seen.has(cur.id)) {
    seen.add(cur.id);
    chain.unshift(cur);
    cur = cur.parentId ? folderById(cur.parentId) : null;
  }
  return chain;
}

function folderPathText(id) {
  if (!id) return '根目录';
  const chain = folderPath(id);
  return chain.length ? chain.map((c) => c.name).join(' / ') : '根目录';
}

function isDescendantFolder(candidateId, ancestorId) {
  if (!candidateId || !ancestorId) return false;
  const seen = new Set();
  let cur = folderById(candidateId);
  while (cur && !seen.has(cur.id)) {
    if (cur.id === ancestorId) return true;
    seen.add(cur.id);
    cur = cur.parentId ? folderById(cur.parentId) : null;
  }
  return false;
}

function folderChildCount(folderId) {
  const subFolders = (state.folders || []).filter((x) => (x.parentId || null) === folderId).length;
  const subFiles = (state.files || []).filter((f) => (f.folderId || null) === folderId).length;
  return subFolders + subFiles;
}

function canDeleteFile(f) {
  if (!state.user) return false;
  if (state.user.isAdmin) return true;
  return !!f.owner && f.owner === state.user.username;
}

function canEditFolder(folder) {
  if (!state.user) return false;
  if (state.user.isAdmin) return true;
  return !!folder.owner && folder.owner === state.user.username;
}

function syncViewButtons() {
  $$('#viewSwitch .view-btn').forEach((btn) => {
    const on = btn.dataset.view === state.fileView;
    btn.classList.toggle('is-active', on);
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
}

function setFileView(view) {
  state.fileView = view === 'card' ? 'card' : 'list';
  try { localStorage.setItem(LS_FILE_VIEW, state.fileView); } catch (_) {}
  syncViewButtons();
  renderFiles();
}

function enterFolder(id) {
  state.currentFolderId = id || null;
  renderFiles();
}

/** 渲染面包屑路径条，非当前目录节点支持直接拖放移动文件/文件夹。 */
function renderFolderBar() {
  const bar = $('#folderBar');
  if (!bar) return;
  bar.textContent = '';

  const curId = state.currentFolderId;
  if (!curId) {
    bar.hidden = true;
    return;
  }
  bar.hidden = false;

  const chain = folderPath(curId);
  const rootBtn = document.createElement('button');
  rootBtn.type = 'button';
  rootBtn.className = 'crumb';
  setText(rootBtn, '全部文件');
  rootBtn.addEventListener('click', () => enterFolder(null));
  attachDropTarget(rootBtn, null);
  bar.appendChild(rootBtn);

  chain.forEach((folder, idx) => {
    const sep = document.createElement('span');
    sep.className = 'crumb-sep';
    sep.textContent = '/';
    bar.appendChild(sep);

    const isLast = idx === chain.length - 1;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'crumb' + (isLast ? ' is-current' : '');
    btn.title = folder.name;
    setText(btn, folder.name);
    if (isLast) {
      btn.disabled = true;
    } else {
      btn.addEventListener('click', () => enterFolder(folder.id));
      attachDropTarget(btn, folder.id);
    }
    bar.appendChild(btn);
  });
}

/* ------------------------------------------------- 内部拖拽移动 */

let draggingItem = null;

function canDropIntoFolder(item, targetFolderId) {
  if (!item) return false;
  const target = targetFolderId || null;
  if (item.type === 'file') {
    const f = (state.files || []).find((x) => x.id === item.id);
    return !!f && canDeleteFile(f) && (f.folderId || null) !== target;
  }
  if (item.type === 'folder') {
    const folder = folderById(item.id);
    if (!folder || !canEditFolder(folder)) return false;
    if ((folder.parentId || null) === target) return false;
    if (target === item.id || isDescendantFolder(target, item.id)) return false;
    return true;
  }
  return false;
}

function attachDraggable(el, item) {
  el.draggable = true;
  el.addEventListener('dragstart', (e) => {
    draggingItem = item;
    el.classList.add('is-dragging');
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
      try { e.dataTransfer.setData('application/x-lanshare-item', JSON.stringify(item)); } catch (_) {}
      try { e.dataTransfer.setData('text/plain', item.name || ''); } catch (_) {}
    }
  });
  el.addEventListener('dragend', () => {
    draggingItem = null;
    el.classList.remove('is-dragging');
    $$('.is-drop-target').forEach((n) => n.classList.remove('is-drop-target'));
  });
}

function attachDropTarget(el, targetFolderId) {
  let depth = 0;

  el.addEventListener('dragenter', (e) => {
    if (!canDropIntoFolder(draggingItem, targetFolderId)) return;
    e.preventDefault();
    depth++;
    el.classList.add('is-drop-target');
  });

  el.addEventListener('dragover', (e) => {
    if (!canDropIntoFolder(draggingItem, targetFolderId)) return;
    e.preventDefault();
    e.stopPropagation();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
  });

  el.addEventListener('dragleave', () => {
    if (!draggingItem) { depth = 0; el.classList.remove('is-drop-target'); return; }
    depth = Math.max(0, depth - 1);
    if (depth === 0) el.classList.remove('is-drop-target');
  });

  el.addEventListener('drop', (e) => {
    if (!draggingItem) return;
    e.preventDefault();
    e.stopPropagation();
    depth = 0;
    el.classList.remove('is-drop-target');
    const item = draggingItem;
    draggingItem = null;
    if (!canDropIntoFolder(item, targetFolderId)) return;
    moveItemToFolder(item, targetFolderId || null);
  });
}

async function moveItemToFolder(item, targetFolderId) {
  const url = item.type === 'folder' ? ('/api/folders/' + item.id) : ('/api/files/' + item.id);
  const body = item.type === 'folder'
    ? { parentId: targetFolderId || 0 }
    : { folderId: targetFolderId || 0 };
  const destLabel = targetFolderId ? ('「' + folderPathText(targetFolderId) + '」') : '根目录';

  try {
    const res = await fetch(url, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '移动失败', 'err');
      return;
    }
    toast('已移动到 ' + destLabel, 'ok');
    await loadFiles();
  } catch (_) {
    toast('网络错误，移动失败', 'err');
  }
}

/* ------------------------------------------------- 共用操作按钮 */

function buildDownloadLink(f) {
  const dl = document.createElement('a');
  dl.className = 'btn btn-icon';
  dl.href = f.downloadUrl;
  dl.title = '下载';
  dl.setAttribute('aria-label', '下载 ' + f.name);
  dl.download = f.name;
  dl.innerHTML = DL_SVG;
  dl.addEventListener('click', (e) => e.stopPropagation());
  dl.addEventListener('dblclick', (e) => e.stopPropagation());
  return dl;
}

function buildRenameBtn(title, onClick) {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'btn btn-icon';
  btn.title = title;
  btn.setAttribute('aria-label', title);
  btn.innerHTML = EDIT_SVG;
  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    onClick();
  });
  btn.addEventListener('dblclick', (e) => e.stopPropagation());
  return btn;
}

function buildPinBtn(f, container) {
  const pin = document.createElement('button');
  pin.type = 'button';
  pin.className = 'btn btn-icon' + (f.pinned ? ' is-active' : '');
  pin.title = f.pinned ? '取消置顶' : '置顶';
  pin.setAttribute('aria-label', pin.title + ' ' + f.name);
  pin.innerHTML = PIN_SVG;
  pin.addEventListener('click', (e) => {
    e.stopPropagation();
    togglePin(f, container);
  });
  pin.addEventListener('dblclick', (e) => e.stopPropagation());
  return pin;
}

function buildDeleteBtn(label, onClick) {
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'btn btn-icon is-danger';
  del.title = '删除';
  del.setAttribute('aria-label', '删除 ' + label);
  del.innerHTML = DEL_SVG;
  del.addEventListener('click', (e) => {
    e.stopPropagation();
    onClick();
  });
  del.addEventListener('dblclick', (e) => e.stopPropagation());
  return del;
}

/* ------------------------------------------------- 渲染列表视图与卡片视图 */

function renderFiles() {
  const allFiles = state.files || [];
  const allFolders = state.folders || [];
  const tokens = searchTokens();
  const searching = tokens.length > 0;
  const curId = state.currentFolderId;

  const folders = searching
    ? allFolders.filter((fd) => nameMatches(fd.name, tokens))
    : allFolders.filter((fd) => (fd.parentId || null) === curId);

  const files = searching
    ? allFiles.filter((f) => nameMatches(f.name, tokens))
    : allFiles.filter((f) => (f.folderId || null) === curId);

  const curFolder = curId ? folderById(curId) : null;
  const hasUpRow = !searching && !!curFolder;

  renderFolderBar();
  renderFilesEmpty(allFiles.length + allFolders.length, folders.length + files.length, searching, hasUpRow);
  renderUsage(allFiles.length, files, searching);

  const tableWrap = $('#fileTableWrap');
  const grid = $('#fileGrid');
  const isCard = state.fileView === 'card';
  if (tableWrap) tableWrap.hidden = isCard;
  if (grid) grid.hidden = !isCard;

  if (isCard) {
    renderFilesGrid(folders, files, tokens, searching, curFolder);
  } else {
    renderFilesTable(folders, files, tokens, searching, curFolder);
  }
}

function renderFilesTable(folders, files, tokens, searching, curFolder) {
  const body = $('#fileBody');
  if (!body) return;
  body.innerHTML = '';

  if (!searching && curFolder) {
    const parentId = curFolder.parentId || null;
    const tr = document.createElement('tr');
    tr.className = 'up-row';

    const tdName = document.createElement('td');
    const wrap = document.createElement('div');
    wrap.className = 'cell-name';
    const icon = document.createElement('span');
    icon.className = 'ftype ftype-up';
    icon.innerHTML = UP_SVG;
    const nm = document.createElement('div');
    nm.className = 'fname';
    setText(nm, '.. 返回上一级');
    wrap.append(icon, nm);
    tdName.appendChild(wrap);
    tdName.addEventListener('click', () => enterFolder(parentId));

    const tdSize = document.createElement('td');
    tdSize.className = 'cell-size';
    setText(tdSize, '上级目录');

    const tdTime = document.createElement('td');
    tdTime.className = 'cell-time';
    setText(tdTime, '—');

    const tdOwner = document.createElement('td');
    tdOwner.className = 'cell-owner';
    setText(tdOwner, '—');

    const tdAct = document.createElement('td');
    tdAct.className = 'cell-act';

    tr.append(tdName, tdSize, tdTime, tdOwner, tdAct);
    attachDropTarget(tr, parentId);
    body.appendChild(tr);
  }

  folders.forEach((folder) => {
    const tr = document.createElement('tr');
    tr.className = 'folder-row';
    tr.dataset.folderId = folder.id;

    const tdName = document.createElement('td');
    const wrap = document.createElement('div');
    wrap.className = 'cell-name';

    const icon = document.createElement('span');
    icon.className = 'ftype ftype-folder';
    icon.innerHTML = FOLDER_SVG;

    const textWrap = document.createElement('div');
    textWrap.style.minWidth = '0';
    const nm = document.createElement('div');
    nm.className = 'fname';
    nm.title = folder.name;
    const hl = highlightedName(folder.name, tokens);
    if (hl) nm.appendChild(hl); else setText(nm, folder.name);
    textWrap.appendChild(nm);

    if (searching && folder.parentId) {
      const tag = document.createElement('span');
      tag.className = 'fpath-tag';
      tag.title = folderPathText(folder.parentId);
      setText(tag, folderPathText(folder.parentId));
      textWrap.appendChild(tag);
    }

    wrap.append(icon, textWrap);
    tdName.appendChild(wrap);
    tdName.addEventListener('click', () => enterFolder(folder.id));

    const tdSize = document.createElement('td');
    tdSize.className = 'cell-size';
    const cnt = folderChildCount(folder.id);
    setText(tdSize, cnt > 0 ? cnt + ' 项' : '空');

    const tdTime = document.createElement('td');
    tdTime.className = 'cell-time';
    setText(tdTime, fmtTime(folder.createdAt));

    const tdOwner = document.createElement('td');
    tdOwner.className = 'cell-owner';
    setText(tdOwner, folder.owner || '—');

    const tdAct = document.createElement('td');
    tdAct.className = 'cell-act';
    if (canEditFolder(folder)) {
      tdAct.appendChild(buildRenameBtn('重命名文件夹', () => openFolderModal('rename', folder)));
      tdAct.appendChild(buildDeleteBtn(folder.name, () => deleteFolder(folder, tr)));
      attachDraggable(tr, { type: 'folder', id: folder.id, name: folder.name });
    }

    tr.append(tdName, tdSize, tdTime, tdOwner, tdAct);
    attachDropTarget(tr, folder.id);
    body.appendChild(tr);
  });

  files.forEach((f) => {
    const tr = document.createElement('tr');
    tr.dataset.id = f.id;
    if (f.pinned) tr.classList.add('is-pinned');

    const tdName = document.createElement('td');
    const wrap = document.createElement('div');
    wrap.className = 'cell-name';

    const previewable = isRepoPreviewable(f);
    const icon = document.createElement('span');
    icon.className = 'ftype' + (previewable ? ' is-previewable' : '');
    icon.innerHTML = fileIconSVG(f.name);

    const textWrap = document.createElement('div');
    textWrap.style.minWidth = '0';
    const nm = document.createElement('div');
    nm.className = 'fname' + (previewable ? ' is-previewable' : '');
    nm.title = previewable ? (f.name + '（点击直接打开预览）') : f.name;
    const hl = highlightedName(f.name, tokens);
    if (hl) nm.appendChild(hl); else setText(nm, f.name);

    if (previewable) {
      const openFn = () => openRepoPreview(f);
      icon.title = '点击打开预览';
      icon.addEventListener('click', openFn);
      nm.addEventListener('click', openFn);
    } else if (canDeleteFile(f)) {
      nm.classList.add('is-renamable');
      nm.addEventListener('dblclick', () => startRename(f, nm, tr));
    }
    textWrap.appendChild(nm);

    if (f.pinned) {
      const pin = document.createElement('span');
      pin.className = 'fpin';
      pin.title = '已置顶';
      pin.innerHTML = PIN_SVG;
      textWrap.appendChild(pin);
    }
    if (searching && f.folderId) {
      const tag = document.createElement('span');
      tag.className = 'fpath-tag';
      tag.title = folderPathText(f.folderId);
      setText(tag, folderPathText(f.folderId));
      textWrap.appendChild(tag);
    }

    wrap.append(icon, textWrap);
    tdName.appendChild(wrap);

    const tdSize = document.createElement('td');
    tdSize.className = 'cell-size';
    setText(tdSize, f.sizeText || humanSize(f.size));

    const tdTime = document.createElement('td');
    tdTime.className = 'cell-time';
    setText(tdTime, fmtTime(f.createdAt));

    const tdOwner = document.createElement('td');
    tdOwner.className = 'cell-owner';
    setText(tdOwner, f.owner || '—');

    const tdAct = document.createElement('td');
    tdAct.className = 'cell-act';
    tdAct.appendChild(buildDownloadLink(f));

    if (canDeleteFile(f)) {
      tdAct.appendChild(buildRenameBtn('重命名', () => startRename(f, nm, tr)));
      tdAct.appendChild(buildPinBtn(f, tr));
      tdAct.appendChild(buildDeleteBtn(f.name, () => deleteFile(f, tr)));
      attachDraggable(tr, { type: 'file', id: f.id, name: f.name });
    }

    tr.append(tdName, tdSize, tdTime, tdOwner, tdAct);
    body.appendChild(tr);
  });
}

function renderFilesGrid(folders, files, tokens, searching, curFolder) {
  const grid = $('#fileGrid');
  if (!grid) return;
  grid.innerHTML = '';

  if (!searching && curFolder) {
    const parentId = curFolder.parentId || null;
    const card = document.createElement('div');
    card.className = 'fcard is-up';

    const thumb = document.createElement('div');
    thumb.className = 'fcard-thumb';
    thumb.classList.add('has-svg');
    const badge = document.createElement('span');
    badge.className = 'fcard-svg-icon is-up';
    badge.innerHTML = UP_SVG;
    thumb.appendChild(badge);

    const info = document.createElement('div');
    info.className = 'fcard-info';
    const nm = document.createElement('div');
    nm.className = 'fcard-name';
    setText(nm, '.. 返回上一级');
    const meta = document.createElement('div');
    meta.className = 'fcard-meta';
    setText(meta, parentId ? folderPathText(parentId) : '根目录');
    info.append(nm, meta);

    card.append(thumb, info);
    card.addEventListener('click', () => enterFolder(parentId));
    attachDropTarget(card, parentId);
    grid.appendChild(card);
  }

  folders.forEach((folder) => {
    const card = document.createElement('div');
    card.className = 'fcard is-folder';
    card.dataset.folderId = folder.id;

    const thumb = document.createElement('div');
    thumb.className = 'fcard-thumb';
    thumb.classList.add('has-svg', 'is-folder-thumb');
    const badge = document.createElement('span');
    badge.className = 'fcard-svg-icon is-folder';
    badge.innerHTML = FOLDER_SVG;
    thumb.appendChild(badge);

    if (canEditFolder(folder)) {
      const acts = document.createElement('div');
      acts.className = 'fcard-actions';
      acts.appendChild(buildRenameBtn('重命名文件夹', () => openFolderModal('rename', folder)));
      acts.appendChild(buildDeleteBtn(folder.name, () => deleteFolder(folder, card)));
      thumb.appendChild(acts);
      attachDraggable(card, { type: 'folder', id: folder.id, name: folder.name });
    }

    const info = document.createElement('div');
    info.className = 'fcard-info';
    const nm = document.createElement('div');
    nm.className = 'fcard-name';
    nm.title = folder.name;
    const hl = highlightedName(folder.name, tokens);
    if (hl) nm.appendChild(hl); else setText(nm, folder.name);

    const cnt = folderChildCount(folder.id);
    const meta = document.createElement('div');
    meta.className = 'fcard-meta';
    setText(meta, (cnt > 0 ? cnt + ' 项' : '空文件夹') + ' · ' + fmtTime(folder.createdAt));

    info.append(nm, meta);
    if (searching && folder.parentId) {
      const p = document.createElement('div');
      p.className = 'fcard-path';
      setText(p, folderPathText(folder.parentId));
      info.appendChild(p);
    }

    card.append(thumb, info);
    card.addEventListener('click', () => enterFolder(folder.id));
    attachDropTarget(card, folder.id);
    grid.appendChild(card);
  });

  files.forEach((f) => {
    const card = document.createElement('div');
    const previewable = isRepoPreviewable(f);
    card.className = 'fcard'
      + (f.pinned ? ' is-pinned' : '')
      + (previewable ? ' is-previewable' : '');
    card.dataset.id = f.id;
    card.title = previewable
      ? (f.name + '（双击打开预览）')
      : (f.name + '（双击下载）');

    const thumb = document.createElement('div');
    thumb.className = 'fcard-thumb';
    const ext = extOf(f.name);

    const makeIcon = () => {
      const badge = document.createElement('span');
      badge.className = 'fcard-svg-icon is-file';
      badge.innerHTML = fileIconSVG(f.name);
      return badge;
    };

    if (isRepoImage(f)) {
      const img = document.createElement('img');
      img.src = f.downloadUrl + '?inline=1';
      img.alt = f.name;
      img.loading = 'lazy';
      img.decoding = 'async';
      img.draggable = false;
      img.addEventListener('error', () => {
        thumb.classList.add('has-svg');
        img.replaceWith(makeIcon());
      });
      thumb.appendChild(img);
    } else {
      thumb.classList.add('has-svg');
      thumb.appendChild(makeIcon());
    }

    if (f.pinned) {
      const pinTag = document.createElement('span');
      pinTag.className = 'fcard-pin';
      pinTag.title = '已置顶';
      pinTag.innerHTML = PIN_SVG;
      thumb.appendChild(pinTag);
    }

    const dl = buildDownloadLink(f);
    const acts = document.createElement('div');
    acts.className = 'fcard-actions';
    acts.appendChild(dl);

    const info = document.createElement('div');
    info.className = 'fcard-info';
    const nm = document.createElement('div');
    nm.className = 'fcard-name';
    nm.title = f.name;
    const hl = highlightedName(f.name, tokens);
    if (hl) nm.appendChild(hl); else setText(nm, f.name);

    const meta = document.createElement('div');
    meta.className = 'fcard-meta';
    setText(meta, (f.sizeText || humanSize(f.size)) + ' · ' + fmtTime(f.createdAt));

    info.append(nm, meta);
    if (searching && f.folderId) {
      const p = document.createElement('div');
      p.className = 'fcard-path';
      setText(p, folderPathText(f.folderId));
      info.appendChild(p);
    }

    if (canDeleteFile(f)) {
      acts.appendChild(buildRenameBtn('重命名', () => startRename(f, nm, card)));
      acts.appendChild(buildPinBtn(f, card));
      acts.appendChild(buildDeleteBtn(f.name, () => deleteFile(f, card)));
      attachDraggable(card, { type: 'file', id: f.id, name: f.name });
    }
    thumb.appendChild(acts);

    card.append(thumb, info);
    card.addEventListener('dblclick', () => {
      if (previewable) {
        openRepoPreview(f);
      } else {
        dl.click();
      }
    });
    grid.appendChild(card);
  });
}

/* ------------------------------------------------- 文件夹弹窗与操作 */

const folderModalState = { mode: 'create', folder: null };

function openFolderModal(mode, folder) {
  if (!state.user) {
    toast('请先登录后再管理文件夹', 'err');
    openAuth('login');
    return;
  }
  const modal = $('#folderModal');
  if (!modal) return;

  folderModalState.mode = mode === 'rename' && folder ? 'rename' : 'create';
  folderModalState.folder = folder || null;

  const isRename = folderModalState.mode === 'rename';
  setText($('#folderModalTitle'), isRename ? '重命名文件夹' : '新建文件夹');
  setText($('#folderModalSub'), isRename
    ? '修改「' + folder.name + '」的名称'
    : '将在「' + folderPathText(state.currentFolderId) + '」下创建');
  setText($('#btnDoFolder'), isRename ? '保存' : '创建');
  setText($('#folderMsg'), '');

  const input = $('#folderNameInput');
  input.value = isRename ? folder.name : '';
  modal.hidden = false;
  setTimeout(() => {
    input.focus();
    if (isRename) input.select();
  }, 30);
}

function closeFolderModal() {
  const modal = $('#folderModal');
  if (modal) modal.hidden = true;
}

async function submitFolderModal(e) {
  if (e) e.preventDefault();
  const btn = $('#btnDoFolder');
  if (btn.disabled) return;

  const name = trim($('#folderNameInput').value);
  if (!name) {
    setText($('#folderMsg'), '请输入文件夹名称');
    return;
  }

  const isRename = folderModalState.mode === 'rename' && folderModalState.folder;
  if (isRename && name === folderModalState.folder.name) {
    closeFolderModal();
    return;
  }

  btn.disabled = true;
  setText($('#folderMsg'), '');
  try {
    const url = isRename ? ('/api/folders/' + folderModalState.folder.id) : '/api/folders';
    const method = isRename ? 'PATCH' : 'POST';
    const payload = isRename ? { name } : { name, parentId: state.currentFolderId || 0 };
    const res = await fetch(url, {
      method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      setText($('#folderMsg'), data.error || (isRename ? '重命名失败' : '创建失败'));
      return;
    }
    closeFolderModal();
    toast((isRename ? '已重命名为：' : '已创建文件夹：') + data.name, 'ok');
    await loadFiles();
  } catch (_) {
    setText($('#folderMsg'), '网络错误，请重试');
  } finally {
    btn.disabled = false;
  }
}

async function deleteFolder(folder, container) {
  const cnt = folderChildCount(folder.id);
  const msg = cnt > 0
    ? '确定删除文件夹「' + folder.name + '」？其中的 ' + cnt + ' 个子项将移回上级目录。'
    : '确定删除空文件夹「' + folder.name + '」？';
  if (!window.confirm(msg)) return;

  if (container) container.classList.add('is-busy');
  try {
    const res = await fetch('/api/folders/' + folder.id, { method: 'DELETE' });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '删除文件夹失败', 'err');
      if (container) container.classList.remove('is-busy');
      return;
    }
    if (state.currentFolderId === folder.id) {
      state.currentFolderId = folder.parentId || null;
    }
    toast('已删除文件夹：' + folder.name, 'ok');
    await loadFiles();
  } catch (_) {
    toast('网络错误，删除文件夹失败', 'err');
    if (container) container.classList.remove('is-busy');
  }
}

/* ------------------------------------------------- TXT 文本预览弹窗 */

let textPreviewReq = 0;

function decodeTextBuffer(buf) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(buf);
  } catch (_) {
    try {
      return new TextDecoder('gb18030').decode(buf);
    } catch (_) {
      return new TextDecoder('utf-8').decode(buf);
    }
  }
}

async function openTextPreview(f) {
  const modal = $('#textModal');
  const body = $('#textPreviewBody');
  const notice = $('#textModalNotice');
  if (!modal || !body) return;

  const reqId = ++textPreviewReq;
  setText($('#textModalName'), f.name || '文本预览');
  setText($('#textModalMeta'), (f.sizeText || humanSize(f.size)) + ' · ' + fmtTime(f.createdAt));

  const dl = $('#textModalDownload');
  if (dl) {
    dl.href = f.downloadUrl;
    dl.setAttribute('download', f.name || '');
  }

  const MAX_FULL_BYTES = 2 * 1024 * 1024;
  const PREVIEW_BYTES = 512 * 1024;
  const isTruncated = Number(f.size) > MAX_FULL_BYTES;

  if (notice) {
    if (isTruncated) {
      setText(notice, '文件较大（' + (f.sizeText || humanSize(f.size)) + '），仅预览前 512 KB；完整内容请点击右上角下载。');
      notice.hidden = false;
    } else {
      notice.hidden = true;
    }
  }

  body.textContent = '正在加载文本内容…';
  modal.hidden = false;

  try {
    const headers = isTruncated ? { Range: 'bytes=0-' + (PREVIEW_BYTES - 1) } : {};
    const res = await fetch(f.downloadUrl + '?inline=1', { headers });
    if (reqId !== textPreviewReq || modal.hidden) return;
    if (!res.ok) {
      body.textContent = '加载失败（HTTP ' + res.status + '），请点击右上角下载后查看。';
      return;
    }
    const buf = await res.arrayBuffer();
    if (reqId !== textPreviewReq || modal.hidden) return;
    body.textContent = decodeTextBuffer(buf);
  } catch (_) {
    if (reqId === textPreviewReq && !modal.hidden) {
      body.textContent = '网络错误，无法读取文件内容，请尝试下载。';
    }
  }
}

function closeTextModal() {
  const modal = $('#textModal');
  if (!modal || modal.hidden) return;
  textPreviewReq++;
  modal.hidden = true;
  const body = $('#textPreviewBody');
  if (body) body.textContent = '';
}

/** 就地重命名文件（列表与卡片视图共用）。 */
function startRename(f, nm, container) {
  if (container.querySelector('.fname-edit')) return;

  const input = document.createElement('input');
  input.className = 'fname-edit';
  input.type = 'text';
  input.value = f.name;
  input.maxLength = 200;   // 与服务端 SafeDisplayName 的 200 rune 上限对齐
  input.spellcheck = false;

  let settled = false;
  const finish = async (commit) => {
    if (settled) return;
    settled = true;

    const next = (input.value || '').trim();
    input.replaceWith(nm);

    if (!commit || !next || next === f.name) return;

    container.classList.add('is-busy');
    try {
      const res = await fetch('/api/files/' + f.id, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: next }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || '重命名失败', 'err');
        container.classList.remove('is-busy');
        return;
      }
      toast('已重命名为：' + (data.name || next), 'ok');
      await loadFiles();
    } catch (_) {
      container.classList.remove('is-busy');
      toast('网络错误，重命名失败', 'err');
    }
  };

  input.addEventListener('click', (e) => e.stopPropagation());
  input.addEventListener('dblclick', (e) => e.stopPropagation());
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      finish(true);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      finish(false);
    }
  });
  input.addEventListener('blur', () => finish(true));

  nm.replaceWith(input);
  input.focus();
  const dot = input.value.lastIndexOf('.');
  const caret = dot > 0 ? dot : input.value.length;
  input.setSelectionRange(0, caret);
}

/** 切换置顶，并用服务端返回的完整列表重排。 */
async function togglePin(f, container) {
  container.classList.add('is-busy');
  try {
    const res = await fetch('/api/files/' + f.id + '/pin', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '设置置顶失败', 'err');
      container.classList.remove('is-busy');
      return;
    }
    toast(data.pinned ? '已置顶：' + data.name : '已取消置顶：' + data.name, 'ok');
    await loadFiles();
  } catch (_) {
    container.classList.remove('is-busy');
    toast('网络错误，设置置顶失败', 'err');
  }
}

async function deleteFile(f, container) {
  if (!window.confirm('确定删除「' + f.name + '」？此操作不可恢复。')) return;

  container.classList.add('is-busy');
  try {
    const res = await fetch('/api/files/' + f.id, { method: 'DELETE' });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '删除失败', 'err');
      container.classList.remove('is-busy');
      return;
    }
    toast('已删除：' + f.name, 'ok');
    setStatus('已删除 ' + f.name, '');
    await loadFiles();
    refreshStatus();
  } catch (_) {
    toast('网络错误，删除失败', 'err');
    container.classList.remove('is-busy');
  }
}

/** 打开文件选择框。 */
function pickFiles() {
  if (!state.user) {
    toast('请先登录后再上传到文件仓库', 'err');
    openAuth('login');
    return;
  }
  $('#fileInput').click();
}

/**
 * 同时上传的文件数。
 *
 * 选 2 是给路由器定的：同时跑 10 个「收网络 + 写磁盘 + 算 SHA256 + fsync」
 * 会明显抬高负载，而 1～2 路基本已经能把千兆内网的磁盘吃满。
 * 服务端另有同值的信号量兜底（LANSHARE_UPLOAD_CONCURRENCY），
 * 防止开多个浏览器绕过这里的排队。
 */
const UPLOAD_CONCURRENCY = 2;

/**
 * 跑一批任务，同时在飞的最多 concurrency 个，完成一个补一个。
 *
 * tasks 里的每一项都是「拿到控制权后才执行」的函数 ——
 * 这样剩下的一直留在数组里等着，不会被提前创建出 XHR 连接。
 */
function runWithConcurrency(tasks, concurrency) {
  let i = 0;
  const n = Math.min(concurrency, tasks.length);

  const next = () => {
    if (i >= tasks.length) return;
    const task = tasks[i++];
    // 用 Promise.resolve().then(task) 而不是直接 task()：
    // 这样任务里万一同步抛异常也会被 then 的第二个回调接住，
    // 队列不会因为一个坏任务就停摆。
    Promise.resolve().then(task).then(next, next);
  };

  for (let k = 0; k < n; k++) next();
}

/** 上传一批文件。 */
function uploadFiles(fileList) {
  const list = Array.from(fileList || []);
  if (!list.length) return;

  if (!state.user) {
    toast('请先登录后再上传到文件仓库', 'err');
    openAuth('login');
    return;
  }

  const box = $('#uploadList');
  box.hidden = false;

  // 先给每个文件建好进度条（显示「等待中」），再交给队列跑。
  // 这样用户一眼能看到总量，也知道哪些还没轮到，而不是以为漏掉了。
  const jobs = list.map((file) => {
    const row = makeUploadRow(file, box);
    setText(row.pct, '等待中');
    return () => uploadOne(file, row);
  });
  runWithConcurrency(jobs, UPLOAD_CONCURRENCY);
}

/** 建一条上传进度条，返回它的几个可更新节点。 */
function makeUploadRow(file, box) {
  const row = document.createElement('div');
  row.className = 'up-item';
  row.innerHTML = '<div class="up-line">'
    + '<span class="up-name"></span><span class="up-pct">0%</span>'
    + '</div><div class="up-bar"><div class="up-fill"></div></div>';
  setText($('.up-name', row), file.name);
  box.appendChild(row);

  return { row, fill: $('.up-fill', row), pct: $('.up-pct', row) };
}

/**
 * 上传单个文件，用 XMLHttpRequest 以获得真实上传进度。
 *
 * 返回的 Promise 在请求彻底结束（成功 / 失败 / 中断）时才 resolve，
 * 供上传队列判断何时放行下一个。
 */
function uploadOne(file, ui) {
  const { row, fill, pct } = ui;
  const box = row.parentElement;

  let markDone;
  const finished = new Promise((r) => { markDone = r; });

  const form = new FormData();
  // 顺序有讲究：必须先把 kind / folderId / name 放进 FormData，再放文件。
  // 服务端是流式解析 multipart —— 遇到文件 part 就立刻开始边收边写，
  // 排在文件后面的字段它已经读不到了。
  form.append('kind', 'permanent');
  if (state.currentFolderId) {
    form.append('folderId', String(state.currentFolderId));
  }
  form.append('name', file.name);
  form.append('file', file, file.name);

  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/files', true);

  xhr.upload.addEventListener('progress', (e) => {
    if (!e.lengthComputable) return;
    const p = Math.round((e.loaded / e.total) * 100);
    fill.style.width = p + '%';
    setText(pct, p + '%');
  });

  // 进度条到 100% 只代表「浏览器把文件发完了」，不代表服务端存完了 ——
  // 之后还有 fsync、rename、写库这几步。以前这段时间进度条就停在 100%，
  // 看起来像卡死；这里显式切到「保存中」，让用户知道还在干活。
  xhr.upload.addEventListener('load', () => {
    fill.style.width = '100%';
    setText(pct, '保存中…');
    row.classList.add('is-saving');
  });

  xhr.addEventListener('load', () => {
    let data = {};
    try { data = JSON.parse(xhr.responseText); } catch (_) {}
    row.classList.remove('is-saving');

    if (xhr.status >= 200 && xhr.status < 300) {
      row.classList.add('is-done');
      setText(pct, '完成');
      fill.style.width = '100%';
      toast('已上传：' + file.name, 'ok');
      // 稍作停留让用户看到 100%，然后移除进度条并刷新列表。
      setTimeout(() => {
        row.remove();
        if (!box.children.length) box.hidden = true;
      }, 1200);
      loadFiles();
      refreshStatus();
    } else {
      row.classList.add('is-error');
      setText(pct, '失败');
      toast(data.error || ('上传失败：' + file.name), 'err');
      setTimeout(() => {
        row.remove();
        if (!box.children.length) box.hidden = true;
      }, 3200);
    }
  });

  xhr.addEventListener('error', () => {
    row.classList.remove('is-saving');
    row.classList.add('is-error');
    setText(pct, '失败');
    toast('网络错误，上传失败：' + file.name, 'err');
    setTimeout(() => {
      row.remove();
      if (!box.children.length) box.hidden = true;
    }, 3200);
  });

  // loadend 在 load / error / abort / timeout 之后都会触发，
  // 用它收口最稳 —— 任何一个分支漏掉都会让队列永久卡住一个位置。
  xhr.addEventListener('loadend', () => markDone());

  xhr.send(form);
  return finished;
}

/* ------------------------------------------------------------------ 7b. 聊天室文件 */

/**
 * 把一批文件发到当前房间。
 *
 * 与文件仓库上传的区别：
 *   - 不需要登录，前提是「已经在房间里」；
 *   - 走 /api/sessions/{code}/files，文件随房间销毁而删除；
 *   - 上传成功后还要通过 WebSocket 广播一张卡片，别人才看得到。
 *
 * 所以这里是「先 HTTP 传文件，再 WS 发消息」两步 ——
 * 文件本体不走 WebSocket（大文件会把内存和带宽都吃掉），
 * WS 只负责告诉大家「有这么个东西，去这个地址下」。
 */
function sendFilesToRoom(fileList) {
  const list = Array.from(fileList || []);
  if (!list.length) return;

  if (!state.room) {
    toast('请先创建或加入房间，再发文件', 'err');
    return;
  }
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
    toast('尚未连接到房间，文件未发送', 'err');
    return;
  }

  const box = $('#chatUploadList');
  box.hidden = false;

  // 与文件仓库同样限量并发：聊天室传的也常常是几百 MB 的安装包。
  const jobs = list.map((file) => {
    const ui = makeUploadRow(file, box);
    setText(ui.pct, '等待中');
    return () => sendOneChatFile(file, ui);
  });
  runWithConcurrency(jobs, UPLOAD_CONCURRENCY);
}

/** 上传单个聊天文件并广播卡片。 */
function sendOneChatFile(file, ui) {
  const { row, fill, pct } = ui;
  const box = row.parentElement;

  let markDone;
  const finished = new Promise((r) => { markDone = r; });

  const finish = (delay) => {
    setTimeout(() => {
      row.remove();
      if (!box.children.length) box.hidden = true;
    }, delay);
  };

  const form = new FormData();
  form.append('file', file, file.name);

  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/sessions/' + encodeURIComponent(state.room) + '/files', true);

  xhr.upload.addEventListener('progress', (e) => {
    if (!e.lengthComputable) return;
    const p = Math.round((e.loaded / e.total) * 100);
    fill.style.width = p + '%';
    setText(pct, p + '%');
  });

  // 同文件仓库：100% 只是发完了，服务端还要落盘，显式提示「保存中」。
  xhr.upload.addEventListener('load', () => {
    fill.style.width = '100%';
    setText(pct, '保存中…');
    row.classList.add('is-saving');
  });

  xhr.addEventListener('load', () => {
    let data = {};
    try { data = JSON.parse(xhr.responseText); } catch (_) {}
    row.classList.remove('is-saving');

    if (xhr.status >= 200 && xhr.status < 300) {
      fill.style.width = '100%';
      setText(pct, '完成');
      row.classList.add('is-done');
      finish(900);

      // 第二步：告诉房间里所有人。
      // 本地乐观渲染那张卡片，随后服务端广播回来会靠 cid 原地替换，
      // 所以这里发的 cid 必须和乐观节点的 cid 一致。
      const cid = 'c' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
      state.pendingSelf.add(cid);
      appendMessage({
        id: 'local-' + cid,
        cid,
        type: 'file',
        content: data.name,
        sender: state.myName || '我',
        senderId: state.myId,
        sentAt: Date.now(),
        fileName: data.name,
        fileSize: data.size,
        fileText: data.sizeText,
        fileUrl: data.downloadUrl,
        // 本地上传时服务端还没回卡片，先用扩展名猜一次 ——
        // 猜对了图片直接以缩略图形出现，不会「先卡片后变图」闪一下。
        isImage: IMAGE_EXTS.indexOf(extOf(data.name)) >= 0
      }, true, true);

      try {
        state.ws.send(JSON.stringify({ type: 'file', fileId: data.id, cid }));
      } catch (_) {
        state.pendingSelf.delete(cid);
        const el = document.getElementById('msg-local-' + cid);
        if (el) el.remove();
        toast('文件已上传，但通知发送失败', 'err');
      }
    } else {
      row.classList.add('is-error');
      setText(pct, '失败');
      toast(data.error || ('发送失败：' + file.name), 'err');
      finish(3200);
    }
  });

  xhr.addEventListener('error', () => {
    row.classList.remove('is-saving');
    row.classList.add('is-error');
    setText(pct, '失败');
    toast('网络错误，发送失败：' + file.name, 'err');
    finish(3200);
  });

  xhr.addEventListener('loadend', () => markDone());

  xhr.send(form);
  return finished;
}

/* ------------------------------------------------------------------ 8. 绑定与初始化 */

function bindEvents() {
  // ---- 房间 ----
  $('#btnCreate').addEventListener('click', openCreate);
  $('#btnJoin').addEventListener('click', joinSession);
  $('#btnCreateMissing').addEventListener('click', createMissingSession);
  $('#btnLeave').addEventListener('click', () => leaveRoom(false));

  // ---- 创建房间弹窗 ----
  $('#createForm').addEventListener('submit', (e) => {
    e.preventDefault();
    doCreate();
  });
  $('#btnCloseCreate').addEventListener('click', closeCreate);
  $('#createModal').addEventListener('mousedown', (e) => {
    if (e.target === $('#createModal')) closeCreate();
  });
  // 房间号输入：只保留大写字母数字，和加入框保持一致的手感。
  const createCode = $('#createCode');
  createCode.addEventListener('input', () => {
    const pos = createCode.selectionStart;
    createCode.value = createCode.value.toUpperCase().replace(/[^A-Z0-9]/g, '');
    createCode.setSelectionRange(pos, pos);
  });
  $$('#ttlOptions .ttl-item').forEach((b) => {
    b.addEventListener('click', () => pickTTL(b));
  });

  // 房间号输入：自动大写、Enter 直接加入。
  const joinCode = $('#joinCode');
  joinCode.addEventListener('input', () => {
    resetJoinFeedback();
    const pos = joinCode.selectionStart;
    joinCode.value = joinCode.value.toUpperCase().replace(/[^A-Z0-9]/g, '');
    joinCode.setSelectionRange(pos, pos);
  });
  joinCode.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); joinSession(); }
  });

  // 点击会话码复制。
  $('#sessionCode').addEventListener('click', async () => {
    const ok = await copyText(state.room || '');
    toast(ok ? '会话码已复制：' + state.room : '复制失败', ok ? 'ok' : 'err');
  });

  // ---- 消息输入 ----
  const input = $('#msgInput');
  input.addEventListener('input', () => {
    autoGrow(input);
    syncSendButton();
  });

  // ---- 粘贴截图 ----
  //
  // 粘的是图片就拦下来，放进待发送预览条，**不立即上传** ——
  // 剪贴板里常常是误复制的内容，给一次看见缩略图再决定发不发的机会。
  //
  // 粘的是纯文字 / 链接就**什么都不做**，让浏览器按默认行为把文本
  // 插进 textarea：手动 insertText 会丢掉光标位置、撤销历史和输入法状态。
  input.addEventListener('paste', (e) => {
    const files = pickClipboardImages(e.clipboardData);
    if (!files.length) return;

    // 只有确实拿到了图片才 preventDefault —— 否则会把纯文本粘贴也吃掉。
    e.preventDefault();

    if (!state.room) {
      toast('请先创建或加入房间，再粘贴图片', 'err');
      return;
    }
    attachClipboardImages(files);
  });

  input.addEventListener('keydown', (e) => {
    // Enter 发送，Shift+Enter 换行。中文输入法组合中不触发。
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendMessage();
    }
  });
  $('#btnSend').addEventListener('click', sendMessage);

  // ---- 主题切换 ----
  $$('#themeSwitch [data-theme-opt]').forEach((btn) => {
    btn.addEventListener('click', () => applyTheme(btn.dataset.themeOpt, true));
  });

  // ---- 文件 ----
  $('#btnUpload').addEventListener('click', pickFiles);
  $('#btnNewFolder').addEventListener('click', () => openFolderModal('create'));
  $$('#viewSwitch .view-btn').forEach((btn) => {
    btn.addEventListener('click', () => setFileView(btn.dataset.view));
  });

  // ---- 文件夹弹窗 ----
  $('#folderForm').addEventListener('submit', submitFolderModal);
  $('#btnCloseFolder').addEventListener('click', closeFolderModal);
  $('#folderModal').addEventListener('mousedown', (e) => {
    if (e.target === $('#folderModal')) closeFolderModal();
  });

  // ---- TXT 预览弹窗 ----
  $('#btnCloseText').addEventListener('click', closeTextModal);
  $('#btnCopyText').addEventListener('click', async () => {
    const body = $('#textPreviewBody');
    const ok = await copyText(body ? body.textContent : '');
    toast(ok ? '已复制文本内容' : '复制失败，请手动选择', ok ? 'ok' : 'err');
  });
  $('#textModal').addEventListener('mousedown', (e) => {
    if (e.target === $('#textModal')) closeTextModal();
  });

  // ---- 文件搜索（纯前端过滤，不请求服务端）----
  const searchInput = $('#fileSearch');
  if (searchInput) {
    // 边打边过滤：列表已经在本地，重绘一次的成本远低于一次往返，
    // 也就不需要防抖 —— 防抖反而会让打字快的人觉得卡。
    searchInput.addEventListener('input', applySearch);
    // Esc 清空搜索。这个键在输入框里没有别的语义，抢来当「退出搜索」最顺手。
    // 停止冒泡：否则会顺带触发全局的 Esc（关弹窗），一次按键办两件事。
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && searchInput.value) {
        e.preventDefault();
        e.stopPropagation();
        clearSearch();
      }
    });
  }
  const clearBtn = $('#btnClearSearch');
  if (clearBtn) clearBtn.addEventListener('click', clearSearch);

  $('#btnLoginFromLock').addEventListener('click', () => {
    // 首次部署时直接切到注册，省掉一次点击。
    const needSetup = state.status && state.status.needSetup;
    openAuth(needSetup ? 'register' : 'login');
  });

  // ---- 首次部署引导 ----
  $('#btnSetup').addEventListener('click', () => openAuth('register'));

  // ---- 登录 / 注册切换 ----
  $$('#authTabs .tab').forEach((t) => {
    t.addEventListener('click', () => setAuthMode(t.dataset.tab));
  });

  // ---- 管理员面板 ----
  $('#btnAdmin').addEventListener('click', openAdmin);
  $('#btnCloseAdmin').addEventListener('click', closeAdmin);
  $('#regToggle').addEventListener('change', (e) => {
    toggleRegistration(e.target.checked);
  });

  $('#fileInput').addEventListener('change', (e) => {
    uploadFiles(e.target.files);
    e.target.value = '';   // 允许重复选择同一个文件
  });

  // ---- 聊天室发文件 ----
  $('#btnChatFile').addEventListener('click', () => {
    if (!state.room) {
      toast('请先创建或加入房间', 'err');
      return;
    }
    $('#chatFileInput').click();
  });
  $('#chatFileInput').addEventListener('change', (e) => {
    sendFilesToRoom(e.target.files);
    e.target.value = '';
  });

  // ---- 拖拽上传（左右两侧各自独立）----
  //
  // 两个面板语义不同，所以各挂一套：
  //   左（实时传输区）→ 发到当前房间，凭房间号，随房间销毁
  //   右（文件仓库）  → 存进永久仓库，需登录
  // 用同一个工厂函数装配，避免两份几乎相同的 dragenter/leave/drop 代码走偏。
  setupDropZone($('#livePane'), {
    // 未进房间时不能接收：给中性提示而不是高亮，免得用户以为松手能成。
    canDrop: () => !!state.room,
    hint: '请先创建或加入房间，再拖入文件',
    onDrop: (files) => sendFilesToRoom(files)
  });

  setupDropZone($('#filesPane'), {
    canDrop: () => !!state.user,
    hint: '请先登录后再上传到文件仓库',
    // 未登录时顺手把登录框弹出来，省一次点击。
    onReject: () => openAuth('login'),
    onDrop: (files) => uploadFiles(files)
  });

  // 阻止在页面其他位置误拖导致浏览器直接打开文件。
  ['dragover', 'drop'].forEach((t) => {
    document.addEventListener(t, (e) => {
      const inPane = e.target instanceof Element
        && (e.target.closest('#livePane') || e.target.closest('#filesPane'));
      if (!inPane) e.preventDefault();
    });
  });

  // ---- 弹窗 ----
  $('#loginForm').addEventListener('submit', doLogin);
  $('#btnCloseLogin').addEventListener('click', closeLogin);
  $('#loginModal').addEventListener('mousedown', (e) => {
    if (e.target === $('#loginModal')) closeLogin();
  });
  $('#adminModal').addEventListener('mousedown', (e) => {
    if (e.target === $('#adminModal')) closeAdmin();
  });

  // ---- 图片灯箱 ----
  // 点背景（而不是点在图上或工具条上）才关；左键拖动后松手不触发关闭。
  const lb = $('#lightbox');
  if (lb) {
    const stage = $('#lightboxStage');
    const img = $('#lightboxImg');
    lb.addEventListener('click', (e) => {
      if (lightboxView.suppressClick) {
        lightboxView.suppressClick = false;
        return;
      }
      if (e.target === lb || (stage && e.target === stage)) closeLightbox();
    });
    if (stage && img) {
      stage.addEventListener('wheel', (e) => {
        if (lb.hidden || !img.naturalWidth) return;
        e.preventDefault();
        let dy = e.deltaY;
        if (e.deltaMode === 1) dy *= 16;
        else if (e.deltaMode === 2) dy *= 400;
        dy = Math.max(-240, Math.min(240, dy));
        if (!dy) return;
        const factor = Math.exp(-dy * 0.0018);
        setLightboxScale(lightboxView.scale * factor, false, { x: e.clientX, y: e.clientY });
      }, { passive: false });

      img.addEventListener('dragstart', (e) => e.preventDefault());
      img.addEventListener('pointerdown', (e) => {
        if (lb.hidden || !img.naturalWidth) return;
        if (e.pointerType === 'mouse' && e.button !== 0) return;
        e.preventDefault();
        lightboxView.dragging = true;
        lightboxView.pointerId = e.pointerId;
        lightboxView.startX = e.clientX;
        lightboxView.startY = e.clientY;
        lightboxView.startTx = lightboxView.tx;
        lightboxView.startTy = lightboxView.ty;
        lightboxView.moved = false;
        stage.classList.add('is-dragging');
        try { img.setPointerCapture(e.pointerId); } catch (_) {}
      });
      img.addEventListener('pointermove', (e) => {
        if (!lightboxView.dragging || e.pointerId !== lightboxView.pointerId) return;
        const dx = e.clientX - lightboxView.startX;
        const dy = e.clientY - lightboxView.startY;
        if (Math.abs(dx) > 3 || Math.abs(dy) > 3) {
          lightboxView.moved = true;
          lightboxView.fit = false;
        }
        lightboxView.tx = lightboxView.startTx + dx;
        lightboxView.ty = lightboxView.startTy + dy;
        clampLightboxOffset();
        applyLightboxTransform();
      });
      const endLightboxDrag = (e) => {
        if (!lightboxView.dragging) return;
        if (e && lightboxView.pointerId !== null && e.pointerId !== lightboxView.pointerId) return;
        const wasMoved = lightboxView.moved;
        const pid = lightboxView.pointerId;
        lightboxView.dragging = false;
        lightboxView.pointerId = null;
        lightboxView.moved = false;
        stage.classList.remove('is-dragging');
        if (pid !== null && img.hasPointerCapture && img.hasPointerCapture(pid)) {
          try { img.releasePointerCapture(pid); } catch (_) {}
        }
        if (wasMoved) {
          lightboxView.suppressClick = true;
          setTimeout(() => { lightboxView.suppressClick = false; }, 0);
        }
      };
      img.addEventListener('pointerup', endLightboxDrag);
      img.addEventListener('pointercancel', endLightboxDrag);
      img.addEventListener('lostpointercapture', endLightboxDrag);
    }
    const closeBtn = $('#btnCloseLightbox');
    if (closeBtn) closeBtn.addEventListener('click', closeLightbox);
    $('#btnZoomOut').addEventListener('click', () => setLightboxScale(lightboxView.scale / 1.25));
    $('#btnZoomIn').addEventListener('click', () => setLightboxScale(lightboxView.scale * 1.25));
    $('#btnZoomFit').addEventListener('click', () => setLightboxScale(1, true));
    $('#btnZoomActual').addEventListener('click', () => setLightboxScale(1, false, { reset: true }));
    window.addEventListener('resize', () => setLightboxScale(lightboxView.scale, lightboxView.fit));
  }

  // ---- 快捷键 ----
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    // 灯箱与预览弹窗优先级最高，先关顶层再关下层。
    if (!$('#lightbox').hidden) { closeLightbox(); return; }
    if (!$('#textModal').hidden) { closeTextModal(); return; }
    if (!$('#folderModal').hidden) { closeFolderModal(); return; }
    if (!$('#adminModal').hidden) { closeAdmin(); return; }
    if (!$('#createModal').hidden) { closeCreate(); return; }
    if (!$('#loginModal').hidden) closeLogin();
  });

  // ---- 离开页面时提示（仅在会话中） ----
  window.addEventListener('beforeunload', (e) => {
    if (state.room) {
      e.preventDefault();
      e.returnValue = '';
    }
  });

  // 初始状态：无文字无图 → 按钮灰着。
  // （HTML 里写死了 disabled，这里必须显式同步一次，否则永远解不开。
  //   历史上这个按钮就一直是灰的 —— 靠它发送等于没有发送按钮。）
  syncSendButton();
  renderAttachments();
}

function hasFiles(e) {
  if (draggingItem) return false;
  if (!e.dataTransfer) return false;
  const t = e.dataTransfer.types;
  if (!t) return false;
  return Array.prototype.indexOf.call(t, 'Files') >= 0;
}

/**
 * 给一个面板装配拖拽接收能力。
 *
 * 为什么要有 dragDepth 计数：dragenter / dragleave 会在鼠标掠过每个子元素时
 * 反复触发，直接用布尔量会在子元素之间闪断，高亮就抖。计数到 0 才真正退出。
 *
 * @param pane    面板根元素（需 position: relative，遮罩靠它定位）
 * @param opts.canDrop   () => boolean，当前是否能接收
 * @param opts.onDrop    (FileList) => void
 * @param opts.onReject  () => void，不能接收却松手时的反馈（可选）
 * @param opts.hint      不能接收时的提示文案
 */
function setupDropZone(pane, opts) {
  if (!pane) return;
  let depth = 0;

  const clear = () => {
    depth = 0;
    pane.classList.remove('is-dropping', 'is-dropping-off');
  };

  pane.addEventListener('dragenter', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    depth++;
    const ok = opts.canDrop();
    pane.classList.toggle('is-dropping', ok);
    pane.classList.toggle('is-dropping-off', !ok);
  });

  pane.addEventListener('dragover', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    // 不能接收时给 none，让光标本身也表达「放不下」，比只看遮罩更早被察觉。
    e.dataTransfer.dropEffect = opts.canDrop() ? 'copy' : 'none';
  });

  pane.addEventListener('dragleave', () => {
    depth = Math.max(0, depth - 1);
    if (depth === 0) pane.classList.remove('is-dropping', 'is-dropping-off');
  });

  pane.addEventListener('drop', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    const ok = opts.canDrop();
    clear();

    if (!ok) {
      toast(opts.hint || '当前无法接收文件', 'err');
      if (opts.onReject) opts.onReject();
      return;
    }
    opts.onDrop(e.dataTransfer.files);
  });
}

/** 从 localStorage 恢复上次的会话码、视图模式与界面主题。 */
function restoreLocal() {
  // 不自动重连会话 —— 会话是临时的，自动恢复会让用户莫名其妙进到旧房间。
  // 只把上次的码填进输入框，方便一键回去。
  let theme = 'auto';
  try {
    const c = localStorage.getItem(LS_CODE);
    const el = $('#joinCode');
    if (c && el) el.value = c;
    const v = localStorage.getItem(LS_FILE_VIEW);
    if (v === 'card' || v === 'list') state.fileView = v;
    const t = localStorage.getItem(LS_THEME);
    if (t === 'light' || t === 'dark') theme = t;
  } catch (_) {}
  syncViewButtons();
  applyTheme(theme, false);
}

/**
 * 每秒刷新房间存活倒计时。
 *
 * 用 1 秒而不是 1 分钟：最后一分钟内要能看着秒数跳动，
 * 用户才知道"确实快到期了"而不是页面卡住了。
 * 页面不可见时跳过（标签页在后台没人看，省点 CPU）。
 */
function startRelativeTimer() {
  setInterval(() => {
    if (!state.room || document.hidden) return;
    if (state.expiresAt) {
      renderTTL();
      if (state.expiresAt - Date.now() <= 0) handleRoomExpired();
    }
  }, 1000);
}

async function init() {
  bindEvents();
  restoreLocal();
  autoGrow($('#msgInput'));
  startRelativeTimer();

  setStatus('正在读取状态…', '');

  await loadMe();

  // 拉一次运行状态，顺便显示在底栏（版本 / 存储占用）。
  try {
    const res = await fetch('/api/status');
    if (res.ok) {
      state.status = await res.json();
      renderSetup();
      setStatus('LAN Share ' + (state.status.version || '') + ' · 就绪',
        '仓库 ' + (state.status.permanentNum || 0) + ' 个文件 · ' + humanSize(state.status.permanentUse || 0));

      // 首次部署：直接把注册表单推到用户面前，不用他自己找入口。
      if (state.status.needSetup) {
        setStatus('首次部署 · 请创建管理员账号', '');
        setTimeout(() => openAuth('register'), 260);
      }
    }
  } catch (_) {
    setStatus('无法连接服务', '');
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
