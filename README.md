# LAN Share

部署在 **Kwrt/OpenWrt 路由器**上的局域网内容传输工具。

左侧是**实时传输区**（WebSocket，随手发文本/链接/文件），右侧是**文件仓库**（HTTP，长期保存文件）。
不依赖公网、不需要客户端、不在机房电脑上装任何软件。

**左右两个面板都支持拖拽上传**：把文件拖到左侧即发进当前房间（随房间销毁而删除），
拖到右侧即存入文件仓库（需登录，可长期保存）。

> 文件仓库**浏览和下载不需要登录**，上传 / 删除才需要 —— 分享链接发出去对方就能取。
> 详见「文件仓库」一节的鉴权表。

---

## 一、它解决什么问题

- 不方便登录微信/QQ 时，快速把链接、文本或文件发给同桌
- U 盘传文件效率低
- 随手建个一次性房间把文件传过去，聊完就散，文件随房间一起消失，不留痕
- 常用文件需要长期放在局域网里，随时下载

---

## 二、设计前提：这是路由器程序，不是服务器程序

所有技术选择都服务于「长期跑在 AX1800 这类 ARM64 路由器上」这个前提：

| 约束 | 做法 |
|---|---|
| ARM64 | `GOARCH=arm64` 交叉编译 |
| 无运行时依赖 | `CGO_ENABLED=0` + Pure Go SQLite（`modernc.org/sqlite`） |
| 低内存/低 CPU | 聊天消息只存内存；索引精简；日志只记关键事件 |
| 少写 Overlay | 数据库/文件/日志统一落 `/mnt/data_mmcblk0p27` |
| 单文件升级 | 前端用 `go:embed` 打进二进制，只替换一个文件 |
| 自动重启 | procd `respawn`，并处理数据分区晚挂载 |
| 安全边界 | Go 只监听 `127.0.0.1:18080`，由 nginx 反代 |

**运行环境里最终只有一个 `lan-share` 文件。**

---

## 三、技术栈

**后端** Go 1.24+ · 标准库 `net/http`（无 router 依赖）· 手写 RFC 6455 WebSocket 服务端

**前端** 原生 HTML + CSS + JavaScript —— 无 React/Vue/Tailwind/Bootstrap/jQuery，无构建步骤

**存储** SQLite（Pure Go 驱动）存元数据 · 文件系统存文件本体

---

## 四、项目结构

```text
lan-share/
├── cmd/server/
│   ├── main.go              入口、静态资源托管、优雅关闭
│   └── web/                 ← 前端资源（embed 源目录，必须在此）
│       ├── index.html
│       ├── css/style.css
│       └── js/app.js
│
├── internal/
│   ├── config/              配置与环境变量
│   ├── logger/              带大小轮转的日志
│   ├── storage/             SQLite 打开/迁移/全部数据访问
│   ├── auth/                bcrypt 密码哈希与登录
│   ├── session/             实时会话（房间号 + 存活时长，仅内存）
│   ├── httpx/               登录 Cookie、JSON 响应辅助
│   ├── websocket/           手写 RFC 6455 服务端
│   ├── files/               文件落盘、SHA256、文件名清洗
│   ├── cleanup/             无主聊天文件 / 半成品文件 / 会话清理
│   └── api/                 路由与全部 handler（含 chat_files.go）
│
├── web/                     前端源文件（同步到 cmd/server/web/）
│
├── deploy/
│   ├── lan-share-direct.init /etc/init.d/lan-share（procd，直连 0.0.0.0:18080）
│   ├── lan-share.init        /etc/init.d/lan-share（procd，nginx 反代 127.0.0.1:18080）
│   └── nginx-lan-share.conf  nginx 反代配置
│
│   ↑ 只有这两套部署语义，没有第三套（早先的 minimal 变体已删除 ——
│     它和 direct 的唯一差别只是环境变量写法，却多出一份需要同步维护的脚本）
│
├── scripts/
│   ├── build.bat            Windows 一键交叉编译（**纯 ASCII，勿加中文**）
│   ├── build.sh             bash 一键交叉编译
│   ├── verify-arch.py       校验 dist/ 产物架构（ELF64/AArch64 而非 PE）
│   ├── ws-smoke.js          WebSocket + 房间 + 聊天文件冒烟测试（零依赖，Node 手写 RFC 6455）
│   ├── anon-access-smoke.js 仓库「匿名可读、不可写」权限边界测试
│   ├── dedup-smoke.js       仓库 sha256 去重端到端测试（按磁盘文件数断言）
│   ├── make-preview.js      生成静态 UI 预览（无截图环境的交付路径）
│   └── ttl-expiry-test.js   存活时长 / 倒计时语义测试
│
├── go.mod
└── README.md
```

> **注意**：`go:embed` 只能嵌入**包目录内**的文件，所以前端真实位置是
> `cmd/server/web/`。根目录的 `web/` 是便于编辑的源副本，改完记得同步：
>
> ```powershell
> Copy-Item web\* cmd\server\web -Recurse -Force
> ```

---

## 五、构建

### Windows

```powershell
.\scripts\build.bat          # 或指定版本号
.\scripts\build.bat 0.2.0
```

### bash / Linux / macOS

```bash
./scripts/build.sh
```

### 手动编译

```powershell
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
go build -trimpath -ldflags "-s -w -X lanshare/internal/api.Version=0.2.0" -o lan-share ./cmd/server
```

产物 `dist/lan-share`：约 **10.3 MB** 的独立 Linux ARM64 ELF（已验证 ELF magic / 64-bit / `e_machine=0xB7`）。

### ⚠️ 编译后必须校验架构

`go build` 有个**静默失败**：万一 `GOOS` / `GOARCH` 没生效，它会**无声地把当前平台的
二进制写进 `dist/lan-share`** —— 文件名看起来完全正常。这个问题在本机不会报任何错，
只会在你把文件传到路由器上、执行时报 `Exec format error` 才暴露（白跑一趟）。

所以两个构建脚本最后都会跑 `scripts/verify-arch.py` 读回头几个字节断言：

```text
  [ OK ] dist\lan-share         ELF64 LE e_machine=0xB7 (linux/arm64, 9.94 MB)
  [ OK ] dist\lan-share.exe     PE/MZ (windows/amd64, 10.79 MB)
```

