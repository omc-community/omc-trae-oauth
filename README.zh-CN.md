<p align="center">
  <img src="assets/logo.png" width="96" height="96" alt="OMC Trae OAuth logo">
</p>

<h1 align="center">OMC Trae OAuth</h1>

<p align="center">
  <a href="https://github.com/WizisCool/oh-my-cpa">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-dark.svg">
      <img src="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-light.svg" alt="Oh-My-CPA" height="22">
    </picture>
  </a>
</p>

<p align="center">
  专为 Oh-My-CPA 适配，兼容
  <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI</a>
  的原生 Trae 与 Trae CN OAuth 登录
</p>

<p align="center">
  <a href="https://github.com/omc-community/omc-trae-oauth/releases"><img src="https://img.shields.io/github/v/release/omc-community/omc-trae-oauth?label=release" alt="Release"></a>
  <a href="https://github.com/omc-community/omc-trae-oauth/actions/workflows/build.yml"><img src="https://github.com/omc-community/omc-trae-oauth/actions/workflows/build.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/CLIProxyAPI-v8+-4f46e5" alt="CLIProxyAPI v8+">
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license">
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a>
</p>

OMC Trae OAuth 为 CLIProxyAPI 增加 `trae` 提供商。在面板的 OAuth 流程中用 Trae
或 Trae CN 账号登录，CPA 即可获得来自 Trae 自身 IDE 目录的模型发现、兼容 OpenAI
的执行与工具调用，以及标准化配额，无需额外进程，也无需本机安装 Trae 客户端。

本插件是 [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa) 生态的一部分；
Oh-My-CPA 是面向 CLIProxyAPI 的自托管控制面与用量控制台。

> **实验性。** 协议行为移植自 `magpie-community/plugins` 中 MIT 许可的
> `packages/trae` 插件。该插件的 Trae CN 协议由两个开源中转项目推导而来，国际站
> 协议只在一个 SG 区域的 Free 账号上验证过。本 Go 实现已通过离线 fake 与原生 ABI
> 检查，但尚未用真实 Trae 账号验证。请在依赖某个构建前用自己的账号验证登录与对话，
> 并反馈观察到的问题。

## 两个入口，一个提供商

Trae 账号可以通过两个部署登录。两者产出相同的凭证结构（`"type": "trae"`）并共用
同一个执行器，但它们是不同主机上的独立服务，因此插件注册两个 OAuth 登录入口，而不是
一个入口加一个站点下拉框。

| 入口 | 登录标识 | 登录页 | 模型主机 |
| --- | --- | --- | --- |
| Trae | `trae` | `https://www.trae.ai/authorization` | `coresg-normal.trae.ai`（SG）、`coreva-normal.trae.ai`（US） |
| Trae CN | `trae-cn` | `https://www.trae.cn/authorization` | `trae-api-cn.mchost.guru` |

在支持按入口渲染元数据的面板上（CPA v8.0.51+），每个入口显示为独立卡片，拥有自己的
名称与 Logo。加载插件需要 **CPA v8.0.49+**（schema 10 首次随该版本发布）；独立名称与
图标需要 v8.0.51+。在更旧的主机上两个标识仍然可用，面板会显示插件自身的名称与 Logo。
配额 `429` 会在 **CPA v8.0.62+** 上冷却整个凭证；更旧的主机只冷却上报的模型。

- 选择入口即选择门户。入口优先于任何 `site` 提示。
- 每个凭证以 `site` 和 `oauth_provider` 记录登录它的门户。
- `site`（默认 `trae`）只是登录请求未指定入口时的回退值，例如主机仅以插件标识启动
  登录。
- 显式但非法的 `site` 值会被拒绝，而不是静默回退。

## 浏览器登录，无需粘贴回调

Trae 的授权页完成登录后，会把浏览器重定向到 `127.0.0.1` 上的回调，携带账号的
Cloud-IDE-JWT、刷新令牌与身份信息。插件为本次流程启动该回调监听器，把收到的内容保存
在认证材料旁，由 CPA 的轮询读取。无需手动粘贴任何内容。

监听器绑定在 CPA 主机的 `127.0.0.1` 上。因此完成登录的浏览器必须运行在该主机上，
或通过端口转发访问其回环地址。远程部署的 CPA 无法从另一台机器的浏览器完成此流程，
因为 Trae 只会把回调发往回环地址。

