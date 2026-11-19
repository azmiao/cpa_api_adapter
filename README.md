# CPA API Adapter

CLIProxyAPI（CPA）原生插件：按路由和客户端 Key 分发模型，提供 OpenAI `v1` 与 Codex `backend-api` 接口。

## 安装

1. 在 CPA 插件商店添加源，安装 `cpa-api-adapter`：

   ```text
   https://raw.githubusercontent.com/azmiao/cpa_api_adapter/main/registry.json
   ```

   也可从 [Releases](https://github.com/azmiao/cpa_api_adapter/releases) 下载对应平台 ZIP，将动态库放入 CPA 插件目录。
2. 合并以下配置，重启 CPA：

   ```yaml
   plugins:
     enabled: true
     dir: plugins
     configs:
       cpa-api-adapter:
         enabled: true
   ```

3. 打开「API 适配器」，用 **CPA 管理密钥**登录。新增 API 后「保存全部」，在「设置」确认插件已就绪。

## 使用场景

API 以 **路由前缀 + 出口格式 + API Key** 联合唯一；仅三项全部相同才拒绝保存。
API Key 填 **CPA 已有的客户端 Key**，不是管理密钥。CPA 删除 Key 后，插件配置仍保留，但该 Key 会失效。

### 1. 一个 CPA，向不同 Key 分发不同模型

在 CPA 创建客户端 Key，再在插件中为各 Key 新增 API：

| API | 路由前缀 | 出口格式 | API Key | 暴露模型 |
| --- | --- | --- | --- | --- |
| 分发 A | /api | v1 | CPA_KEY_A | model-a |
| 分发 B | /api | v1 | CPA_KEY_B | model-b |

- 「CPA 内部地址」填写 CPA 本身的地址，通常为 `http://127.0.0.1:8317`。
- 在「模型」中获取目录、添加需要暴露的模型；对外 ID 可与 CPA 目标模型相同，也可设置别名。
- 客户端共用 `https://api.example.com/api/v1`，使用各自的 Key，得到各自的模型列表。

**不添加模型表示开放全部；添加后只开放所选模型。** 此限制仅作用于插件路由，需自行限制绕过插件直接访问 CPA 原有接口。

### 2. 为 oh-my-pi 补齐 backend-api 接入

适合通过 CPA 使用 Codex / Responses 模型，并覆盖 oh-my-pi 的 `openai-codex` 提供商。插件提供 Codex 风格模型目录、模型别名与能力配置，以及 HTTP / SSE / WebSocket Responses 和 compact 转发。

1. 新增 API：路由前缀填写 `/codex`，出口格式选择 `backend-api`，填入 CPA 已有的 Key 和内部地址。
2. 获取并选择模型；按需设置显示名称、上下文窗口、输入类型和推理等级，保存。
3. 在 `~/.omp/agent/models.yml` 合并：

   ```yaml
   providers:
     openai-codex:
       baseUrl: "https://api.example.com/codex/backend-api"
       api: openai-codex-responses
       apiKey: CPA_API_KEY
   ```

将 `CPA_API_KEY` 环境变量设为对应客户端 Key。运行 `omp models openai-codex --json` 查看目录；需要刷新时运行 `omp models refresh`。

上游仍须支持相应模型及 Responses 接口；插件不将 Chat Completions 转换为 Responses，也不会使模型获得本身不支持的能力。

## 部署场景

插件运行在 CPA 进程内，所有 API 共用一个监听端点。**CPA 内部地址指向 CPA 原有端口，不是插件监听端口。** 以下反代路径对应上面的 `/api/v1` 与 `/codex/backend-api`，请按实际配置调整。

### 1. 宿主机直接部署

有本机 Caddy / Nginx 时，推荐 Unix：

```yaml
listen_mode: unix
socket_path: /run/cpa-api/adapter.sock
socket_mode: "0660"
```

合并到 `plugins.configs.cpa-api-adapter`，或在 WebUI「设置」填写。先创建 `/run/cpa-api`，确保 CPA 可写、反代可访问；`0660` 套接字需要共同用户组。

在「接入」选择 Caddy / Nginx，复制配置到现有 CPA 站点并重载，保留原有 CPA 转发。Windows 使用绝对套接字路径，反代可选 Caddy。

没有本机反代时，可选择 TCP、监听 `127.0.0.1:18318`，本机客户端直接使用 `http://127.0.0.1:18318/api/v1`。需要其他主机访问时绑定可达的内网 IP，并限制访问来源。

### 2. Docker 部署，反代也在本机

CPA 与 Caddy 等反代位于同一宿主机时，用共享目录连接 Unix 套接字，**无需发布插件 TCP 端口**。

1. 创建 `./cpa-api-socket`，设置目录权限，使 CPA 容器可写、反代容器可访问；`0660` 套接字需两侧进程具有共同用户组。
2. 合并到现有 Compose：

   ```yaml
   services:
     cli-proxy-api:
       volumes:
         - ./cpa-api-socket:/run/cpa-api
     caddy:
       volumes:
         - ./cpa-api-socket:/run/cpa-api:ro
   ```

3. 插件选择 Unix，路径 `/run/cpa-api/adapter.sock`，权限 `0660`；CPA 内部地址通常仍为 `http://127.0.0.1:8317`。
4. 在已有 Caddy 站点块中新增：

   ```caddyfile
   @adapter path /api/v1/* /codex/backend-api/*
   handle @adapter {
       reverse_proxy unix//run/cpa-api/adapter.sock
   }
   ```

保留原有 CPA 转发，应用 Compose 挂载并重载反代。Nginx 用户可直接复制 WebUI 的 Unix 配置。

### 3. Docker 部署，反代在另一台服务器

假设 CPA 宿主机内网 IP 为 `10.0.0.10`。不同主机不能共享 Unix 套接字，改用 TCP：

1. 插件「设置」选择 TCP，监听 `0.0.0.0:18318`。普通 Docker 桥接网络不能直接绑定宿主机 IP。
2. 在 CPA 容器的现有 Compose 中追加端口发布，保留原有端口：

   ```yaml
   services:
     cli-proxy-api:
       ports:
         - "10.0.0.10:18318:18318"
   ```

3. 远端反代连接 **CPA 宿主机 IP:发布端口**，不是 `127.0.0.1` 或 `0.0.0.0`。在远端 Caddy 的已有站点块中新增：

   ```caddyfile
   @adapter path /api/v1/* /codex/backend-api/*
   handle @adapter {
       reverse_proxy 10.0.0.10:18318
   }
   ```

   Nginx 的 `proxy_pass` 使用 `http://10.0.0.10:18318`，不附加 URI；保留认证头、WebSocket 升级，并关闭 SSE 缓冲和缓存。
4. 应用端口发布和反代配置，防火墙只允许反代服务器访问该端口。TCP 提供明文 HTTP；不可信网络应使用 VPN 等加密链路。

客户端使用反代的公网 HTTPS 地址，不使用容器监听地址。TCP 模式不提供一键反代配置；远端反代由用户自行配置。

## 接入与检查

- Base URL 自动生成：Unix 使用管理页面 Origin；TCP 使用监听地址。通配绑定使用页面主机名预览，容器映射端口、域名或 HTTPS 不同时，在客户端自行填写实际入口。
- 保存后未就绪：检查端口占用、套接字目录及权限；修正后再次保存。请求体上限默认 32 MB，反代限制不应更小。
- 返回 401：检查客户端 Key 是否仍存在于 CPA、是否匹配当前路由，以及反代是否保留认证头。管理密钥只用于登录页面，保存在页面内存中。
- 从旧版升级：`v0.4.1` 加载时自动移除 `apis` 中废弃的 `public_base_url`，无需先手动改配置；WebUI「保存全部」后写回干净配置。旧全局上游和映射仍需迁入 `apis`。

## 构建

需要 Go 1.24+ 和目标平台 C 编译器：

```sh
go test ./internal/adapter
CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared \
  -o dist/cpa-api-adapter.so ./cmd/cpa-api-adapter
python scripts/smoke_plugin.py dist/cpa-api-adapter.so 0.4.1
```

Windows / macOS 使用对应动态库扩展名；Release 工作流负责多平台构建与打包。