它能认出「ELF 但架构不对」「是 PE 不是 ELF」这两种情况，**失败时返回非零退出码**，
让构建中止。也可以单独手动跑：

```powershell
python scripts\verify-arch.py
```

> **`scripts/build.bat` 必须保持纯 ASCII。** `cmd.exe` 按 OEM 代码页（简中 Windows 是 GBK）
> 读取 `.bat`，写进去的 UTF-8 中文会被解成乱码，其首个 token 还会**被当成命令执行**，
> 于是爆出一整屏 `'xxx' 不是内部或外部命令`。更坏的是 `set GOOS=linux` 这类行
> 一旦被破坏，环境变量就不生效 → 编出 Windows 二进制覆盖 `dist/lan-share`。
> 中文说明请写在 README 里，`.bat` 一律用英文注释。

---

## 六、部署到路由器

有**两条路**，先选一条：

| | 直连模式 | 反代模式 |
|---|---|---|
| 监听 | `0.0.0.0:18080` | `127.0.0.1:18080` |
| 环境变量 | `LANSHARE_LISTEN=0.0.0.0:18080` | `LANSHARE_LISTEN=127.0.0.1:18080` |
| 脚本 | `deploy/lan-share-direct.init` | `deploy/lan-share.init` |
| 对外入口 | 就是 18080 本身 | nginx `:80` |
| 需要装 nginx | 不用 | 要 |
| 谁能访问 | 整个局域网 / 校园网 | 只有穿过 `:80` 的流量 |
| 适合 | 内网自用、图省事 | 想收窄暴露面 |

> **只有这两套部署语义。** 早先还有个 `minimal` 变体，已删除 —— 它和 direct 的
> 唯一差别只是环境变量写法，却多出一份要跟着改的脚本，纯属负担。

> **两条路的区别只有一个：暴露面。**
> 直连时任何人访问 `http://<路由器IP>:18080` 就能用；
> 反代时 Go 藏在回环里，校园网口碰不到 18080，只能从 `:80` 进。
> 权限模型（浏览下载开放、上传删除需登录）两条路完全一样。

### 1. 准备目录

```sh
# 本机 /data → /mnt/data_mmcblk0p27 是软链，两者等价。
# 脚本里用真实挂载点，避免依赖软链。
mkdir -p /mnt/data_mmcblk0p27/lan-share
```

### 2. 上传二进制

把 `dist/lan-share`（linux/arm64，**必须**是这个）传到：

```
/mnt/data_mmcblk0p27/lan-share/lan-share
```

然后加上可执行位：

```sh
chmod +x /mnt/data_mmcblk0p27/lan-share/lan-share
```

> **确认架构**：在路由器上 `uname -m` 应当输出 `aarch64`。
> 若不是（比如 `mips`、`armv7l`），现在这个二进制跑不起来，
> 需要按对应的 `GOARCH` 重新编译。

### 3. 起服务前先看它要碰哪些文件

只读，不产生任何副作用：

```sh
cd /mnt/data_mmcblk0p27/lan-share
./lan-share -print-paths
```

确认「数据根目录 / 数据库 / 文件仓库」都指向你期望的位置再继续。

### 4A. 直连模式（不装 nginx）

```sh
cp deploy/lan-share-direct.init /etc/init.d/lan-share
chmod +x /etc/init.d/lan-share
/etc/init.d/lan-share enable     # 开机自启
/etc/init.d/lan-share start
/etc/init.d/lan-share status
```

之后访问 `http://<路由器IP>:18080`。

启动日志里会出现一条
`[ERROR] 警告：当前监听地址不是回环地址，服务可能直接暴露到局域网/校园网…`
—— 这是**设计如此，不是故障**。程序刻意把非回环监听标成 ERROR 级别，
防止有人不知不觉把服务暴露出去。

### 4B. 反代模式（需要 nginx）

```sh
cp deploy/lan-share.init /etc/init.d/lan-share
chmod +x /etc/init.d/lan-share
/etc/init.d/lan-share enable
/etc/init.d/lan-share start

cp deploy/nginx-lan-share.conf /etc/nginx/conf.d/lan-share.conf
nginx -t && /etc/init.d/nginx reload
```

之后访问 `http://10.0.0.1/`。

### 5. 创建第一个用户

**第一个注册的人自动成为管理员**，不需要命令行。浏览器打开页面会自动引导你注册：

- 数据库为空时，注册入口**默认开放**（`/api/status` 返回 `needSetup: true`）
- 首个注册成功的账号获得 `role = admin`
- 从此注册**自动关闭**，后续账号必须由管理员在「管理 → 注册开关」里打开

想跳过 UI、直接在路由器上建号也可以：

```sh
cd /mnt/data_mmcblk0p27/lan-share
./lan-share -init-user=admin:你的密码
```

> 首个用户自动为 admin；若库里已有用户，则按普通用户（`user`）创建。
> 命令会先打印**目标数据库路径**，确认它指向你期望的位置再继续 ——
> 这个提示是为了避免「以为改了 A 库、实际改的是 B 库」。

### 5.1 注册开关与角色

| 角色 | 能力 |
|---|---|
| `admin` | 全部普通能力 + 用户列表 + 开关注册 + 改他人角色 + 删除任意文件 |
| `user` | 上传、删除**自己**的文件；**浏览和下载对所有人开放**（含未登录） |

- 空库时注册必然开放，避免「没人能成为管理员」的死局
- 关掉注册后仍有管理员可随时再打开
- **不能把最后一个管理员降级**（接口返回 `400`），防止把自己锁在门外
- 老库（`users` 表没有 `role` 列）启动时自动迁移，并把最早的用户提升为管理员

### 6. 验证部署成功

```sh
# 进程在不在
ps | grep lan-share

# 端口在不在听（直连模式应当是 0.0.0.0:18080）
netstat -ltnp | grep 18080

# 程序自己的日志（含轮转）
tail -f /mnt/data_mmcblk0p27/lan-share/logs/lan-share.log

# procd 收走的 stdout
logread | grep lan-share
```

再从**另一台电脑**（机房/宿舍）打开 `http://<路由器IP>:18080`，
能看到页面、能注册管理员，就算成功。