登录所用的设备在流程开始时生成一次，并告知授权页，之后随每个请求发送。Trae 会丢弃
每次都换新设备的请求。Trae CN 的设备 ID 采用其客户端使用的 16 位数字格式；国际站保留
登录时的 19 位格式。

## 功能

- 通过 Trae 自身的浏览器登录支持 Trae 与 Trae CN，每个门户一个登录入口。
- Cloud-IDE-JWT 刷新，刷新令牌单次使用，同一凭证的并发刷新共享一次交换。Trae CN
  的续期会先按 IDE 的方式用账号的 P-256 设备密钥绑定设备，Trae 拒绝时回退到
  cloudide 交换。
- 从 IDE 的 `batch_get_detail_param` 目录发现模型，覆盖所有聊天 function，并保留
  目录给出的 `__dev`、`__max` 模型名、Max 上下文变体、推理等级与图片支持。目录读取
  失败时使用随包附带的回退列表。
- 兼容 OpenAI 的聊天补全输入与输出，支持流式与非流式、原生工具调用、以文本块写出的
  工具调用、推理内容、图片输入，以及带提示缓存计数的 token 用量。
- 标准化配额：Trae CN 的积分与国际站的美元额度。
- 按凭证的并发控制。
- 导入已有的 `"type": "trae"` 认证文件。
- 错误映射：登录失效与用量上限保留各自的状态码；Trae 拒绝聊天 function 时改用另一个
  function 重试；其余错误折叠为可读的上游失败。Trae 在用量上限上给出 `Retry-After` 时，
  限流同时带上还剩多久，CPA 因此会让凭证休息，而不是在窗口内继续重试。

## 安装

把本仓库加入插件商店源：

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-trae-oauth:
      enabled: true
```

然后在插件商店安装 `omc-trae-oauth`，在面板中打开它的 OAuth 卡片。商店与动态库名为
`omc-trae-oauth`；CPA 的凭证提供商标识仍为 `trae`。登录、配置、配额与排障见
[docs/USAGE.md](docs/USAGE.md)。

推送 `v*` 标签时，GitHub Actions 会创建发行版。

## 配置

| 字段 | 类型 | 默认值 | 取值 |
| --- | --- | --- | --- |
| `site` | enum | `trae` | 登录请求未指定入口时的回退门户：`trae`、`trae-cn` |
| `max_concurrency` | integer | `3` | 每个凭证 1–100 个并发请求 |
| `client_version` | string | `0.1.69` | 请求模型网关时声明的 TRAE SOLO 客户端版本 |
| `max_tokens` | integer | `32000` | 客户端未指定时的输出预算，1–1000000 |

## 开发

插件实现位于 [`go/`](go/)。下载模块后，测试无需网络：

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
```

为当前机器构建原生库：

```sh
cd go
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-trae-oauth.so .
```

macOS 使用 `.dylib`，Windows 使用 `.dll`。`build/` 已被 git 忽略。

发行工作流会从标签构建以下产物：

```text
omc-trae-oauth_<version>_linux_amd64.zip
omc-trae-oauth_<version>_linux_arm64.zip
omc-trae-oauth_<version>_darwin_amd64.zip
omc-trae-oauth_<version>_darwin_arm64.zip
omc-trae-oauth_<version>_windows_amd64.zip
omc-trae-oauth_<version>_windows_arm64.zip
checksums.txt
omc-trae-oauth.omc.json
```

每个压缩包根目录包含正确命名的 `omc-trae-oauth.so`、`omc-trae-oauth.dylib` 或
`omc-trae-oauth.dll`。`checksums.txt` 覆盖所有压缩包，插件商店在解包前会校验下载的
压缩包。`omc-trae-oauth.omc.json` 是 [docs/USAGE.md](docs/USAGE.md) 中描述的可选
说明声明；插件对外提供的字节与发布文件完全一致。

提交规范、测试要求与生态开发标准见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 项目结构

```text
assets/logo.png         插件贴纸：插件在商店中的形象
assets/trae.svg         Trae 入口的单色标志
assets/trae-color.svg   Trae CN 入口的绿色标志
go/                     Go/cgo 插件实现与测试
go/assets/              内嵌图标与 omc.json 声明
docs/USAGE.md           运维安装与使用指南
```

Go 实现是独立代码。协议来源，包括浏览器登录与设备绑定续期，记录在
[NOTICE.md](NOTICE.md)。

## 许可证

[MIT](LICENSE)
