# 配置参考

[English](README.md) | 简体中文

通常可以直接在 Web 界面修改配置。手动配置时，可参考本目录的 JSON 文件；示例下载器地址和 Generic 来源地址需要替换，示例内容来源默认关闭。

`aninode init` 创建缺失的基础配置，`run` 和 `serve` 启动时也会补齐基础结构。配置使用严格 JSON，不支持注释或未知字段。

```text
config/
├── organizer.json
├── clients/
│   └── <id>.json
└── sources/
    └── <id>.json
```

## 媒体目录：`organizer.json`

```json
{
  "source": "/media/downloads",
  "target": "/media/library",
  "extensions": [".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm"]
}
```

| 字段 | 说明 |
|---|---|
| `source` | 下载根目录的绝对路径，其下使用 `TV/` 和 `Movies/` |
| `target` | 媒体库根目录的绝对路径，其下使用 `TV/` 和 `Movies/` |
| `extensions` | 作为媒体识别的扩展名，可带或不带开头的点 |

两个目录不能重叠，必须位于同一个非根目录下，并支持相互建立硬链接。容器中使用一个 `/media` 挂载，详见[容器部署说明](../docs/CONTAINER_DATA.md)。

## 下载器：`clients/*.json`

最多允许一个下载器配置文件。文件名必须与 `id` 一致，例如 `clients/qbittorrent.json`：

```json
{
  "id": "qbittorrent",
  "type": "qbittorrent",
  "url": "http://qbittorrent:8080",
  "username": "admin",
  "path_mappings": [
    {"remote": "/downloads", "local": "/media/downloads"}
  ],
  "enabled": true
}
```

| 字段 | 说明 |
|---|---|
| `id` | 配置名称，与文件名一致 |
| `type` | `qbittorrent`、`transmission` 或 `aria2` |
| `url` | 下载器的 HTTP(S) 地址或 RPC 端点 |
| `username` | qBittorrent 或 Transmission 用户名 |
| `path_mappings` | 将下载器看到的 `remote` 路径转换为 aninode 看到的 `local` 绝对路径 |
| `enabled` | 是否启用 |

启用的下载器供所有作品使用。密码或 RPC 密钥通过 WebUI 设置和覆盖；下载器 JSON 不保存任何密钥材料，可恢复的集成密钥统一加密存放在 `/config/secrets/integrations/`。

下载器应保留未完成文件标记：`.!qB`、`.part` 或相邻的 `.aria2` 文件。没有这些标记时，尚未完成的文件可能被当作可整理媒体。

## 内容来源：`sources/*.json`

文件名必须与 `id` 一致。内置来源使用与 `provider` 同名的 ID：`mikan`、`dmhy` 或 `nyaa`。

```json
{
  "id": "mikan",
  "provider": "mikan",
  "enabled": true,
  "priority": 5
}
```

| 字段 | 说明 |
|---|---|
| `id` | 来源名称，与文件名一致 |
| `provider` | `mikan`、`dmhy`、`nyaa` 或 `generic` |
| `rss` | RSS 地址列表，每项包含 `url` 和可选的 `name` |
| `search` | 仅 Generic 可用，包含 `url_template` |
| `priority` | 多个发布对应同一集时，数值小的优先 |
| `enabled` | 是否启用 |

内置来源自带 RSS 和搜索地址，也可用 `rss` 替换默认订阅。Generic 可配置 RSS、搜索或两者：

```json
{
  "id": "custom",
  "provider": "generic",
  "rss": [{"url": "https://example.com/anime.xml"}],
  "search": {"url_template": "https://example.com/search?q={query}"},
  "enabled": false,
  "priority": 50
}
```

`url_template` 必须且只能包含一个 `{query}`。上例中的地址是占位符，需要替换。搜索返回格式需能被 Generic 来源解析器识别，不能将任意网站页面直接作为搜索接口。

## 作品规则：`.aninode.json`

规则放在源文件旁边，不放在 `config/`：

- 剧集：`<source>/TV/<作品目录>/.aninode.json`
- 电影：`<source>/Movies/<作品目录>/.aninode.json`

剧集各季共用作品根目录的一个文件。示例：

```json
{
  "title": "Example",
  "year": 2026,
  "sources": ["mikan"],
  "filters": {"groups": ["ANi"]},
  "folders": {
    "Season 01": {"season": 1}
  }
}
```

| 字段 | 说明 |
|---|---|
| `title` | 作品标题 |
| `year` | 可选年份 |
| `enabled` | 默认启用，`false` 关闭该作品的自动管理 |
| `sources` | 使用的来源 ID；省略或留空表示所有已启用来源 |
| `filters` | `groups`、`resolutions`、`subtitles` 三类允许列表 |
| `output.title` | 媒体库中的标题，省略时使用作品标题 |
| `blacklist` | 排除的源文件名或相对路径，使用不区分大小写的 glob 匹配 |
| `folders` | 剧集一级文件夹对应的目标季和集数偏移 |
| `specials` | 无法从文件名识别季集时，手动指定季集 |
| `movie.classify` | 电影文件的分类覆盖，键为源目录内的相对路径 |

`filters` 的每一项独立生效：省略或空数组表示不限，非空数组表示只接受列出的值。程序不会把已有文件的属性自动保存为筛选条件。

### 修正季数和集数

```json
{
  "title": "Example",
  "folders": {
    "Batch A": {"season": 2, "episode_offset": -10}
  },
  "specials": [
    {"source": "OVA.mkv", "season": 0, "episode": 1}
  ]
}
```

`folders` 的键是作品目录下真实的一级文件夹名。`season` 为目标季（0–99），目标集数为识别集数加上 `episode_offset`。上述 `Batch A` 中的第 13 集会整理为第二季第 3 集。`specials.source` 必须精确匹配文件名。

这些规则调整媒体库路径，不移动源文件。程序会为新增文件夹补上推测的映射，并清理已不存在文件夹的规则；推测不准确时可手动修改。

### 电影分类

`movie.classify` 可选值为 `version`、`extra`、`trailer`、`interview`、`featurette`、`deleted_scene`、`behind_the_scenes`、`exclude` 和 `auto`。例如：

```json
{
  "title": "Example Movie",
  "movie": {
    "classify": {"preview.mkv": "trailer", "sample.mkv": "exclude"}
  }
}
```

## 校验

```sh
aninode check --config-dir ./config
```

此命令读取配置和作品声明，校验字段与引用，并输出数量摘要，不修改配置。它不验证下载器账号是否能登录；连接状态可在 Web 界面查看。
