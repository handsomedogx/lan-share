/**
 * 一次性 WebSocket 冒烟测试：验证 cid 回传、去重链路、身份归属，
 * 以及房间号 / 存活时长。
 *
 * 为什么用 Node 而不是 Go：Node 自带 crypto/net，手写最小 RFC 6455
 * 客户端即可，零依赖，也不用为测试再往仓库里塞一个 Go 测试客户端。
 * 而且它能直接构造自定义请求头 —— 验证「反代场景下显示名取真实来源」
 * 这条恰恰需要伪造 X-Forwarded-For。
 *
 * 用法： node scripts/ws-smoke.js [端口] [会话码]
 *   会话码留空则自动创建一个。
 */

const net = require('net');
const crypto = require('crypto');
const http = require('http');

const PORT = Number(process.argv[2] || 18080);
const HOST = '127.0.0.1';

const GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

function httpJson(method, path, body) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const req = http.request({
      host: HOST, port: PORT, method, path,
      headers: data
        ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) }
        : {}
    }, (res) => {
      let buf = '';
      res.on('data', (c) => { buf += c; });
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(buf); } catch (_) {}
        // 注册/登录会下发会话 Cookie，后面上传仓库文件要用。
        const setCookie = res.headers['set-cookie'];
        const cookie = setCookie ? String(setCookie[0]).split(';')[0] : null;
        resolve({ status: res.statusCode, body: parsed, raw: buf, cookie });
      });
    });
    req.on('error', reject);
    if (data) req.write(data);
    req.end();
  });
}

/** 发一个 multipart/form-data 上传（单文件字段名 file），可带 Cookie。 */
function httpMultipart(path, filename, content, cookie) {
  const boundary = '----lanshare' + crypto.randomBytes(8).toString('hex');
  const head = Buffer.from(
    `--${boundary}\r\n`
    + `Content-Disposition: form-data; name="file"; filename="${filename}"\r\n`
    + 'Content-Type: application/octet-stream\r\n\r\n', 'utf8');
  const tail = Buffer.from(`\r\n--${boundary}--\r\n`, 'utf8');
  const payload = Buffer.concat([head, content, tail]);

  return new Promise((resolve, reject) => {
    const headers = {
      'Content-Type': 'multipart/form-data; boundary=' + boundary,
      'Content-Length': payload.length
    };
    if (cookie) headers['Cookie'] = cookie;
    const req = http.request({
      host: HOST, port: PORT, method: 'POST', path, headers
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const buf = Buffer.concat(chunks);
        let parsed = null;
        try { parsed = JSON.parse(buf.toString('utf8')); } catch (_) {}
        resolve({ status: res.statusCode, body: parsed, raw: buf.toString('utf8') });
      });
    });
    req.on('error', reject);
    req.write(payload);
    req.end();
  });
}

/** 带 Cookie 的 multipart 上传（上传到文件仓库用）。 */
function httpMultipartLogin(path, filename, content, cookie) {
  return httpMultipart(path, filename, content, cookie);
}

/** 发一个请求并把响应体当文本拿回来（用于检查内嵌的 HTML 内容）。 */
function httpGetText(path) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: HOST, port: PORT, method: 'GET', path }, (res) => {
      let buf = '';
      res.setEncoding('utf8');
      res.on('data', (c) => { buf += c; });
      res.on('end', () => resolve({ status: res.statusCode, text: buf }));
    });
    req.on('error', reject);
    req.end();
  });
}

/** 发一个请求并把响应体当二进制拿回来（用于下载校验）。 */
function httpRaw(method, path) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: HOST, port: PORT, method, path }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({
        status: res.statusCode,
        buffer: Buffer.concat(chunks),
        headers: res.headers
      }));
    });
    req.on('error', reject);
    req.end();
  });
}

/** 带 Cookie 的原始请求。 */
function httpRawLogin(method, path, cookie) {
  return new Promise((resolve, reject) => {
    const headers = cookie ? { Cookie: cookie } : {};
    const req = http.request({ host: HOST, port: PORT, method, path, headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({
        status: res.statusCode,
        buffer: Buffer.concat(chunks),
        headers: res.headers
      }));
    });
    req.on('error', reject);
    req.end();
  });
}

/** 最小 RFC 6455 客户端：只做文本帧收发，够验证用。 */
class WS {
  /**
   * @param path    握手路径，例如 /ws/session/ABCD
   * @param headers 额外请求头。用于模拟反向代理透传真实来源
   *                （X-Forwarded-For / X-Real-IP）—— 浏览器的
   *                WebSocket 构造函数设不了头，只有手写握手才能造出来。
   */
  constructor(path, headers) {
    this.path = path;
    this.headers = headers || null;
    this.sock = null;
    this.buf = Buffer.alloc(0);
    this.handlers = [];
    this.fragOp = 0;
    this.fragChunks = [];
    this.ready = false;
  }

