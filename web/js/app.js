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

/** 节流的 requestAnimationFrame 包装，用于滚动到底部。 */
let scrollScheduled = false;
function scheduleScrollBottom() {
  if (scrollScheduled) return;
  scrollScheduled = true;
  requestAnimationFrame(() => {
    scrollScheduled = false;
    const box = $('#messages');
    if (box) box.scrollTop = box.scrollHeight;
  });
}

/** 判断消息区是否已经在底部附近（用于决定要不要自动跟随滚动）。 */
function nearBottom(box, slack) {
  if (!box) return true;
  return box.scrollHeight - box.scrollTop - box.clientHeight <= (slack == null ? 80 : slack);
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

  authMode: 'login',     // login | register
  status: null,          // /api/status 最近一次结果

  room: null,            // 当前会话码
  ws: null,              // WebSocket 实例
  wsReady: false,
  myName: '',            // 本连接的显示名 —— 完全由服务端分配（客户端 IP），见 hello.self
  reconnectTimer: null,
  reconnectDelay: 1000,
  manualLeave: false,    // 主动退出时不要自动重连
  members: [],           // 在线成员
  pendingSelf: new Set(), // 本地已乐观渲染、等服务端确认的 cid 集合
  // expiresAt = 0 表示「还没从服务端拿到存活信息」或「房间没有到期时间」。
  // 房间现在必定有时长（没有不限时选项），所以正常路径上它会很快被 hello 填上。
  expiresAt: 0,          // 房间到期时刻（Unix 毫秒）
  ttlMinutes: 0          // 房间总存活时长（分钟），用于算剩余比例
};

const LS_CODE = 'lanshare.code';

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

/* ------------------------------------------------------------------ 4. 网络状态指示 */

function setNet(kind, text) {
  const dot = $('#netPill .dot');
  if (dot) dot.className = 'dot dot-' + kind;
  setText($('#netText'), text);
}

/* ------------------------------------------------------------------ 5. 登录态 */

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

  // 锁屏卡片的文案也随状态变化。
  // 注意用词：遮罩只挡上传，浏览和下载对所有人开放 —— 文案不能说成「仓库需要登录」。
  if (needSetup) {
    setText($('#lockTitle'), '先创建一个账号');
    setText($('#lockText'), '这是第一次部署，第一个注册的人将成为管理员。浏览和下载无需登录。');
    setText($('#btnLoginFromLock'), '创建管理员账号');
  } else if (!s.registrationOpen) {
    setText($('#lockTitle'), '登录后可上传');
    setText($('#lockText'), '浏览和下载无需登录。登录后可以上传、管理自己的文件。注册已关闭，请联系管理员开通。');
    setText($('#btnLoginFromLock'), '立即登录');
  } else {
    setText($('#lockTitle'), '登录后可上传');
    setText($('#lockText'), '浏览和下载无需登录。登录或注册后即可上传、管理自己的文件。');
    setText($('#btnLoginFromLock'), '立即登录');
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
  const btn = $('#btnDoCreate');
  const code = trim($('#createCode').value).toUpperCase();

  // 前端先做一遍格式校验，省一次往返；服务端仍会再校验一次。
  if (code && !/^[A-Z0-9]{4,8}$/.test(code)) {
    setText($('#createMsg'), '房间号需为 4 - 8 位字母或数字');
    return;
  }

  btn.disabled = true;
  setText($('#createMsg'), '');
  try {
    const res = await fetch('/api/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code, ttlMinutes: currentTTL() })
    });
    const data = await res.json().catch(() => ({}));
    if (res.status === 409) {
      setText($('#createMsg'), '该房间号已被占用，换一个吧');
      return;
    }
    if (!res.ok) {
      setText($('#createMsg'), data.error || '创建房间失败');
      return;
    }
    closeCreate();
    toast('房间已创建：' + data.code, 'ok');
    enterRoom(data.code, data);
  } catch (_) {
    setText($('#createMsg'), '网络错误，无法创建房间');
  } finally {
    btn.disabled = false;
  }
}

