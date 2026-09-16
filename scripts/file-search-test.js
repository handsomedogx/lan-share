#!/usr/bin/env node
/**
 * file-search-test.js —— 文件仓库「按文件名搜索」的纯逻辑测试（零依赖）
 *
 * 为什么要单开一个脚本：搜索**完全在前端做**（列表本来就已全量拉到本地），
 * 没有任何接口可以断言，端到端脚本（ws-smoke 那一类）够不着它。
 *
 * 做法是用 vm 把**真实的** web/js/app.js 跑在一个假 document 上，
 * 只取三个纯函数（searchTokens / nameMatches / highlightedName）来验。
 * 刻意不复制一份实现进测试里 —— 复制出来的那份永远不会跟着源文件一起改，
 * 过一阵就成了「测试全绿但功能已坏」。
 *
 * highlightedName 只用到 document 的三个构造函数，所以假 DOM 只要这么多；
 * app.js 末尾按 readyState 决定是否 init()，给 'loading' 就不会触发初始化。
 *
 * 用法：node scripts/file-search-test.js
 */

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC = path.join(__dirname, '..', 'web', 'js', 'app.js');

/* ------------------------------------------------ 假 DOM（只为那三个函数） */

/**
 * 节点用普通对象表示，末尾再序列化回 HTML 字符串 —— 比断言对象树直观得多。
 * textContent 必须做成取值器：源文件是用 `mark.textContent = ...` 填内容的，
 * 普通属性名对不上，高亮内容会静默变成空串（这个坑本脚本自己踩过一次）。
 */
function node(type, text) {
  const n = {
    type,
    text: text || '',
    children: [],
    appendChild(child) { this.children.push(child); return child; }
  };
  Object.defineProperty(n, 'textContent', {
    get() { return this.text; },
    set(v) { this.text = v == null ? '' : String(v); }
  });
  return n;
}

const fakeDocument = {
  readyState: 'loading',          // 别让 app.js 真的跑 init()
  addEventListener() {},
  createDocumentFragment: () => node('#fragment'),
  createTextNode: (t) => node('#text', t),
  createElement: (tag) => node(tag)
};

const ctx = {
  document: fakeDocument,
  window: {},
  console,
  setTimeout,
  clearTimeout,
  localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
  navigator: {},
  requestAnimationFrame() {}
};

vm.createContext(ctx);
vm.runInContext(fs.readFileSync(SRC, 'utf8'), ctx, { filename: SRC });

const { searchTokens, nameMatches, highlightedName } = ctx;
if (typeof searchTokens !== 'function' || typeof nameMatches !== 'function'
  || typeof highlightedName !== 'function') {
  console.error('[FATAL] web/js/app.js 里找不到搜索相关的三个函数，脚本需要同步更新');
  process.exit(1);
}

/** 直接改写 app.js 里的 state.fileQuery（同一 context 共享全局词法作用域）。 */
function setQuery(q) {
  vm.runInContext('state.fileQuery = ' + JSON.stringify(q), ctx);
}

function serialize(n) {
  if (n.type === '#text') return n.text;
  // 元素的内容 = textContent（高亮的 <mark> 就是这么填的）+ 子节点。
  const inner = n.text + n.children.map(serialize).join('');
  if (n.type === '#fragment') return inner;
  return '<' + n.type + '>' + inner + '</' + n.type + '>';
}

/** 去掉所有 <mark> 标签，用于验证「高亮没有吃掉或重复任何字符」。 */
const stripMark = (s) => s.replace(/<\/?mark>/g, '');

/* ------------------------------------------------------------- 断言小工具 */

let pass = 0;
let fail = 0;

function eq(actual, expected, label) {
  const a = JSON.stringify(actual);
  const b = JSON.stringify(expected);
  if (a === b) {
    pass++;
    console.log('  [ OK ] ' + label);
  } else {
    fail++;
    console.log('  [FAIL] ' + label);
    console.log('         期望 ' + b);
    console.log('         实际 ' + a);
  }
}

function ok(cond, label) {
  eq(!!cond, true, label);
}

/* -------------------------------------------------------------- 1. 分词 */