> **首次部署的唯一硬性顺序：先起服务 → 再注册管理员。**
> 注册入口只在空库时自动开放。若先手动 `-init-user` 建过号，
> 之后再加账号就必须靠管理员开注册开关了。

---

## 七、最终架构

**直连模式**（`deploy/lan-share-direct.init`）：

```text
        机房 / 宿舍电脑
               │  HTTP / WebSocket，直连 18080
               ▼
        <路由器IP>:18080          ← Go 监听 0.0.0.0，局域网内均可访问
               │
           LAN Share
          /         \
    WebSocket        HTTP
   临时实时通信    文件 / API / 登录
                        │
             ┌──────────┴──────────┐
          SQLite               Filesystem
             └──────────┬──────────┘
                        ▼
          /mnt/data_mmcblk0p27/lan-share/
```

**反代模式**（`deploy/lan-share.init` + `nginx-lan-share.conf`）：

```text
        机房 / 宿舍电脑
               │  HTTP / WebSocket
               ▼
          10.0.0.1:80
               │
            nginx                ← 唯一对外入口
               │
         127.0.0.1:18080         ← Go 只监听回环，校园网口碰不到
               │
           LAN Share
          /         \
    WebSocket        HTTP
   临时实时通信    文件 / API / 登录
                        │
             ┌──────────┴──────────┐
          SQLite               Filesystem
             └──────────┬──────────┘
                        ▼
          /mnt/data_mmcblk0p27/lan-share/
```

---

## 八、接口一览

### 登录态（Cookie + 服务端 Session）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/register` | 注册；空库或注册开关打开时可用，成功后自动登录 |
| POST | `/api/auth/login` | 登录，下发 `HttpOnly` Cookie |
| POST | `/api/auth/logout` | 登出 |
| GET | `/api/auth/me` | 当前登录用户 |

返回体统一为 `{id, username, role, isAdmin}`，**永不包含密码哈希**。

### 管理后台（需 `admin`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/users` | 用户列表（含各自文件数） |
| GET | `/api/admin/settings` | 读取设置（`registrationOpen`） |
| PUT | `/api/admin/settings` | 开/关注册：`{"registrationOpen": true}` |
| PUT | `/api/admin/users/{id}/role` | 改角色：`{"role":"admin"\|"user"}`，拒绝降级最后一个管理员 |

鉴权顺序：未登录 → `401`；已登录但非管理员 → `403`。

### 实时会话

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/sessions` | 创建房间，可自定义房间号与存活时长 |
| GET | `/api/sessions/{code}` | 查询房间是否存在、剩余存活时间与在线人数 |
| POST | `/api/sessions/{code}/files` | **上传聊天文件**（multipart，字段 `file`）；**不需要登录** |
| GET | `/ws/session/{code}` | WebSocket 连接（`?name=` 可选昵称） |

`POST /api/sessions` 请求体（两个字段都可省略）：

```json
{ "code": "ABC123", "ttlMinutes": 60 }
```

- **`code`（房间号）**：4–8 位字母或数字，大小写不敏感（服务端统一转大写）。
  留空则由系统随机生成。已被占用且未过期 → `409`；长度或字符不合法 → `400`。
- **`ttlMinutes`（存活时长）**：只接受白名单档位 `10 / 60 / 360 / 1440`，
  非白名单值 → `400`。留空默认 `60`（1 小时）。
  **`0` 会被拒绝** —— 0 曾表示「不限时」，现已移除：一个永不自动消失的房间
  既违背「临时传输、用完即毁」的定位，也让聊天文件失去回收触发点。
  房间必须有确定的终点。
- 响应带 `expiresAt`（毫秒时间戳）与 `remainingSeconds`（`0` 表示已过期）。
  正常路径上房间必定有时长，所以不会出现「无期限」的房间。
- **过期即自动销毁**：后台每分钟回收一次，到点后房间连同全部内存消息**和它名下的聊天文件**一并消失，
  同名房间号随之释放，可再次创建（见下方「回收」）。

> 为什么把时长做成白名单而不是任意分钟数：房间是「用完就散」的东西，
> 给一排现成档位比让用户填数字更快，也避免了「填 3 分钟又后悔」这类无意义的边界情况。

### 聊天室文件

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/sessions/{code}/files` | 上传到房间，返回 `{id, name, size, sizeText, downloadUrl}` |
| GET | `/api/chat-files/{id}` | 下载聊天文件，支持 Range |

为什么上传**不要求登录**：实时传输区的定位就是「不方便登录聊天软件时随手传点东西」。
**房间号本身就是凭据** —— 只有拿到它的人才能进这个房间，也才能往里面传。
这与「文件仓库必须登录」形成清晰分界：

```
左边（实时传输）= 凭房间号，用完即毁；右边（文件仓库）= 凭账号，长期保存
```

下载同样不校验房间号：URL 本身是 WebSocket 广播出去的，能拿到就说明当时在房间里。
反之若强制带房间号，「别人转发过来的下载链接」会失效 —— 而这恰恰是局域网传文件最常见的用法。

`kind` 上的两态区分了生命周期：

| kind | 归属 | 何时消失 |
|---|---|---|
| `permanent` | 文件仓库 | 手动删除（或管理员删） |
| `chat` | 聊天房间（带 `room_code`） | **房间销毁时立即删除** |

> 只有这两个取值。历史上还有过 `temporary`（按 `expires_at` 到期自动删），
> 已随那套逻辑一起移除，数据库里也不会再有该值。

房间销毁 → 文件清理有**双保险**：

1. **即时**：`session.Manager` 的房间销毁回调（`SetRoomGoneHandler`）触发 `purgeRoomFiles`，
   删掉磁盘文件 + 数据库记录。回调在**释放全局锁之后**执行 —— 删盘是慢 I/O，
   持锁会拖住所有房间。
2. **兜底**：清理协程每 10 分钟用「活跃房间号集合」与数据库里的 `chat` 文件做差集，
   回收「房间已删、文件还没来得及删」的中间态（比如进程恰好被杀）。

另外，`GET /api/chat-files/{id}` 在**房间已不存在时立即返回 `410 Gone`** ——
即便清理还没跑到，语义上「房间没了 = 文件没了」也是立刻成立的。