/** 加入会话（先查一次是否存在，给出更友好的错误）。 */
async function joinSession() {
  const code = trim($('#joinCode').value).toUpperCase();
  const hint = $('#joinHint');
  setText(hint, '');

  if (!/^[A-Z0-9]{4,8}$/.test(code)) {
    setText(hint, '房间号为 4 - 8 位字母或数字');
    return;
  }

  const btn = $('#btnJoin');
  btn.disabled = true;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(code));
    if (res.status === 404) {
      setText(hint, '房间不存在、已过期或已结束');
      return;
    }
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      setText(hint, data.error || '无法加入该房间');
      return;
    }
    enterRoom(code);
  } catch (_) {
    setText(hint, '网络错误，请重试');
  } finally {
    btn.disabled = false;
  }
}

/** 进入房间：更新 UI 并建立 WebSocket。 */
function enterRoom(code, info) {
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
  state.members = [];
  state.pendingSelf.clear();
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
  state.expiresAt = 0;
  state.ttlMinutes = 0;
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
  setNet('idle', '未连接');
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
  setNet('warn', '连接中…');

  ws.addEventListener('open', () => {
    state.wsReady = true;
    state.reconnectDelay = 1000;
    setNet('ok', '已连接');
    setStatus('会话 ' + state.room + ' · 已连接', '');
    setText($('#btnSend'), '发送');
  });

  ws.addEventListener('message', (ev) => {
    let data;
    try { data = JSON.parse(ev.data); } catch (_) { return; }
    handleWSEvent(data);
  });

  ws.addEventListener('close', () => {
    state.wsReady = false;
    state.ws = null;
    if (state.manualLeave || !state.room) {
      setNet('idle', '未连接');
      return;
    }
    setNet('bad', '已断开');
    scheduleReconnect();
  });

  ws.addEventListener('error', () => {
    // 具体错误交给 close 处理，这里只更新指示。
    if (state.room && !state.manualLeave) setNet('bad', '连接异常');
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
      // 服务端告知本连接的显示名。昵称留空时服务端会用 IP 兜底，
      // 所以这里必须采用服务端的值，否则判断「这条是不是我发的」会永远不成立。
      if (data.self) state.myName = data.self;
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
  if (!content) return;

  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
    toast('尚未连接，消息未发出', 'err');
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
  setText($('#btnSend'), '发送');
  input.focus();
}

/**
 * 构造一张文件卡片。
 *
 * 卡片本身就是一个 <a>，点哪儿都能下载 —— 文件消息里「下载」是唯一主操作，
 * 没必要再放一个需要精确点击的按钮。
 */
function buildFileCard(m) {
  const a = document.createElement('a');
  a.className = 'msg-file';
  a.href = m.fileUrl || ('/api/chat-files/' + m.fileId);
  a.setAttribute('download', m.fileName || '');
  a.title = '点击下载 ' + (m.fileName || '');

  const icon = document.createElement('span');
  icon.className = 'msg-file-icon';
  const ext = extOf(m.fileName || '');
  setText(icon, ext ? ext.slice(0, 4) : 'FILE');

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
 * 渲染一条消息。
 *
 * @param m         消息体
 * @param animate   是否播放入场动画
 * @param optimistic 是否是本地乐观渲染（此刻还没有服务端 id）
 * @param mineHint  显式指定「是我发的」。服务端回显替换乐观节点时会用到：
 *                  那一刻 cid 已从 pendingSelf 移除，靠 cid 或 sender 都判不出来。
 *
 * mine 的判定顺序：显式提示 > 乐观渲染 > cid 命中 > sender 比较。
 * 最后一条只是兜底（比如刷新后收到历史回放），不能单独依赖：
 * 同一台机器开两个标签页时服务端给出的是同一个 IP，
 * 光比 sender 分不清「我发的」和「另一个标签页发的」。
 */
function appendMessage(m, animate, optimistic, mineHint) {
  const box = $('#messages');
  if (!box) return;

  const mine = mineHint === true
    || optimistic === true
    || (!!m.cid && state.pendingSelf.has(m.cid))
    || (!!state.myName && m.sender === state.myName);
  const follow = nearBottom(box);

  const el = document.createElement('div');
  el.className = 'msg'
    + (mine ? ' is-mine' : '')
    + (m.type === 'link' ? ' is-link' : '')
    + (m.type === 'file' ? ' is-file' : '');
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
    body.appendChild(buildFileCard(m));
  } else {
    // 用 textContent 而不是 innerHTML：用户内容永不参与 HTML 解析。
    body.textContent = m.content;
  }

  // 工具栏（复制 / 打开）
  const tools = document.createElement('div');
  tools.className = 'msg-tools';

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

/** 渲染未登录遮罩。
 *
 * 遮罩**只盖住上传按钮**（`inset: 44px -8px -8px auto`，靠 CSS 定位）。
 * 列表、下载、删除按钮都在遮罩之外 ——
 * 未登录时能看能下，只是不能传，所以不该把整块区域糊住。
 */
function renderLock() {
  const need = !state.user;
  const overlay = $('#lockOverlay');
  if (overlay) overlay.hidden = !need;
  const btn = $('#btnUpload');
  if (btn) btn.disabled = need;
}

/** 拉取文件列表。 */
async function loadFiles() {
  renderLock();

  try {
    const res = await fetch('/api/files?kind=permanent');
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      // 列表已对所有人开放，401 理论上不会出现；真出现了就当空列表处理，
      // 别把「未登录」渲染成错误提示。
      state.files = [];
      renderFiles();
      toast(data.error || '读取文件列表失败', 'err');
      return;
    }
    state.files = data.files || [];
    renderFiles();
    setText($('#usageText'), state.files.length + ' 个文件 · 共 ' + (data.totalText || '0 B'));
    setText($('#usageHint'), state.user ? '长期保存在路由器数据分区' : '浏览与下载无需登录');
  } catch (_) {
    setStatus('文件列表读取失败', '');
  }
}

function renderFiles() {
  const body = $('#fileBody');
  const empty = $('#fileEmpty');
  if (!body) return;

  body.innerHTML = '';
  const list = state.files || [];
  if (empty) empty.hidden = list.length > 0;

  list.forEach((f) => {
    const tr = document.createElement('tr');
    tr.dataset.id = f.id;

    // 文件名 + 类型图标
    const tdName = document.createElement('td');
    const wrap = document.createElement('div');
    wrap.className = 'cell-name';

    const ext = extOf(f.name);
    const icon = document.createElement('span');
    icon.className = 'ftype';
    icon.dataset.t = ext;
    setText(icon, ext ? ext.slice(0, 4) : 'FILE');

    const textWrap = document.createElement('div');
    textWrap.style.minWidth = '0';
    const nm = document.createElement('div');
    nm.className = 'fname';
    nm.title = f.name;
    setText(nm, f.name);
    textWrap.appendChild(nm);

    wrap.append(icon, textWrap);
    tdName.appendChild(wrap);

    // 大小
    const tdSize = document.createElement('td');
    tdSize.className = 'cell-size';
    setText(tdSize, f.sizeText || humanSize(f.size));

    // 时间
    const tdTime = document.createElement('td');
    tdTime.className = 'cell-time';
    setText(tdTime, fmtTime(f.createdAt));

    // 上传者
    const tdOwner = document.createElement('td');
    tdOwner.className = 'cell-owner';
    setText(tdOwner, f.owner || '—');

    // 操作：下载对所有人开放；删除只给「能删的人」渲染按钮。
    const tdAct = document.createElement('td');
    tdAct.className = 'cell-act';

    const dl = document.createElement('a');
    dl.className = 'btn btn-icon';
    dl.href = f.downloadUrl;
    dl.title = '下载';
    dl.setAttribute('aria-label', '下载 ' + f.name);
    dl.download = f.name;
    dl.innerHTML = '<svg viewBox="0 0 20 20" width="16" height="16">'
      + '<path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>'
      + '<path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>';
    tdAct.appendChild(dl);

    if (canDeleteFile(f)) {
      const del = document.createElement('button');
      del.className = 'btn btn-icon is-danger';
      del.title = '删除';
      del.setAttribute('aria-label', '删除 ' + f.name);
      del.innerHTML = '<svg viewBox="0 0 20 20" width="16" height="16">'
        + '<path d="M4.5 6h11M8 6V4.6a1 1 0 011-1h2a1 1 0 011 1V6M6.5 6l.7 9.2a1.4 1.4 0 001.4 1.3h2.8a1.4 1.4 0 001.4-1.3L13.5 6" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      del.addEventListener('click', () => deleteFile(f, tr));
      tdAct.appendChild(del);
    }

    tr.append(tdName, tdSize, tdTime, tdOwner, tdAct);
    body.appendChild(tr);
  });
}

/** 当前身份能否删掉这个文件。
 *
 * 与服务端 handleDelete 的规则一一对应，避免给用户点一个必然 403 的按钮。
 * 这只是**界面上的礼貌**，真正的闸门在服务端 —— 前端隐藏按钮不算安全措施。
 */
function canDeleteFile(f) {
  if (!state.user) return false;
  if (state.user.isAdmin) return true;
  return !!f.owner && f.owner === state.user.username;
}

async function deleteFile(f, tr) {
  if (!window.confirm('确定删除「' + f.name + '」？此操作不可恢复。')) return;

  tr.classList.add('is-busy');
  try {
    const res = await fetch('/api/files/' + f.id, { method: 'DELETE' });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(data.error || '删除失败', 'err');
      tr.classList.remove('is-busy');
      return;
    }
    toast('已删除：' + f.name, 'ok');
    state.files = state.files.filter((x) => x.id !== f.id);
    renderFiles();
    setStatus('已删除 ' + f.name, '');
  } catch (_) {
    toast('网络错误，删除失败', 'err');
    tr.classList.remove('is-busy');
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

  list.forEach((file) => uploadOne(file, box));
}

/** 上传单个文件，用 XMLHttpRequest 以获得真实上传进度。 */
function uploadOne(file, box) {
  const row = document.createElement('div');
  row.className = 'up-item';
  row.innerHTML = '<div class="up-line">'
    + '<span class="up-name"></span><span class="up-pct">0%</span>'
    + '</div><div class="up-bar"><div class="up-fill"></div></div>';
  setText($('.up-name', row), file.name);
  box.appendChild(row);

  const fill = $('.up-fill', row);
  const pct = $('.up-pct', row);

  const form = new FormData();
  // 顺序有讲究：必须先把 kind / name 放进 FormData，再放文件。
  // 服务端是流式解析 multipart —— 遇到文件 part 就立刻开始边收边写，
  // 排在文件后面的字段它已经读不到了，kind 拿不到就会按最严格的
  // 「永久文件（需登录）」处理。
  form.append('kind', 'permanent');
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

  xhr.send(form);
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
  list.forEach((file) => sendOneChatFile(file, box));
}

/** 上传单个聊天文件并广播卡片。 */
function sendOneChatFile(file, box) {
  const row = document.createElement('div');
  row.className = 'up-item';
  row.innerHTML = '<div class="up-line">'
    + '<span class="up-name"></span><span class="up-pct">0%</span>'
    + '</div><div class="up-bar"><div class="up-fill"></div></div>';
  setText($('.up-name', row), file.name);
  box.appendChild(row);

  const fill = $('.up-fill', row);
  const pct = $('.up-pct', row);

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
        sentAt: Date.now(),
        fileName: data.name,
        fileSize: data.size,
        fileText: data.sizeText,
        fileUrl: data.downloadUrl
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

  xhr.send(form);
}

/* ------------------------------------------------------------------ 8. 绑定与初始化 */

function bindEvents() {
  // ---- 房间 ----
  $('#btnCreate').addEventListener('click', openCreate);
  $('#btnJoin').addEventListener('click', joinSession);
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
    setText($('#btnSend'), '发送');
  });
  input.addEventListener('keydown', (e) => {
    // Enter 发送，Shift+Enter 换行。中文输入法组合中不触发。
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendMessage();
    }
  });
  $('#btnSend').addEventListener('click', sendMessage);

  // ---- 文件 ----
  $('#btnUpload').addEventListener('click', pickFiles);
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

  // ---- 快捷键 ----
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
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
}

function hasFiles(e) {
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

/** 从 localStorage 恢复上次的会话码。 */
function restoreLocal() {
  // 不自动重连会话 —— 会话是临时的，自动恢复会让用户莫名其妙进到旧房间。
  // 只把上次的码填进输入框，方便一键回去。
  try {
    const c = localStorage.getItem(LS_CODE);
    const el = $('#joinCode');
    if (c && el) el.value = c;
  } catch (_) {}
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

  setNet('warn', '检测中…');
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
    setNet('bad', '离线');
  }

  if (!state.room) setNet('idle', '未连接');
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