console.log('\n== searchTokens：搜索词 → 关键词数组 ==');
setQuery('');
eq(searchTokens(), [], '空串 → 没有关键词');
setQuery('   ');
eq(searchTokens(), [], '纯空白 → 没有关键词（trim 之后才切分）');
setQuery('  ABC  ');
eq(searchTokens(), ['abc'], '去空白 + 转小写');
setQuery('截图 2024');
eq(searchTokens(), ['截图', '2024'], '空格切分成多个关键词');
setQuery('截图   2024');
eq(searchTokens(), ['截图', '2024'], '连续空格不产生空关键词');
ok(searchTokens().every((t) => t !== ''), '任何关键词都不为空（空串会让 indexOf 死循环）');

/* -------------------------------------------------------------- 2. 匹配 */

console.log('\n== nameMatches：全部关键词命中才算命中 ==');
ok(nameMatches('Meeting-0912.MP4', ['meeting']), '大小写不敏感');
ok(nameMatches('截图-2024-09-16.png', ['截图', '2024']), '多关键词 AND —— 都命中');
eq(nameMatches('截图-2024-09-16.png', ['截图', '2024zzz']), false, '多关键词 AND —— 有一个不命中就不算');
ok(nameMatches('路由器固件说明.pdf', ['固件']), '中间子串也算命中，不要求前缀');
ok(nameMatches('会议录像-0912.mp4', ['mp4']), '扩展名同样参与匹配');
ok(nameMatches('报告(2024).pdf', ['(2024)']), '特殊字符按字面量处理，不当正则');
eq(nameMatches('', ['x']), false, '空文件名不会命中任何关键词');
eq(nameMatches(null, ['x']), false, 'null 文件名不会炸');
eq(nameMatches('任意', []), true, '没有关键词 = 不过滤（调用方本来也不会走到这里）');

/* ------------------------------------------------------------ 3. 高亮 */

console.log('\n== highlightedName：命中片段高亮 ==');
eq(highlightedName('截图.png', []), null, '没有关键词 → 返回 null，调用方退回纯文本');
eq(highlightedName('截图.png', ['zzz']), null, '没有命中 → 返回 null');

const one = serialize(highlightedName('会议录像-0912.mp4', ['0912']));
eq(one, '会议录像-<mark>0912</mark>.mp4', '单次命中，前后文本都在');
eq(stripMark(one), '会议录像-0912.mp4', '高亮不丢字符、不重复字符');

const many = serialize(highlightedName('ab-ab-ab.txt', ['ab']));
eq(many, '<mark>ab</mark>-<mark>ab</mark>-<mark>ab</mark>.txt', '同一关键词的多次出现都高亮');

const upper = serialize(highlightedName('IMG_001.PNG', ['img']));
eq(upper, '<mark>IMG</mark>_001.PNG', '高亮按原文大小写显示，而不是关键词的小写');

const two = serialize(highlightedName('截图-2024-09-16 会议.png', ['截图', '会议']));
eq(two, '<mark>截图</mark>-2024-09-16 <mark>会议</mark>.png', '两个关键词分别高亮');

// 重叠区间必须合并：'aa' 落在 [0,2)、'ab' 落在 [1,3)，不合并会切出嵌套节点。
const overlap = serialize(highlightedName('aab', ['aa', 'ab']));
eq(overlap, '<mark>aab</mark>', '重叠的命中区间合并成一段');

// 同一关键词重复给出（多敲一个一样的词）也一样，且顺带守住死循环：
// 每轮 indexOf 都从 i + 长度 继续，空关键词才会原地打转。
const dup = serialize(highlightedName('aaa', ['a', 'a']));
eq(dup, '<mark>aaa</mark>', '重复关键词不产生重复/嵌套节点');

const tail = serialize(highlightedName('a.txt', ['txt']));
eq(tail, 'a.<mark>txt</mark>', '命中在末尾时不吞掉前面的文本');
const head = serialize(highlightedName('txt-a', ['txt']));
eq(head, '<mark>txt</mark>-a', '命中在开头时不吞掉后面的文本');
const whole = serialize(highlightedName('txt', ['txt']));
eq(whole, '<mark>txt</mark>', '整名命中');

/* ------------------------------------------------------------ 汇总 */

console.log('\n' + (fail === 0 ? '全部通过' : '有失败') + '：' + pass + ' 通过 / ' + fail + ' 失败');
process.exit(fail === 0 ? 0 : 1);