**复用过期房间号时，旧房间先清完、新房间才出现。** 聊天文件只记 `room_code`，
如果先把旧房间从表里删掉、再异步去清理磁盘，中间这个窗口里若有人用同一个房间号
建了新房间，他就会**看到上一代遗留的聊天文件** —— 那是别人传的东西。

所以房间号进入「退役中」（retiring）状态：`CreateWithCode` 在复用过期号时
先摘除旧房间并标记 retiring，锁外同步跑完销毁回调，**跑完才把新房间放进去**。
在退役期间，这个号码既不能被创建、也不会被随机生成器选中、回收协程也会跳过它 ——
对外的表现就是「号还是被人占着」，直到旧资源彻底清完。
`kind != chat` 的文件一律 `404`，防止有人拿这个免登录入口下载仓库文件。

### 文件仓库

> 仓库**只有永久文件**。上传一律 `kind=permanent`，没有别的取值。
>
> 历史上存在过 `temporary`（按 `expires_at` 到期自动删），但 UI 早已不再写入它，
> 后端那套读写逻辑因此成了纯负债，已被整体移除 —— 包括 `files.expires_at` 列、
> 过期回收分支与 `temporary/` 目录。若将来要做「限时分享」，
> 应当重新设计（独立的表 + 显式过期策略），而不是复活一个从未被使用的 kind 值。

**读开放、写收紧** —— 这是刻意的：

| 方法 | 路径 | 鉴权 |
|---|---|---|
| GET | `/api/files?kind=permanent` | **无需登录** |
| GET | `/api/files/{id}` | **无需登录**（支持 Range） |
| POST | `/api/files` | 需登录 |
| PATCH | `/api/files/{id}` | 需登录，且**只能改自己的**（管理员不限）；body `{"name":"新名字"}` |
| POST | `/api/files/{id}/pin` | 需登录，且**只能动自己的**（管理员不限）；body `{}` 翻转、`{"pinned":true}` 显式设置 |
| DELETE | `/api/files/{id}` | 需登录，且**只能删自己的**（管理员不限） |

理由：文件仓库是「局域网共享盘」。分享出去一条链接，对方直接就能下 —— 这才是它
存在的意义，为了下载去建个账号纯属添堵。而**删除是不可逆的破坏性操作**，
一旦匿名开放，任何人凭一个从列表里读到的 id 就能抹掉全仓库的文件。
上传同理：不登录就不知道文件算谁的，归属链会断掉。

**改名与置顶走的是和删除完全相同的权限规则**，服务端由同一个
`writableFile()` 统一把关，三处不会各自漂移。理由也一样：改的是
「所有人都会看见的东西」—— 名字显示在整个共享盘上，置顶会改掉所有人的列表顺序。
不登录就谈不上「这是你的文件」，匿名改名等于把文件名变成公共涂鸦墙。

**改名是纯元数据操作。** 只改数据库里的 `original_name`，磁盘上的
`stored_name` 分毫不动（存储名本来就是随机生成的 `<unixms>-<hex>.bin`）。
因此改名不搬文件、不重算哈希、不影响已发出的下载链接。名字进库前必须过
`files.SafeDisplayName()`：剥离路径、去掉控制字符与引号、限 200 字符、
空名回退成 `file`。

**改名只改一条记录。** 内容去重后多条记录可能共享同一个 `stored_name`，
`UPDATE ... WHERE id = ?` 只命中本行 —— 这正是「各人看到自己的文件名」成立的前提。

**置顶排序在服务端做。** `ListFiles` 的排序是
`ORDER BY pinned DESC, created_at DESC, id DESC`。仓库是多人共用的共享盘，
置顶是「这块盘上的共识」，只有落在一处排序，所有客户端（含直连 API 的）
看到的顺序才一致；前端再排一次只会变成两处需要同步维护的逻辑。

**`pinned` 是列表项上的一个布尔字段。** 列表（`GET /api/files`）与单条
（`GET /api/files/{id}`、上传、改名、置顶的响应）返回的都是同一个 `fileResp` 结构：

```json
{
  "id": 12,
  "name": "路由器固件.img",
  "size": 31457280,
  "sizeText": "30.0 MB",
  "kind": "permanent",
  "owner": "alice",
  "createdAt": 1789446096000,
  "pinned": true,
  "downloadUrl": "/api/files/12"
}
```

（`sha256` 带 `omitempty`，值为空时不出现。`createdAt` 是 Unix 毫秒。）

前端把它当**只读**信号用：置顶行加 `.is-pinned` 类、文件名后跟一个图钉标记，
排序本身不碰 —— 服务端已经排好了。`POST /api/files/{id}/pin` 的响应就是被改过
之后的那一条，`pinned` 值以服务端为准（连传两次 `{"pinned":true}`，
第二次仍是 `true`，不是翻转）。

> 为什么不做成「每个用户各自的置顶」：那需要一张 `user_id × file_id` 的关联表，
> 而这块盘的使用场景是几个人在一台路由器上互换文件，共享一份顺序比各看各的更合用。
> 真要改成分人视图，第一件事是把这行 ORDER BY 拆开，而不是在前端加过滤。

**删除时按「磁盘对象」计数，而不是按内容指纹。** 仓库对同内容文件做去重：
多条数据库记录可以共享同一个磁盘文件（`stored_name` 相同）。所以删掉一条记录
**不代表**能把磁盘文件抹掉 —— 只有「这个 `stored_name` 已经没有任何记录引用」
才轮到删盘。

计数口径必须是 `stored_name` 本身。若按 `sha256 + kind + owner_id` 去数，
「甲和乙各传了一份相同内容、磁盘上其实只有一份」的场景会被误判成两份独立文件，
删掉甲那条就把乙还能用的文件一起删了（**磁盘文件丢失，但乙的记录还在**）。

而且计数**失败时绝不删盘**：

```text
CountFilesByStoredName 报错  →  记 ERROR 日志，直接返回 200，磁盘文件原样保留
```

宁可留下一个孤儿文件（清理协程会兜底），也不能因为一次数据库抖动就误删用户数据。
早先版本的 bug 正是这里 —— 计数出错时用了零值 `0` 继续往下走，
表面「成功」实则把文件删了，与注释写的「保守处理」完全相反。

