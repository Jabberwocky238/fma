# fma

[English](README.md) | [简体中文](README.ZH-CN.md)

**fma 是一个轻量邮件服务，唯一需要的外部服务依赖是 S3。** 支持 SMTP、POP3、IMAP 和 JMAP，面向多节点高可用、并发连接与低占用设计。

快速安装并启动（先准备已有桶、账户及 TLS 证书；下方填入真实凭证）：

```sh
curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh | bash
FMA_S3_BUCKET=fma FMA_S3_ACCESS_KEY_ID=your-key FMA_S3_SECRET_ACCESS_KEY=your-secret fma
```

**目录**

1. [介绍](#fma)
2. [哲学和设计](#design)
3. [如何启动](#run)
   - [3.1. 直接启动](#run-direct)
   - [3.2. systemd 启动与 gen.sh](#run-systemd)
   - [3.3. Docker 启动](#run-docker)
   - [3.4. Kubernetes 启动](#run-kubernetes)
4. [JMAP 使用](#jmap)
5. [CI、测试与 Fals3y](#testing)
6. [桶布局](#bucket)
7. [许可证与鸣谢](#credits)

<a id="design"></a>

## 2. 哲学和设计

运行时代码全部放在 `main.go`，Go 测试全部放在 `main_test.go`。复用协议库，用明确的数据类型和 JSON 标签连接协议与 S3；脚本、部署模板和文档可独立存放。欢迎任何部分的 PR、修改、补充和 AI 辅助编程，唯一不能破坏的架构约束是单文件哲学，详见 [CONTRIBUTING.md](CONTRIBUTING.md) 和 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。

- **高可用**：多个节点共享一个桶，节点宕机后，其他节点可以接管持久化的投递任务。
- **高并发设计**：并行处理协议连接，通过 S3 条件写协调跨节点任务归属和邮箱更新。
- **占用小**：单个 Go 二进制，无需本地数据库、Redis、邮件暂存目录或 Docker。实际内存和吞吐取决于邮件大小、连接数与 S3 延迟，目前没有公布生产环境基准数据。

可用性依赖 S3，以及将客户端流量导向健康节点的接入设施。节点故障后，原有连接需要重连。
账户、邮件、文件夹、任务队列和租约全部存放在 S3。证书默认从 S3 读取，Kubernetes 部署则直接只读挂载 TLS Secret。邮件二进制不提供注册、用户管理、CSV 导入、本地数据库、磁盘缓存或临时文件管理能力。HTTP 提供 JMAP 和存活检查，DEBUG/INFO/WARN 日志输出到 stdout，ERROR/FATAL 输出到 stderr。

JMAP 与其他协议共用邮件记录和 MIME 对象。邮件相关数据保存在账户的 `mail/` prefix；账户文件只保存文件夹、身份、UID/状态计数、租约与当前事务的提交信息。属性、成员关系、blob 引用和排序索引仅在内存中维护，冷启动从权威记录重建。更新只写变更记录和一个条件提交文件，不再重写整个邮箱；同账户写入仍通过 S3 条件写协调。

### 支持的 RFC 与范围

| RFC | 协议 | 当前范围 |
| --- | --- | --- |
| [RFC 5321](https://www.rfc-editor.org/rfc/rfc5321.html) | SMTP | 入站投递与对外 SMTP 传输 |
| [RFC 6409](https://www.rfc-editor.org/rfc/rfc6409.html) | SMTP Submission | 程序和邮件客户端经认证提交邮件；公开端口 587 |
| [RFC 4954](https://www.rfc-editor.org/rfc/rfc4954.html) | SMTP AUTH | 在 TLS 保护的提交连接上认证 |
| [RFC 3207](https://www.rfc-editor.org/rfc/rfc3207.html) | SMTP STARTTLS | 将 SMTP 连接升级为 TLS |
| [RFC 1939](https://www.rfc-editor.org/rfc/rfc1939.html) | POP3 | 下载、UIDL、DELE 标记及 QUIT 提交删除 |
| [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501.html) | IMAP4rev1 | 邮箱读写、搜索、标记、文件夹与订阅 |
| [RFC 2595](https://www.rfc-editor.org/rfc/rfc2595.html) | IMAP/POP3 TLS | IMAP STARTTLS 与 POP3 STLS |
| [RFC 4616](https://www.rfc-editor.org/rfc/rfc4616.html) | SASL PLAIN | 在 TLS 内使用密码认证 |
| [RFC 2045](https://www.rfc-editor.org/rfc/rfc2045.html) | MIME | 保留原始 MIME、编码正文及附件 |
| [RFC 2046](https://www.rfc-editor.org/rfc/rfc2046.html) | MIME media types | 多部分邮件及附件 |
| [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620.html) | JMAP Core | 会话、HTTP/JSON 方法、结果引用、blob 和状态同步；尚未实现推送订阅 |
| [RFC 8621](https://www.rfc-editor.org/rfc/rfc8621.html) | JMAP Mail | Mailbox、Email、Thread、SearchSnippet、Identity、EmailSubmission；读写、搜索、附件与变更同步 |

### 任务归属与故障恢复

多个节点可以共享同一个桶。`<domain>/.lock` 只控制外发任务扫描与领取，不阻塞协议流量或已领取任务。租约记录持有者、启动时间、续期时间和过期时间，每 15 秒续期，30 秒过期。释放时使用条件写标记过期，防止删掉后继节点的锁。节点时钟需要同步。

锁持有者立即扫描 `<domain>/.outbox/`，之后每 15 秒补扫一次。执行前使用条件 PUT 写入 preclaim。每节点最多持有 1024 个活动任务；一批领取 1024 个或达到容量时，立即释放扫描锁，下一轮扫描时再竞争。已经领取的任务继续执行。

Preclaim 记录持有者、开始时间和固定 20 秒超时，不续期。到期取消执行，任务留在 S3，供下一轮重新领取；扫描节点宕机还需等待扫描锁过期。旧执行者无法通过过期 ETag 覆盖新执行者。

临时 SMTP 错误会保存每个收件人的重试状态，从 `-queue-retry` 开始指数退避。已确认成功的收件人不会重复重试；永久失败在本地退信存储完成后结束。

Preclaim 内嵌于任务对象，删除任务会同时删除 preclaim。完成时先通过条件写保存不可再次领取的终态，再删除对象；如果删除失败或期间宕机，后续扫描只重试删除，不再次发送。`-queue` 查看未清理任务，不保留已完成任务历史。

S3 和远端 SMTP 之间没有共同事务：远端已经接受邮件，但节点超时或未能保存确认时，重新投递可能产生重复邮件。因此投递语义是至少一次，不是恰好一次。邮箱 UID 分配和目录更新通过 S3 条件写避免节点之间互相覆盖。

<a id="run"></a>

## 3. 如何启动

### 参数介绍

命令行优先于表中对应的环境变量，再使用默认值。`—` 表示二进制不读取对应环境变量或没有命令行开关；生成器变量另列，不要混用。日志级别使用无前缀的 `LOG_LEVEL`。

| 参数 | 二进制读取的环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `-s3-endpoint` | `FMA_S3_ENDPOINT` | `empty / 空` | S3 端点；为空时使用 AWS |
| `-s3-bucket` | `FMA_S3_BUCKET` | `required / 必填` | 已存在的桶 |
| `-s3-region` | `FMA_S3_REGION` | `us-east-1` | S3 region |
| `—` | `FMA_S3_ACCESS_KEY_ID` | `required / 必填` | S3 访问密钥 ID |
| `—` | `FMA_S3_SECRET_ACCESS_KEY` | `required / 必填` | S3 访问密钥 |
| `—` | `FMA_S3_SESSION_TOKEN` | `empty / 空` | 可选临时凭证 token |
| `-cert` | `—` | `cert.pem` | S3 内的证书链对象键 |
| `-key` | `—` | `key.pem` | S3 内的私钥对象键 |
| `-tls-dir` | `—` | `empty / 空` | 从挂载目录读取 tls.crt 和 tls.key，替代 S3 证书 |
| `-smtp` | `—` | `127.0.0.1:2525` | 入站 SMTP |
| `-submission` | `—` | `127.0.0.1:1587` | 需要认证的 SMTP STARTTLS |
| `-smtps` | `—` | `127.0.0.1:1465` | 需要认证的 SMTP TLS |
| `-pop3` | `—` | `127.0.0.1:1110` | POP3 STLS |
| `-pop3s` | `—` | `127.0.0.1:1995` | POP3 TLS |
| `-imap` | `—` | `127.0.0.1:1143` | IMAP STARTTLS |
| `-imaps` | `—` | `127.0.0.1:1993` | IMAP TLS |
| `-http` | `—` | `127.0.0.1:8080` | JMAP HTTP 后端及 / 活性检查；公开访问须经 HTTPS |
| `-jmap-url` | `FMA_JMAP_URL` | `https://mail.<domain>` | 向 JMAP 客户端公布的 HTTPS origin |
| `-outbound` | `FMA_OUTBOUND_MODE` | `disabled` | disabled、direct（MX 直投）或 relay（另一台 SMTP 服务器代投） |
| `—` | `FMA_RELAY_ADDR` | `required in relay / relay 必填` | 中继 host:port |
| `—` | `FMA_RELAY_USER` | `required in relay / relay 必填` | 中继认证用户名 |
| `—` | `FMA_RELAY_PASSWORD` | `empty / 空` | 中继密码；与 PASSWORD_FILE 至少填写一个 |
| `—` | `FMA_RELAY_PASSWORD_FILE` | `empty / 空` | 保存中继密码的 S3 对象键 |
| `—` | `FMA_RELAY_TLS` | `starttls` | starttls 或 implicit |
| `—` | `FMA_RELAY_CA_FILE` | `empty / 空` | 可选自定义 CA 的 S3 对象键 |
| `-queue-retry` | `—` | `1m` | SMTP 投递任务初始重试间隔 |
| `-stream-workers` | `FMA_STREAM_WORKERS` | `4` | MIME 解析任务池大小（1–128），参数优先于环境变量 |
| `-queue` | `—` | `false` | 检查 SMTP 投递任务，不显示邮件正文 |
| `-version` | `—` | `false` | 打印版本、commit 和发行时间；无需 S3 |
| `-h` | `—` | `—` | 显示命令行帮助 |
| `—` | `LOG_LEVEL` | `info` | debug、info、warn、error；在邮件配置之前读取 |

生成器还读取下表。上表中的 S3、relay、外发和日志环境变量同样可用于生成器；生成器的桶默认 `fma`，其余共享默认值与上表一致。

| 生成器环境变量 | 默认值 | 用途 |
| --- | --- | --- |
| `FMA_MAIL_HOST` | `required / 必填` | 生成 Nginx 和证书路径所用的公共服务主机名，不是托管域列表 |
| `FMA_JMAP_URL` | `https://<mail-host>` | Public JMAP origin / JMAP 公开地址 |
| `FMA_DEPLOY_USER` | `current user / 当前用户` | Service user / 服务用户 |
| `FMA_DEPLOY_UID` | `selected user UID / 所选用户 UID` | Service UID / 服务 UID |
| `FMA_DEPLOY_HOME` | `selected user home / 所选用户主目录` | Service home / 服务主目录 |
| `FMA_BINDIR` | `root: /usr/local/bin; user: ~/.local/bin` | Binary directory / 二进制目录 |
| `FMA_CONFIG_DIR` | `root: /etc/fma; user: ~/.config/fma` | Configuration directory / 配置目录 |
| `FMA_SYSTEMD_USER_DIR` | `root: /etc/systemd/system; user: ~/.config/systemd/user` | Unit directory for the selected mode / 当前模式的单元目录 |
| `FMA_CERT_KEY` | `cert.pem` | Maps to -cert / 对应 -cert |
| `FMA_KEY_KEY` | `key.pem` | Maps to -key / 对应 -key |
| `FMA_QUEUE_RETRY` | `1m` | Maps to -queue-retry / 对应 -queue-retry |
| `FMA_STREAM_WORKERS` | `4` | 启动时创建的 MIME 解析 worker 数量 |
| `FMA_SMTP_PORT` | `2525` | Loopback listener port / 回环监听端口 |
| `FMA_SUBMISSION_PORT` | `1587` | Loopback listener port / 回环监听端口 |
| `FMA_SMTPS_PORT` | `1465` | Loopback listener port / 回环监听端口 |
| `FMA_POP3_PORT` | `1110` | Loopback listener port / 回环监听端口 |
| `FMA_POP3S_PORT` | `1995` | Loopback listener port / 回环监听端口 |
| `FMA_IMAP_PORT` | `1143` | Loopback listener port / 回环监听端口 |
| `FMA_IMAPS_PORT` | `1993` | Loopback listener port / 回环监听端口 |
| `FMA_HTTP_PORT` | `8080` | Loopback listener port / 回环监听端口 |
| `FMA_LINEAGE` | `/etc/letsencrypt/live/<mail-host>` | Certbot certificate directory / Certbot 证书目录 |
| `FMA_WEBROOT` | `/var/www/certbot` | ACME webroot / ACME 验证目录 |
| `FMA_OVERWRITE` | `no` | Allow replacing generated configuration / 是否覆盖生成配置 |

安装器参数也固定如下：

| 参数 / 环境变量 | 默认值 | 用途 |
| --- | --- | --- |
| `--systemd` | off | Install binary and service / 安装二进制及服务 |
| `--config-dir PATH` | generated by installer / 安装器生成 | Reuse generated configuration with --systemd / 复用生成配置 |
| `--uninstall` | off | Remove this execution mode's installation / 卸载当前执行模式的安装 |
| `FMA_INSTALL_DIR` | root: /usr/local/bin; user: ~/.local/bin | Binary installation directory / 二进制安装目录 |
| `FMA_DEPLOY_DIR` | root: /etc/fma/deploy; user: ~/.config/fma/deploy | Generator workspace / 生成器工作目录 |
| `FMA_REPO` | Jabberwocky238/fma | Release repository / 发行仓库 |

<a id="run-direct"></a>

### 3.1. 直接启动

直接安装最新稳定版：

```sh
curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh | bash
```

支持 Linux、macOS、Windows 的 amd64 和 arm64，普通用户默认安装到 `~/.local/bin/fma`，root 安装到 `/usr/local/bin/fma`。下载后校验 SHA-256 和二进制版本号。运行 `fma --version` 查看版本；如有需要，将 `~/.local/bin` 加入 PATH。

已安装相同或更新版本时保持不变；旧版本或无法识别的版本会询问 `Update? [y/N]`，只有输入 `y` 才更新。下载、校验失败保留原二进制。`FMA_INSTALL_DIR` 可修改安装目录，`FMA_REPO` 可选择 fork 仓库。Windows 在 Git Bash/MSYS/Cygwin 中运行安装器，需要 Bash、curl 和 unzip；脚本识别平台和架构后下载 ZIP 并安装 `fma.exe`。Linux/macOS 使用 tar.gz，`--systemd` 仅支持 Linux。

编译需要 Go 1.25 或更高版本。桶必须提前存在，并支持一致的读取和列表操作、ETag，以及原子的条件 PUT（`If-None-Match`、`If-Match`）。

```sh
export FMA_S3_ENDPOINT=http://127.0.0.1:9000
export FMA_S3_BUCKET=fma
export FMA_S3_REGION=us-east-1
export FMA_S3_ACCESS_KEY_ID=local
export FMA_S3_SECRET_ACCESS_KEY=local
./fma
```

Fals3y 接受任意凭证，但 SDK 仍需要密钥对。其他 S3 服务应使用真实凭证，临时凭证可设置 `FMA_S3_SESSION_TOKEN`。邮件服务不读取本地 AWS 配置文件。邮件配置统一自动添加 `FMA_` 前缀；命令行 `-s3-endpoint`、`-s3-bucket`、`-s3-region` 优先于环境变量。桶不可用时启动失败。默认监听回环地址，使用 `./fma -h` 查看端口。

启动前将 TLS 证书链和私钥上传为 `cert.pem`、`key.pem`。`-cert`、`-key` 指定桶内对象键，不是本地路径。`FMA_RELAY_PASSWORD_FILE`、`FMA_RELAY_CA_FILE` 同样指向桶内对象。证书和中继配置只在启动时加载。`-tls-dir /run/fma/tls` 改为从只读挂载目录读取 `tls.crt` 和 `tls.key`，替代 S3 证书对象。

全局结构化 logger 使用 **`LOG_LEVEL`**，不带 `FMA_` 前缀，在加载邮件配置前读取。支持 `debug`、`info`、`warn`、`error`，默认 `info`。启动时先检查完整配置，关键配置缺失直接退出，然后才连接 S3 和监听端口；关闭外发等可运行的情况输出 warning。`--version` 不需要 S3 配置，`--queue` 只需要 S3。

<a id="run-systemd"></a>

### 3.2. systemd 启动与 gen.sh

在 Linux 上同时安装 **systemd 服务**：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --systemd
```

该模式使用发行包内的部署模板，交互生成配置，然后安装、启用并启动 `fma.service`。需要可用的 systemd 管理器（非 root 模式需要用户会话），并提前准备好 S3 桶和证书对象。普通用户的生成器保存在 `~/.config/fma/deploy`，root 则保存在 `/etc/fma/deploy`，配置位于其 `generated/` 子目录；可通过 `FMA_DEPLOY_DIR` 修改位置。

复用仓库内已经生成的配置：

```sh
bash install.sh --systemd --config-dir deploy/generated
```

此时以生成配置中的用户和路径为准，优先于 `FMA_INSTALL_DIR`。二进制已是最新版仍可安装服务；拒绝更新二进制时也跳过服务变更。不带 `--systemd` 只安装二进制。

使用 `systemctl --user status fma` 查看状态，`journalctl --user -u fma` 查看日志。需要退出登录后持续运行时，在主机上配置该用户的 lingering。

安装器会以 warning 明确打印当前是 **root 模式** 还是 **用户模式**，并显示路径：

| 模式 | 二进制 | 配置 | systemd 单元 | 管理命令 |
| --- | --- | --- | --- | --- |
| root | `/usr/local/bin/fma` | `/etc/fma` | `/etc/systemd/system/fma.service` | `systemctl` |
| 普通用户 | `~/.local/bin/fma` | `~/.config/fma` | `~/.config/systemd/user/fma.service` | `systemctl --user` |

root 服务使用 `multi-user.target`，用户服务使用 `default.target`。root 查看日志使用 `journalctl -u fma`。
以安装时相同的执行者直接卸载：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --uninstall
```

卸载会停止并禁用对应服务，删除二进制、服务单元、安装的环境文件和安装器生成的部署文件，不请求下载发行包。自定义安装路径记录在对应默认配置目录的 `install.paths` 中。保留无关文件和所有 S3 数据；root 卸载不会遍历其他用户的家目录。

root 安装还会创建 `/bin/fma` 到安装二进制的软链接，默认目标为 `/usr/local/bin/fma`。卸载时仅清除指向该安装二进制的软链接，不覆盖或删除其他程序占用的 `/bin/fma`。

默认关闭外发。设置 `FMA_OUTBOUND_MODE=direct` 使用 MX 直投，或设置为 `relay` 使用 SMTP 中继。中继就是另一台 SMTP 服务器：fma 把外发邮件交给它，由它负责投递到收件人的邮件服务，需要配置中继地址和凭证。这只影响外部投递，不影响收信或读取本地邮箱。

```sh
bash deploy/gen.sh
make install
```

生成器询问域名、Linux 服务用户及 UID、安装路径、S3 连接与凭证、证书对象键、外发配置、重试间隔、本地协议端口和 Certbot 路径。终端内输入密钥时隐藏内容。

每一项输入均先校验，域名、IPv4/IPv6、端点 URL、中继地址、路径或端口有误时提示错误，并重新询问当前项。非密钥输入去除首尾空白，域名转为小写；密钥内容原样保留。Region 等字段有默认值，直接回车即可使用 `us-east-1` 等默认值。生成器检查格式，S3 访问能力在服务启动时检查。

模板位于 `deploy/template/`，使用 `@@NAME@@` 占位符；输出到 `deploy/generated/`，Git 忽略该目录，发行包不包含生成配置。默认私有权限，覆盖前询问，取消输入不会破坏已有配置。

每次询问前先读取对应环境变量；已设置时直接校验并使用，不再询问。环境变量无效时直接报错退出，日志不打印密钥。Kubernetes 或其他自动化环境可使用：

```sh
export FMA_MAIL_HOST=mail.example.com
export FMA_S3_ENDPOINT=https://s3.example.com
export FMA_S3_BUCKET=fma
export FMA_S3_ACCESS_KEY_ID=your-access-key
export FMA_S3_SECRET_ACCESS_KEY=your-secret-key
bash deploy/gen.sh --non-interactive
```

非交互模式中，未设置的字段使用默认值，必填项缺失直接退出。Region 默认 `us-east-1`。覆盖已有生成文件需设置 `FMA_OVERWRITE=yes`，默认保留。交互模式同样优先使用环境变量。仅用于生成器的变量会渲染为服务启动参数，不会给邮件进程增加管理 API。


在 Linux 目标机上以生成配置中选定的用户执行 `make install`，它读取生成配置、编译二进制、安装对应的 systemd 服务和环境文件，再启用并重启服务。配置缺失会在编译或安装前报错。安装路径取自生成的 `install.mk`，修改路径需要重新生成。

将生成的 `nginx-http.conf`、`nginx-https.conf` 放入 Nginx HTTP 上下文；`nginx-stream.conf` 放在顶层，位于 `http {}` 外。公开端口为标准邮件端口，上游回环端口与服务一致。证书需覆盖公共服务主机名及客户端连接时使用的全部主机名。服务启动前上传初始证书和账户密码对象。

将生成的 `renew-hook.sh` 安装为 root 执行的 Certbot deploy hook，使用服务用户的 S3 配置上传续期证书并重启用户服务。该集成需要 Bash、AWS CLI、Nginx、`runuser` 和 systemd；生成器本身只需要 Bash 和常规 Unix 工具。Nginx 配置和 root hook 需单独安装。

<a id="run-docker"></a>

### 3.3. Docker 启动

[Dockerfile](Dockerfile) 使用 Go 构建阶段和 Alpine 3.23 运行阶段，包含 CA 证书。运行身份为 UID/GID 65532，无需数据卷；账户、TLS 密钥、邮件和队列仍在已有 S3 桶中。构建方式参考 [Docker 多阶段构建文档](https://docs.docker.com/build/building/multi-stage/)。

```sh
cp .env.example .env
# 编辑 .env，填写域名、S3 端点、桶和凭证。
# 提前向桶上传 cert.pem、key.pem 和账户对象。
docker compose up -d --build
docker compose logs -f fma
docker compose down
```

Compose 发布标准邮件 TCP 端口，HTTP 存活检查仅映射到宿主机回环地址的 8080 端口。容器内监听 `0.0.0.0`；S3 地址里的 localhost 指的是容器自身，应使用容器可访问的外部 S3 地址。`.env` 已被 Git 忽略。不创建 S3 容器、本地数据库或数据卷，容器根文件系统只读。容器直接运行 fma，不使用 systemd 或安装脚本。

自行构建镜像并注入 commit：

```sh
docker build --build-arg COMMIT="$(git rev-parse HEAD)" -t fma:local .
```

可另外指定 `VERSION`、`RELEASE_TIME` 构建参数；未指定时使用 `dev-{datetime}` 和 UTC 构建时间。构建上下文仅包含 Go 源码、模块文件和 Dockerfile，不包含环境文件或生成的凭证。

复用 Compose 配置运行发布的镜像：

```sh
FMA_IMAGE=ghcr.io/jabberwocky238/fma:latest docker compose up -d --no-build --pull always
```

JMAP 经宿主机回环地址 8080 提供 HTTP 后端，请接入 HTTPS 反向代理，并设置匹配的 `FMA_JMAP_URL`。上传限制至少 4 GiB；不要让代理缓冲附件到本地磁盘。

<a id="run-kubernetes"></a>

### 3.4. Kubernetes 启动

[deploy/kubernetes/](deploy/kubernetes/) 提供 Kustomize 配置，包括两个副本的 Deployment、ConfigMap、TCP LoadBalancer Service 和 PodDisruptionBudget。Pod 使用非 root 身份，不挂载本地数据卷或 Kubernetes API 凭证，共享同一个 S3 桶。TCP Service 暴露七个邮件端口，独立的 HTTP Service 将 JMAP 后端接入 Ingress。集群需要支持 LoadBalancer，或按现有 TCP 接入设施修改 Service 类型。

编辑 `configmap.yaml` 中的域名、S3 端点和桶。证书直接使用 Kubernetes 的 `fma-tls` TLS Secret，以只读方式挂载到 `/run/fma/tls`，不再从 S3 读取。已有 cert-manager 证书时，在 `deployment.yaml` 修改 `secretName` 指向对应 Secret，或提前创建：

```sh
kubectl create namespace fma --dry-run=client -o yaml | kubectl apply -f -
kubectl -n fma create secret tls fma-tls --cert=fullchain.pem --key=privkey.pem
```

该部署无需向 S3 上传证书对象。证书仅在启动时加载，Secret 续期后需要滚动重启 Pod，或使用集群已有的 Secret 重载控制器；fma 不写入或管理挂载文件。

在 `kustomization.yaml` 指定已发布的 GHCR 版本或自行推送的镜像。在 `fma` 命名空间创建凭证 Secret：

```sh
kubectl -n fma create secret generic fma-s3 \
  --from-literal=FMA_S3_ACCESS_KEY_ID="$FMA_S3_ACCESS_KEY_ID" \
  --from-literal=FMA_S3_SECRET_ACCESS_KEY="$FMA_S3_SECRET_ACCESS_KEY" \
  --from-literal=FMA_S3_SESSION_TOKEN="${FMA_S3_SESSION_TOKEN:-}" \
  --dry-run=client -o yaml | kubectl -n fma apply -f -
kubectl apply -k deploy/kubernetes
kubectl -n fma rollout status deployment/fma
kubectl -n fma get service fma
```

私有镜像包需要在 Deployment 配置拉取凭证。固定版本标签可使部署结果可复现；`latest` 在 Pod 启动时拉取，发布新镜像不会自动重启已有 Pod。修改环境配置或更新 `latest` 后，执行 `kubectl -n fma rollout restart deployment/fma`。按实际负载配置资源 requests/limits。退出宽限期为 120 秒。

启动、就绪和存活探针使用 HTTP `/`，检查进程是否运行，不持续检查 S3 可用性。参考 [Kubernetes 探针文档](https://kubernetes.io/docs/concepts/workloads/pods/probes/)。

JMAP 使用 `fma-http` ClusterIP Service 和 `ingress.yaml`，HTTPS 证书同样来自 `fma` 命名空间的 `fma-tls` Secret。将 Ingress 主机名和 ConfigMap 中的 `FMA_JMAP_URL` 改成实际域名；集群需要已有 Ingress 控制器，并按该控制器配置上传限制及关闭请求缓冲。

<a id="jmap"></a>

## 4. JMAP 使用

JMAP 客户端使用 `https://mail.example.com/.well-known/jmap` 发现会话，通过 HTTP Basic 使用已有账户或别名及密码认证。从返回值读取 `apiUrl`、`uploadUrl`、`downloadUrl` 和 `primaryAccounts`，账户 ID 是不透明标识，不是登录名。公开地址不同时设置 `FMA_JMAP_URL`。HTTP 后端应放在 HTTPS 代理之后；`/` 仍是无需认证的活性检查。

```sh
export JMAP_USER=alice@example.com
export JMAP_PASSWORD='your-password'
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  https://mail.example.com/.well-known/jmap
```

客户端需要支持 JMAP Mail，而不只是通讯录或日历。填写上述会话 URL 和账户凭证。HTTP/JSON 调用的完整读取、发信与附件请求示例见[英文版同一章节](README.md#jmap)，两种语言对应同一接口。

| 操作 | 方法与流程 |
| --- | --- |
| 读取与搜索 | `Mailbox/get`、`Email/query`、`Email/get`、`Thread/get`、`SearchSnippet/get` |
| 文件夹修改 | `Mailbox/set` 创建、改名、父目录、订阅和删除 |
| 邮件修改 | `Email/set` 创建草稿、修改 `keywords` / `mailboxIds`、删除 |
| 导入 MIME | 向 `uploadUrl` 上传 RFC 5322 原文，再用 `Email/import` 提交 `blobId` 和目标 `mailboxIds` |
| 附件 | 上传字节后，将返回的 `blobId` 写入草稿 `attachments`；通过 `downloadUrl` 下载 `Email/get` 返回的附件 blobId |
| 发信 | `Identity/get` → `Email/set` 创建草稿 → `EmailSubmission/set` 提交 emailId 和 identityId |
| 投递结果 | `EmailSubmission/get` 的 `undoStatus` 和每个收件人的 `deliveryStatus` |
| 增量同步 | 保存每种类型的 state，用 `/changes` 和 `sinceState` 获取 created/updated/destroyed，再刷新或删除本地缓存条目 |
| 查询同步 | 保存 queryState，用 `Email/queryChanges` 获取变化；遇到 cannotCalculateChanges 时重新查询 |
| 并发修改 | 传入 ifInState；遇到 stateMismatch 时先刷新再合并修改 |

读取请求的 `using` 包含 `urn:ietf:params:jmap:core` 与 `urn:ietf:params:jmap:mail`；发信再加 `urn:ietf:params:jmap:submission`。请求发往会话返回的 apiUrl：

```sh
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  --header 'Content-Type: application/json' --data-binary @request.json \
  https://mail.example.com/api
```

附件上传地址用会话提供的 uploadUrl，并替换 `{accountId}`：

```sh
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  --header 'Content-Type: application/octet-stream' \
  --header 'Content-Disposition: attachment; filename="document.pdf"' --data-binary @document.pdf \
  'https://mail.example.com/upload/ACCOUNT_ID/'
```

将 `{"blobId":"UPLOADED_BLOB_ID","type":"application/pdf","name":"document.pdf"}` 放入草稿的 attachments 数组。支持 multipart MIME、二进制附件、空文件与 Unicode 文件名。收到的附件只从认证账户所属的邮件解析，知道其他账户的 blob 哈希不会获得访问权。JMAP 单次 HTTP 上传限制为 4 GiB，邮件附件总计最多 4 GiB；完整 MIME 保留 6 GiB 上限，为 Base64 编码和头部留空间。JMAP 上传直接流式接收原始字节，组装 MIME 时才进行 Base64 编码。

To/Cc/Bcc 都参与信封收件人计算；传出的 MIME 删除 Bcc 头。外部投递需要 direct 或 relay，本地投递在关闭外发时仍可使用。提交记录保存在 S3，重启后由原有 15 秒调度器继续处理；完成时清除活动 claim 和重试索引，保留供 JMAP 查询的投递结果。Claim 固定 20 秒；JMAP 每次传输尝试限制为 6 秒，为带条件的完成写入和库要求的时钟偏差留出余量，临时失败会重试。

SMTP、IMAP、POP3 和 JMAP 共享邮箱：SMTP 收信可在 JMAP 中读取，JMAP 对文件夹、标记、邮件的修改对其他协议可见。持久化的 `/changes` 与 `/queryChanges` 游标支持重启和跨节点同步。本版本未启用 EventSource 与 Web Push 订阅，客户端通过轮询变化同步；RFC 表不代表实现了所有可选扩展。

<a id="testing"></a>

## 5. CI、测试与 Fals3y

### 本地 Fals3y

安装原生 [Fals3y](https://github.com/LukeOfEarth/fals3y) 二进制后执行：

```sh
make build
python3 scripts/local.py
```

脚本启动 Fals3y，按需创建 `fma` 桶和测试证书，再启动邮件服务；不创建账户，不使用 Docker。Fals3y 数据目录默认为 `~/.local/share/fals3y/data`，邮件进程只通过 S3 访问数据。按 Ctrl+C 停止两个服务。

`--bucket <name>` 选择已有桶，`--data` 修改 Fals3y 数据目录。该本地环境使用无鉴权 S3 端点和自签名 TLS 证书。

POP3/STLS 和 POP3S 使用 Jabberwocky238/go-pop3（migadu/go-pop3 的性能修复 fork），由库管理协议、TLS、SASL PLAIN 和连接，fma 提供 S3 认证及邮箱会话。`DELE` 标记删除，`RSET` 撤销标记，`QUIT` 提交删除；未执行 `QUIT` 就断开连接会保留邮件。

### 测试与构建

```sh
make test
```

运行格式检查、`go vet`、Go race 测试，以及原生 Fals3y 上的 S3/SMTP/POP3/IMAP 集成测试。覆盖并发领取、超时接管、旧执行者完成写入拒绝、1024 任务让出锁、清理中断、并发 UID 分配、超过 1000 个对象的列表、外发重试、文件夹和进程重启。也覆盖附件、抄送和密送场景。邮件测试进程运行在空的只读工作目录中，无需 Docker。`FALS3Y_BIN` 可覆盖默认的 `~/.local/bin/fals3y`。

构建时通过链接参数分别注入 `version`、`commit` 和 `releaseTime`。`make build`、`make test` 默认使用 `dev-20260910T120000Z` 形式的 UTC 版本号、完整 commit 和 RFC3339 UTC 构建时间。可通过 `VERSION`、`COMMIT`、`RELEASE_TIME` 覆盖。请用 Make 构建以填充这些信息；`fma --version` 第一行为版本，后续为 commit 和发布时间字段。

JMAP 全套测试入口：

```sh
python3 scripts/verify_jmap.py
```

该脚本复用原生 Fals3y 隔离测试环境，已接入 `make test` 和 CI。覆盖认证/账户隔离、别名与代理限制、文件夹层级、状态冲突、邮件导入/创建/删除、搜索/线程、附件上传下载与复用、跨协议读写、To/Cc/Bcc、队列和进程重启。存储适配器另外运行 naust-jmap 的原子批量写、条件断言、排序扫描、并发和重新打开契约测试。

### CI 与发布

GitHub Actions 在分支 push 和 PR 上使用 Go 1.25 及当前稳定版，执行格式、vet、race、原生 Fals3y 集成测试、构建和 GoReleaser 配置检查。

推送新的语义版本 tag，检查通过后自动发布 GitHub Release：

```sh
git tag v0.2.0
git push origin v0.2.0
```

GoReleaser 无 CGO 构建 Linux、macOS、Windows 的 amd64/arm64 二进制，注入 tag 版本、完整 commit 和 UTC 发行构建时间。该时间发生在 GitHub Release 实际发布之前。快照版本为 `dev-{datetime}`。发行包包含二进制、两种语言的 README、MIT 许可证、部署模板及 SHA-256 校验文件；Windows 为 ZIP，其他平台为 tar.gz。带预发布后缀的 tag 生成预发布版本。发布使用工作流自带、拥有 `contents: write` 权限的 `GITHUB_TOKEN`，无需个人 token 或 Docker。

镜像发布独立且**仅支持手动触发**：在 Actions → Publish GHCR image → Run workflow 执行，或运行：

```sh
gh workflow run ghcr.yml
```

工作流读取最新稳定 GitHub Release 的 tag，检出该 tag 对应的 commit，只构建 **Linux amd64/arm64**，推送到 `ghcr.io/jabberwocky238/fma:<release-tag>` 和 `:latest`。构建元信息使用发行版本、源码 commit 和 Release 发布时间，`latest` 随之提升到该发行版镜像。该 Release 必须包含 Dockerfile；工作流不构建 main 上尚未发布的改动，不提供 macOS/Windows 容器镜像。使用 `GITHUB_TOKEN` 的 `packages: write` 权限认证。二进制 Release 仍覆盖三个操作系统和两种架构。

本地验证打包：

```sh
goreleaser check
goreleaser release --snapshot --clean
```

工作流要求本项目目录就是 GitHub 仓库根目录。

<a id="bucket"></a>

## 6. 桶布局

### 多邮件域名

从桶的一级 prefix 自动发现托管域，不需要 `-domain`、`-domains` 或域名列表环境变量。创建 `example.com/alice/.password` 和 `example.com/alice/.kind`，即可使用 `alice@example.com`；在 `example.org/` 下配置同名账户，两者密码和邮箱独立。登录必须填写完整邮箱地址。新增域和账户可立即收信，外发扫描器每 15 秒发现新域。

账户状态、邮件和附件保存在 `<域名>/<账户>/` 内；每个域独立使用 `.outbox/` 和 `.lock`，JMAP account ID 和缓存也包含域名。正常跨域投递或显式 proxy 转发会在目标账户保存数据。托管域中的未知账户直接拒绝，不转为外部投递。

不兼容旧的桶根账户布局，不执行迁移，请直接按新布局配置账户。证书和中继连接配置仍由整个服务共用，保留在域 prefix 外。证书须覆盖客户端实际连接的主机名，中继须允许所有托管域的发件地址。`FMA_JMAP_URL` 可指定所有账户共用的 JMAP 入口；未指定时按账户域公布 `https://mail.<域名>`。SMTP greeting/EHLO 使用 JMAP URL 的主机名，未配置 URL 时使用操作系统主机名。

### 账户类型

每个账户位于 `<domain>/<id>/`。**必须存在 `<domain>/<id>/.kind`**，且只允许以下三种类型。类型唯一决定行为，其他类型残留的配置文件不会生效，避免冲突。

| `.kind` | 具体配置 | 行为 |
| --- | --- | --- |
| `account` | `.password` 保存非空密码 | 实体邮箱，可以登录协议收发 |
| `alias` | `.alias` 保存另一个本地 ID | 解析到目标 ID；仅当最终目标为 account 时可登录 |
| `proxy` | `.proxy` 保存一个完整邮箱地址 | 只转发，不允许登录，不在本地保留原邮件 |

先准备具体配置，最后写入 `.kind` 启用账户：

```sh
printf '%s' 'your-password' | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/example.com/alice/.password
printf '%s' account | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/example.com/alice/.kind
```

设置 `.kind=account` 后，覆盖 `.password` 修改密码，删除密码对象后新的登录与本地 SMTP 收件人检查会被拒绝，无需重启。已认证会话不会自动撤销。

登录必须使用完整邮箱地址，例如 `alice@example.com`；不接受裸用户名或存储路径。本地用户名为 1–64 个小写字母、数字、点、连字符或下划线，首字符必须是字母或数字。密码不能为空或包含内嵌换行，末尾 CR/LF 会被去除。密码以明文对象存储，依靠桶的访问控制保护。每次认证和本地收件人检查均读取 S3，没有账户列表或密码缓存。

设置 `<domain>/<alias>/.kind=alias`，在 `<domain>/<alias>/.alias` 写入同域的目标本地 ID，即可在外部配置别名。跨域转发使用 proxy 的完整目标地址。别名保留自己的前缀，使用根账户密码并共享根邮箱。每次登录和收件人检查都会解析别名链，拒绝循环引用和不存在的账户。`.profile.json` 等隐藏元信息对象不会被当作邮件。协议会话区分登录 ID 与根 ID；SMTP、POP3 和 IMAP 不提供头像或个人资料管理 API。

代理设置为 `<domain>/<id>/.kind=proxy`，在 `.proxy` 中写入一个完整目标邮箱地址。本地目标直接解析投递；外部目标进入 S3 持久化队列，需要启用 `direct` 或 `relay`。未经认证的外部来信可以投递到已配置的本地代理，但不能自行选择任意外部目标。

转发保持 MIME 正文和附件不变，对外转发时只增加 `X-FMA-Proxy-Hops` 头用于限制循环。别名/代理本地解析最多 16 跳，外部代理转发最多 16 跳。每次 RCPT 都读取 S3 路由，接受邮件后任务保存当时的目标，修改代理不影响已入队邮件。多个收件人指向同一目标时只投递一次。

转发保留原信封发件人，尚未实现 SRS 重写，目标方的发件人策略仍可能拒绝转发邮件。永久失败会把不含邮件正文的诊断信息保存到 `<domain>/<proxy>/.proxy-errors/<task-id>.json`，随后同步删除任务及 preclaim，不给代理创建实体邮箱。

`.kind` 缺失或值无效时拒绝登录和投递。**已有账户需通过外部工具补上 `.kind=account`，已有别名补上 `.kind=alias`。** 不提供自动迁移或注册 API。切换类型时先准备新类型配置，最后替换 `.kind`。

| 对象键 | 内容 |
| --- | --- |
| `<domain>/<id>/.kind` | account / alias / proxy |
| `<domain>/<account>/.password` | Account password / 账户密码 |
| `<domain>/<alias>/.alias` | Target local ID / 目标本地 ID |
| `<domain>/<proxy>/.proxy` | Forwarding address / 转发邮箱地址 |
| `<domain>/<proxy>/.proxy-errors/<id>.json` | Failure diagnostic without body / 无正文的失败诊断 |
| `<domain>/<account>/.jmap/state.json` | 最小账户状态：文件夹、身份、UID/状态计数、账户租约和当前事务决定；不保存邮件记录或查询索引 |
| `<domain>/<account>/mail/<escaped-subject>_<timestamp>/<escaped-filename>` | 流式写入的不可变 MIME 或上传附件；超过 10 MiB 使用 gzip |
| `<physical-object>.blob-<blobId>.json` | 与正文/附件同目录的不可变 blob 描述，记录物理对象和编码/大小；blobId 到描述文件的查找表仅在内存中 |
| `<domain>/<account>/mail/<mail-id>/attachments/<part-id>/<escaped-filename>` | 解码后的 MIME 部件，超过 1 MiB 使用 gzip |
| `<blob-descriptor>.mime.json` | 同邮件 prefix 内的 MIME 结构、部件哈希、大小和预览 |
| `<domain>/<account>/mail/<mail-id>/.fma/*.fma.json` | 邮件记录及单邮件变更历史 |
| `<domain>/<account>/mail/.records/`, `mail/.history/`, `mail/.uploads/` | 线程/提交记录、批量变更历史、正文发布前登记的上传记录 |
| `<owner-record>.prepare.<transaction>` | 条件提交前写入的不可变事务记录；不属于查询索引 |
| `<domain>/<account>/.jmap/blobs/*` | 旧版正文、描述、MIME 元数据和部件定位记录的兼容读取路径 |
| `<domain>/.outbox/<id>.json` | SMTP outbound task and embedded preclaim / SMTP 外发任务及内嵌 preclaim |
| `<domain>/.lock` | Shared 15-second scan/renewal lease / 共享的 15 秒扫描与续期租约 |
| `cert.pem, key.pem` | TLS certificate and key unless mounted from a Secret / TLS 证书与私钥，Secret 挂载时不需要 |

服务只使用域名/账户布局，不发现或迁移旧的桶根邮箱。二进制不提供注册或管理命令。未引用的上传与 MIME 对象留在 S3，不使用本地暂存目录或后台清理器。

邮件物理目录 ID 使用 `subject + UTC timestamp`，时间戳精确到纳秒。
主题先解码 RFC 2047，再与文件名分别做百分号转义；`/`、`%`、`?`、`#`、Unicode、单独的 `.` 和 `..` 不会改变路径层级，并限制各段长度。S3 条件创建保证时间戳或名称碰撞时报错，不覆盖已有内容。
MIME 正文使用 `message.eml`；HTTP 上传可通过 `Content-Disposition` 提供附件名，未提供时使用 `attachment.bin`，无主题时使用 `untitled`。接收 MIME 时并行上传原文和解析部件，附件解码后直接流式存入独立对象，成功提交后发布 JSON 元数据。首次 JMAP 获取元数据直接读记录，首次下载直接读附件对象。保留原文供 IMAP/POP3 使用，因此增加存储用量；没有新记录的旧邮件仍走原有读取路径。
JMAP 的 blobId 保留为内容标识。分片直接上传到命名路径，完成后在同目录发布不可变描述文件，提交时不再复制整对象。描述文件保存读取字节所需的编码信息；反向查找表从对象名称重建，不写入 S3。附件父邮件关系由目录推导并校验 MIME 元数据，不再写 `.origin.json`；旧邮件的部件定位信息仅在内存中重建，不再写 `.part`。提交队列通过顶层 prefix 发现账户，不再创建 `.jmap-queue` 索引。
旧对象可继续读取。首次访问会将旧版整账户 `state.json` 拆分为权威记录，再条件切换账户文件；旧索引不写入新格式。部署前必须停止旧节点，旧版本无法读取新格式。每批更新先写 prepare 对象，再以 ETag 条件写发布事务决定；下一次写入、每秒刷新或正常退出将已提交记录落实到固定路径。中途退出可依据提交决定恢复，未提交的 prepare 不可见。当前保留 prepare、删除墓碑和未引用的 blob，不自动清理。

热缓存中的 Get/MultiGet 直接读取内存；Scan 使用排序键范围，纯索引批次不写 S3。读缓存最多每秒检查账户版本，写入前强制检查；跨节点版本变化和冷启动需要读取记录并重建索引，仍有与邮箱规模相关的恢复成本。所有已访问账户的索引保留在进程内存中，当前没有缓存淘汰策略。

SMTP 通过 `go.mod replace` 使用固定版本的性能修复 fork（[PR #312](https://github.com/emersion/go-smtp/pull/312)）。DATA reader 的单文件补丁改编自 [uponusolutions/go-smtp](https://github.com/uponusolutions/go-smtp/blob/86ff2622fb52f86371265b74a976333ff53c10a0/internal/textsmtp/dotreader.go) 的跨行扫描，保留原有服务端 API、默认 4 KiB 协议缓冲和行长度检查；fma 在 TLS 下方增加可复用的 1 MiB TCP 输入缓冲，合并小读取。POP3 直接导入独立模块 `github.com/Jabberwocky238/go-pop3`；`main` 包含性能修复和 fork 说明，`pr` 仅向上游提交性能补丁（[PR #3](https://github.com/migadu/go-pop3/pull/3)）。

POP3 v0.1.6 增加 ARM64 向量扫描，并保留通用实现。独立 2 GiB 编码测试的吞吐量再提升 2.04 倍，但完整下载尚未验证出稳定加速。CPU、RSS、不同输入类型的对照和复现命令见 [PERFORMANCE.md](PERFORMANCE.md)。

S3 上传缓冲按 1 MiB 分块，直接组合成 8 MiB 上传分片，不进行拼接复制。每次上传最多四片在途或正在填充，全局在用分片缓冲上限 1 GiB，按需分配。跨账户对象复制使用 S3 CopyObject 或 UploadPartCopy。JMAP MIME 解析使用固定版本的[流式优化 fork](https://github.com/Jabberwocky238/naust-jmap/commit/ea60168)，通过缓冲区复用和分块解码提速，并在写入时持久化 MIME 元数据和独立部件，重启后的首次请求即可使用。接收与解析之间轮转三个 1 MiB 缓冲块，转移所有权后才复用；原文与部件使用独立的有界压缩池，避免相互等待资源。

任务池启动时创建 4 个 worker，可通过 `FMA_STREAM_WORKERS=16` 或 `-stream-workers 16` 调整。空闲 worker 从共享队列领取 MIME 解析任务，队列容量与 worker 数相同，满时接收端等待，形成背压。三个 1 MiB 轮转缓冲在获准的任务开始执行后才分配；每个任务启动两个辅助 goroutine，让原始 MIME SHA-256 和 gzip/对象写入与接收、解析并行，所有消费者用完后才复用缓冲块。辅助任务不占用额外的任务池槽位，因此仅配置一个 worker 也能运行；网络协议和 S3 分片仍使用各自的并发机制。增加 worker 提升多封邮件并发处理能力，不会把单个 Base64 流自动切成八份。取消和退出会通知任务并排空队列，原文与部件的压缩池隔离。

实测吞吐量和当前瓶颈见 [PERFORMANCE.md](PERFORMANCE.md)。

吞吐量测试默认使用 2 GiB 附件，同时报告传输字节和原始附件字节的 MiB/s。
`--smtp-transfer bdat` 单独测量已有的 SMTP CHUNKING 流式路径；默认仍测 DATA，保留其点转义处理耗时：

```sh
python3 scripts/test_streaming.py --mail --size-mib 2048 --report /tmp/fma-data.json
python3 scripts/test_streaming.py --mail --size-mib 2048 --smtp-transfer bdat --report /tmp/fma-bdat.json
```

<a id="credits"></a>

## 7. 许可证与鸣谢

[MIT](LICENSE)，Copyright © 2026 Jabberwocky238。

感谢以下项目；各项目保持自己的许可证，不会被 fma 的 MIT 许可证替代。

| 项目 | 用途 | 许可证 |
| --- | --- | --- |
| [emersion/go-smtp](https://github.com/emersion/go-smtp) | SMTP | [MIT](https://github.com/emersion/go-smtp/blob/v0.25.0/LICENSE) |
| [emersion/go-imap](https://github.com/emersion/go-imap) | IMAP | [MIT](https://github.com/emersion/go-imap/blob/v1.2.1/LICENSE) |
| [Jabberwocky238/go-pop3](https://github.com/Jabberwocky238/go-pop3)（migadu/go-pop3 fork） | POP3 | [MIT](https://github.com/migadu/go-pop3/blob/v0.1.4/LICENSE) |
| [naust-mail/naust-jmap](https://github.com/naust-mail/naust-jmap) | JMAP Core and Mail / JMAP 核心与邮件 | [Apache-2.0](https://github.com/naust-mail/naust-jmap/blob/main/LICENSE) |
| [emersion/go-message](https://github.com/emersion/go-message) | MIME | [MIT](https://github.com/emersion/go-message/blob/v0.18.2/LICENSE) |
| [emersion/go-sasl](https://github.com/emersion/go-sasl) | SASL | [MIT](https://github.com/emersion/go-sasl/blob/master/LICENSE) |
| [aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) | S3 SDK | [Apache-2.0](https://github.com/aws/aws-sdk-go-v2/blob/v1.36.3/LICENSE) |
| [aws/smithy-go](https://github.com/aws/smithy-go) | AWS SDK support / AWS SDK 支持 | [Apache-2.0](https://github.com/aws/smithy-go/blob/v1.22.2/LICENSE) |
| [klauspost/compress](https://github.com/klauspost/compress) | Streaming gzip compression and decompression | [BSD-3-Clause](https://github.com/klauspost/compress/blob/v1.20.0/LICENSE) |
| [WireGuard](https://github.com/WireGuard/wireguard-go/blob/ecfc5a8d54462e18e13c72173e2623d16d8e25a0/device/pools.go) | WaitPool design, adapted with cancellation | [MIT](https://github.com/WireGuard/wireguard-go/blob/ecfc5a8d54462e18e13c72173e2623d16d8e25a0/LICENSE) |
| [LukeOfEarth/fals3y](https://github.com/LukeOfEarth/fals3y) | Native S3 for development and integration tests / 开发及集成测试用原生 S3 | [MIT](https://github.com/LukeOfEarth/fals3y/blob/v0.3.0/LICENSE) |
| [golang.org/x/text](https://pkg.go.dev/golang.org/x/text) | Character encodings / 字符编码 | [BSD-3-Clause](https://cs.opensource.google/go/x/text/+/refs/tags/v0.34.0:LICENSE) |
