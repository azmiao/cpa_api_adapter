# CPA OMP Adapter

CLIProxyAPI（CPA）的原生插件：让 oh-my-pi（OMP）以 `openai-codex` 协议发现 CPA 模型，并可在 CPA 管理页面中设置模型别名、显示名和能力。

## 工作方式

插件使用 **本地 Unix 域套接字**与现有反向代理通信，不新增对外 TCP 端口。反向代理把原 CPA 地址下的 `/omp/backend-api/...` 转给插件；插件再使用 CPA 客户端 API Key 调用同一 CPA 实例。普通 CPA 路由保持原样。

```mermaid
flowchart LR
  OMP -->|原 CPA 地址和端口| Proxy[反向代理]
  Proxy -->|适配路由| Socket[本地套接字]
  Socket --> Plugin[插件]
  Plugin -->|CPA 内部地址| CPA[CPA 原有服务]
  Proxy -->|其他路由| CPA
```

需要已有反向代理，或先在 CPA 前部署反向代理。如果 CPA 当前直接占用对外端口，必须把 CPA 迁到内部地址，让反向代理接管原端口；**外部地址不变，但 CPA 内部监听地址需要调整**。插件不能通过 CPA 的标准插件接口直接添加普通客户端路由，因此不能仅安装动态库就跳过这一步。

插件仅影响适配前缀下的请求，不修改其他客户端看到的 CPA 原始模型列表。配置更新会关闭已建立的适配器 WebSocket，客户端需重连。

## 安装

1. 使用支持动态插件的 CPA；本项目实际验证过 CPA **v8.0.76**。管理 API 响应头 `X-CPA-SUPPORT-PLUGIN: 1` 只表示构建支持插件，还需要在配置中启用插件。
2. 将对应平台的动态库命名为 `cpa-omp-adapter` 并复制到 CPA 的插件目录：
   - Linux amd64：`dist/cpa-omp-adapter.so`
   - Windows amd64：`dist/cpa-omp-adapter.dll`
   - 其他平台需自行构建，macOS 使用 `.dylib`。

   本地 `dist/` 同时包含按 CPA 插件商店命名的 ZIP 和 `checksums.txt`；这些构建产物不加入 Git，也尚未发布到插件商店。ZIP 根目录中的动态库可直接解压使用。
3. 在 CPA 配置中启用插件；已有配置请合并，不要覆盖其他插件：

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       cpa-omp-adapter:
         enabled: true
   ```

4. 确保 CPA 已设置管理密钥和至少一个**客户端 API Key**，重启 CPA。通过 CPA 管理中心的「OMP 适配器」菜单打开插件，或访问：

   ```text
   https://<原CPA地址>/v0/resource/plugins/cpa-omp-adapter/index.html
   ```

5. 输入 CPA **管理密钥**连接页面。填写插件访问 CPA 的**内部地址**、套接字路径和路由前缀并保存，再按页面中的示例配置反向代理。管理密钥仅保存在当前页面内存，刷新后需重新输入。
6. 在 OMP 使用页面显示的 Base URL 和 CPA **客户端 API Key**；不要把管理密钥或官方 OpenAI OAuth token 用作客户端密钥。

页面里的「插件已就绪」只表示本地套接字已经监听，**不代表反向代理已配置或对外地址可访问**。更改套接字路径或路由前缀后，要同步更新并重载反向代理。WebUI 只生成配置示例，不会替你修改反向代理。保存通过 CPA v8 管理 API 写入并热重载配置；若原配置是旧格式，CPA 的首次 v8 写入可能将其迁移为 v8 格式。

### 套接字与权限

- 默认路径为系统临时目录下的 `cpa-omp-adapter.sock`；可在 WebUI 更换，父目录必须已经存在。不同 CPA 实例应使用不同路径。
- Linux/macOS 默认权限 `0660`。确保运行 CPA 和反向代理的账号能够访问该目录及套接字；通常应使用受保护的目录和共同用户组。WebUI 可选 `0600`、`0660` 或 `0666`；`0666` 允许所有本机用户连接，应谨慎使用。
- Windows 使用系统文件访问控制，界面中的 Unix 权限不生效。Windows 路径可使用 `C:/.../adapter.sock`，代理也必须支持 Windows Unix 域套接字。
- 容器部署时，CPA 与代理须共享同一套接字目录，并保证 UID/GID 和目录权限匹配。仅映射 CPA 的 TCP 端口不能让另一个容器访问该套接字。
- 插件不会覆盖普通文件或正在使用的套接字；对拒绝连接、确认已失效的套接字会尝试恢复。仍无法启动时，先检查该路径、目录权限和 CPA 日志，不要盲目删除文件。

## 反向代理

以下示例假定 WebUI 中的路由前缀为 `/omp`，套接字为 `/run/cpa-omp/adapter.sock`，CPA 内部地址为 `http://127.0.0.1:8317`。这些值须按实际部署修改，套接字目录也须事先创建。公网应使用 HTTPS；管理接口还应通过 VPN、访问控制等限制访问，不能只依赖反向代理连接到 CPA 的本机地址。

