# LAN Share 大文件上传优化方案

## 1. 文档目的

本文档用于优化 LAN Share 在 Kwrt / OpenWrt 路由器环境下的大文件上传能力。

当前系统在上传约 125MB 文件时曾出现：

- 上传过程中服务卡死
- `lan-share` 进程被 Linux OOM Killer 杀死
- 修复 OOM 后，上传进度达到 100% 时仍会停顿一段时间
- 大文件上传存在额外磁盘读写
- 多文件同时上传时容易造成路由器瞬时负载过高

本次优化的核心目标是将当前的：

```text
浏览器
  ↓
Go multipart 解析
  ↓
临时文件
  ↓
再次复制
  ↓
最终文件
```

优化为：

```text
浏览器
  ↓
Go 流式读取 multipart
  ↓
.part 文件
  ↓
校验 / rename
  ↓
最终文件
```

最终实现：

- 大文件上传不再明显占用大量 RAM
- 不依赖 `/tmp`
- 文件只写入磁盘一次
- 上传完成后的等待时间显著缩短
- 支持数百 MB、数 GB 甚至更大的文件
- 更适合 Kwrt 路由器这种内存有限的部署环境

------

# 2. 当前部署环境

LAN Share 当前部署于 Kwrt 路由器：

```text
系统：Kwrt / OpenWrt
架构：aarch64
内核：Linux 6.12.x
程序目录：
/mnt/data_mmcblk0p27/lan-share

数据磁盘：
/mnt/data_mmcblk0p27

文件系统：
ext4

HTTP 服务端口：
18080
```

服务通过 procd 管理：

```text
/etc/init.d/lan-share
```

程序启动方式：

```text
/mnt/data_mmcblk0p27/lan-share/lan-share
```

路由器本身同时还运行：

```text
PassWall
sing-box
dnsmasq
nginx
rpcd
hostapd
其他 Kwrt 系统服务
```

因此 LAN Share 不应该假设运行环境拥有大量可用内存。

设计时应优先：

```text
少占 RAM
少做文件拷贝
少产生临时文件
限制并发
避免阻塞系统服务
```

------

# 3. 已发现的问题

## 3.1 大文件上传触发 OOM

上传约 125MB 文件时出现：

```text
Out of memory: Killed process xxxx (lan-share)
```

说明 Linux 内存耗尽，并由 OOM Killer 终止了 LAN Share。

最初容易误认为：

```text
磁盘性能不足
```

实际上问题来自 multipart 上传处理方式。

------

# 4. 当前上传实现

当前文件接口中使用类似：

```go
r.ParseMultipartForm(8 << 20)
```

聊天室文件上传中还存在：

```go
r.ParseMultipartForm(4 << 20)
```

之后：

```go
file, header, err := r.FormFile("file")
```

最后进入文件保存层：

```go
io.Copy(io.MultiWriter(f, hasher), reader)
```

其中 `io.Copy()` 本身没有问题。

文件保存部分实际上已经采用了良好的流式处理：

```go
io.Copy(
    io.MultiWriter(f, hasher),
    reader,
)
```

这意味着：

```text
Reader
  ↓
小块读取
  ↓
磁盘
```

并不会一次将整个文件加载到 Go Heap。

真正的问题发生在进入 `Save()` 之前。

------

# 5. ParseMultipartForm 的问题

`ParseMultipartForm()` 会完整解析 multipart 请求。

当文件超过内存阈值后，Go 会将文件内容写入临时文件。

普通 Linux 服务器一般表现为：

```text
multipart
   ↓
/tmp/multipart-xxxx
   ↓
磁盘
```

问题是：

## Kwrt / OpenWrt 的 `/tmp` 是 tmpfs

即：

```text
/tmp
 ↓
tmpfs
 ↓
RAM
```

因此原先实际上发生的是：

```text
125MB 文件
   ↓
HTTP
   ↓
ParseMultipartForm
   ↓
/tmp/multipart-xxx
   ↓
RAM
   ↓
OOM
```

