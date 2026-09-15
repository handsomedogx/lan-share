# LAN Share 简易开发文档

## 1. 项目简介

LAN Share 是一个部署在 **Kwrt/OpenWrt 路由器**上的局域网内容传输工具，主要用于机房、宿舍等局域网环境中，在多台电脑之间快速传输文本、链接和文件。

主要解决以下问题：

- 不方便登录微信、QQ 等聊天软件时快速发送链接或文本
- U 盘传文件效率低
- 临时文件需要快速传给同桌或局域网其他设备
- 常用文件需要长期保存在局域网中，随时下载
- 不依赖公网服务，在校园网内部即可工作

系统分为两个核心区域：

```text
┌───────────────────────────────────────────────┐
│                  LAN Share                    │
├──────────────────────┬────────────────────────┤
│                      │                        │
│      实时传输区       │       文件仓库         │
│                      │                        │
│ 文本 / 链接           │ 上传文件               │
│ 临时消息              │ 下载文件               │
│ WebSocket实时同步     │ 删除文件               │
│ 会话授权码            │ 持久化保存             │
│                      │ 登录后使用             │
│                      │                        │
├──────────────────────┴────────────────────────┤
│                 状态 / 用户信息               │
└───────────────────────────────────────────────┘
```

---

# 2. 部署环境

本项目最重要的设计前提是：

> **服务不是部署在普通 Linux 服务器上，而是长期运行在 Kwrt 路由器中。**

因此开发过程中应始终优先考虑：

- ARM64 架构兼容性
- 较低内存占用
- 较低 CPU 占用
- 尽量减少运行时依赖
- 减少对 Overlay 分区的写入
- 数据统一存放到路由器的大容量数据分区
- 服务能够通过 procd 自动启动和异常重启

当前目标部署设备环境：

```text
设备：
JDCloud / JDBox AX1800 Pro

系统：
Kwrt 25.12-SNAPSHOT

平台：
qualcommax/ipq60xx

CPU 架构：
ARM64 / aarch64

Kernel：
Linux 6.12.x

LAN：
10.0.0.1

Web Server：
nginx

大容量数据分区：
/mnt/data_mmcblk0p27

容量：
约 109 GB
```

因此，本项目不应依赖：

```text
Node.js Runtime
Python Runtime
Java Runtime
Docker Runtime
Redis
PostgreSQL
外部数据库服务
```

最终运行环境应尽量只有：

```text
lan-share
```

一个 Go 编译后的 ARM64 二进制。

---

# 3. 技术栈

## 后端

使用：

```text
Go
```

建议：

```text
Go 1.24+
```

后端负责：

- HTTP Server
- WebSocket
- 用户登录
- 会话管理
- 文件上传
- 文件下载
- 文件删除
- SQLite 数据存储
- 临时文件清理
- 静态页面托管

第一阶段尽量减少第三方依赖。

HTTP Router 可以直接使用：

```go
net/http
```

如果后续路由越来越复杂，再考虑 Chi。

---

## 前端

明确使用：

```text
HTML
CSS
JavaScript
```

不使用：

```text
React
Vue
Angular
Tailwind CSS
Bootstrap
DaisyUI
jQuery
其他 UI 组件库
```

所有 UI：

```text
HTML结构
+
原生CSS
+
原生JavaScript
```

自己完成。

这样做的主要目的并不是追求极致减小文件体积，而是：

- 项目结构简单
- 不需要前端运行时
- 不依赖 npm 生态
- 不需要维护大量依赖
- 页面可以直接嵌入 Go 二进制
- 非常适合路由器长期运行

---

# 4. 推荐项目结构

```text
lan-share/
│
├── cmd/
│   └── server/
│       └── main.go
│
├── internal/
│   ├── auth/
│   ├── session/
│   ├── websocket/
│   ├── file/
│   ├── storage/
│   └── cleanup/
│
├── web/
│   ├── index.html
│   ├── login.html
│   │
│   ├── css/
│   │   └── style.css
│   │
│   ├── js/
│   │   └── app.js
│   │
│   └── assets/
│
├── data/
│
├── go.mod
└── README.md
```

正式部署后则尽量简化为：

```text
/mnt/data_mmcblk0p27/lan-share/
│
├── lan-share
│
├── data/
│   └── lan-share.db
│
├── files/
│   ├── permanent/
│   └── chat/
│
└── logs/
```

其中：

```text
permanent/
```

用于长期文件仓库。

```text
chat/
```

用于聊天房间文件，随房间销毁而删除。

---

# 5. 静态资源打包

前端 HTML、CSS、JS 推荐直接嵌入 Go 二进制。

例如：

```go
//go:embed web/*
var webFS embed.FS
```

最终：

```text
HTML
CSS
JavaScript
Go后端
```

