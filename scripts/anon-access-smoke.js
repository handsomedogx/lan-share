#!/usr/bin/env node
/**
 * anon-access-smoke.js —— 验证「文件仓库可匿名浏览/下载，但不能匿名删除」
 *
 * 权限模型（本次改动）：
 *   列表   GET    /api/files        → 开放（无需登录）
 *   下载   GET    /api/files/{id}   → 开放（无需登录）
 *   上传   POST   /api/files        → 需登录
 *   删除   DELETE /api/files/{id}   → 需登录 + 本人（或管理员）
 *
 * 用法：
 *   node scripts/anon-access-smoke.js [baseUrl]
 *
 * 默认 baseUrl = http://127.0.0.1:18099
 */

'use strict';

const http = require('http');

const BASE = process.argv[2] || 'http://127.0.0.1:18099';

const ADMIN_USER = process.env.LS_ADMIN_USER || 'anonadmin';
const ADMIN_PASS = process.env.LS_ADMIN_PASS || 'pw123456';
const OTHER_USER = 'anonother';
const OTHER_PASS = 'pw123456';

let pass = 0;
let fail = 0;

function ok(cond, label, extra) {
  if (cond) {
    pass++;
    console.log('  PASS  ' + label);
  } else {
    fail++;
    console.log('  FAIL  ' + label + (extra ? '  → ' + extra : ''));
  }
}

function section(title) {
  console.log('\n--- ' + title + ' ---');
}

// ---------------------------------------------------------------- HTTP 工具

function request(method, path, { body, headers = {}, cookies } = {}) {
  return new Promise((resolve, reject) => {
    const url = new URL(path, BASE);
    const hdrs = { ...headers };
    if (cookies) hdrs.Cookie = cookies;
    let payload = null;
    if (body !== undefined) {
      payload = Buffer.isBuffer(body) ? body : Buffer.from(JSON.stringify(body));
      hdrs['Content-Type'] = hdrs['Content-Type'] || 'application/json';
      hdrs['Content-Length'] = payload.length;
    }
    const req = http.request(
      { method, hostname: url.hostname, port: url.port, path: url.pathname + url.search, headers: hdrs },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          const raw = Buffer.concat(chunks);
          let json = null;
          try { json = JSON.parse(raw.toString('utf8')); } catch (_) {}
          resolve({ status: res.statusCode, headers: res.headers, body: raw, json });
        });
      }
    );
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

function grabCookie(res) {
  const sc = res.headers['set-cookie'];
  if (!sc || !sc.length) return null;
  return sc.map((c) => c.split(';')[0]).join('; ');
}

/** 上传一个文件。返回 {status, json, cookie}。cookie 可为 null（匿名）。 */
async function upload(name, content, cookie) {
  const boundary = '----anon' + Date.now() + Math.random().toString(16).slice(2);
  const parts = [];
  parts.push(Buffer.from(
    '--' + boundary + '\r\n' +
    'Content-Disposition: form-data; name="kind"\r\n\r\n' +
    'permanent\r\n'
  ));
  parts.push(Buffer.from(
    '--' + boundary + '\r\n' +
    'Content-Disposition: form-data; name="file"; filename="' + name + '"\r\n' +
    'Content-Type: application/octet-stream\r\n\r\n'
  ));
  parts.push(Buffer.from(content));
  parts.push(Buffer.from('\r\n--' + boundary + '--\r\n'));
  const payload = Buffer.concat(parts);

  const res = await request('POST', '/api/files', {
    body: payload,
    headers: {
      'Content-Type': 'multipart/form-data; boundary=' + boundary,
      'Content-Length': payload.length,
    },
    cookies: cookie || undefined,
  });
  return { status: res.status, json: res.json, body: res.body };
}

// ---------------------------------------------------------------- 主流程