这也是为什么 LAN Share 本身的 RSS 未必显示为 125MB，但系统依然会出现 OOM。

因为：

```text
tmpfs 占用的是系统内存
```

并不完全表现为：

```text
lan-share anonymous RSS
```

------

# 6. 临时修复方案

目前已经通过：

```text
TMPDIR=/mnt/data_mmcblk0p27/lan-share/tmp
```

将 Go multipart 临时文件从：

```text
/tmp
```

迁移到：

```text
/mnt/data_mmcblk0p27/lan-share/tmp
```

从而使临时文件真正写入 ext4 数据分区，而不是 RAM。

推荐服务配置：

```sh
mkdir -p "$PROG_DIR/tmp"

procd_set_param env \
    LANSHARE_ROOT="$PROG_DIR" \
    LANSHARE_LISTEN="$LISTEN_ADDR" \
    TMPDIR="$PROG_DIR/tmp"
```

这已经解决：

```text
上传 125MB → OOM
```

的问题。

但它只是一个兼容性修复，并不是最终架构。

------

# 7. 当前方案仍然存在的问题

## 7.1 文件被写入两次

现在上传一个 125MB 文件时：

```text
浏览器
  ↓
Go
  ↓
tmp/multipart-xxx
```

首先写一次：

```text
125MB
```

然后：

```text
tmp/multipart-xxx
  ↓
io.Copy()
  ↓
files/permanent/xxx.part
```

再写一次：

```text
125MB
```

也就是说：

```text
125MB 上传

磁盘写入约：
250MB

磁盘读取约：
125MB
```

对于 1GB 文件则大致变为：

```text
磁盘写入：
2GB

额外读取：
1GB
```

这种设计在普通服务器上问题不算严重。

但在路由器 eMMC / SD / Flash 存储环境中：

```text
不必要
```

同时还增加：

- IO 等待
- Flash 写入量
- CPU 使用
- 上传完成后的延迟

------

# 8. 为什么进度达到 100% 后还会卡一段时间

当前前端上传进度一般来自：

```javascript
xhr.upload.addEventListener("progress", ...)
```

这里统计的是：

```text
浏览器发送 HTTP Request Body 的进度
```

因此：

```text
100%
```

实际只表示：

```text
浏览器已经把文件发送给服务器
```

并不代表：

```text
服务器已经保存完成
```

当前完整流程是：

```text
浏览器上传
   ↓
XHR 100%
   ↓
ParseMultipartForm 完成
   ↓
FormFile
   ↓
读取临时文件
   ↓
写正式文件
   ↓
SHA256
   ↓
f.Sync()
   ↓
数据库操作
   ↓
HTTP 200
```

因此用户会看到：

```text
98%
99%
100%

停顿……

上传完成
```

这个停顿并不是网络卡住。

而是服务器仍然在执行第二阶段文件处理。

------

# 9. 最终推荐架构

应移除大文件上传接口中的：

```go
ParseMultipartForm()
```

改用：

```go
MultipartReader()
```

进行真正的流式 multipart 解析。

目标架构：

```text
                     ┌─────────────┐
                     │   Browser   │
                     └──────┬──────┘
                            │
                            │ HTTP multipart
                            ▼
                  ┌────────────────────┐
                  │   MultipartReader  │
                  └─────────┬──────────┘
                            │
                            │ io.Reader
                            ▼
                 ┌──────────────────────┐
                 │ xxx.part             │
                 │                      │
                 │ io.Copy              │
                 │ + SHA256             │
                 └──────────┬───────────┘
                            │
                            │ success
                            ▼
                       os.Rename()
                            │
                            ▼
                    permanent file
```

整个过程中：

```text
不产生完整 multipart 临时文件
```

并且：

```text
文件大小 ≠ 内存占用
```

理论上：

```text
100MB 文件
1GB 文件
10GB 文件
```

内存占用都可以保持在较低水平。

------

# 10. 后端优化方案

## 10.1 使用 MultipartReader

