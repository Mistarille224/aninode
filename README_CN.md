# aninode

[English](README.md) | 简体中文

aninode 自动下载和整理动画、剧集与电影，为 Emby、Plex 等媒体服务器生成媒体库目录。它连接已有的下载器，通过硬链接整理文件，保留源文件供下载器继续做种。

支持 qBittorrent、Transmission 和 aria2；内容来源包括 Mikan、DMHY、Nyaa，也可以接入自定义 RSS 和搜索。Web 界面可以管理作品、发现资源、补齐缺集，以及迁移已有下载任务。不配置内容来源时，也可以只整理本地文件。

## 部署

运行环境为 Linux，推荐使用仓库提供的 Docker Compose 配置。下载目录和媒体库必须位于同一文件系统，并通过一个共同的父目录挂载到容器；跨盘无法建立硬链接，程序不会改用复制。

在仓库目录中准备配置：

```sh
cp .env.example .env
mkdir -p config
```

编辑 `.env`：

- `CONFIG_ROOT`：持久化配置目录，默认 `./config`。
- `MEDIA_ROOT`：宿主机的媒体目录，例如 `/srv/media`。
- `PUID`、`PGID`：运行用户的 UID 和 GID，该用户需要有媒体目录的读写权限。
- `UMASK`：aninode 新建文件采用的权限掩码，默认 `022`；共享组需要写权限的 NAS 可使用 `002`。
- `ANINODE_BIND`：宿主机发布地址，默认 `127.0.0.1`（仅本机/反向代理可访问）。
- `ANINODE_PORT`：Web 端口，默认 `7391`。

直接启动容器即可。每次启动时，aninode 都会以 `PUID:PGID` 执行媒体 preflight。对于已经存在的源/目标目录，会检查两边可读写，并实际创建一次临时硬链接后立即清理；**preflight 不再创建缺失目录，也不会因为首次配置的路径尚不存在而阻止 WebUI 启动**，这样可以先进入设置修正挂载路径。已经存在但权限错误或跨文件系统的路径仍会在服务启动前直接报错：

```sh
docker compose up -d
docker compose logs -f aninode
```

Compose 默认只将 Web 端口绑定到宿主机 `127.0.0.1`。在 Docker 宿主机上打开 `http://localhost:7391`，或在它前面配置 HTTPS 反向代理。首次登录前，先在宿主机获取 10 分钟有效的一次性初始化码：

```sh
docker compose exec aninode aninode setup-code
```

在 WebUI 中同时填写初始化码和至少 8 个字符的管理员密码。初始化码过期后再次执行上面的命令会生成新码；设置成功后初始化码立即删除。以后只使用普通登录密码。首次启动还会创建基础整理配置，在「设置」中填写下载器地址并启用需要的内容来源。