> 未登录时前端只把**上传按钮**盖住（`.lock-overlay` 是一张小卡片浮在上传按钮上），
> 列表和下载按钮照常可见可点。不要把整块面板糊住 —— 那会连「看和拿」一起挡住。
> 界面隐藏按钮只是礼貌，**真正的闸门在服务端**：`handleDelete` 里
> `httpx.CurrentUser(r, s.store) == nil` → `401`，这一步不可能被前端绕过。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/status` | 运行状态（含 `userCount` / `needSetup` / `registrationOpen`） |

### WebSocket 协议

上行（客户端 → 服务端）：

```json
{ "type": "text", "content": "把这个复制过去", "cid": "c1a2b3c4" }
{ "type": "link", "content": "https://github.com/example", "cid": "c1a2b3c5" }
{ "type": "file", "fileId": 42, "cid": "c1a2b3c6" }
```

`cid` 是发送方生成的临时标识（可选）。服务端**原样回传**，前端靠它认出
「这条广播就是我刚发的那条」，从而用服务端版本替换掉本地乐观渲染的那条。

> 为什么不用「昵称 + 内容」做指纹：昵称可能为空（服务端会用 IP 兜底，
> 此时前端并不知道自己叫什么），内容也可能重复 —— 两者都会让去重失效。
> `cid` 是唯一可靠的依据。

**文件卡片的分享是两步的**，这样文件本体走 HTTP、卡片走 WebSocket：

1. 先把文件 `POST` 到 `/api/sessions/{code}/files`，拿到 `{id, name, size, downloadUrl}`
2. 再通过 WebSocket 发 `{"type":"file","fileId":42,"cid":"..."}`

上行只带 `fileId`，**文件名 / 大小 / 下载地址一律由服务端查库补全** ——
客户端没法伪造一个指向别人文件的卡片。服务端还会校验
`file.kind == chat && file.roomCode == 房间号`，不满足就回一句
「文件不存在或已被清理」（而不是「存在但不属于你」，避免泄漏信息）。

下行统一信封：

```json
{ "event": "hello",   "code": "K7MF", "self": "127.0.0.1", "online": 2, "members": ["a","b"], "history": [], "expiresAt": 1758000000000, "remainingSeconds": 3599 }
{ "event": "join",    "online": 3, "members": ["a","b","c"] }
{ "event": "leave",   "online": 2, "members": ["a","b"] }
{ "event": "message", "message": { "id":"...", "type":"link", "content":"...", "sender":"...", "sentAt": 0, "cid":"..." } }
{ "event": "message", "message": { "id":"...", "type":"file", "content":"报表.xlsx", "sender":"...", "sentAt": 0,
                                  "fileName":"报表.xlsx", "fileSize":20480, "fileText":"20.0 KB",
                                  "fileUrl":"/api/chat-files/42", "cid":"..." } }
{ "event": "error",   "error": "消息过长（上限 4000 字）" }
```

- **`hello.self` 是服务端给这条连接分配的显示名**。昵称留空时服务端用
  客户端 IP 兜底（同一台机器开多个标签页会拿到相同 IP）。
  前端必须采用这个值，否则判断「这条是不是我发的」永远不成立。
- `hello.expiresAt` / `hello.remainingSeconds` 让前端一进房就显示倒计时。
- 服务端会**自动识别 URL**：内容整体匹配 `https?://` 即判为 `link`，保证各客户端类型一致。
- 广播包含发送者自己；发送者用它认领并替换本地的乐观渲染条目。
- `cid` **只在实时广播里出现**，写进历史时会清空 —— 历史回放不需要去重语义。
- 消息**只存内存**，最多保留 300 条。房间到点即整体销毁。
- 连接一个**不存在或已过期**的房间号：已过期 → `410 Gone`；
  从未创建过 → 自动按默认时长创建（方便直接分享链接拉人）。

### WebSocket 分帧实现

服务端自己手写 RFC 6455，内部分成**两层**，不要混在一起：

| 层 | 职责 |
|---|---|
| 帧层 | 只读一个 frame：解析 FIN / opcode / mask / 长度（含 126 / 127 扩展长度）、校验 RSV、解掩码 |
| 消息层 | 维护「当前这条消息还没收完」的状态，把 data / continuation 帧**拼成一条完整消息**，处理夹在中间的控制帧 |

**为什么必须分开**：一条逻辑消息允许被拆成多个帧（fragmentation）——
首个数据帧带 opcode（`text` / `binary`），后续帧都是 `continuation`（opcode `0`），
最后一片 `FIN=1`。中间**允许**穿插 `ping` / `pong` / `close` 这类控制帧，
它们必须被立即处理，**不能**被当成消息内容的一部分拼进去。

按帧处理的实现对同一帧序列是完全无能为力的：`hel` 收到一帧就当成一条消息处理完，
后面那片 `lo` 会被当成「又一条消息」，用户看到的是两条都是半截的文本。
分片是浏览器在消息较大时的正常行为，不是异常 —— 不拼接就是稳态出错。

```text
frame1  FIN=0  opcode=text(1)         payload="hel"
frame2  FIN=1  opcode=continuation(0) payload="lo"     →  一条消息 "hello"

frame1  FIN=0  opcode=text(1)         payload="hel"
frame2  FIN=1  opcode=ping(9)         payload="x"      →  立刻回 pong，不影响消息
frame3  FIN=1  opcode=continuation(0) payload="lo"     →  一条消息 "hello"
```

累计长度上限（`maxMessageSize`）要对**拼接后的总长**判断，
不能只看单帧 —— 否则一堆小片能拼出超大的消息，等于绕过了限制。

---

## 九、环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `LANSHARE_LISTEN` | `127.0.0.1:18080` | 监听地址 |
| `LANSHARE_ROOT` | `/mnt/data_mmcblk0p27/lan-share` | 数据根目录 |
| `LANSHARE_DB` | `<ROOT>/data/lan-share.db` | SQLite 路径 |
| `LANSHARE_FILES` | `<ROOT>/files` | 文件仓库根目录 |
| `LANSHARE_LOG` | `<ROOT>/logs/lan-share.log` | 日志路径 |
| `LANSHARE_MAX_UPLOAD_MB` | `0`（不限） | **文件仓库**单文件上传上限；`0` 表示仓库不限 |
| `LANSHARE_CHAT_UPLOAD_MB` | `100` | **聊天室**单文件上限；`0` 表示聊天室不限 |
| `LANSHARE_UPLOAD_CONCURRENCY` | `2` | 服务端同时处理的上传数；`0` 表示不限 |
| `TMPDIR` | `<ROOT>/tmp` | 临时文件目录。**不要指向 `/tmp`**（OpenWrt 上是 tmpfs＝内存，大文件会 OOM）。程序启动时若未设置会自动设成 `<ROOT>/tmp` |