原：

```go
if err := r.ParseMultipartForm(8 << 20); err != nil {
    ...
}

file, header, err := r.FormFile("file")
```

优化为：

```go
mr, err := r.MultipartReader()
if err != nil {
    http.Error(w, "invalid multipart request", http.StatusBadRequest)
    return
}
```

然后：

```go
for {
    part, err := mr.NextPart()

    if errors.Is(err, io.EOF) {
        break
    }

    if err != nil {
        return
    }

    switch part.FormName() {

    case "file":
        // 直接流式处理文件

    case "kind":
        // 读取少量文本字段
    }
}
```

文件 Part 本身就是：

```go
io.Reader
```

因此可以直接传给文件存储层。

------

# 11. Save 层建议

当前：

```go
io.Copy(io.MultiWriter(f, hasher), reader)
```

应该保留。

这部分设计是合理的。

推荐整体逻辑：

```go
func Save(reader io.Reader) error {

    tmpPath := finalPath + ".part"

    f, err := os.Create(tmpPath)
    if err != nil {
        return err
    }

    hasher := sha256.New()

    _, err = io.Copy(
        io.MultiWriter(f, hasher),
        reader,
    )

    if err != nil {
        f.Close()
        os.Remove(tmpPath)
        return err
    }

    if err := f.Sync(); err != nil {
        ...
    }

    if err := f.Close(); err != nil {
        ...
    }

    return os.Rename(tmpPath, finalPath)
}
```

文件流程：

```text
network
   ↓
xxx.part
   ↓
fsync
   ↓
xxx
```

------

# 12. 为什么需要 `.part`

不建议直接写：

```text
xxx.zip
```

应该先写：

```text
xxx.zip.part
```

这样即使：

```text
网络断开
浏览器取消
程序异常
路由器重启
磁盘写入失败
```

也不会留下一个看起来正常但实际上不完整的文件。

上传成功之后：

```go
os.Rename(partPath, finalPath)
```

改名操作在同一文件系统下一般非常快。

最终目录中：

```text
正常文件
```

和：

```text
未完成文件
```

可以明显区分。

------

# 13. SHA256 优化

当前边保存边：

```go
SHA256
```

是合理设计：

```go
io.Copy(
    io.MultiWriter(file, sha256Hasher),
    reader,
)
```

相比：

```text
保存完成
 ↓
重新读取整个文件
 ↓
计算 SHA256
```

当前方式只读取数据一次。

推荐保留。

------

# 14. f.Sync() 是否保留

当前存在：

```go
f.Sync()
```

推荐保留。

它能够降低：

```text
HTTP 已返回成功

但数据实际上仍停留在 page cache
```

导致异常断电时文件丢失的风险。

代价是：

```text
上传末尾可能有短暂等待
```

但在重构为单次写入之后：

```text
f.Sync()
```

造成的停顿应该比现在明显更短。

如果以后更追求性能，可以将：

```text
durability
```

设计成配置项。

例如：

```yaml
storage:
  fsync: true
```

默认：

```text
true
```

------

# 15. 上传大小限制

即使采用流式上传，也必须限制最大请求大小。

否则用户可以提交：

```text
100GB
```

请求持续占用：

```text
磁盘
连接
文件句柄
CPU
```

推荐：

```go
r.Body = http.MaxBytesReader(
    w,
    r.Body,
    maxUploadSize,
)
```

例如：

```text
默认最大文件：
5GB
```

或者提供配置：

```text
LANSHARE_MAX_UPLOAD_SIZE
```

例如：

```text
5368709120
```

表示：

```text
5GB
```

不要使用：

```text
ParseMultipartForm 的 maxMemory
```

作为上传文件大小限制。

这是两个不同概念。

------

# 16. 前端进度优化

当前：

```text
0%
↓
100%
↓
等待
↓
完成
```

容易让用户认为程序卡死。

建议至少设计两个状态。

## 上传阶段

```text
正在上传 73%
```

当：