### Caddy

在**已有 CPA 站点块**中增加匹配规则，保留原有的 CPA 转发。不要另外创建新的对外端口：

```caddyfile
@omp path /omp/backend-api/*
reverse_proxy @omp unix//run/cpa-omp/adapter.sock

# 已有 CPA 转发示例；站点中已有该规则时不要重复添加
reverse_proxy 127.0.0.1:8317
```

Windows 路径示例：`reverse_proxy @omp "unix/C:/path/to/adapter.sock"`。WebUI 可根据当前路径生成 Caddy 或 Nginx 片段。Caddy 自动处理 WebSocket 升级；请保留 SSE 流式传输。

### Nginx

将 `map` 放在 `http` 块，将 `location` 放在现有 CPA 的 `server` 块；保留已有的 CPA 反向代理规则：

```nginx
map $http_upgrade $omp_connection_upgrade {
    default upgrade;
    ''      close;
}

location ^~ /omp/backend-api/ {
    client_max_body_size 32m;
    proxy_pass http://unix:/run/cpa-omp/adapter.sock:/omp/backend-api/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $omp_connection_upgrade;
    proxy_buffering off;
    proxy_cache off;
    proxy_request_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

修改后先运行 `nginx -t`，通过后再按现有运维流程重载。`client_max_body_size` 应与 WebUI 的请求体上限一致；代理和 CPA 的其他限制也不能小于该值。必须保留查询参数和客户端认证头；不要开启模型目录缓存，也不要缓冲 SSE。UI 中生成的 Nginx 示例面向 Unix 路径；Windows 可选择 Caddy。

## OMP 配置

在 `~/.omp/agent/models.yml` 合并以下 provider 配置；`CPA_API_KEY` 是客户端 API Key 对应的环境变量，设置环境变量后再启动 OMP：

```yaml
providers:
  openai-codex:
    baseUrl: "https://<原CPA地址>/omp/backend-api"
    api: openai-codex-responses
    apiKey: CPA_API_KEY
```

使用 `omp models openai-codex --json` 检查发现结果；如有旧缓存可使用 `omp models refresh`。例如带前缀模型 `account-a/gpt-5.5` 的选择器为 `openai-codex/account-a/gpt-5.5`，自定义别名 `work-codex` 的选择器为 `openai-codex/work-codex`。

## 模型与账号安全

- 不添加映射时，直接返回 CPA 的当前 Codex 格式目录；目录是注册信息，不保证账号实时可用或有剩余额度。
- 新增映射时，目标应为 CPA 中完整的模型 ID，包括账号前缀。自定义 ID 与目标 ID 相同时只覆盖元数据，不重写模型请求。
- 不同 ID 的别名会在普通推理、SSE、WebSocket 和压缩请求中映射回目标，并在支持的响应模型字段中还原别名。每次别名请求都会以客户端密钥检查 CPA 当前目录；目标不存在或不唯一时拒绝请求，不回退到其他模型。
- 别名与 CPA 现有模型 ID 冲突时，目录和该别名请求返回 `409`，需要修改别名。缺失的目标不会被加入目录，直接请求该别名返回 `404`。
- 别名和显示名**不保证账号隔离**。不同账号应使用唯一前缀，并检查 CPA 的 `routing.force-model-prefix`、凭据选择和故障转移配置；需要严格隔离时，必须在实际部署中验证失效、冷却与故障转移不会切换账号。
- 模型能力默认继承目标目录；仅覆盖已确认的上下文窗口、输入类型和推理等级。OMP 只读取部分目录字段，例如 v18.22.0 不直接使用目录里的 `max_tokens`，不能据此保证输出上限或工具能力。
- 插件不会保存管理密钥、客户端 API Key 或 OAuth 凭据，不读取 OAuth 凭据文件；但原生插件与 CPA 进程具有同等权限，应只安装可信构建。

## 从源码构建和验证

需要 Go 1.24+、对应平台的 C 编译器和 CGO。以下命令在目标平台本机构建；生成的 `.h` 是 Go 的构建副产物，部署只需要动态库。

```sh
mkdir -p dist
go test ./internal/adapter
CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared \
  -o dist/cpa-omp-adapter.so ./cmd/cpa-omp-adapter
```

Windows PowerShell（已安装可供 Go 使用的 GCC）：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = '1'
go build -buildvcs=false -buildmode=c-shared -o dist/cpa-omp-adapter.dll ./cmd/cpa-omp-adapter
```

动态库需与 CPA 的操作系统和架构一致。当前 Windows amd64 构建已在 CPA v8.0.76、Caddy 和 OMP v18.22.0 的隔离环境验证；两个上游账号使用本地模拟服务，并非真实 OAuth 账号。Linux amd64 构建已交叉编译，尚未在 Linux CPA 运行时验证。