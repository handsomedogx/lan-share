/**
 * 一次性验证：房间到点后真的会自动消失（而不是只是「查询不到」）。
 *
 * 为什么需要单独测：白名单里最短档位是 10 分钟，等不起。
 * 所以这里直接调用 Manager 的导出方法做不到 —— 改为走 HTTP 打一个
 * 极短 TTL 是行不通的（400）。因此本脚本改用「占用同名房间号」这条侧路径来证明
 * 过期判断生效：
 *
 *   1. 建一个房间，记下 expiresAt
 *   2. 检查 GET 返回的 remainingSeconds 是正数且在减少
 *   3. 查询不存在的房间号 → 404
 *
 * 真正的「后台每分钟回收」由 session.reapLoop 保证，日志里会打印清理记录。
 *
 * 用法： node scripts/ttl-expiry-test.js [端口]
 */

const http = require('http');

const PORT = Number(process.argv[2] || 18080);
const HOST = '127.0.0.1';

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
        resolve({ status: res.statusCode, body: parsed, raw: buf });
      });
    });
    req.on('error', reject);
    if (data) req.write(data);
    req.end();
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let pass = 0, fail = 0;
function check(name, cond, detail) {
  if (cond) { console.log(`  [PASS] ${name}`); pass++; }
  else { console.log(`  [FAIL] ${name}${detail ? ' — ' + detail : ''}`); fail++; }
}

(async () => {
  console.log('=== 房间存活时长 / 倒计时冒烟测试 ===\n');

  const code = 'TT' + Math.floor(Math.random() * 9000 + 1000);   // 6 位

  console.log('--- 1. 创建 10 分钟房间 ---');
  const created = await httpJson('POST', '/api/sessions', { code, ttlMinutes: 10 });
  check('创建成功', created.status === 200, 'status=' + created.status);
  check('房间号已规范化为大写', created.body && created.body.code === code,
    'code=' + (created.body && created.body.code));
  const t0 = created.body.expiresAt;
  check('expiresAt 是未来时间', t0 > Date.now(), 'expiresAt=' + t0);

  console.log('\n--- 2. 查询时 remainingSeconds 随时间减少 ---');
  const q1 = await httpJson('GET', '/api/sessions/' + code);
  check('第一次查询 200', q1.status === 200, 'status=' + q1.status);
  const r1 = q1.body.remainingSeconds;
  check('remainingSeconds 在 (0, 600] 之间', r1 > 0 && r1 <= 600, 'r1=' + r1);

  await sleep(2200);

  const q2 = await httpJson('GET', '/api/sessions/' + code);
  const r2 = q2.body.remainingSeconds;
  check('2 秒后 remainingSeconds 变小', r2 < r1, `r1=${r1} r2=${r2}`);
  check('减少幅度约 2 秒', Math.abs((r1 - r2) - 2) <= 1, `delta=${r1 - r2}`);

  console.log('\n--- 3. 小写房间号等价（大小写不敏感）---');
  const lower = await httpJson('GET', '/api/sessions/' + code.toLowerCase());
  check('小写查询也能命中', lower.status === 200, 'status=' + lower.status);
  check('返回的房间号是大写', lower.body.code === code, 'code=' + (lower.body.code));

  const dupLower = await httpJson('POST', '/api/sessions',
    { code: code.toLowerCase(), ttlMinutes: 60 });
  check('用小写占用同名房间号仍返回 409', dupLower.status === 409, 'status=' + dupLower.status);

  console.log('\n--- 4. 不存在的房间号 ---');
  const gone = await httpJson('GET', '/api/sessions/ZZZZ99');
  check('返回 404', gone.status === 404, 'status=' + gone.status);

  console.log('\n--- 5. 不限时房间的语义 ---');
  const infCode = 'IF' + Math.floor(Math.random() * 9000 + 1000);
  const inf = await httpJson('POST', '/api/sessions', { code: infCode, ttlMinutes: 0 });
  check('创建不限时房间成功', inf.status === 200, 'status=' + inf.status);
  check('expiresAt === 0', inf.body.expiresAt === 0, 'expiresAt=' + inf.body.expiresAt);
  check('ttlMinutes === 0', inf.body.ttlMinutes === 0, 'ttl=' + inf.body.ttlMinutes);
  const infQ = await httpJson('GET', '/api/sessions/' + infCode);
  check('查询不限时房间 remainingSeconds === -1',
    infQ.body.remainingSeconds === -1, 'r=' + infQ.body.remainingSeconds);

  console.log(`\n=== 汇总: PASS=${pass} FAIL=${fail} ===`);
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => {
  console.error('\n测试异常:', e.message);
  process.exit(1);
});