```javascript
xhr.upload.progress
```

达到 100% 后：

```text
正在保存…
```

最终收到 HTTP 成功响应：

```text
上传完成
```

推荐状态：

```text
等待
↓
上传中
↓
保存中
↓
完成
```

而不是：

```text
上传中
↓
100%
↓
卡住
↓
完成
```

------

# 17. 服务端真实处理进度

如果以后希望更加精确，可以增加：

```text
上传状态 API
```

或者：

```text
SSE
WebSocket
```

例如：

```json
{
  "stage": "writing",
  "received": 89456640,
  "total": 131072000
}
```

不过当前版本没有必要立即实现。

因为：

```text
浏览器 → 服务端
```

和：

```text
服务端 → 文件
```

经过流式重构后基本同步进行。

因此普通：

```text
XHR upload progress
```

已经足够接近真实进度。

------

# 18. 多文件上传并发控制

当前前端如果使用：

```javascript
files.forEach(file => uploadOne(file))
```

则意味着：

```text
选择 10 个文件
↓
同时创建 10 个 HTTP 上传请求
```

对于 PC 服务端问题不大。

但 LAN Share 部署环境是路由器。

可能同时发生：

```text
10 个网络连接
10 个文件写入
10 个 SHA256
10 个 fsync
```

会显著增加：

```text
CPU
磁盘 IO
文件句柄
内存
系统负载
```

------

# 19. 推荐上传并发数

推荐：

```text
默认并发：
2
```

对于路由器而言：

```text
1～2
```

已经足够。

推荐实现简单的任务队列：

```text
files[]

       ┌── upload 1
queue ─┤
       └── upload 2

剩余文件等待
```

完成一个：

```text
立即开始下一个
```

------

# 20. 服务端并发保护

仅仅限制前端不够。

用户可能：

```text
自己调用 API
多个浏览器
多个客户端
```

因此后端也建议增加上传 Semaphore。

例如：

```go
uploadSem := make(chan struct{}, 2)
```

上传开始：

```go
uploadSem <- struct{}{}
```

完成：

```go
defer func() {
    <-uploadSem
}()
```

超过并发数可以：

```text
等待
```

或者返回：

```text
429 Too Many Requests
```

推荐 LAN Share：

```text
等待
```

用户体验更好。

------

# 21. 内存目标

重构之后，应达到：

```text
上传 10MB 文件
上传 125MB 文件
上传 1GB 文件
```

Go 进程内存变化不应与：

```text
文件大小
```

线性增长。

理想状态例如：

```text
基础 RSS：
30～50MB

上传 125MB：
40～70MB

上传 1GB：
40～70MB
```

这里只表示目标趋势。

具体占用取决于：

```text
Go runtime
数据库
连接数
缓存
系统环境
```

核心验收条件：

```text
1GB 文件不能导致进程占用增加接近 1GB
```

------

# 22. TMPDIR 后续处理

完成 MultipartReader 重构后：

```text
普通文件上传
```

理论上已经不需要依赖：

```text
TMPDIR
```

但建议仍然保留：

```text
TMPDIR=/mnt/data_mmcblk0p27/lan-share/tmp
```

作为安全措施。

原因是未来：

```text
其他依赖
其他 Go 标准库
未来功能
```

仍可能使用：

```text
os.CreateTemp()
```

避免任何未来临时文件意外进入：

```text
/tmp tmpfs
```

对路由器应用来说是好习惯。

------

# 23. 临时目录清理

程序启动时建议清理：

```text
tmp
```

中的历史文件。

但不要粗暴：

```go
os.RemoveAll(tmpDir)
```

然后重新建立目录。

更加安全的是：

```text
只清理 LAN Share 自己创建的文件
```

例如：

```text
multipart-*
*.part
upload-*
```

并且只清理：

```text
超过一定时间
```

例如：

```text
24 小时
```

避免：

```text
正在上传时
```

误删当前文件。

------

# 24. `.part` 文件清理

程序异常退出时可能残留：