(async () => {
  console.log('目标服务: ' + BASE);

  // ---- 准备两个账号（第一个注册的成为管理员）----
  section('准备账号');
  let adminCookie = null;
  let otherCookie = null;

  {
    const r = await request('POST', '/api/auth/register', {
      body: { username: ADMIN_USER, password: ADMIN_PASS },
    });
    if (r.status === 200) {
      adminCookie = grabCookie(r);
      ok(!!adminCookie, '注册管理员 ' + ADMIN_USER);
    } else {
      // 已存在 → 登录
      const l = await request('POST', '/api/auth/login', {
        body: { username: ADMIN_USER, password: ADMIN_PASS },
      });
      adminCookie = grabCookie(l);
      ok(l.status === 200, '管理员 ' + ADMIN_USER + ' 登录', 'status=' + l.status + ' ' + JSON.stringify(l.json));
    }
  }

  if (!adminCookie) {
    console.log('\n无法获得管理员会话，终止。');
    process.exit(1);
  }

  // 打开注册开关，好加第二个普通用户。
  {
    const r = await request('PUT', '/api/admin/settings', {
      body: { registrationOpen: true },
      cookies: adminCookie,
    });
    ok(r.status === 200, '管理员打开注册开关', 'status=' + r.status);
  }

  {
    const r = await request('POST', '/api/auth/register', {
      body: { username: OTHER_USER, password: OTHER_PASS },
    });
    if (r.status === 200) {
      otherCookie = grabCookie(r);
      ok(!!otherCookie, '注册普通用户 ' + OTHER_USER);
    } else {
      const l = await request('POST', '/api/auth/login', {
        body: { username: OTHER_USER, password: OTHER_PASS },
      });
      otherCookie = grabCookie(l);
      ok(l.status === 200, '普通用户 ' + OTHER_USER + ' 登录', 'status=' + l.status);
    }
  }

  // 关掉注册，恢复原状。
  await request('PUT', '/api/admin/settings', {
    body: { registrationOpen: false },
    cookies: adminCookie,
  });

  // ---- 清空遗留的测试文件 ----
  section('清理遗留测试数据');
  {
    const r = await request('GET', '/api/files?kind=permanent', { cookies: adminCookie });
    const list = (r.json && r.json.files) || [];
    let removed = 0;
    for (const f of list) {
      if (String(f.name).startsWith('anon-')) {
        await request('DELETE', '/api/files/' + f.id, { cookies: adminCookie });
        removed++;
      }
    }
    ok(true, '清理 ' + removed + ' 个遗留测试文件');
  }

  // ---- 造一个真实文件（管理员上传）----
  section('准备一个仓库文件');
  const CONTENT = Buffer.from('anon-access-smoke-payload-' + Date.now() + '\n');
  const FILE_NAME = 'anon-sample.txt';
  let fileId = null;
  {
    const r = await upload(FILE_NAME, CONTENT, adminCookie);
    ok(r.status === 200 && r.json && r.json.id, '管理员上传成功', 'status=' + r.status + ' ' + JSON.stringify(r.json));
    fileId = r.json && r.json.id;
  }

  if (!fileId) {
    console.log('\n无法创建测试文件，终止。');
    process.exit(1);
  }

  // ---- 1. 匿名列表 ----
  section('1. 匿名列表（应放行）');
  {
    const r = await request('GET', '/api/files?kind=permanent'); // 不带 cookie
    ok(r.status === 200, '匿名 GET /api/files → 200', 'status=' + r.status);
    const list = (r.json && r.json.files) || [];
    ok(list.some((f) => f.id === fileId), '匿名列表里能看到那个文件');
  }

  // ---- 2. 匿名下载 ----
  section('2. 匿名下载（应放行，且内容逐字节一致）');
  {
    const r = await request('GET', '/api/files/' + fileId); // 不带 cookie
    ok(r.status === 200, '匿名 GET /api/files/{id} → 200', 'status=' + r.status);
    ok(r.body.equals(CONTENT), '匿名下载内容逐字节一致', 'len=' + r.body.length + ' want=' + CONTENT.length);
    const cd = r.headers['content-disposition'] || '';
    ok(cd.includes('attachment'), 'Content-Disposition 是 attachment');
  }

  // ---- 3. 匿名上传（必须拒绝）----
  section('3. 匿名上传（应 401）');
  {
    const r = await upload('anon-should-fail.txt', Buffer.from('nope'), null);
    ok(r.status === 401, '匿名 POST /api/files → 401', 'status=' + r.status);
  }

  // ---- 4. 匿名删除（核心：必须拒绝）----
  section('4. 匿名删除（应 401 —— 本轮最关键的一条）');
  {
    const r = await request('DELETE', '/api/files/' + fileId); // 不带 cookie
    ok(r.status === 401, '匿名 DELETE /api/files/{id} → 401', 'status=' + r.status);

    // 反向验证：文件必须还在。只断言 401 不足以证明「没删掉」。
    const chk = await request('GET', '/api/files/' + fileId);
    ok(chk.status === 200, '匿名删除后文件仍然存在（反向验证）', 'status=' + chk.status);
    ok(chk.body.equals(CONTENT), '文件内容未被破坏');
  }

  // ---- 5. 别人删除（应 403）----
  section('5. 普通用户删别人的文件（应 403）');
  {
    const r = await request('DELETE', '/api/files/' + fileId, { cookies: otherCookie });
    ok(r.status === 403, '他人 DELETE → 403', 'status=' + r.status);

    const chk = await request('GET', '/api/files/' + fileId);
    ok(chk.status === 200, '403 之后文件仍存在（反向验证）');
  }

  // ---- 6. 本人 / 管理员删除（应成功）----
  section('6. 有权限者删除（应成功）');
  {
    // 先造一个普通用户自己的文件，验证「本人可删」。
    const mine = Buffer.from('owned-by-other-' + Date.now() + '\n');
    const up = await upload('anon-other-owns.txt', mine, otherCookie);
    ok(up.status === 200 && up.json && up.json.id, '普通用户上传自己的文件', 'status=' + up.status);
    const myId = up.json && up.json.id;

    if (myId) {
      const r = await request('DELETE', '/api/files/' + myId, { cookies: otherCookie });
      ok(r.status === 200, '本人 DELETE 自己的文件 → 200', 'status=' + r.status);
    }

    // 管理员删管理员的文件。
    const r = await request('DELETE', '/api/files/' + fileId, { cookies: adminCookie });
    ok(r.status === 200, '管理员 DELETE → 200', 'status=' + r.status);

    const chk = await request('GET', '/api/files/' + fileId);
    ok(chk.status === 404, '删除后确实取不到了（404）', 'status=' + chk.status);
  }

  // ---- 7. 静态页面：未登录提示的文案与形态 ----
  section('7. 登录页文案与服务端渲染的 HTML');
  {
    const r = await request('GET', '/');
    const html = r.body.toString('utf8');
    ok(r.status === 200, '取到首页 HTML');
    ok(!html.includes('文件仓库需要登录'), '页面里已无「文件仓库需要登录」字样');

    // 只切片看 #lockOverlay 容器，避开其它位置的合理用词。
    // 用固定宽度切窗：容器从浮卡换成横幅后内部结构变了，
    // 以前靠 'lock-card' 定位结尾的写法会算出空切片，断言全部失真。
    const start = html.indexOf('id="lockOverlay"');
    ok(start >= 0, '页面里能找到 #lockOverlay');
    if (start >= 0) {
      const slice = html.slice(start, start + 700);
      ok(slice.includes('登录后可上传'), '提示条标题是「登录后可上传」', slice.slice(0, 120));
      ok(slice.includes('浏览和下载无需登录'), '提示条文案说明了下载无需登录');
    }

    // CSS 层回归守卫：提示条一旦回到绝对定位，就会重新压住列表最右侧的下载列。
    const cssRes = await request('GET', '/css/style.css');
    if (cssRes.status === 200) {
      const css = cssRes.body.toString('utf8');
      const i = css.indexOf('.lock-banner');
      ok(i >= 0, 'CSS 里有 .lock-banner');
      if (i >= 0) {
        const rule = css.slice(i, css.indexOf('}', i));
        ok(!/position:\s*absolute/.test(rule), '提示条在文档流里，不是绝对定位浮层', rule.replace(/\s+/g, ' ').slice(0, 120));
      }

      // 同一层再守两条今天踩过的布局：
      //   1) 对方消息气泡要自己收缩（align-self），否则会被 .messages 的
      //      align-items:stretch 拉满整行；这里顺带排除 short-message 又被名字行撑宽的老毛病。
      //   2) 窄屏必须是「文档单滚动容器」：html/body 交出固定高度 + .messages 不再自建滚动条。
      //      一旦回退，手指从聊天区下拉就又被内层吃掉（手机端页面不动）。
      const bodyRule = css.slice(css.indexOf('.msg-body'), css.indexOf('}', css.indexOf('.msg-body')));
      ok(/align-self:\s*flex-start/.test(bodyRule), '消息气泡按内容收缩（.msg-body align-self: flex-start）',
        bodyRule.replace(/\s+/g, ' ').slice(0, 120));
      const narrow = css.slice(css.indexOf('@media (max-width: 1000px)'));
      ok(/html,\s*body\s*\{\s*height:\s*auto/.test(narrow), '窄屏让文档自己滚（html/body height: auto）');
      ok(/\.messages\s*\{[^}]*overflow:\s*visible/.test(narrow), '窄屏消息区不再是内层滚动容器');
    } else {
      ok(false, '取到 /css/style.css 以校验提示条定位', 'status=' + cssRes.status);
    }
  }

  // ---- 8. 公开下载链接真的能裸用（模拟「把链接发出去」）----
  section('8. 模拟分享链接：全新连接、无任何凭据');
  {
    const up = await upload('anon-share-link.txt', Buffer.from('shared-file\n'), adminCookie);
    const id = up.json && up.json.id;
    ok(!!id, '管理员上传一个用于分享的文件');

    if (id) {
      const r = await request('GET', '/api/files/' + id, { headers: { 'User-Agent': 'curl/8.0' } });
      ok(r.status === 200, '裸连接直接下载 → 200', 'status=' + r.status);
      ok(r.body.toString('utf8') === 'shared-file\n', '内容正确');
      await request('DELETE', '/api/files/' + id, { cookies: adminCookie });
    }
  }

  console.log('\n========================================');
  console.log('PASS=' + pass + '  FAIL=' + fail);
  console.log('========================================');
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('测试脚本异常: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