全部包含在：

```text
lan-share
```

一个程序中。

这样升级程序时，只需要替换一个文件。

---

# 6. 页面设计

主页面采用左右布局。

```text
┌────────────────────────────────────────────────────────┐
│ LAN Share                              用户 / 登录      │
├───────────────────────────┬────────────────────────────┤
│                           │                            │
│       实时传输             │        文件仓库            │
│                           │                            │
│ 会话：K7MF                │ 文件名     大小     时间    │
│ 在线：2                   │                            │
│                           │ test.zip   25MB    16:30   │
│ ------------------------  │ demo.exe   12MB    15:20   │
│                           │                            │
│ https://example.com       │                            │
│ [复制] [打开]             │                            │
│                           │                            │
│ 这是测试内容              │                            │
│                           │                            │
│ ------------------------  │                            │
│ 输入内容...               │         [上传文件]         │
│                  [发送]   │                            │
└───────────────────────────┴────────────────────────────┘
```

建议整体采用：

```text
现代简洁
浅色/深色均可
圆角
轻阴影
低饱和度
```

避免做成传统路由器管理后台风格。

---

# 7. 实时传输区

实时区域采用：

```text
WebSocket
```

客户端连接：

```text
/ws/session/{sessionCode}
```

例如：

```text
/ws/session/K7MF
```

用户通过授权码加入会话。

授权码建议采用：

```text
K7MF
A9KD
Q2XP
```

这种短随机字符串。

而不是简单的：

```text
1234
0000
```

---

## 消息类型

第一版只需要：

```text
text
link
```

例如：

```json
{
    "type": "text",
    "content": "把这个复制过去"
}
```

链接：

```json
{
    "type": "link",
    "content": "https://github.com/example"
}
```

---

# 8. 文件仓库

文件仓库与实时聊天分离。

文件上传、下载不要通过 WebSocket。

使用普通 HTTP。

接口例如：

```text
GET    /api/files
POST   /api/files
GET    /api/files/{id}
DELETE /api/files/{id}
```

上传：

```text
POST /api/files
```

下载：

```text
GET /api/files/123
```

文件内容直接保存在：

```text
/mnt/data_mmcblk0p27/lan-share/files/
```

数据库只记录：

```text
文件ID
原始文件名
实际存储名
文件大小
上传用户
SHA256
上传时间
```

不要把文件本身存入 SQLite。

---

# 9. 文件仓库

最初设想过分「临时 / 永久」两套系统，最终只保留了**永久文件仓库**一种。

## 永久文件

用于：

```text
以后还会用到
```

存放：

```text
files/permanent/
```

必须登录后才能上传、下载和管理。

永久文件不会自动删除。

> 早期版本还有一个「临时文件」系统（`files/temporary/`，按 1/6/24 小时到期自动删）。
> UI 后来改成只写永久文件，那套逻辑再没有调用方，已被整体移除
> （含 `files.expires_at` 列与过期回收协程）。要做「限时分享」应当重新设计，
> 而不是复活一个没人用的类型。

---

# 10. 用户系统

第一版无需复杂权限体系。

只需要：

```text
username
password
```

数据库：

```text
users
```

字段：

```text
id
username
password_hash
created_at
```

密码禁止明文保存。

使用：

```text
Argon2id
```

或：

```text
bcrypt
```

进行密码 Hash。

---

# 11. 登录状态

推荐使用：

```text
Cookie + Server Session
```

而不是在浏览器：

```text
localStorage
```

里面长期保存 JWT。

Cookie 推荐：

```text
HttpOnly
SameSite=Lax
```

局域网 HTTP 环境下第一版可以不启用 Secure。

未来启用 HTTPS 后再开启：

```text
Secure
```

---

# 12. SQLite

SQLite 数据库建议放在：

```text
/mnt/data_mmcblk0p27/lan-share/data/lan-share.db
```

不要放在系统 Overlay 中。

基础表：

```text
users
sessions
messages
files
```

其中实时聊天消息第一版甚至可以：

```text
只存在内存
```

不用持久化。

这样：

```text
服务器重启
或
会话结束
```

聊天消息直接消失。

符合“临时传输”的定位。

---

# 13. 网络监听

这是本项目在 Kwrt 环境中的重要安全设计。

Go 服务推荐只监听：

```text
127.0.0.1:18080
```

例如：

```go
http.ListenAndServe("127.0.0.1:18080", handler)
```

不要直接：

```text
0.0.0.0:18080
```

暴露服务。

网络结构：

```text
机房电脑
    │
    │
    ▼
10.0.0.1
    │
  nginx
    │
    ▼
127.0.0.1:18080
    │
 LAN Share
```

这样：

```text
校园网接口
```

无法直接访问 Go 服务端口。