```text
xxx.part
```

建议程序启动时扫描：

```text
files/permanent
files/chat
```

删除：

```text
超过 24 小时
```

仍未完成的：

```text
*.part
```

或者记录：

```text
upload session
```

以后进行更精确的恢复。

当前版本无需实现断点续传。

------

# 25. 错误处理中必须保证清理

以下情况：

```text
网络中断
io.Copy 失败
磁盘满
文件超过限制
客户端取消
SHA256 失败
数据库失败
```

应保证：

```text
关闭文件
删除 .part
释放 semaphore
关闭 multipart part
```

推荐大量使用：

```go
defer
```

处理资源释放。

------

# 26. 磁盘空间检查

开始上传之前推荐检查目标分区剩余空间。

例如：

```text
剩余空间：
400MB

上传：
1GB
```

应尽早拒绝。

否则可能：

```text
上传到 40%
↓
磁盘满
↓
失败
```

体验较差。

可以使用：

```text
statfs
```

检查：

```text
/mnt/data_mmcblk0p27
```

可用空间。

建议额外保留：

```text
5%～10%
```

安全空间。

------

# 27. 下载路径同样保持流式

下载文件不要：

```go
os.ReadFile()
```

正确方式：

```go
http.ServeFile()
```

或者：

```go
io.Copy(w, file)
```

这样：

```text
1GB 下载
```

也不会导致：

```text
1GB RAM
```

占用。

如果当前下载已经使用：

```text
http.ServeContent
http.ServeFile
io.Copy
```

则无需修改。

------

# 28. 聊天室文件上传

目前系统存在：

```text
文件仓库上传
```

以及：

```text
聊天室临时文件上传
```

两条上传链路。

两个接口都需要统一采用：

```text
MultipartReader
```

不要只修改：

```text
/api/files
```

否则聊天室发送大文件仍可能：

```text
tmp 临时文件
二次拷贝
额外 IO
```

推荐抽象统一函数：

```go
func readMultipartUpload(
    r *http.Request,
    field string,
) (...)
```

避免两个接口分别维护不同逻辑。

------

# 29. 推荐抽象

建议将 HTTP multipart 解析与文件存储拆分。

例如：

```text
api/
   upload.go

files/
   store.go
```

API 层负责：

```text
HTTP
multipart
参数校验
权限
大小限制
```

Store 层负责：

```text
写文件
SHA256
.part
rename
元数据
```

不要让：

```text
files.Save()
```

依赖：

```text
*multipart.FileHeader
```

更加理想的是依赖：

```go
io.Reader
```

例如：

```go
type SaveRequest struct {
    Filename string
    Size     int64
    Reader   io.Reader
}
```

这样 Storage 层与：

```text
HTTP multipart
```

完全解耦。

未来甚至可以支持：

```text
WebDAV
CLI
局域网客户端
拖拽上传
API
```

而无需修改文件存储核心。

------

# 30. 推荐数据流

最终建议：

```text
Browser
   │
   │ multipart/form-data
   ▼
HTTP Handler
   │
   ├── MaxBytesReader
   │
   ├── MultipartReader
   │
   ├── auth
   │
   └── metadata
   ▼
File Store
   │
   ├── sanitize filename
   ├── create .part
   ├── io.Copy
   ├── SHA256
   ├── fsync
   ├── close
   ├── rename
   └── DB
   ▼
HTTP 200
```

------

# 31. 安全性

上传文件名必须继续执行：

```text
sanitize
```

禁止用户提供：

```text
../../etc/passwd
```

或者：

```text
/foo/bar
```

影响目标目录。

必须只使用：

```text
文件 basename
```

并对：

```text
非法字符
长度
空文件名
重复文件
```

进行处理。

------

# 32. 上传超时

大文件上传可能持续较长时间。

不要设置过小：

```text
WriteTimeout
ReadTimeout
```

例如：

```text
30 秒
```

否则：

```text
1GB 文件
```

可能被 HTTP Server 主动关闭。

推荐：