  connect() {
    return new Promise((resolve, reject) => {
      const key = crypto.randomBytes(16).toString('base64');
      let extra = '';
      if (this.headers) {
        for (const k of Object.keys(this.headers)) extra += k + ': ' + this.headers[k] + '\r\n';
      }
      this.sock = net.connect(PORT, HOST, () => {
        this.sock.write(
          `GET ${this.path} HTTP/1.1\r\n`
          + `Host: ${HOST}:${PORT}\r\n`
          + 'Upgrade: websocket\r\n'
          + 'Connection: Upgrade\r\n'
          + `Sec-WebSocket-Key: ${key}\r\n`
          + 'Sec-WebSocket-Version: 13\r\n'
          + extra
          + '\r\n'
        );
      });
      this.sock.on('error', reject);
      this.sock.on('data', (d) => this._onData(d, key, resolve, reject));
    });
  }

  _onData(d, key, resolve, reject) {
    this.buf = Buffer.concat([this.buf, d]);

    if (!this.ready) {
      const idx = this.buf.indexOf('\r\n\r\n');
      if (idx < 0) return;
      const head = this.buf.slice(0, idx).toString('latin1');
      if (!/^HTTP\/1\.1 101/.test(head)) {
        reject(new Error('握手失败: ' + head.split('\r\n')[0]));
        return;
      }
      const expect = crypto.createHash('sha1').update(key + GUID).digest('base64');
      if (!head.includes(expect)) {
        reject(new Error('Sec-WebSocket-Accept 不匹配'));
        return;
      }
      this.buf = this.buf.slice(idx + 4);
      this.ready = true;
      resolve();
    }
    this._drain();
  }

  _drain() {
    for (;;) {
      if (this.buf.length < 2) return;
      const b0 = this.buf[0], b1 = this.buf[1];
      const fin = (b0 & 0x80) !== 0;
      const op = b0 & 0x0f;
      let len = b1 & 0x7f;
      let off = 2;
      if (len === 126) {
        if (this.buf.length < 4) return;
        len = this.buf.readUInt16BE(2); off = 4;
      } else if (len === 127) {
        if (this.buf.length < 10) return;
        len = Number(this.buf.readBigUInt64BE(2)); off = 10;
      }
      if (this.buf.length < off + len) return;
      const payload = this.buf.slice(off, off + len);
      this.buf = this.buf.slice(off + len);

      if (op === 0x8) { this.sock.end(); return; }
      if (op === 0x9) { this._frame(0xA, payload); continue; }
      if (op === 0xA) continue;

      if (op === 0x0) {
        this.fragChunks.push(payload);
        if (fin) {
          const full = Buffer.concat(this.fragChunks);
          this.fragChunks = [];
          this._emit(this.fragOp, full.toString('utf8'));
        }
        continue;
      }
      if (!fin) { this.fragOp = op; this.fragChunks = [payload]; continue; }
      this._emit(op, payload.toString('utf8'));
    }
  }

  _emit(op, text) {
    if (op !== 0x1) return;
    let obj = null;
    try { obj = JSON.parse(text); } catch (_) {}
    if (obj) this.handlers.forEach((h) => h(obj));
  }

  _frame(op, payload) {
    const mask = crypto.randomBytes(4);
    const p = Buffer.isBuffer(payload) ? payload : Buffer.from(payload, 'utf8');
    let header;
    if (p.length < 126) {
      header = Buffer.alloc(2);
      header[1] = 0x80 | p.length;
    } else if (p.length < 65536) {
      header = Buffer.alloc(4);
      header[1] = 0x80 | 126;
      header.writeUInt16BE(p.length, 2);
    } else {
      header = Buffer.alloc(10);
      header[1] = 0x80 | 127;
      header.writeBigUInt64BE(BigInt(p.length), 2);
    }
    header[0] = 0x80 | op;
    const masked = Buffer.alloc(p.length);
    for (let i = 0; i < p.length; i++) masked[i] = p[i] ^ mask[i & 3];
    this.sock.write(Buffer.concat([header, mask, masked]));
  }

  send(obj) { this._frame(0x1, JSON.stringify(obj)); }

  onEvent(fn) { this.handlers.push(fn); }

  close() {
    try { this._frame(0x8, Buffer.alloc(0)); } catch (_) {}
    try { this.sock.end(); } catch (_) {}
  }
}