# 14. 略



---

# 15. Go 编译

开发环境可以是 Windows。

因为目标设备是：

```text
Linux ARM64
```

所以使用交叉编译。

PowerShell：

```powershell
$env:GOOS="linux"
$env:GOARCH="arm64"
$env:CGO_ENABLED="0"

go build -o lan-share ./cmd/server
```

生成：

```text
lan-share
```

然后复制到：

```text
/mnt/data_mmcblk0p27/lan-share/
```

---

# 16. SQLite 驱动要求

因为目标环境是 Kwrt/OpenWrt ARM64，因此应尽量避免依赖：

```text
CGO
glibc
额外动态链接库
```

SQLite 推荐选择：

```text
Pure Go SQLite Driver
```

确保：

```text
CGO_ENABLED=0
```

也能正常编译。

目标是生成尽可能独立的：

```text
Linux ARM64 ELF
```

程序。

---

# 17. Kwrt 服务管理

最终为 LAN Share 创建：

```text
/etc/init.d/lan-share
```

使用 Kwrt/OpenWrt 的：

```text
procd
```

管理。

需要支持：

```text
start
stop
restart
enable
disable
```

并开启：

```text
开机自启动
异常退出自动重启
```

使用方式：

```bash
/etc/init.d/lan-share enable
/etc/init.d/lan-share start
```

查看：

```bash
/etc/init.d/lan-share status
```

---

# 18. 日志设计

由于运行设备是路由器，日志必须控制数量。

不建议长期记录：

```text
每个WebSocket心跳
每个HTTP请求
每次文件读取
```

只记录：

```text
服务启动
服务停止
异常
登录失败
文件上传
文件删除
WebSocket重大错误
```

可以直接写入：

```text
logread
```

或限制大小写入：

```text
/mnt/data_mmcblk0p27/lan-share/logs/
```

禁止无限增长。

---

# 19. 第一阶段功能

第一阶段只实现核心功能。

```text
1. 首页布局

2. 创建临时会话

3. 输入授权码加入会话

4. WebSocket实时发送文本

5. 自动识别URL

6. 一键复制消息

7. 用户登录

8. 文件列表

9. 文件上传

10. 文件下载

11. 文件删除

12. SQLite

13. nginx反代

14. procd启动
```

暂时不要实现：

```text
WebRTC
文件分片
秒传
断点续传
多级文件夹
用户注册
复杂权限
管理员后台
聊天记录永久保存
在线预览Office
视频预览
PWA
```

---

# 20. 第二阶段功能

第一版稳定后再考虑：

```text
临时文件传输

拖拽上传

粘贴图片

文件搜索

上传进度

文件大小限制

二维码加入会话

自动清理临时文件

共享剪贴板

暗色模式

文件重命名
```

如果大文件传输最终发现：

```text
PC → 路由器 → PC
```

对路由器 IO 或 CPU 压力较大，再考虑：

```text
WebRTC DataChannel
```

实现 PC 与 PC 局域网 P2P 文件直传。

---

# 21. 开发原则

整个项目始终遵循以下原则：

### 路由器优先

这是一个：

```text
运行在 Kwrt 路由器上的程序
```

而不是普通服务器程序。

任何技术选择都应该首先考虑：

```text
ARM64
资源占用
依赖
存储
升级
稳定性
```

### 单二进制优先

目标：

```text
一个 lan-share 文件即可运行
```

### 原生前端优先

使用：

```text
HTML + CSS + JavaScript
```

不因为一个简单 UI 引入完整前端框架。

### 文件系统优先

大文件：

```text
Filesystem
```

元数据：

```text
SQLite
```

### LAN 优先

系统主要服务：

```text
10.0.0.0/24
```

局域网用户。

默认不面向公网。

---

# 22. 最终目标架构

```text
               机房 / 宿舍电脑
                       │
                       │ HTTP / WebSocket
                       ▼
                 10.0.0.1
                       │
                    nginx
                       │
                       ▼
              127.0.0.1:18080
                       │
                  LAN Share
                    (Go)
                 /           \
                /             \
         WebSocket            HTTP
        临时实时通信       文件/API/登录
                              │
                   ┌──────────┴──────────┐
                   │                     │
                SQLite               Filesystem
                   │                     │
                   └──────────┬──────────┘
                              │
                              ▼
                 /mnt/data_mmcblk0p27
```

最终用户体验应达到：

```text
打开浏览器
    ↓
访问 share.lan
    ↓
输入 4~6 位会话码
    ↓
立即发送文本/链接
```

需要长期保存文件时：

```text
登录
 ↓
文件仓库
 ↓
上传 / 下载 / 删除
```

整个系统无需依赖公网、无需安装客户端，也无需在机房电脑上安装任何软件。