```text
ReadHeaderTimeout
```

可以较短。

但 Body 上传超时：

```text
应足够宽松
```

或者根据实际情况设计：

```text
Idle timeout
```

而不是限制整个文件必须多少秒传完。

------

# 33. 服务重启行为

当前由 procd：

```text
respawn
```

管理服务时，如果发生：

```text
panic
OOM
异常退出
```

可能自动拉起 LAN Share。

但不能将：

```text
自动重启
```

视为上传容错。

上传中的：

```text
.part
```

仍然需要正确处理。

------

# 34. 日志建议

建议对文件上传增加结构化日志。

例如：

```text
upload started
filename=test.zip
size=125829120
user=xxx
```

完成：

```text
upload completed
filename=test.zip
size=125829120
duration=4.2s
sha256=...
```

失败：

```text
upload failed
filename=test.zip
received=83886080
error=no space left on device
```

这样以后排查：

```text
网络问题
磁盘问题
OOM
用户取消
```

会非常方便。

但不要：

```text
每读取几十 KB 打一条日志
```

否则会产生大量无意义日志。

------

# 35. 可增加上传性能指标

后续可以增加：

```text
平均上传速度
```

例如：

```text
125 MB
3.4 秒

36.7 MB/s
```

方便判断瓶颈到底在：

```text
Wi-Fi
校园网
交换机
CPU
磁盘
```

而不是凭感觉判断：

```text
“是不是硬盘不行”
```

------

# 36. 本轮优化不建议立即实现的功能

以下功能目前不是必须：

```text
分片上传
断点续传
秒传
多线程上传
对象存储
WebRTC 文件传输
P2P
```

LAN Share 当前定位是：

```text
局域网轻量文件共享
```

HTTP 单连接流式上传已经足够。

一个正确实现的：

```text
io.Reader → io.Copy
```

完全可以处理：

```text
数 GB 文件
```

无需为了“大文件”立即引入复杂分片协议。

------

# 37. 后续什么时候需要分片上传

只有未来出现：

```text
10GB+ 超大文件
网络质量差
需要断点续传
浏览器经常切后台
需要失败后继续
```

再考虑：

```text
chunk upload
```

例如：

```text
POST /uploads
POST /uploads/:id/chunks/:n
POST /uploads/:id/complete
```

当前没有必要。

------

# 38. 优化优先级

## P0：必须完成

### 1. 保留 TMPDIR 修复

```text
TMPDIR=/mnt/data_mmcblk0p27/lan-share/tmp
```

防止再次使用：

```text
/tmp tmpfs
```

------

### 2. `ParseMultipartForm` 改为 `MultipartReader`

覆盖：

```text
文件仓库上传
聊天室文件上传
```

彻底消除 multipart 完整临时文件。

------

### 3. 直接流式写 `.part`

数据路径：

```text
HTTP
 ↓
io.Copy
 ↓
.part
```

------

### 4. 上传失败清理 `.part`

所有失败路径必须清理。

------

# 39. P1：强烈建议

### 5. 前端增加“正在保存”状态

```text
100%
↓
正在保存…
↓
完成
```

------

### 6. 前端限制上传并发为 2

避免一次性大量请求冲击路由器。

------

### 7. 后端增加 Semaphore

防止绕过前端限制。

------

### 8. 增加 MaxBytesReader

明确上传最大值。

------

# 40. P2：后续优化

包括：

```text
磁盘空间预检查
上传耗时日志
上传速度统计
.part 定期清理
临时目录清理
上传状态 API
断点续传
```

------

# 41. 重构后的预期效果

原：

```text
125MB

HTTP
 ↓
125MB tmp
 ↓
读取 125MB
 ↓
写 125MB 正式文件
 ↓
fsync
```

优化后：

```text
125MB

HTTP
 ↓
125MB .part
 ↓
fsync
 ↓
rename
```

减少：

```text
一次完整文件写入
一次完整文件读取
multipart 临时文件
```

------

# 42. 预期改善

