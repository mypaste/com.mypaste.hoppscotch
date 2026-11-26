# Hoppscotch 本地

MyPaste extension `com.mypaste.hoppscotch` (version `1.1.0`).

设置窗口的一级菜单「Hoppscotch」，一个只在你自己机器上收发请求的 API 工作台。左边是工作区：方法、地址、查询参数、请求头、请求体（JSON / 表单 / 文本）与 Bearer、Basic 认证各占一页，集合可以建文件夹、收藏示例、把一次发送变成一条请求；右边是响应：状态、耗时、大小、响应头、正文，JSON 直接美化，代码与文档（HTML、SVG、图片、PDF 等）就显示在这一页，不再另开窗口，响应可以切到预览，也可以展开放大到整个工作区；「代码」把一次请求写成 cURL、Python、Go、Node.js 或 PHP，「文档」把请求、集合或整个工作区写成 Markdown，默认按排版预览，也能切回源文。失败时页面逐项说明失败发生在哪一步、哪个地址、什么原因。窗口窄成侧边栏时左侧收进抽屉，右侧独占整页。环境变量写成 `{{变量}}`，发送前替换。

集合和历史的入口都在「导入 / 导出」：「导入」接受 Postman 集合并保留分组与变量，也接受「导出」复制出来的工作区备份；「导出」把全部集合、环境与历史拼成一份标记 `mypaste.hoppscotch.workspace` 的 JSON 复制到剪贴板（超过宿主一行的长度上限时先丢最旧的历史），再导入时按 ID 并回当前工作区，已有的不动、缺的补上，导入两次不会多出第二份。页面先是空白的，然后向自己的进程要回上次保存的工作区；在答复回来之前做的改动会和答复并合，不会因为载入而丢掉。

请求只由扩展自己的进程发出（本机直连，不经宿主的网络代理），WebSocket 一次调用走完连接、发送、收集与关闭，把整段记录带回来。工作区只保存在扩展的私有存储里，卸载即删除；没有账号、没有云端同步，也不连接 Hoppscotch 的任何服务。`entity.read` 是宿主要求菜单页文本输入必须声明的能力，扩展只读写自己的存储，不读取宿主的历史。

The Hoppscotch tab of the settings window is a local-only API workbench. The left half builds the request — method, URL, query parameters, headers, body, and Bearer / Basic authentication, one page each — over a tree of collections with folders, saved examples, and history; the right half shows the response's status, time, size, headers and body, with JSON prettified, code and documents (HTML, SVG, images, PDF and the like) drawn inline on the page instead of in a separate window, a preview tab, and an expand action that gives the response the whole workspace. 代码 writes a request as cURL, Python, Go, Node.js or PHP; 文档 writes the request, a collection or the whole workspace as Markdown, shown as a rendered preview by default with a tab back to the source. A failed request gets a card that names the failing step, the address it dialled, and the reason, line by line. Below 760 points the workspace folds into a drawer and the response takes the frame.

Import / export sit in the same row. 导入 takes a Postman collection, keeping its groups and variables, and also takes back the workspace backup that 导出 produces: one JSON document marked `mypaste.hoppscotch.workspace` holding every collection, environment and history entry, copied to the clipboard (the oldest history goes first when it would exceed the host's one-line limit). Restoring merges by ID over the current workspace — existing items are left alone, missing ones are added, and importing the same backup twice adds nothing. The page opens blank and asks its own process for the saved workspace; any edit made before the answer arrives is merged with it rather than overwritten.

The compiled runtime is the only thing that dials a URL and the only thing that touches the saved document: one process per `hoppscotch.call`, one invocation line in and one answer line out. A WebSocket call is one whole session — connect, send, collect, close — with the transcript coming back in that single answer. The workspace lives in the extension's private storage, keyed by package ID, and goes away with the package. There is no account, no cloud sync, and no connection to any Hoppscotch service. `entity.read` is declared because the host requires it for text input piped from a menu page; the extension only reads and writes its own storage and never the host's history.

This repository is the extension's own source home, split out of the former `mypaste-code-extensions` monorepo. It contains only this extension's tracked source files. The published `.mypaste-plugin` archives and the marketplace index live in [`mypaste-extensions-catalog`](https://github.com/mypaste/mypaste-extensions-catalog).

The manifest speaks MyPaste plugin protocol v3. Installation, capability mediation, and content-digest verification are performed by the MyPaste desktop host.

## Files

- `mypaste.plugin.json`: package identity, capabilities (`entity.read`, `storage.plugin`), the menu page, and the internal `hoppscotch.call` command.
- `panel.html`: the settings menu page — the request builder, the response pane, the collections and environment tree, and the workspace backup, calling `hoppscotch.call`.
- `src/`: the extension's own Go process.
  - `internal/api`: one HTTP exchange — URL building, timeout, redirects, the response body with images carried as base64 and its size caps.
  - `internal/ws`: one WebSocket session — connect, send, collect frames, close.
  - `internal/state`: validation of the saved workspace document before it is read or written.
  - `internal/hook`: the host line protocol, including `storage.plugin` reads and writes.
  - `cmd/mypaste-hoppscotch`: dispatch of `send` / `ws` / `load` / `save`.

## Build

```bash
cd src && CGO_ENABLED=0 go build -trimpath ./cmd/mypaste-hoppscotch
```

The released archive carries one binary per platform under `bin/<goos>/<goarch>/` (darwin arm64 and amd64) plus the manifest and the page.

## License

MIT. See [LICENSE](LICENSE).
