/**
 * 文件仓库 sha256 去重的端到端验证。
 *
 * 为什么单独一个脚本：仓库上传需要登录，而干净库上第一个注册的用户才是管理员，
 * 且 ws-smoke.js 跑在可能已关闭注册的库上（会 SKIP 掉仓库那一段）。
 * 这里自己起一个**全新的库**，走完整链路：
 *
 *   注册 → 上传 A → 再上传同内容但不同文件名 → 断言两条记录共用磁盘文件
 *   → 删掉第一条 → 断言文件仍在（第二条还能下）
 *   → 删掉第二条 → 断言磁盘文件真的没了
 *
 * 用法（server 已在 127.0.0.1:18080 跑起来）：
 *   node scripts/dedup-smoke.js
 *
 * 需要指向一个**干净的**数据根目录（LANSHARE_ROOT=... 起服务），
 * 且最好在 ws-smoke.js **之前**跑 —— 注册在第一个用户之后会自动关闭，
 * 而本脚本要注册两个用户。
 */
'use strict';

const http = require('http');
const HOST = process.env.LS_HOST || '127.0.0.1';
const PORT = Number(process.env.LS_PORT || 18080);
const ROOT = process.env.LS_ROOT || '.devdata-dedup';

let pass = 0;
let fail = 0;
// 管理员 Cookie：注册开关一旦关闭，后面要靠它把注册打开再建第二个用户。
let adminCookie = null;

function check(name, ok, detail) {
  if (ok) {
    console.log('  [PASS] ' + name);
    pass++;
  } else {
    console.log('  [FAIL] ' + name + (detail ? '  ' + detail : ''));
    fail++;
  }
}

function httpJson(method, path, data) {
  return new Promise((resolve, reject) => {
    const body = data ? JSON.stringify(data) : null;
    const req = http.request(
      {
        host: HOST,
        port: PORT,
        method,
        path,
        headers: body
          ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) }
          : {}
      },
      (res) => {
        let buf = '';
        res.on('data', (c) => { buf += c; });
        res.on('end', () => {
          let parsed = null;
          try { parsed = JSON.parse(buf); } catch (_) {}
          const sc = res.headers['set-cookie'];
          resolve({
            status: res.statusCode,
            body: parsed,
            raw: buf,
            cookie: sc ? String(sc[0]).split(';')[0] : null
          });
        });
      }
    );
    req.on('error', reject);
    if (body) req.write(body);
    req.end();
  });
}

/** 带 Cookie 的 multipart 上传。 */
function uploadFile(path, filename, content, cookie) {
  return new Promise((resolve, reject) => {
    const boundary = '----lsdedup' + Math.random().toString(36).slice(2);
    const head = Buffer.from(
      '--' + boundary + '\r\n' +
      'Content-Disposition: form-data; name="file"; filename="' + filename + '"\r\n' +
      'Content-Type: application/octet-stream\r\n\r\n'
    );
    const tail = Buffer.from('\r\n--' + boundary + '--\r\n');
    const payload = Buffer.concat([head, Buffer.isBuffer(content) ? content : Buffer.from(content), tail]);

    const headers = {
      'Content-Type': 'multipart/form-data; boundary=' + boundary,
      'Content-Length': payload.length
    };
    if (cookie) headers.Cookie = cookie;

    const req = http.request({ host: HOST, port: PORT, method: 'POST', path, headers }, (res) => {
      let buf = '';
      res.on('data', (c) => { buf += c; });
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(buf); } catch (_) {}
        resolve({ status: res.statusCode, body: parsed, raw: buf });
      });
    });
    req.on('error', reject);
    req.write(payload);
    req.end();
  });
}

/** 拿原始响应（下载校验用）。 */
function rawGet(path, cookie) {
  return new Promise((resolve, reject) => {
    const headers = cookie ? { Cookie: cookie } : {};
    const req = http.request({ host: HOST, port: PORT, method: 'GET', path, headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks) }));
    });
    req.on('error', reject);
    req.end();
  });
}

