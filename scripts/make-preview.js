#!/usr/bin/env node
/**
 * make-preview.js —— 生成自包含的「文件仓库」预览 HTML
 *
 * 为什么需要它：本机无法对浏览器截图（GUI 进程被沙箱禁止），
 * 所以想把界面渲染结果给用户看，只能生成一个自身包含 CSS 的静态 HTML，
 * 由用户在预览面板里打开。
 *
 * 做法：读真实的 web/index.html + web/css/style.css，
 * 把 <link> 换成内联 <style>，剥掉 <script>（预览不需要真跑逻辑），
 * 然后把关键状态（登录/未登录、搜索态）手工写死成静态 DOM。
 *
 * 输出两份（都是一次性交付物，看完即可删；`preview-*.html` 已在 .gitignore 里）：
 *   preview-files-pane.html   未登录 + 提示条成一行，不遮挡下载列
 *   preview-file-search.html  搜索态：只剩命中项，命中片段高亮
 *
 * 用法：node scripts/make-preview.js
 */

'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'web/index.html'), 'utf8');
const css = fs.readFileSync(path.join(ROOT, 'web/css/style.css'), 'utf8');

/**
 * 演示数据：三个文件。搜索态只取第一条（名字里有「固件」）。
 *
 * data-t 一律小写：CSS 里的选择器写的是 `.ftype[data-t="pdf"]`，
 * 早先这里写成 "PDF"，图标配色其实一条都没命中过 —— 预览里看不出来，
 * 因为都退化成同一个灰底，而灰色也挺像回事。
 */
const demoRows = [`
          <tr>
            <td><div class="cell-name"><span class="ftype" data-t="pdf">PDF</span><div style="min-width:0"><div class="fname" title="路由器固件说明.pdf">路由器固件说明.pdf</div></div></div></td>
            <td class="cell-size">1.8 MB</td>
            <td class="cell-time">今天 14:22</td>
            <td class="cell-owner">boss</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>`,
`
          <tr>
            <td><div class="cell-name"><span class="ftype" data-t="mp4">MP4</span><div style="min-width:0"><div class="fname" title="会议录像-0912.mp4">会议录像-0912.mp4</div></div></div></td>
            <td class="cell-size">86.4 MB</td>
            <td class="cell-time">今天 11:05</td>
            <td class="cell-owner">alice</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>`,
`
          <tr class="is-pinned">
            <td><div class="cell-name"><span class="ftype" data-t="zip">ZIP</span><div style="min-width:0"><div class="fname" title="驱动备份.zip">驱动备份.zip</div><span class="fpin" title="已置顶"><svg viewBox="0 0 20 20" width="16" height="16"><path d="M12.6 3.2a1 1 0 011.5.1l2.6 2.6a1 1 0 01-.1 1.5l-1.9 1.5-.4 3.1a.8.8 0 01-1.3.6L10 9.9l-3.4 3.4a.6.6 0 01-.9-.9L9.1 9 6.4 6.3a.8.8 0 01.6-1.3l3.1-.4 1.5-1.9z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/><path d="M5.6 14.4l-1.9 1.9" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg></span></div></div></td>
            <td class="cell-size">12.1 MB</td>
            <td class="cell-time">昨天 20:41</td>
            <td class="cell-owner">boss</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>`];

/**
 * 生成一份预览 HTML。
 *
 * @param {object} opts
 * @param {boolean} opts.search 是否演示搜索态
 */
function buildPreview(opts) {
  const searching = !!(opts && opts.search);

  // 1) 内联 CSS，去掉 <link rel=stylesheet>
  let out = html.replace(
    /<link[^>]*rel=["']stylesheet["'][^>]*>/i,
    '<style>\n' + css + '\n</style>'
  );

  // 2) 去掉所有 <script>：预览只呈现静态结果，不跑逻辑
  out = out.replace(/<script[\s\S]*?<\/script>/gi, '');

  // 3) 去掉外链字体等（离线可开）
  out = out.replace(/<link[^>]*rel=["']preconnect["'][^>]*>/gi, '');

  // 4) 去掉未登录提示条的 hidden（让它显示出来）。
  // 只认 id，不认类名 —— 这个容器从浮卡改成横幅时类名换过，
  // 按类名匹配的旧写法会静默失配，预览里就看不到提示条。
  out = out.replace(/(id="lockOverlay")\s+hidden/, '$1');

  // 5) 写死表格行：替换空的 tbody 内容。
  // 搜索态只留命中「固件」的那一行，并把命中片段包上 <mark> ——
  // 真实现里这段由 highlightedName() 生成，预览只是把同样的结构手写出来。
  const rows = searching
    ? [demoRows[0].replace(
      '>路由器固件说明.pdf<',
      '>路由器<mark>固件</mark>说明.pdf<'
    )]
    : demoRows;
  out = out.replace(
    /(<tbody id="fileBody">)([\s\S]*?)(<\/tbody>)/,
    '$1' + rows.join('') + '\n        $3'
  );

  // fileEmpty 要保持 hidden
  out = out.replace(
    /(<div class="table-empty" id="fileEmpty")(?![^>]*hidden)/,
    '$1 hidden'
  );

  // 6) 搜索态：搜索框里填上关键词，× 同时显形（真实现里它由 JS 按输入内容切换）。
  if (searching) {
    out = out.replace(/(<input id="fileSearch"[^>]*?)>/, '$1 value="固件">');
    out = out.replace(/(<button[^>]*id="btnClearSearch"[^>]*?)\s+hidden/, '$1');
  }

  // 用量条文案：搜索态写「命中 / 总数」
  out = out.replace(
    /<span id="usageText">—<\/span>/,
    searching
      ? '<span id="usageText">1 / 3 个文件 · 共 1.8 MB</span>'
      : '<span id="usageText">3 个文件 · 共 100.3 MB</span>'
  );
  out = out.replace(
    /<span class="usage-hint" id="usageHint"><\/span>/,
    '<span class="usage-hint" id="usageHint">浏览与下载无需登录</span>'
  );

  // 7) 底部加一条说明条，避免用户误以为这是真服务
  const note = searching
    ? '静态预览（搜索态）· 搜索框在「文件仓库」标题右侧：输入即过滤，命中片段高亮；点 × 或按 Esc 清空'
    : '静态预览（未登录状态）· 看文件仓库：提示条在列表上方成一行，不覆盖任何文件的下载按钮';
  out = out.replace(
    /<body[^>]*>/i,
    (m) => m + `
  <div style="position:fixed;left:0;right:0;bottom:0;z-index:9999;padding:7px 14px;
              font:12px/1.5 -apple-system,'Segoe UI',sans-serif;color:#fff;
              background:#3b4252;text-align:center;">
    ${note}
  </div>`
  );

  return out;
}

[
  ['preview-files-pane.html', { search: false }],
  ['preview-file-search.html', { search: true }]
].forEach(([name, opts]) => {
  const out = buildPreview(opts);
  const p = path.join(ROOT, name);
  fs.writeFileSync(p, out, 'utf8');
  console.log('已生成: ' + p + '（' + (out.length / 1024).toFixed(1) + ' KB）');
});