> **两个上传上限完全独立，互不回落。** 早先的实现里，不做任何设置会让聊天室
> 「悄悄继承」仓库的上限；`0` 又会被当成「没配置」而回落到仓库值 —— 于是
> 「仓库不限、聊天室 100MB」这种最常见的组合根本表达不出来。
> 现在两者各自解析、各自生效：
>
> ```text
> LANSHARE_MAX_UPLOAD_MB=0        文件仓库不限
> LANSHARE_CHAT_UPLOAD_MB=0       聊天室不限
> ```
>
> 运行时的实际上限由**每次上传调用显式传入**，不再挂在文件服务对象上 ——
> 谁调用谁负责给出自己的口径，仓库和聊天室因此不可能互相污染。

命令行参数：

```text
-init-user=用户名:密码   创建用户（已存在则跳过）
-print-paths            打印本次运行使用的路径后退出
-version                打印版本号
```

---

## 十、数据布局

```text
/mnt/data_mmcblk0p27/lan-share/
├── lan-share                  二进制（唯一可执行文件）
├── data/
│   └── lan-share.db           SQLite：users / sessions / files
├── files/
│   ├── permanent/             文件仓库（长期保存）
│   └── chat/                  聊天室文件，随房间销毁而删除
└── logs/
    └── lan-share.log          超过 2MB 自动轮转为 .log.1
```

> 聊天**消息**完全在内存里，不落盘；房间到点销毁时消息随之消失。
> 聊天**文件本体**必须落盘（可能几十 MB），但数据库记录带 `room_code`，
> 房间一销毁就跟着删 —— 对用户而言和消息一样是「一起消失的」。

文件本体一律存文件系统，**不进 SQLite**。存储名由「时间戳 + 随机串」生成，
与原始文件名解耦，从根上杜绝路径穿越；下载时通过 `Content-Disposition`
还原中文原名。

---

## 十一、第一阶段已实现

首页布局 · **自定义房间号** · **房间存活时长（10 分钟 / 1 小时 / 6 小时 / 24 小时）** ·
**到期自动销毁** · WebSocket 实时文本 · 自动识别 URL ·
**聊天室文件分享（随房间一起销毁）** · **左右双侧拖拽上传** · **自定义开关样式** ·
一键复制消息 · **首个用户自动成为管理员** · 可开关的注册 · 管理员后台（用户列表 / 改角色） ·
登录 · 文件列表 · 上传 / 下载 / 删除（含归属校验）· **双击文件名就地改名** ·
**文件置顶（置顶项在列表最前）** · SQLite · nginx 反代 · procd 启动

**刻意未做**（按文档要求）：WebRTC、分片/秒传/断点续传、多级文件夹、
复杂权限（当前只有 admin/user 两级）、聊天记录持久化、Office 预览、视频预览、PWA。

---

## 十二、安全说明

- 密码 bcrypt 哈希（cost 10），绝不存明文；登录失败不区分「用户不存在/密码错」
- Cookie `HttpOnly` + `SameSite=Lax`；局域网 HTTP 下暂不开 `Secure`
- **监听地址决定暴露面**（详见第六节的两条路）：
  - 反代模式：Go 只监听 `127.0.0.1`，公网/校园网口碰不到 18080
  - 直连模式：监听 `0.0.0.0`，**整个局域网都能访问**。程序会在启动日志里
    打一条 ERROR 级别的告警提醒你，这是刻意设计而非故障
- 响应统一带 `X-Content-Type-Options: nosniff`，下载一律 `attachment`，
  避免上传的 HTML/SVG 被当页面执行
- 存储名与原始名解耦，文件名经过清洗，接口不接受任何磁盘路径
- 文件仓库**读开放、写收紧**：列表与下载无需登录（便于分享链接），
  但上传 / 改名 / 置顶 / 删除必须登录，并校验归属（管理员不限）
- 删除是不可逆操作，因此**绝不匿名放行**；`handleDelete` 里未登录直接 `401`，
  且在动任何数据之前完成判定。改名与置顶共用同一个 `writableFile()` 守卫，
  权限规则只有一份，不会出现「删除收紧了、改名忘了改」这类缺口
- 管理员判定以**数据库里的 `role` 字段**为准，每次请求经会话联表实时读取，
  降级即刻生效（不缓存在 Cookie 里）

---

## 十三、常见问题

**改了前端但页面没变？**
`go:embed` 在**编译期**打包资源，改完必须重新编译；并确认同步到了
`cmd/server/web/`。

**服务起不来？**
`logread | grep lan-share` 或看 `logs/lan-share.log`。数据分区晚挂载的情况
已在 procd 脚本里用 mount trigger + 等待逻辑处理。

**本地调试时页面显示乱套、弹窗自己去不掉？**
先确认 CSS 顶部那条 `[hidden] { display: none !important; }` 还在。
`hidden` 属性来自 UA 样式表，优先级低于作者样式 —— 只要组件自己写了
`display`（弹窗是 `display: grid`，会话栏/消息区是 `display: flex`），
`hidden` 就会静默失效，本该隐藏的元素会全部显示出来。

**开关（toggle）看着「糊成一团」？**
那是**对比度**问题，不是几何问题。轨道用 `--border-strong`（`rgb(214,217,224)`）配纯白滑块时，
两者亮度只差约 15%，滑块在视觉上会溶进轨道。现在轨道改成明确的 `#c3c8d2`，
滑块加了 `box-shadow` 描边，并补了 hover / active / 暗色模式。
> 判断这类问题时值得先量一下渲染结果（轨道实际 42×25、滑块 16px，与 CSS 自洽），
> 确认不是布局错位，再去调颜色 —— 否则会在错误的方向上改半天。