/** 带 Cookie 的 GET JSON。 */
function httpJsonAuth(path, cookie) {
  return new Promise((resolve, reject) => {
    const headers = cookie ? { Cookie: cookie } : {};
    const req = http.request({ host: HOST, port: PORT, method: 'GET', path, headers }, (res) => {
      let buf = '';
      res.on('data', (c) => { buf += c; });
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(buf); } catch (_) {}
        resolve({ status: res.statusCode, body: parsed, raw: buf });
      });
    });
    req.on('error', reject);
    req.end();
  });
}

function del(path, cookie) {
  return new Promise((resolve, reject) => {
    const headers = cookie ? { Cookie: cookie } : {};
    const req = http.request({ host: HOST, port: PORT, method: 'DELETE', path, headers }, (res) => {
      let buf = '';
      res.on('data', (c) => { buf += c; });
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(buf); } catch (_) {}
        resolve({ status: res.statusCode, body: parsed, raw: buf });
      });
    });
    req.on('error', reject);
    req.end();
  });
}

/**
 * 直接数磁盘上的文件个数 —— 断言的最终依据。
 *
 * 只看数据库记录是不够的：记录数对了但盘上多留一份，正是这个功能要消灭的问题。
 */
function countStoredFiles(dir) {
  const fs = require('fs');
  const path = require('path');
  try {
    return fs.readdirSync(dir).filter((n) => !n.endsWith('.part')).length;
  } catch (_) {
    return -1;
  }
}

