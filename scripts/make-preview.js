#!/usr/bin/env node
/**
 * make-preview.js —— 生成一份自包含的「文件仓库」预览 HTML
 *
 * 为什么需要它：本机无法对浏览器截图（GUI 进程被沙箱禁止），
 * 所以想把界面渲染结果给用户看，只能生成一个自身包含 CSS 的静态 HTML，
 * 由用户在预览面板里打开。
 *
 * 做法：读真实的 web/index.html + web/css/style.css，
 * 把 <link> 换成内联 <style>，剥掉 <script>（预览不需要真跑逻辑），
 * 然后把关键状态（登录/未登录）手工写死成静态 DOM。
 *
 * 用法：node scripts/make-preview.js
 */

'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const OUT = path.join(ROOT, 'preview-files-pane.html');

const html = fs.readFileSync(path.join(ROOT, 'web/index.html'), 'utf8');
const css = fs.readFileSync(path.join(ROOT, 'web/css/style.css'), 'utf8');

// 1) 内联 CSS，去掉 <link rel=stylesheet>
let out = html.replace(
  /<link[^>]*rel=["']stylesheet["'][^>]*>/i,
  '<style>\n' + css + '\n</style>'
);

// 2) 去掉所有 <script>
out = out.replace(/<script[\s\S]*?<\/script>/gi, '');

// 3) 去掉外链字体等（离线可开）
out = out.replace(/<link[^>]*rel=["']preconnect["'][^>]*>/gi, '');

// 4) 首屏：模拟「未登录 + 仓库里已有 3 个文件」
//    把遮罩的 hidden 去掉，把表格内容写死，并让左侧实时区显示一个空态。
const demoRows = `
          <tr>
            <td><div class="cell-name"><span class="ftype" data-t="PDF">PDF</span><div style="min-width:0"><div class="fname" title="路由器固件说明.pdf">路由器固件说明.pdf</div></div></div></td>
            <td class="cell-size">1.8 MB</td>
            <td class="cell-time">今天 14:22</td>
            <td class="cell-owner">boss</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>
          <tr>
            <td><div class="cell-name"><span class="ftype" data-t="MP4">MP4</span><div style="min-width:0"><div class="fname" title="会议录像-0912.mp4">会议录像-0912.mp4</div></div></div></td>
            <td class="cell-size">86.4 MB</td>
            <td class="cell-time">今天 11:05</td>
            <td class="cell-owner">alice</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>
          <tr>
            <td><div class="cell-name"><span class="ftype" data-t="ZIP">ZIP</span><div style="min-width:0"><div class="fname" title="驱动备份.zip">驱动备份.zip</div></div></div></td>
            <td class="cell-size">12.1 MB</td>
            <td class="cell-time">昨天 20:41</td>
            <td class="cell-owner">boss</td>
            <td class="cell-act">
              <a class="btn btn-icon" title="下载" href="#" onclick="return false">
                <svg viewBox="0 0 20 20" width="16" height="16"><path d="M10 3.5v8.6M6.6 9l3.4 3.4L13.4 9" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M4 15.5h12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>
              </a>
            </td>
          </tr>`;

// 去掉锁遮罩的 hidden（让它显示出来）
out = out.replace(
  /(<div class="lock-overlay" id="lockOverlay") hidden/,
  '$1'
);

// 写死表格行：替换空的 tbody 内容
out = out.replace(
  /(<tbody id="fileBody">)([\s\S]*?)(<\/tbody>)/,
  '$1' + demoRows + '\n        $3'
);

// fileEmpty 要保持 hidden
out = out.replace(
  /(<div class="table-empty" id="fileEmpty")(?![^>]*hidden)/,
  '$1 hidden'
);

// 用量条文案
out = out.replace(
  /<span id="usageText">—<\/span>/,
  '<span id="usageText">3 个文件 · 共 100.3 MB</span>'
);
out = out.replace(
  /<span class="usage-hint" id="usageHint"><\/span>/,
  '<span class="usage-hint" id="usageHint">浏览与下载无需登录</span>'
);

// 顶部加一条说明条，避免用户误以为这是真服务
out = out.replace(
  /<body[^>]*>/i,
  (m) => m + `
  <div style="position:fixed;left:0;right:0;bottom:0;z-index:9999;padding:7px 14px;
              font:12px/1.5 -apple-system,'Segoe UI',sans-serif;color:#fff;
              background:#3b4252;text-align:center;">
    静态预览（未登录状态）· 只看文件仓库右上角：遮罩只盖住「上传」按钮，下载按钮仍然可见可点
  </div>`
);

fs.writeFileSync(OUT, out, 'utf8');
console.log('已生成: ' + OUT);
console.log('大小: ' + (out.length / 1024).toFixed(1) + ' KB');
