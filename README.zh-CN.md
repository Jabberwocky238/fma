# fma

[English](README.md) | [简体中文](README.zh-CN.md)

**fma 是一个轻量邮件服务，唯一需要的外部服务依赖是 S3。**
支持 SMTP、POP3 和 IMAP，所有持久化数据均保存在同一个 S3 桶中。

- **高可用**：多个节点共享一个桶，节点宕机后，其他节点可以接管持久化的投递任务。
- **高并发设计**：并行处理协议连接，通过 S3 条件写协调跨节点任务归属和邮箱更新。
- **占用小**：单个 Go 二进制，无需本地数据库、Redis、邮件暂存目录或 Docker。实际内存和吞吐取决于邮件大小、连接数与 S3 延迟，目前没有公布生产环境基准数据。

可用性依赖 S3，以及将客户端流量导向健康节点的接入设施。节点故障后，原有连接需要重连。
账户、邮件、文件夹、任务队列和租约全部存放在 S3。证书默认从 S3 读取，Kubernetes 部署则直接只读挂载 TLS Secret。邮件二进制不提供注册、用户管理、CSV 导入、本地数据库、磁盘缓存或临时文件管理能力。HTTP 只提供存活检查，DEBUG/INFO/WARN 日志输出到 stdout，ERROR/FATAL 输出到 stderr。

## 安装发行版

直接安装最新稳定版：

```sh
curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh | bash
```

支持 Linux、macOS、Windows 的 amd64 和 arm64，普通用户默认安装到 `~/.local/bin/fma`，root 安装到 `/usr/local/bin/fma`。下载后校验 SHA-256 和二进制版本号。运行 `fma --version` 查看版本；如有需要，将 `~/.local/bin` 加入 PATH。

已安装相同或更新版本时保持不变；旧版本或无法识别的版本会询问 `Update? [y/N]`，只有输入 `y` 才更新。下载、校验失败保留原二进制。`FMA_INSTALL_DIR` 可修改安装目录，`FMA_REPO` 可选择 fork 仓库。Windows 在 Git Bash/MSYS/Cygwin 中运行安装器，需要 Bash、curl 和 unzip；脚本识别平台和架构后下载 ZIP 并安装 `fma.exe`。Linux/macOS 使用 tar.gz，`--systemd` 仅支持 Linux。

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

## 本地运行