**服务起不来，报 `no such column: room_code`？**
这是**老库升级**的场景：`files` 表是旧版本建的，没有 `room_code` 列。
根因是 `idx_files_room` 这条 `CREATE INDEX` 原本写在 schema 大字符串里，
而那段 schema 在「补列」**之前**执行 —— 老库跑到这句就报
`no such column`，整个迁移中断。

已修复：索引挪到 `ALTER TABLE ... ADD COLUMN` 之后执行，并且**不放在 `if` 里**，
这样「列已存在但索引缺失」的半成品库也能被修好。

> 这个 bug 新库永远遇不到（`CREATE TABLE` 时就带了列），只在老库升级时炸。
> 所以回归测试 `TestRoomCodeColumnMigrates` 必须**真的造一张缺列的旧 `files` 表** ——
> 如果表名不叫 `files`，`CREATE TABLE IF NOT EXISTS` 会建出带列的新表，测试就永远抓不到。

**大文件上传失败？**
先分清是**哪个面板**：仓库看 `LANSHARE_MAX_UPLOAD_MB`，聊天室看
`LANSHARE_CHAT_UPLOAD_MB` —— 两者独立，改一个不会影响另一个（`0` 都表示不限）。
再确认 nginx 的 `client_max_body_size` 不小于实际要传的大小。

**上传进度到 100% 后还停一会儿？**
这是正常的：浏览器发完请求体之后，服务端还要 fsync + rename + 写库。
界面会显示「保存中…」。若这段停顿特别长，看日志里的 `speed=` ——
千兆局域网理论约 110MB/s，实测低一个数量级就该去查 Wi-Fi 或磁盘写入，
而不是先怀疑程序。

**上传被拒（507 磁盘空间不足）？**
程序会预先查一次剩余空间并预留 5%，避免传到一半才失败。
清理文件仓库后重试即可。

**想直接本地调试？**

```sh
LANSHARE_ROOT=./devdata LANSHARE_LISTEN=127.0.0.1:18080 ./dist/lan-share-<os>-<arch>
```

> **务必显式指定 `LANSHARE_ROOT`。** 默认值是 `/mnt/data_mmcblk0p27/lan-share`，
> 在 Windows 上会被解析成 `C:\mnt\data_mmcblk0p27\lan-share` —— 那里可能躺着
> 早期测试的旧数据库，会出现「状态跟预期不符」的怪象。

**怎么验证 WebSocket 没坏？**

服务起来后跑冒烟测试（零依赖，不需要装 npm 包）：

```sh
node scripts/ws-smoke.js 18080
```

它会自动建会话、连上去，断言：

- `hello.self` 存在且与 `members` 一致
- `cid` 原样回传、内容正确、回显 `sender` 等于自己的 `self`
- 不带 `cid` 的老客户端仍能正常广播
- 历史条目不携带 `cid`
- **自定义房间号**建得出来、重复号返回 `409`、过短/含非法字符返回 `400`
- **存活时长**只收白名单值 `{10, 60, 360, 1440}`、默认 `60`、`expiresAt` 与所选档位吻合；
  **`0` 会被拒绝（`400`）** —— 「不限时」档位已移除，房间必须有确定的终点
- **聊天文件**：上传返回 `200`、WS 卡片由服务端补齐名称/大小/URL、
  下载内容与上传**逐字节一致**、伪造 `fileId` 被拒、上传到不存在的房间返回 `404`
- **免登录入口不能被滥用**：仓库文件（`kind=permanent`）走 `/api/chat-files/{id}`
  返回 `404`，而同一文件走正规 `/api/files/{id}` 返回 `200` —— 对照成立才说明拦截有效
- **仓库的读开放 / 写收紧**：同一文件匿名下载 `200`、匿名删除 `401`

**怎么验证仓库权限没被改坏？**

```sh
node scripts/anon-access-smoke.js http://127.0.0.1:18080
```

专测「匿名可读、不可写」这条边界，含**反向验证**（断言 `401` 之后还会再查一次文件是否存在 ——
只断言状态码无法区分「拦截生效」和「文件本来就不在」）：

- 匿名列表 `/api/files` → `200`，能看到文件
- 匿名下载 `/api/files/{id}` → `200` 且**逐字节一致**
- 匿名上传 → `401`；匿名删除 → `401`，**且文件仍在**
- 他人删除 → `403`，**且文件仍在**；本人 / 管理员删除 → `200`，之后 `404`
- 模拟「把链接发出去」：全新连接、无任何凭据，直接下载成功

**怎么验证仓库内容去重？**

```sh
LS_ROOT=./devdata node scripts/dedup-smoke.js
```

需要先对同一个 `LANSHARE_ROOT` 起服务。它**按磁盘上的文件数**断言（不只查库）：

- 同内容传两次 → 磁盘仍 1 份、库里 2 条记录各自保留文件名、两条都能下
- 删一条 → 文件仍在（还有引用）；删第二条 → 文件才消失
- 跨用户不复用（甲、乙各占一份）；同用户再次上传才复用
- 甲删光自己的 → 只影响甲那份，乙那份保留

> **去重命中「坏记录」时保留新上传。** 库里有一条 `sha256` 匹配的记录，但它指向的
> 磁盘文件已经丢了（比如手工清理过 `files/`）—— 这时若直接把新传的文件删掉、
> 只留那条坏记录，用户就会看到「上传成功，下载 404」。所以去重前会先 `Exists()`
> 探一下目标文件是否真在盘上：不在就**留着自己刚存的那份**并记日志，
> 而不是盲目相信数据库里的旧指向。

可用 `node scripts/ws-smoke.js 18080 <房间号>` 复用已有房间。
当前共 **44 条断言**，全绿。

再跑一遍存活时长的语义测试：

```sh
node scripts/ttl-expiry-test.js 18080
```

覆盖：`expiresAt` 是否落在未来、`remainingSeconds` 是否随时间递减、
房间号大小写不敏感、白名单外的档位（含 `0`）一律 `400`。当前 **15 条断言**，全绿。

**「过期自动删除」怎么保证？**

后台回收依赖一分钟的 ticker，真等 10 分钟去验证不现实。
所以这部分由 Go 单元测试直接覆盖 —— 测试里把房间的到期时间**手动拨到过去**，
再跑一轮与线上完全相同的回收逻辑：

