# 内置 MCP 插件（Builtins）

本目录存放项目预置的 MCP 插件清单。每个 JSON 文件描述一个 MCP server 的启动方式，可通过前端「一键导入内置 MCP」按钮或调用 `POST /api/v1/mcp/servers/import-builtins` 批量导入到数据库。

## 清单字段

| 字段          | 说明                                                                 |
|---------------|----------------------------------------------------------------------|
| `key`         | 内置唯一标识（用于导入时判重）                                       |
| `name`        | MCP 名称（写入 `mcp_server.name`）                                   |
| `description` | 描述（写入 `mcp_server.description`）                                |
| `transport`   | `stdio` / `sse` / `streamable_http`                                  |
| `command`     | stdio 启动命令                                                       |
| `args`        | 启动参数数组，支持 `${HOME}` `${PWD}` 等环境变量占位符               |
| `env`         | 环境变量 map                                                         |
| `url`         | sse / streamable_http 的服务地址                                     |
| `headers`     | sse / streamable_http 的请求头                                       |

## 当前预置

| key          | 用途                            | 需要什么运行时           |
|--------------|---------------------------------|--------------------------|
| `playwright` | 浏览器自动化（`--headed` 可视）| Node.js（`npx`）         |
| `filesystem` | 本地文件读写                    | Node.js（`npx`）         |
| `fetch`      | 抓 URL → Markdown               | Python + `uvx` / `pipx`  |
| `git`        | 本地 git 仓库操作               | Python + `uvx` / `pipx`  |

## 新增一个内置 MCP

1. 在本目录新建 `<key>.json`，字段参考上表。
2. 重启 `wf-server`（清单在启动时读入，不必重编前端）。
3. 前端 MCP 页面「一键导入内置 MCP」下拉里就会出现。