安装原生 [Fals3y](https://github.com/LukeOfEarth/fals3y) 二进制后执行：

```sh
make build
python3 scripts/local.py
```

脚本启动 Fals3y，按需创建 `fma` 桶和测试证书，再启动邮件服务；不创建账户，不使用 Docker。Fals3y 数据目录默认为 `~/.local/share/fals3y/data`，邮件进程只通过 S3 访问数据。按 Ctrl+C 停止两个服务。

`--bucket <name>` 选择已有桶，`--data` 修改 Fals3y 数据目录。该本地环境使用无鉴权 S3 端点和自签名 TLS 证书。

POP3/STLS 和 POP3S 使用 migadu/go-pop3，由库管理协议、TLS、SASL PLAIN 和连接，fma 提供 S3 认证及邮箱会话。`DELE` 标记删除，`RSET` 撤销标记，`QUIT` 提交删除；未执行 `QUIT` 就断开连接会保留邮件。

## 账户与别名

每个 ID 都是桶根目录下的前缀。**必须存在 `<id>/.kind`**，且只允许以下三种类型。类型唯一决定行为，其他类型残留的配置文件不会生效，避免冲突。

| `.kind` | 具体配置 | 行为 |
| --- | --- | --- |
| `account` | `.password` 保存非空密码 | 实体邮箱，可以登录协议收发 |
| `alias` | `.alias` 保存另一个本地 ID | 解析到目标 ID；仅当最终目标为 account 时可登录 |
| `proxy` | `.proxy` 保存一个完整邮箱地址 | 只转发，不允许登录，不在本地保留原邮件 |

先准备具体配置，最后写入 `.kind` 启用账户：

```sh
printf '%s' 'your-password' | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/alice/.password
printf '%s' account | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/alice/.kind
```

设置 `.kind=account` 后，覆盖 `.password` 修改密码，删除密码对象后新的登录与本地 SMTP 收件人检查会被拒绝，无需重启。已认证会话不会自动撤销。

用户名为 1–64 个小写字母、数字、点、连字符或下划线，首字符必须是字母或数字。密码不能为空或包含内嵌换行，末尾 CR/LF 会被去除。密码以明文对象存储，依靠桶的访问控制保护。每次认证和本地收件人检查均读取 S3，没有账户列表或密码缓存。

设置 `<alias>/.kind=alias`，在 `<alias>/.alias` 写入目标本地 ID，即可在外部配置别名。别名保留自己的前缀，使用根账户密码并共享根邮箱。每次登录和收件人检查都会解析别名链，拒绝循环引用和不存在的账户。`.profile.json` 等隐藏元信息对象不会被当作邮件。协议会话区分登录 ID 与根 ID；SMTP、POP3 和 IMAP 不提供头像或个人资料管理 API。

代理设置为 `<id>/.kind=proxy`，在 `.proxy` 中写入一个完整目标邮箱地址。本地目标直接解析投递；外部目标进入 S3 持久化队列，需要启用 `direct` 或 `relay`。未经认证的外部来信可以投递到已配置的本地代理，但不能自行选择任意外部目标。

转发保持 MIME 正文和附件不变，对外转发时只增加 `X-FMA-Proxy-Hops` 头用于限制循环。别名/代理本地解析最多 16 跳，外部代理转发最多 16 跳。每次 RCPT 都读取 S3 路由，接受邮件后任务保存当时的目标，修改代理不影响已入队邮件。多个收件人指向同一目标时只投递一次。

转发保留原信封发件人，尚未实现 SRS 重写，目标方的发件人策略仍可能拒绝转发邮件。永久失败会把不含邮件正文的诊断信息保存到 `<proxy>/.proxy-errors/<task-id>.json`，随后同步删除任务及 preclaim，不给代理创建实体邮箱。

`.kind` 缺失或值无效时拒绝登录和投递。**已有账户需通过外部工具补上 `.kind=account`，已有别名补上 `.kind=alias`。** 不提供自动迁移或注册 API。切换类型时先准备新类型配置，最后替换 `.kind`。

## 连接 S3

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

## 任务归属与故障恢复

多个节点可以共享同一个桶。`.lock` 只控制外发任务扫描与领取，不阻塞协议流量或已领取任务。租约记录持有者、启动时间、续期时间和过期时间，每 15 秒续期，30 秒过期。释放时使用条件写标记过期，防止删掉后继节点的锁。节点时钟需要同步。

锁持有者立即扫描 `.outbox/`，之后每 15 秒补扫一次。执行前使用条件 PUT 写入 preclaim。每节点最多持有 1024 个活动任务；一批领取 1024 个或达到容量时，立即释放扫描锁，下一轮扫描时再竞争。已经领取的任务继续执行。

Preclaim 记录持有者、开始时间和固定 20 秒超时，不续期。到期取消执行，任务留在 S3，供下一轮重新领取；扫描节点宕机还需等待扫描锁过期。旧执行者无法通过过期 ETag 覆盖新执行者。

临时 SMTP 错误会保存每个收件人的重试状态，从 `-queue-retry` 开始指数退避。已确认成功的收件人不会重复重试；永久失败在本地退信存储完成后结束。

Preclaim 内嵌于任务对象，删除任务会同时删除 preclaim。完成时先通过条件写保存不可再次领取的终态，再删除对象；如果删除失败或期间宕机，后续扫描只重试删除，不再次发送。`-queue` 查看未清理任务，不保留已完成任务历史。

S3 和远端 SMTP 之间没有共同事务：远端已经接受邮件，但节点超时或未能保存确认时，重新投递可能产生重复邮件。因此投递语义是至少一次，不是恰好一次。邮箱 UID 分配和目录更新通过 S3 条件写避免节点之间互相覆盖。

## 桶布局

| 对象键 | 内容 |
| --- | --- |
| `<id>/.kind` | 必填类型：account、alias 或 proxy |
| `<user>/.password` | 账户密码 |
| `<alias>/.alias` | 目标本地 ID |
| `<proxy>/.proxy` | 转发目标邮箱地址 |
| `<proxy>/.proxy-errors/<id>.json` | 不含正文的失败诊断 |
| `<user>/<uid>.json`、`<user>/next` | 收件箱邮件、UID 计数器 |
| `<user>/folders` | 文件夹目录、UIDVALIDITY、订阅、存储 ID |
| `<user>/.folders/<id>/` | 其他文件夹邮件和计数器 |
| `.outbox/<id>.json` | 任务正文、收件人状态、归档状态、preclaim |
| `.lock` | 扫描租约 |
| `cert.pem`、`key.pem` | TLS 证书、私钥 |

文件夹改名只更新目录，不复制邮件正文；删除后重建会分配新存储 ID。清理失败可能留下不可达对象。旧布局应在停止旧服务后通过外部工具迁移，二进制不提供导入功能。

## 外发与部署

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
export FMA_DOMAIN=example.com
export FMA_S3_ENDPOINT=https://s3.example.com
export FMA_S3_BUCKET=fma
export FMA_S3_ACCESS_KEY_ID=your-access-key
export FMA_S3_SECRET_ACCESS_KEY=your-secret-key
bash deploy/gen.sh --non-interactive
```

非交互模式中，未设置的字段使用默认值，必填项缺失直接退出。Region 默认 `us-east-1`。覆盖已有生成文件需设置 `FMA_OVERWRITE=yes`，默认保留。交互模式同样优先使用环境变量。仅用于生成器的变量会渲染为服务启动参数，不会给邮件进程增加管理 API。

| 配置项 | 环境变量 |
| --- | --- |
| 域名与服务身份 | `FMA_DOMAIN`、`FMA_DEPLOY_USER`、`FMA_DEPLOY_UID`、`FMA_DEPLOY_HOME` |
| 安装路径 | `FMA_BINDIR`、`FMA_CONFIG_DIR`、`FMA_SYSTEMD_USER_DIR`（同样用于系统级单元） |
| S3 | `FMA_S3_ENDPOINT`、`FMA_S3_BUCKET`、`FMA_S3_REGION`、`FMA_S3_ACCESS_KEY_ID`、`FMA_S3_SECRET_ACCESS_KEY`、`FMA_S3_SESSION_TOKEN` |
| 证书对象键 | `FMA_CERT_KEY`、`FMA_KEY_KEY` |
| 外发 | `FMA_OUTBOUND_MODE`、`FMA_RELAY_ADDR`、`FMA_RELAY_TLS`、`FMA_RELAY_USER`、`FMA_RELAY_PASSWORD`、`FMA_RELAY_PASSWORD_FILE`、`FMA_RELAY_CA_FILE`、`FMA_QUEUE_RETRY` |
| 端口 | `FMA_SMTP_PORT`、`FMA_SUBMISSION_PORT`、`FMA_SMTPS_PORT`、`FMA_POP3_PORT`、`FMA_POP3S_PORT`、`FMA_IMAP_PORT`、`FMA_IMAPS_PORT`、`FMA_HTTP_PORT` |
| Certbot 路径 | `FMA_LINEAGE`、`FMA_WEBROOT` |
| 日志与覆盖 | 无前缀的 `LOG_LEVEL`、`FMA_OVERWRITE` |

在 Linux 目标机上以生成配置中选定的用户执行 `make install`，它读取生成配置、编译二进制、安装对应的 systemd 服务和环境文件，再启用并重启服务。配置缺失会在编译或安装前报错。安装路径取自生成的 `install.mk`，修改路径需要重新生成。

将生成的 `nginx-http.conf`、`nginx-https.conf` 放入 Nginx HTTP 上下文；`nginx-stream.conf` 放在顶层，位于 `http {}` 外。公开端口为标准邮件端口，上游回环端口与服务一致。HTTPS 证书需覆盖域名、`www.<domain>` 和 `mail.<domain>`。服务启动前上传初始证书和账户密码对象。

将生成的 `renew-hook.sh` 安装为 root 执行的 Certbot deploy hook，使用服务用户的 S3 配置上传续期证书并重启用户服务。该集成需要 Bash、AWS CLI、Nginx、`runuser` 和 systemd；生成器本身只需要 Bash 和常规 Unix 工具。Nginx 配置和 root hook 需单独安装。

## Docker 与 Docker Compose

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

## Kubernetes

[deploy/kubernetes/](deploy/kubernetes/) 提供 Kustomize 配置，包括两个副本的 Deployment、ConfigMap、TCP LoadBalancer Service 和 PodDisruptionBudget。Pod 使用非 root 身份，不挂载本地数据卷或 Kubernetes API 凭证，共享同一个 S3 桶。Service 暴露七个邮件端口，HTTP 健康检查仅供集群内部使用。集群需要支持 LoadBalancer，或按现有 TCP 接入设施修改 Service 类型。

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

## 测试与构建

```sh
make test
```

运行格式检查、`go vet`、Go race 测试，以及原生 Fals3y 上的 S3/SMTP/POP3/IMAP 集成测试。覆盖并发领取、超时接管、旧执行者完成写入拒绝、1024 任务让出锁、清理中断、并发 UID 分配、超过 1000 个对象的列表、外发重试、文件夹和进程重启。也覆盖附件、抄送和密送场景。邮件测试进程运行在空的只读工作目录中，无需 Docker。`FALS3Y_BIN` 可覆盖默认的 `~/.local/bin/fals3y`。

构建时通过链接参数分别注入 `version`、`commit` 和 `releaseTime`。`make build`、`make test` 默认使用 `dev-20260910T120000Z` 形式的 UTC 版本号、完整 commit 和 RFC3339 UTC 构建时间。可通过 `VERSION`、`COMMIT`、`RELEASE_TIME` 覆盖。请用 Make 构建以填充这些信息；`fma --version` 第一行为版本，后续为 commit 和发布时间字段。

## CI 与发布

GitHub Actions 在分支 push 和 PR 上使用 Go 1.25 及当前稳定版，执行格式、vet、race、原生 Fals3y 集成测试、构建和 GoReleaser 配置检查。

推送新的语义版本 tag，检查通过后自动发布 GitHub Release：

```sh
git tag v0.1.0
git push origin v0.1.0
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

## 许可证

[MIT](LICENSE)，Copyright © 2026 Jabberwocky238。

## 鸣谢

感谢以下项目为 fma 提供基础能力：

- [emersion/go-smtp](https://github.com/emersion/go-smtp)：SMTP 服务端与客户端。
- [emersion/go-imap](https://github.com/emersion/go-imap)：IMAP 协议与服务端。
- [migadu/go-pop3](https://github.com/migadu/go-pop3)：POP3 协议与服务端。
- [Fals3y](https://github.com/LukeOfEarth/fals3y)：本地开发和集成测试使用的原生 S3 兼容服务。