(async () => {
  console.log('=== 文件仓库 sha256 去重端到端测试 ===\n');
  console.log('数据根目录: ' + ROOT + '\n');

  const permDir = ROOT + '/files/permanent';

  // ---- 0. 前置：确保有一个管理员账号在手 ----
  //
  // 分两种情况：
  //   a) 干净库 —— 第一个注册的用户即管理员，直接注册；
  //   b) 已有用户的库 —— 注册已关闭（403），用脚本约定的固定账号登录。
  // 这样脚本既能跑在干净库上，也能在同一个库上反复跑。
  const ADMIN_USER = process.env.LS_ADMIN_USER || 'dedupadmin';
  const ADMIN_PASS = process.env.LS_ADMIN_PASS || 'pw123456';

  async function tryRegister(u, p) {
    return httpJson('POST', '/api/auth/register', { username: u, password: p });
  }
  async function tryLogin(u, p) {
    return httpJson('POST', '/api/auth/login', { username: u, password: p });
  }

  let reg = await tryRegister(ADMIN_USER, ADMIN_PASS);
  let cookie = (reg.status === 200 || reg.status === 201) ? reg.cookie : null;
  if (!cookie) {
    // 注册关着 —— 说明库里已有用户，试试约定账号。
    reg = await tryLogin(ADMIN_USER, ADMIN_PASS);
    cookie = reg.status === 200 ? reg.cookie : null;
  }
  if (!cookie) {
    console.log('  [SKIP] 既注册不上也登不进（register=' + reg.status + '）。');
    console.log('        请对全新的 LANSHARE_ROOT 运行，或用 LS_ADMIN_USER/LS_ADMIN_PASS');
    console.log('        指定一个已存在账号。');
    console.log('\n=== 汇总: PASS=' + pass + ' FAIL=' + fail + ' ===');
    process.exit(fail === 0 ? 0 : 1);
  }
  adminCookie = cookie;
  check('拿到管理员会话 Cookie', !!cookie);

  // 打开注册，方便后面建第二个用户做隔离测试。
  const openReg = await new Promise((resolve, reject) => {
    const body = JSON.stringify({ registrationOpen: true });
    const req = http.request(
      {
        host: HOST, port: PORT, method: 'PUT', path: '/api/admin/settings',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body),
          Cookie: cookie
        }
      },
      (res) => {
        let buf = '';
        res.on('data', (c) => { buf += c; });
        res.on('end', () => resolve({ status: res.statusCode, raw: buf }));
      }
    );
    req.on('error', reject);
    req.write(body);
    req.end();
  });
  check('管理员可以打开注册开关', openReg.status === 200,
    'status=' + openReg.status + ' raw=' + openReg.raw.slice(0, 160));

  // 起点：把该账号名下已有的仓库文件清空，让「磁盘文件数」这个断言有意义。
  // （同一个库反复跑时，上一轮可能留下残余。）
  const existing = await httpJsonAuth('/api/files?kind=permanent', cookie);
  const leftovers = ((existing.body && existing.body.files) || []);
  for (const f of leftovers) {
    await del('/api/files/' + f.id, cookie);
  }
  check('清空起点残留', countStoredFiles(permDir) === 0,
    '清理了 ' + leftovers.length + ' 条后仍有 ' + countStoredFiles(permDir) + ' 个文件');

  // ---- 1. 上传第一份 ----
  const CONTENT = Buffer.from('去重测试内容，这份内容应当只落一次盘。\n');
  const up1 = await uploadFile('/api/files', 'first-name.txt', CONTENT, cookie);
  check('第一次上传成功', up1.status === 200, 'status=' + up1.status + ' raw=' + up1.raw.slice(0, 160));
  const id1 = up1.body && up1.body.id;
  const sha1 = up1.body && up1.body.sha256;
  check('返回了 sha256', !!sha1, 'sha256=' + sha1);
  check('第一次上传后磁盘有 1 个文件', countStoredFiles(permDir) === 1,
    '实际 ' + countStoredFiles(permDir));

  // ---- 3. 上传同内容、不同文件名 ----
  const up2 = await uploadFile('/api/files', 'second-name-不同.txt', CONTENT, cookie);
  check('第二次上传（同内容）成功', up2.status === 200, 'status=' + up2.status);
  const id2 = up2.body && up2.body.id;
  check('两次 sha256 相同', !!(up2.body && up2.body.sha256 === sha1));
  check('两条记录 id 不同（各有各的记录）', !!id1 && !!id2 && id1 !== id2,
    'id1=' + id1 + ' id2=' + id2);

  // 核心断言：磁盘上仍然只有一份。
  const after2 = countStoredFiles(permDir);
  check('去重生效：磁盘上仍只有 1 个文件', after2 === 1, '实际 ' + after2 + ' 个文件');

  // ---- 4. 两条记录都能下载，且内容正确 ----
  const dl1 = await rawGet('/api/files/' + id1, cookie);
  const dl2 = await rawGet('/api/files/' + id2, cookie);
  check('第一条记录可下载', dl1.status === 200, 'status=' + dl1.status);
  check('第二条记录可下载（指向同一份磁盘文件）', dl2.status === 200, 'status=' + dl2.status);
  check('第一条内容正确', dl1.body.equals(CONTENT));
  check('第二条内容正确', dl2.body.equals(CONTENT));

  // 文件名各自独立 —— 这是「共享磁盘、私有记录」的价值所在。
  const list = await httpJsonAuth('/api/files?kind=permanent', cookie);
  const names = ((list.body && list.body.files) || []).map((f) => f.name);
  check('列表里两条记录各自保留原始文件名',
    names.indexOf('first-name.txt') >= 0 && names.indexOf('second-name-不同.txt') >= 0,
    JSON.stringify(names));

  // ---- 5. 删第一条：文件必须仍在 ----
  const d1 = await del('/api/files/' + id1, cookie);
  check('删除第一条记录返回 200', d1.status === 200, 'status=' + d1.status + ' raw=' + d1.raw.slice(0, 160));
  check('删除一条后磁盘文件仍在（还有记录引用）', countStoredFiles(permDir) === 1,
    '实际 ' + countStoredFiles(permDir));
  const dl2b = await rawGet('/api/files/' + id2, cookie);
  check('第二条仍可下载（共享文件没被误删）', dl2b.status === 200, 'status=' + dl2b.status);
  check('第二条内容仍然正确', dl2b.body.equals(CONTENT));

  // ---- 6. 删第二条：现在磁盘文件该消失了 ----
  const d2 = await del('/api/files/' + id2, cookie);
  check('删除第二条记录返回 200', d2.status === 200, 'status=' + d2.status);
  const afterDel = countStoredFiles(permDir);
  check('引用归零后磁盘文件被删除', afterDel === 0, '实际 ' + afterDel + ' 个文件');

  // ---- 7. 不同上传者不共享（隔离性）----
  // 换个账号上传同样的内容：应当**不**复用前一个人的磁盘文件。
  // 注册开关已在步骤 0 里由管理员打开。
  const uname2 = 'dedup' + Math.floor(Math.random() * 900000 + 100000);
  const reg2 = await httpJson('POST', '/api/auth/register', {
    username: uname2,
    password: 'pw123456'
  });
  if (reg2.status === 200 || reg2.status === 201) {
    // 第 6 步结束时磁盘已被清空，这里从零开始，状态清晰。
    check('隔离性测试起点：磁盘为空', countStoredFiles(permDir) === 0,
      '实际 ' + countStoredFiles(permDir));

    // 甲传 1 份。
    const a1 = await uploadFile('/api/files', 'alice.txt', CONTENT, cookie);
    check('甲上传成功', a1.status === 200, 'status=' + a1.status);
    check('磁盘 1 份（甲的）', countStoredFiles(permDir) === 1,
      '实际 ' + countStoredFiles(permDir));

    // 乙传同样内容 —— 必须**另占一份**，不能复用甲的。
    const b1 = await uploadFile('/api/files', 'bob.txt', CONTENT, reg2.cookie);
    check('乙上传同内容成功', b1.status === 200, 'status=' + b1.status);
    check('跨用户不复用：磁盘变成 2 份', countStoredFiles(permDir) === 2,
      '实际 ' + countStoredFiles(permDir) + ' 个文件（应为 2）');

    // 甲再传一次同内容 —— 这次是复用甲自己的，磁盘不该变成 3 份。
    const a2 = await uploadFile('/api/files', 'alice-2.txt', CONTENT, cookie);
    check('甲再次上传同内容成功', a2.status === 200, 'status=' + a2.status);
    check('同用户复用：磁盘仍是 2 份', countStoredFiles(permDir) === 2,
      '实际 ' + countStoredFiles(permDir) + ' 个文件（应为 2）');

    // 甲删掉自己的一条 —— 乙的文件绝不能受影响。
    const dA1 = await del('/api/files/' + (a1.body && a1.body.id), cookie);
    check('甲删掉自己的一条', dA1.status === 200, 'status=' + dA1.status);
    check('甲删一条后：甲那份仍在（还有 a2 引用）、乙那份也在', countStoredFiles(permDir) === 2,
      '实际 ' + countStoredFiles(permDir) + ' 个文件');

    // 甲删掉最后一条 —— 甲那份磁盘才消失，乙那份不受影响。
    const dA2 = await del('/api/files/' + (a2.body && a2.body.id), cookie);
    check('甲删掉自己最后一条', dA2.status === 200, 'status=' + dA2.status);
    check('引用归零后甲的磁盘文件被删，乙那份保留', countStoredFiles(permDir) === 1,
      '实际 ' + countStoredFiles(permDir) + ' 个文件（应只剩乙那份）');

    // 乙删自己的 —— 磁盘归零。
    const dB1 = await del('/api/files/' + (b1.body && b1.body.id), reg2.cookie);
    check('乙删掉自己的文件', dB1.status === 200, 'status=' + dB1.status);
    check('全部删完，磁盘为空', countStoredFiles(permDir) === 0,
      '实际 ' + countStoredFiles(permDir));
  } else {
    console.log('  [SKIP] 第二个用户注册失败（status=' + reg2.status + '）');
  }

  console.log('\n=== 汇总: PASS=' + pass + ' FAIL=' + fail + ' ===');
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('测试异常: ' + e.message);
  process.exit(1);
});