## 内存

原：

```text
可能受到 tmpfs 影响
125MB 上传可能 OOM
```

优化后：

```text
内存基本与文件大小无关
```

------

## 磁盘 IO

原：

```text
上传 N GB
≈ 2N GB 写入
+ N GB 读取
```

优化后：

```text
上传 N GB
≈ N GB 写入
```

------

## 上传结束延迟

原：

```text
100%
↓
再次复制
↓
SHA256
↓
fsync
↓
完成
```

优化后：

```text
上传过程中已经同时：
写磁盘
+
SHA256

100%
↓
fsync
↓
rename
↓
完成
```

停顿明显缩短。

------

# 43. 验收测试

重构完成后至少测试：

## Test 1

```text
1MB 文件
```

验证普通上传。

------

## Test 2

```text
125MB 文件
```

验证之前触发 OOM 的场景。

要求：

```text
无 OOM
服务不重启
文件 SHA256 正确
```

------

## Test 3

```text
1GB 文件
```

观察：

```bash
free -h
```

Go 进程内存不能随文件大小线性增长。

------

## Test 4

上传过程中执行：

```bash
du -sh /mnt/data_mmcblk0p27/lan-share/tmp
```

理想状态：

```text
不再出现完整的大型 multipart 临时文件
```

------

## Test 5

上传过程中主动取消：

```text
浏览器取消请求
```

检查：

```text
.part 是否正确删除
```

------

## Test 6

同时选择：

```text
10 个大文件
```

确认：

```text
同时最多只有 2 个上传
```

其他进入：

```text
等待队列
```

------

## Test 7

磁盘空间不足模拟。

必须：

```text
返回明确错误
删除 .part
服务继续运行
```

------

# 44. 运行时监控

测试时推荐：

```bash
watch -n 1 '
free -h
echo
ps -eo pid,comm,rss,vsz --sort=-rss | head
echo
df -h /mnt/data_mmcblk0p27
'
```

另外：

```bash
dmesg | tail -100
```

确认没有：

```text
Out of memory
Killed process
I/O error
ext4 error
```

------

# 45. 回滚方案

重构 multipart 上传接口时应保持：

```text
API 路径
HTTP 参数
返回 JSON
数据库结构
前端调用方式
```

尽量不变。

这样如果新上传逻辑出现问题：

```text
只需要回滚后端二进制
```

无需：

```text
迁移数据库
修改已有文件
修改前端协议
```

TMPDIR 修复应继续保留。

------

# 46. 推荐实施顺序

建议按照：

```text
第一阶段

保留 TMPDIR
↓
实现 MultipartReader
↓
直接写 .part
↓
SHA256
↓
rename
```

然后：

```text
第二阶段

前端状态优化
↓
前端并发队列
↓
后端 semaphore
```

最后：

```text
第三阶段

磁盘空间检测
日志
性能统计
残留文件清理
```

不要一次引入：

```text
分片
断点续传
新数据库结构
新上传协议
```

避免将本次单纯的上传性能重构扩大成复杂架构重写。

------

# 47. 最终目标

LAN Share 的文件上传设计原则应确定为：

> 文件大小不应该决定服务器内存占用。

在 Kwrt 路由器环境下，正确的数据路径应该始终尽可能接近：

```text
Network
   ↓
small buffer
   ↓
disk
```

而不是：

```text
Network
   ↓
RAM / tmpfs
   ↓
temporary file
   ↓
second copy
   ↓
disk
```

最终推荐结构：

```text
HTTP MultipartReader
        ↓
     io.Reader
        ↓
io.MultiWriter
   ↙       ↘
file       SHA256
 ↓
.part
 ↓
fsync
 ↓
rename
 ↓
complete
```

这套方案能够同时解决当前已经暴露出的：

```text
OOM
/tmp tmpfs
重复磁盘 IO
上传 100% 后长时间等待
多文件并发负载过高
```

也是目前最适合 LAN Share + Kwrt 部署环境的上传实现方式。