/** 等一个满足条件的事件，超时则失败。 */
function waitFor(ws, pred, label, ms = 3000) {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('超时等待: ' + label)), ms);
    const h = (ev) => {
      if (pred(ev)) {
        clearTimeout(t);
        ws.handlers = ws.handlers.filter((x) => x !== h);
        resolve(ev);
      }
    };
    ws.onEvent(h);
  });
}

let pass = 0, fail = 0;
function check(name, cond, detail) {
  if (cond) { console.log(`  [PASS] ${name}`); pass++; }
  else { console.log(`  [FAIL] ${name}${detail ? ' — ' + detail : ''}`); fail++; }
}

(async () => {
  console.log('=== LAN Share WebSocket cid 去重冒烟测试 ===\n');

  const roomCode = process.argv[3];

  // ---- 0. 房间号与存活时长 ----
  console.log('--- 0. 自定义房间号 / 存活时长 ---');
  const custom = 'QA' + Math.floor(Math.random() * 900 + 100);   // 5 位
  const r0 = await httpJson('POST', '/api/sessions', { code: custom, ttlMinutes: 10 });
  check('自定义房间号创建成功', r0.status === 200 && r0.body && r0.body.code === custom,
    'status=' + r0.status + ' body=' + JSON.stringify(r0.body));
  check('返回 TTL = 10 分钟', r0.body && r0.body.ttlMinutes === 10,
    'ttl=' + (r0.body && r0.body.ttlMinutes));
  check('返回 expiresAt 时间戳', !!(r0.body && r0.body.expiresAt),
    'expiresAt=' + (r0.body && r0.body.expiresAt));
  const ttl10 = r0.body && r0.body.expiresAt;
  check('expiresAt 距现在约 10 分钟',
    ttl10 && Math.abs(ttl10 - Date.now() - 600000) < 15000,
    'diff=' + (ttl10 ? ttl10 - Date.now() : 'n/a'));

  const dup = await httpJson('POST', '/api/sessions', { code: custom, ttlMinutes: 60 });
  check('重复房间号返回 409', dup.status === 409, 'status=' + dup.status);

  const bad = await httpJson('POST', '/api/sessions', { code: 'ab', ttlMinutes: 60 });
  check('过短房间号返回 400', bad.status === 400, 'status=' + bad.status);

  const badChar = await httpJson('POST', '/api/sessions', { code: 'a-b!', ttlMinutes: 60 });
  check('非法字符房间号返回 400', badChar.status === 400, 'status=' + badChar.status);

  const badTTL = await httpJson('POST', '/api/sessions', { code: '', ttlMinutes: 7 });
  check('非白名单 TTL 返回 400', badTTL.status === 400, 'status=' + badTTL.status);

  const noTtl = await httpJson('POST', '/api/sessions', { code: 'NT' + Math.floor(Math.random() * 900 + 100) });
  check('不带 ttlMinutes 用默认值 60', noTtl.status === 200 && noTtl.body.ttlMinutes === 60,
    'ttl=' + (noTtl.body && noTtl.body.ttlMinutes));

  // 0 曾经表示「不限时」，现已从白名单移除：房间必须有确定的终点
  // （否则聊天文件失去回收触发点）。这里正向验证它被拒绝。
  const inf = await httpJson('POST', '/api/sessions',
    { code: 'IF' + Math.floor(Math.random() * 900 + 100), ttlMinutes: 0 });
  check('ttlMinutes=0 已不再合法（不限时选项已移除）', inf.status === 400,
    'status=' + inf.status + ' body=' + JSON.stringify(inf.body));

  const info = await httpJson('GET', '/api/sessions/' + custom);
  check('会话信息可查，带 remainingSeconds', info.status === 200
    && typeof info.body.remainingSeconds === 'number', JSON.stringify(info.body));

  const info404 = await httpJson('GET', '/api/sessions/NOPE99');
  check('不存在的会话返回 404', info404.status === 404, 'status=' + info404.status);

  // ---- 0.5 内嵌页面：创建弹窗的存活时长选项 ----
  // 直接抓服务端吐出来的 HTML（go:embed 的那份），验证「不限时」按钮确实没了。
  // 这一条防的是「后端移除了 0、前端按钮还在」那种两半不一致。
  //
  // 只检查 ttl-options 这个容器内部：代码注释里仍会出现「不限时」这个词
  // （解释为什么移除它），对着全文搜字符串会误报。
  const page = await httpGetText('/');
  check('首页可访问', page.status === 200 && page.text.length > 0,
    'status=' + page.status + ' len=' + page.text.length);

  const optStart = page.text.indexOf('id="ttlOptions"');
  const optEnd = optStart >= 0 ? page.text.indexOf('</div>', optStart) : -1;
  const opts = optStart >= 0 && optEnd > optStart
    ? page.text.slice(optStart, optEnd)
    : '';
  check('找到存活时长容器 #ttlOptions', opts.length > 0,
    'optStart=' + optStart + ' optEnd=' + optEnd);

  check('档位容器内已无「不限时」按钮', opts.indexOf('不限时') < 0, opts.slice(0, 200));
  check('档位容器内已无 data-ttl="0"', opts.indexOf('data-ttl="0"') < 0);
  const ttlCount = (opts.match(/data-ttl="/g) || []).length;
  check('存活时长档位恰好 4 个', ttlCount === 4, '实际 ' + ttlCount + ' 个');
  check('默认选中 1 小时（60 分钟）', opts.indexOf('data-ttl="60"') >= 0
    && opts.indexOf('data-ttl="60" aria-checked="true"') >= 0);

  // ---- 0.6 内嵌页面：图片灯箱的结构 ----
  // 服务端把图片标成 isImage、接口也支持 inline，但如果前端页面上
  // 根本没有灯箱节点，点开大图就是一句空指针 —— 两半要么一起有，要么都没有。
  check('页面含图片灯箱容器 #lightbox', page.text.indexOf('id="lightbox"') >= 0);
  check('页面含灯箱大图 #lightboxImg', page.text.indexOf('id="lightboxImg"') >= 0);
  // 灯箱的下载入口必须存在，否则用户看完大图没有「存下来」的路径。
  check('页面含灯箱下载入口 #lightboxDownload',
    page.text.indexOf('id="lightboxDownload"') >= 0);

  // ---- 0.7 内嵌页面：粘贴截图的待发送预览条 ----
  //
  // 这条链路没有任何服务端节点可断言：图片是浏览器粘贴事件给的
  // 内存 File，上传仍在提交时才发生。能在这里守住的只有
  // 「页面里确实有那个容器和那段脚本」—— 容器缺失时预览条会静默不显示，
  // 用户粘完图看不到任何反馈，还以为粘贴坏了。
  check('页面含待发送预览条 #composerAttach',
    page.text.indexOf('id="composerAttach"') >= 0);
  check('输入框占位符提示可粘贴截图',
    page.text.indexOf('Ctrl+V') >= 0 || page.text.indexOf('粘贴') >= 0);
  console.log('');

  // 建会话（或复用传入的码）
  let code = roomCode;
  if (!code) {
    const r = await httpJson('POST', '/api/sessions');
    code = (r.body && (r.body.code || r.body.session)) || null;
    check('创建会话拿到授权码', !!code, JSON.stringify(r.body));
  }
  console.log('  会话码:', code, '\n');

  console.log('--- 1. 连接并读取 hello.self ---');
  const ws = new WS('/ws/session/' + code);
  await ws.connect();
  const hello = await waitFor(ws, (e) => e.event === 'hello', 'hello');
  check('hello 带 self 字段', typeof hello.self === 'string' && hello.self.length > 0,
    'self=' + JSON.stringify(hello.self));
  check('self 与 members 一致', (hello.members || []).includes(hello.self),
    'self=' + hello.self + ' members=' + JSON.stringify(hello.members));
  console.log('  服务端分配的显示名:', hello.self, '\n');

  console.log('--- 1.5 身份唯一化与消息归属 ---');
  // 这一段防的是实测踩到的真实故障：
  //   显示名默认由客户端 IP 兜底，而部署形态是 nginx 反代
  //   （局域网 → nginx:80 → 127.0.0.1:18080），WebSocket 握手 Hijack 之后
  //   底层连接的对端**永远是 nginx 自己**。于是房间里每个人的显示名都相同，
  //   前端又按「sender == 我的名字」判「这条是不是我发的」——
  //   结果所有人的消息都被标成「我」。
  // 现在服务端保证两件事：名字在房间内唯一化；每条消息带发送连接的 senderId。
  check('hello 带 selfId', typeof hello.selfId === 'string' && hello.selfId.length > 0,
    'selfId=' + JSON.stringify(hello.selfId));

  // 同一 IP 的第二个连接（同机双开 / 同一个反代后面）。
  const wsB = new WS('/ws/session/' + code);
  await wsB.connect();
  const helloB = await waitFor(wsB, (e) => e.event === 'hello', 'helloB');
  check('同 IP 的第二个连接拿到不重名的显示名', helloB.self !== hello.self,
    'a=' + hello.self + ' b=' + helloB.self);
  check('唯一化后的名字以原名做前缀（便于人眼对应）',
    helloB.self.startsWith(hello.self), 'b=' + helloB.self);
  check('两个连接的 selfId 不同', helloB.selfId !== hello.selfId,
    'a=' + hello.selfId + ' b=' + helloB.selfId);
  check('在线名单同时列出两个人', (helloB.members || []).length === 2,
    JSON.stringify(helloB.members));

  // B 发的消息：A 必须能凭 senderId 认出「这不是我发的」。
  const fromBP = waitFor(ws, (e) => e.event === 'message'
    && e.message && e.message.content === '来自B的消息', 'B 的消息');
  wsB.send({ type: 'text', content: '来自B的消息' });
  const fromB = await fromBP;
  check('B 的消息 sender 用的是 B 的唯一名', fromB.message.sender === helloB.self,
    'sender=' + fromB.message.sender);
  check('B 的消息带 senderId（= B 的 selfId）',
    fromB.message.senderId === helloB.selfId, 'senderId=' + fromB.message.senderId);
  check('A 凭 senderId 不会把 B 的消息认成自己发的',
    fromB.message.senderId !== hello.selfId);

  // 反代形态：服务端应采信 X-Forwarded-For，而不是把 nginx 的地址当人名。
  const wsC = new WS('/ws/session/' + code, { 'X-Forwarded-For': '192.168.1.77' });
  await wsC.connect();
  const helloC = await waitFor(wsC, (e) => e.event === 'hello', 'helloC');
  check('反代下显示名取 X-Forwarded-For 里的真实来源',
    helloC.self === '192.168.1.77', 'self=' + helloC.self);
  check('两条不同来源的连接 selfId 也不同', helloC.selfId !== hello.selfId);

  wsB.close();
  wsC.close();
  await new Promise((r) => setTimeout(r, 200));
  console.log('');

  console.log('--- 2. 带 cid 发送，验证原样回传 ---');
  const cid = 'c-test-' + Date.now();
  const echoP = waitFor(ws, (e) => e.event === 'message' && e.message && e.message.cid === cid,
    '带 cid 的回显');
  ws.send({ type: 'text', content: '你好?', cid });
  const echo = await echoP;
  check('回显的 cid 与发送的一致', echo.message.cid === cid);
  check('回显内容正确', echo.message.content === '你好?');
  check('回显带服务端分配的 id', !!echo.message.id && !echo.message.id.startsWith('local-'));
  check('回显 sender 等于自己的 self', echo.message.sender === hello.self,
    'sender=' + echo.message.sender + ' self=' + hello.self);
  console.log('  回显:', JSON.stringify(echo.message), '\n');

  console.log('--- 3. 不带 cid 发送（兼容旧客户端）---');
  const noCidP = waitFor(ws, (e) => e.event === 'message' && e.message
    && e.message.content === '无cid消息' && !e.message.cid, '不带 cid 的回显');
  ws.send({ type: 'text', content: '无cid消息' });
  const noCid = await noCidP;
  check('不带 cid 也能正常广播', noCid.message.content === '无cid消息');
  check('不带 cid 时回显里没有 cid 字段', noCid.message.cid === undefined);
  console.log('\n--- 4. 历史消息里不带 cid ---');
  const ws2 = new WS('/ws/session/' + code);
  await ws2.connect();
  const hello2 = await waitFor(ws2, (e) => e.event === 'hello', 'hello2');
  const hist = hello2.history || [];
  check('历史里有刚才的消息', hist.length >= 2, 'len=' + hist.length);
  check('历史条目全部不带 cid', hist.every((m) => m.cid === undefined),
    JSON.stringify(hist.map((m) => m.cid)));

  console.log('\n--- 5. 聊天室文件上传 / 广播 / 下载 ---');

  // 5.1 上传到存在的房间 → 200，拿到文件 id
  const upBody = Buffer.from('聊天室测试文件内容 chat-file-' + Date.now(), 'utf8');
  const up = await httpMultipart('/api/sessions/' + code + '/files', 'note.txt', upBody);
  check('上传到房间返回 200', up.status === 200, 'status=' + up.status + ' raw=' + up.raw.slice(0, 120));
  const fileId = up.body && up.body.id;
  check('返回文件 id', typeof fileId === 'number' && fileId > 0, 'id=' + fileId);
  check('返回原始文件名', up.body && up.body.name === 'note.txt', 'name=' + (up.body && up.body.name));

  // 5.2 WS 广播 type:"file" 卡片；只带 fileId，名称/大小由服务端补全
  const cardP = waitFor(ws, (e) => e.event === 'message' && e.message && e.message.type === 'file',
    'file 卡片广播');
  const fc = 'c-file-' + Date.now();
  ws.send({ type: 'file', fileId, cid: fc });
  const card = await cardP;
  check('卡片 type = file', card.message.type === 'file');
  check('卡片回传 cid 一致', card.message.cid === fc, 'cid=' + card.message.cid);
  check('卡片文件名由服务端补齐', card.message.fileName === 'note.txt',
    'fileName=' + card.message.fileName);
  check('卡片带可读大小文本', typeof card.message.fileText === 'string' && card.message.fileText.length > 0,
    'fileText=' + card.message.fileText);
  check('卡片 URL 指向 chat-files', card.message.fileUrl === '/api/chat-files/' + fileId,
    'fileUrl=' + card.message.fileUrl);
  console.log('  卡片:', JSON.stringify(card.message), '\n');

  // 5.3 下载拿回原内容
  const dl = await httpRaw('GET', '/api/chat-files/' + fileId);
  check('下载聊天文件返回 200', dl.status === 200, 'status=' + dl.status);
  check('下载内容与上传一致', dl.buffer.equals(upBody),
    'got=' + dl.buffer.length + 'B want=' + upBody.length + 'B');

  // 5.3b 图片内联显示 —— 前端缩略图靠的就是这条通道。
  //
  // 三件事必须同时成立，缺一个缩略图就出不来：
  //   ① 卡片带 isImage=true（前端据此决定渲染成图还是文件卡片）；
  //   ② ?inline=1 返回 image/* 而不是 octet-stream；
  //   ③ Content-Disposition 是 inline，否则浏览器仍然会当附件下载。
  console.log('\n--- 5.3b 图片内联预览 ---');
  // 最小的合法 PNG（1x1 透明像素）。
  const pngBytes = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
    'base64');
  const imgUp = await httpMultipart('/api/sessions/' + code + '/files', 'shot.png', pngBytes);
  check('图片上传成功', imgUp.status === 200 && !!imgUp.body,
    'status=' + imgUp.status + ' raw=' + imgUp.raw.slice(0, 120));
  const imgId = imgUp.body && imgUp.body.id;

  if (imgId) {
    const imgCardP = waitFor(ws, (e) => e.event === 'message'
      && e.message && e.message.type === 'file' && e.message.fileName === 'shot.png',
      '图片卡片广播');
    ws.send({ type: 'file', fileId: imgId });
    const imgCard = await imgCardP;
    check('图片卡片带 isImage=true', imgCard.message.isImage === true,
      'isImage=' + imgCard.message.isImage);

    // 对照组：前面的 note.txt 卡片不该被标成图片。
    check('非图片卡片没有 isImage 标记', !card.message.isImage,
      'isImage=' + card.message.isImage);

    const inl = await httpRaw('GET', '/api/chat-files/' + imgId + '?inline=1');
    check('内联请求返回 200', inl.status === 200, 'status=' + inl.status);
    check('内联返回 image/png', inl.headers['content-type'] === 'image/png',
      'ctype=' + inl.headers['content-type']);
    check('内联是 inline 而非 attachment',
      String(inl.headers['content-disposition'] || '').startsWith('inline'),
      'cd=' + inl.headers['content-disposition']);
    check('内联内容与上传逐字节一致', inl.buffer.equals(pngBytes),
      'got=' + inl.buffer.length + 'B want=' + pngBytes.length + 'B');

    // 反向验证：非图片即使显式要求 inline，也必须回落成附件。
    // 没有这条断言，?inline=1 就会变成「把上传内容当页面渲染」的开关。
    const notImg = await httpRaw('GET', '/api/chat-files/' + fileId + '?inline=1');
    check('非图片带 inline=1 仍是附件',
      String(notImg.headers['content-disposition'] || '').startsWith('attachment'),
      'cd=' + notImg.headers['content-disposition']);
    check('非图片带 inline=1 仍是 octet-stream',
      notImg.headers['content-type'] === 'application/octet-stream',
      'ctype=' + notImg.headers['content-type']);

    // 不带 inline 时，图片也照旧按附件下载（右键「另存为」走这条）。
    const plain = await httpRaw('GET', '/api/chat-files/' + imgId);
    check('图片不带 inline 时按附件下载',
      String(plain.headers['content-disposition'] || '').startsWith('attachment'),
      'cd=' + plain.headers['content-disposition']);
  }

  // 5.3c 粘贴截图链路 —— 服务端这边唯一会被影响的一环就是文件名。
  //
  // 浏览器从剪贴板给的 File 往往叫 `image.png` / `image` / 空串，
  // 前端会按 MIME 补一个带扩展名的名字（见 clipboardImageName）。
  // 服务端的图片判定**只看扩展名**，所以「前端补的名字能不能被认出来」
  // 就是粘贴功能的成败点 —— 名字不对，截图会渲染成普通文件卡片。
  console.log('\n--- 5.3c 粘贴截图：文件名 → isImage ---');
  const pastedName = '截图-20260915-131500.png';   // 前端补名的真实形态：中文+时间戳
  const pastedUp = await httpMultipart('/api/sessions/' + code + '/files', pastedName, pngBytes);
  check('带中文与时间戳的粘贴文件名可上传', pastedUp.status === 200 && !!pastedUp.body,
    'status=' + pastedUp.status + ' raw=' + pastedUp.raw.slice(0, 120));

  const pastedId = pastedUp.body && pastedUp.body.id;
  if (pastedId) {
    check('服务端完整保留中文文件名', pastedUp.body.name === pastedName,
      'name=' + pastedUp.body.name);

    // 注意匹配字段是 fileUrl 而不是 fileId ——
    // 卡片广播里**没有** fileId 字段（服务端只下发 /api/chat-files/{id} 这个 URL）。
    // 拿 fileId 去等会永远等不到，白白超时。
    const cardUrl = '/api/chat-files/' + pastedId;
    const pastedCardP = waitFor(ws, (e) => e.event === 'message'
      && e.message && e.message.type === 'file' && e.message.fileUrl === cardUrl,
      '粘贴图片卡片广播');
    ws.send({ type: 'file', fileId: pastedId });
    const pastedCard = await pastedCardP;
    check('卡片用 fileUrl 而非 fileId 标识文件',
      pastedCard.message.fileUrl === cardUrl && pastedCard.message.fileId === undefined,
      'fileUrl=' + pastedCard.message.fileUrl + ' fileId=' + pastedCard.message.fileId);
    check('粘贴图片被识别为 isImage', pastedCard.message.isImage === true,
      'isImage=' + pastedCard.message.isImage);

    // 把「名字不对」的反面也钉死：扩展名被补错成 .tmp 时不能被当成图片。
    // 这正是 clipboardImageName 存在的理由。
    const wrongUp = await httpMultipart('/api/sessions/' + code + '/files',
      'image.tmp', pngBytes);
    const wrongId = wrongUp.body && wrongUp.body.id;
    if (wrongId) {
      const wrongUrl = '/api/chat-files/' + wrongId;
      const wrongCardP = waitFor(ws, (e) => e.event === 'message'
        && e.message && e.message.type === 'file' && e.message.fileUrl === wrongUrl,
        '扩展名不符的卡片广播');
      ws.send({ type: 'file', fileId: wrongId });
      const wrongCard = await wrongCardP;
      check('内容确实是 PNG 但扩展名是 .tmp → 不标 isImage',
        !wrongCard.message.isImage, 'isImage=' + wrongCard.message.isImage);

      const wrongInl = await httpRaw('GET', '/api/chat-files/' + wrongId + '?inline=1');
      check('扩展名不符时 inline=1 也拿不到 image/*',
        wrongInl.headers['content-type'] === 'application/octet-stream',
        'ctype=' + wrongInl.headers['content-type']);
    }
  }

  // 5.4 伪造 fileId：不存在 / 非聊天文件，一律说「不存在」不泄漏
  const forgedP = waitFor(ws, (e) => e.event === 'error', '伪造 fileId 报错');
  ws.send({ type: 'file', fileId: 99999999 });
  const forged = await forgedP;
  check('不存在的 fileId 被拒绝', !!forged.error, 'error=' + forged.error);

  const foreignP = waitFor(ws, (e) => e.event === 'error', '跨房间 fileId 报错');
  ws.send({ type: 'file', fileId: 0 });
  const foreign = await foreignP;
  check('fileId<=0 被拒绝', !!foreign.error, 'error=' + foreign.error);

  // 5.5 上传到不存在的房间 → 404；过期房间 → 410
  const upMissing = await httpMultipart('/api/sessions/NOPE99/files', 'x.txt', Buffer.from('x'));
  check('上传到不存在的房间返回 404', upMissing.status === 404, 'status=' + upMissing.status);

  const expCode = 'EX' + Math.floor(Math.random() * 900 + 100);
  await httpJson('POST', '/api/sessions', { code: expCode, ttlMinutes: 10 });
  // 房间存活时长最短白名单是 10 分钟；这里靠「过期检测」无法在测试里快速触发，
  // 因此只验证「房间存在时可上传」，过期路径由 Go 单测覆盖。
  const upAlive = await httpMultipart('/api/sessions/' + expCode + '/files', 'y.txt', Buffer.from('y'));
  check('存活房间可上传', upAlive.status === 200, 'status=' + upAlive.status);
  const aliveId = upAlive.body && upAlive.body.id;

  // 5.6 仓库文件（kind=permanent）不能经由 chat-files 下载。
  //
  // 注意这条现在的意义变了：仓库下载本身已经开放给匿名用户，
  // 但 /api/chat-files/{id} 是**房间文件专用**入口，它不校验房间号。
  // 若放行仓库文件，就等于给仓库开了一个完全无校验的旁路 ——
  // 将来仓库若收紧（比如加房间号/口令），这个旁路会静默绕过它。
  // 所以仍要求 404。
  const uname = 'repo' + Math.floor(Math.random() * 900000 + 100000);
  const reg = await httpJson('POST', '/api/auth/register', { username: uname, password: 'pw123456' });
  if (reg.status === 200 || reg.status === 201) {
    const cookie = reg.cookie;
    const repoUp = await httpMultipartLogin('/api/files', 'repo.txt',
      Buffer.from('repo file'), cookie);
    const repoId = repoUp.body && repoUp.body.id;
    check('仓库文件上传成功（用于对照）', repoUp.status === 200 && !!repoId,
      'status=' + repoUp.status + ' raw=' + repoUp.raw.slice(0, 120));
    if (repoId) {
      const repoDl = await httpRaw('GET', '/api/chat-files/' + repoId);
      check('仓库文件不能经由 chat-files 下载', repoDl.status === 404,
        'status=' + repoDl.status);
      // 同一文件走正规仓库接口应当可以下载，证明上一条不是因为文件不存在。
      // 这里刻意**带上 cookie**：匿名也能下，但登录态更不该被拒。
      const okDl = await httpRawLogin('GET', '/api/files/' + repoId, cookie);
      check('仓库文件走正规接口可下载（对照成立）', okDl.status === 200,
        'status=' + okDl.status);
      // 新增：同一文件在**完全匿名**下也必须能下 —— 这是本轮改动的核心语义。
      const anonDl = await httpRaw('GET', '/api/files/' + repoId);
      check('仓库文件匿名下载也放行（本轮改动）', anonDl.status === 200,
        'status=' + anonDl.status);
      // 但删除必须仍然是登录态专属。
      const anonDel = await httpRaw('DELETE', '/api/files/' + repoId);
      check('仓库文件匿名删除被拒（本轮改动）', anonDel.status === 401,
        'status=' + anonDel.status);
      // 收尾：别把测试文件留在仓库里。
      await httpRawLogin('DELETE', '/api/files/' + repoId, cookie);
    }
  } else {
    console.log('  [SKIP] 注册失败，跳过仓库文件对照（status=' + reg.status + '）');
  }

  ws.close();
  ws2.close();
  await new Promise((r) => setTimeout(r, 300));

  // 5.7 房间销毁 → 文件被清理。用一个短生存房间验证「房间没了文件也下载不到」。
  const goneCode = 'GN' + Math.floor(Math.random() * 900 + 100);
  const gr = await httpJson('POST', '/api/sessions', { code: goneCode, ttlMinutes: 10 });
  check('创建待销毁房间', gr.status === 200, 'status=' + gr.status);
  const gUp = await httpMultipart('/api/sessions/' + goneCode + '/files', 'gone.txt', Buffer.from('gone'));
  const goneId = gUp.body && gUp.body.id;
  check('待销毁房间可上传文件', gUp.status === 200 && !!goneId, 'id=' + goneId);

  if (goneId) {
    const before = await httpRaw('GET', '/api/chat-files/' + goneId);
    check('销毁前可下载', before.status === 200, 'status=' + before.status);
  }

  // 房间销毁走「服务端进程内」的路径，外部无法在测试里把 TTL 拨到过去，
  // 也无删除接口。因此这里只断言「房间不存在时立刻 410」的即时判定——
  // 用一个从未创建过的房间的假想文件 id 无法构造，改为断言：
  // 上传到已销毁房间的路径返回 404（房间不存在），与 handleChatUpload 的判定一致。
  const afterGone = await httpMultipart('/api/sessions/' + goneCode + 'XX/files',
    'z.txt', Buffer.from('z'));
  check('房间不存在时上传返回 404', afterGone.status === 404, 'status=' + afterGone.status);

  console.log('  注：房间到点删除 → 聊天文件一并清理，由 Go 单测覆盖（见 internal/session）。');

  console.log(`\n=== 汇总: PASS=${pass} FAIL=${fail} ===`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('\n测试异常:', e.message);
  process.exit(1);
});