```sh
go test ./... -v
```

`internal/session/`（房间生命周期与回调）：

- `TestReapDeletesExpiredRoom`：过期房间被彻底删除，**房间号随之释放可再次占用**
- `TestZeroTTLRejectedByWhitelist`：`0` 不再是合法档位（曾表示「不限时」）
- `TestExpiredRoomReplacementNotifiesGone`：复用过期房间号时，旧房间的销毁回调**必定先跑完**
- `TestGoneFiredBeforeNewRoomVisible`：**旧房间清理完成前，新同名房间不可见** ——
  否则新一代房间会「继承」上一代遗留的聊天文件
- `TestRetiringBlocksConcurrentCreate`：正在退役的房间号不会被并发创建抢进去
- `TestGenerateCodeAvoidsRetiring`：随机房间号也要避开正在退役的号
- `TestReapOnceSkipsRetiring`：回收协程不会去动已经在退役流程里的房间（避免重复触发回调）
- `TestRoomGoneHandlerFires`：房间销毁时回调**恰好触发一次**，可重复回收不重复触发
- `TestNotifyGoneNilSafe`：没注册回调时回收不 panic
- `TestActiveCodes`：活跃房间号集合正确，回收后同步移除
- `TestValidateCode` / `TestValidTTL`：房间号与时长白名单的边界

`internal/storage/`（聊天文件与房间的绑定关系）：

- `TestChatFileRoomCodePersisted`：`room_code` 真的落库 ——
  这条不通，`FilesByRoom` 永远为空，文件就再也没人删了
- `TestFilesByRoom`：按房间查文件，不串房间，且不认领仓库文件
- `TestOrphanChatFiles`：孤儿判定正确，仓库文件永远不算孤儿
- `TestDeleteFileRecord`：删记录后查询报 `ErrNotFound`
- `TestRoomCodeColumnMigrates`：**手写一张缺 `room_code` 列的旧 `files` 表**，
  走真实迁移入口，断言列与索引都被补齐、迁移幂等、老数据不丢
- `TestRoomCodeIndexWithoutColumn`：列有了但索引缺的「半成品库」也能被补建
- `TestExpiresAtColumnRemoved`：**手写一张仍带 `expires_at` 列的旧 `files` 表**
  （并塞一条带过期时间的数据），断言该列被迁移移除、`idx_files_expires` 不留残骸、
  其余索引仍在、老数据一条不丢、迁移幂等
- `TestCountFilesByStoredName` / `TestCountFilesByStoredNameIgnoresKindAndOwner`：
  引用计数**只看 `stored_name`**，不受 `kind` / `owner_id` 影响 ——
  这正是「删甲的文件不该碰上乙的文件」的根据

`internal/websocket/`（分帧 / 分片状态机）：

- `TestSingleFrameText`：单帧文本照常工作
- `TestFragmentedText`（`hel` + `lo` → `hello`）：**分片真的被拼起来**
- `TestThreeFragmentText`：三片及以上
- `TestFragmentedBinary`：`binary` 消息同样支持分片
- `TestPingBetweenFragments`：分片中间插入 `ping`，回 `pong` 且**消息不受影响**
- `TestCloseBetweenFragments`：分片途中收到 `close`，按正常关闭处理
- `TestControlFrameWithFinZero` / `TestControlFrameTooLong` + 扩展长度：
  控制帧必须 `FIN=1` 且 payload ≤ 125，否则协议错误
- `TestOrphanContinuation` / `TestDataFrameWhileFragmented`：非法帧序列被拒
- `TestMissingMask` / `TestReservedBits` / `TestUnknownOpcode`：握手后的帧必须带掩码、RSV 必须为 0
- `TestCumulativeLimit` / `TestExactlyAtLimit` / `TestSingleFrameOverLimit`：
  上限对**拼接后的总长**生效，恰好等于上限放行

`internal/files/`（上传上限与去重探测）：

- `TestSaveHonoursPerCallLimit` / `TestSaveExactlyAtLimitAllowed`：
  上限由调用方传入，恰好等于上限放行
- `TestSaveTooLargeRemovesPart`：超限时半成品文件被清掉，不留垃圾
- `TestSaveWritesToCorrectKindDir`：按 `kind` 落到对应目录
- `TestExists` / `TestExistsRejectsUnsafeNames` / `TestExistsFalseForDirectory`：
  去重前探测目标磁盘文件是否真在
- `TestSaveComputesSHA256`：内容指纹计算正确

`internal/api/`（HTTP 层与端到端行为）：

- `TestAdminSettingsGetWithoutBody`：**`GET` 不带请求体返回 `200`**（原来会强解 JSON 报错）
- `TestAdminSettingsPutThenGet` / `TestAdminSettingsPutInvalidJSON`：`PUT` 才解析 body
- `TestAdminSettingsRequiresAdmin`：鉴权顺序正确
- `TestChatUploadLimitIndependentOfRepoLimit` / `TestChatUploadUnlimitedWhenZero`：
  两个上传上限**真正独立**，`0` 表示不限而非回落
- `TestStaleDedupTargetKeepsNewUpload`：去重命中失效记录时，**新上传被保留**
- `TestDedupStillWorksWhenTargetExists`：目标健在时去重照常生效
- `TestDeleteKeepsFileWhileOtherRecordReferencesIt`：还有引用就不删盘
- `TestDeleteUsesStoredNameNotSHA256`：删除计数口径是 `stored_name`，不是 `sha256`
- `TestNewMsgIDConcurrent`：16 个协程并发取 ID，断言**无一重复**（消息计数器并发安全）

**为什么左侧拖拽用「计数」而不是布尔值？**

`dragenter` / `dragleave` 会在**每个子元素**上冒泡触发。用一个布尔量标记
「正在拖拽」，鼠标从面板移到里面某个元素上时会先 `leave` 再 `enter`，
高亮就会疯狂闪烁。所以用 `dragDepth` 计数器，减到 `0` 才真正清除高亮。

**落点指示不要参与布局。** 遮罩用绝对定位的 `.drop-veil`（`position: absolute` 覆盖整个面板），
而不是往 flex 行里插真实元素 —— 后者会让光标附近的元素位移，触发来回抖动。