如果只在可信局域网内直接访问，可将 `ANINODE_BIND` 改为 `0.0.0.0` 或指定宿主机局域网地址。不要让明文 HTTP 穿过不可信网络；远程访问请使用[容器部署说明](docs/CONTAINER_DATA.md#https-reverse-proxy)中的 HTTPS 反代方案。

下载器密码或 RPC 密钥直接在 WebUI 的下载器设置中输入，保存后立即生效。所有敏感数据统一位于 `/config/secrets`：下载器 JSON 不再保存明文、密文或密钥引用，同一套 secret store 也预留给未来的 TMDB token 等集成密钥。界面只显示是否已设置；留空保留原值，输入新值覆盖，勾选清除则移除密码。无需额外挂载密钥目录或重启容器。下载器地址必须能从 aninode 容器内访问；下载器在另一个容器时，`localhost` 通常不是它的地址。下载器 URL 不允许写成 `http://user:password@host`，用户名和密码必须使用独立字段。

目录挂载、权限、环境变量和备份见[容器部署说明](docs/CONTAINER_DATA.md)。

## 使用

默认目录结构如下，其中 `/media` 对应 `.env` 中的 `MEDIA_ROOT`：

```text
/media/
├── downloads/
│   ├── TV/
│   │   └── Example (2026)/
│   │       ├── .aninode.json
│   │       └── Season 01/
│   │           └── Example S01E01.mkv
│   └── Movies/
│       └── Example Movie (2026)/
│           └── Example Movie.mkv
└── library/
    ├── TV/
    └── Movies/
```

**整理本地文件：** 按上述结构放入文件。每部剧集使用独立目录，其下再放一层文件夹存放媒体；电影放在各自的作品目录中。aninode 会创建 `.aninode.json`，记录标题和整理规则，再将已完成、可识别的媒体硬链接到 `library`。剧集一级文件夹会自动分配目标季，识别错误时可在作品设置中调整季数和集数偏移。

**追更和补档：** 在「发现」中浏览 RSS 或搜索资源，选择作品并下载。作品设置可限制字幕组、分辨率和字幕类型。程序可以补齐已知范围内的缺集；需要指定季末集数时，使用手动范围补全。

**接管已有任务：** 使用「迁移」查看下载器中的任务，确认作品和目录后执行。迁移可能让下载器移动数据，执行前应检查目标路径。

**接入媒体服务器：** 将 `library/TV` 和 `library/Movies` 分别添加到 Emby 或 Plex 的剧集库和电影库。

本地文件默认每分钟检查一次，RSS 默认每 15 分钟检查一次。手动刷新 RSS 只更新发现列表；接受作品或执行自动整理才会触发相应下载和整理操作。

硬链接两端共享同一份文件内容，直接修改媒体内容会影响两端。调整标题或季集映射后，程序会更新媒体库路径并清理同一文件的旧链接；源文件保留。目标路径已有不同文件时会报告冲突，不会覆盖。

下载器应保留未完成标记：qBittorrent 的 `.!qB`、Transmission 的 `.part` 或 aria2 的 `.aria2` 文件，以免未完成文件被当作成品整理。程序不跟随符号链接。

## 配置与命令行

全局配置在 `config/`，每部作品的规则在源目录中的 `.aninode.json`。字段和示例见[配置参考](config.example/README_CN.md)。

| 命令 | 用途 |
|---|---|
| `aninode init` | 创建缺失的基础配置 |
| `aninode preflight` | 检查已存在媒体目录的权限和硬链接能力，不创建缺失目录 |
| `aninode serve` | 启动 Web 界面和定时任务 |
| `aninode check` | 校验配置和作品声明 |
| `aninode run` | 执行一轮下载与整理 |
| `aninode migrate` | 预览已有任务的迁移方案 |
| `aninode version` | 显示版本信息 |

配置目录默认是 `/config`，可用 `--config-dir` 指定。通过 `aninode <命令> --help` 查看参数。`migrate --apply` 需要用 `--group` 指定预览结果中的确认键。

## 开发

后端使用 Go，前端是嵌入二进制的 HTML、CSS 和 JavaScript，无需前端打包。Go 版本要求见 [go.mod](go.mod)，前端检查需要 Node.js 和 npm。

```sh
go build -o aninode ./cmd/aninode
go test ./...
npm --prefix internal/server/web test
npm --prefix internal/server/web run check
```

在 Linux 上验证文件系统操作；并发相关改动可运行 `go test -race ./...`。本地容器构建使用：

```sh
docker compose -f compose.yaml -f compose.dev.yaml up -d --build
```

主要代码位于 `internal/application`（业务流程）、`internal/organizer`（文件整理）、`internal/download`（下载器适配）、`internal/provider`（内容来源）和 `internal/server`（HTTP 与界面）。修改前可查阅[文件系统模型](docs/FILESYSTEM_MODEL.md)和[界面维护说明](internal/server/web/DESIGN.md)。

## 许可证

[Apache-2.0](LICENSE)
