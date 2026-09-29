# 锁文件格式 `go.lock`（gmlv1）

`go.lock` 是 UTF-8、两空格缩进的 JSON。条目（`entries`）始终按 `path` 字典序排列；
对同一份声明，序列化结果**逐字节确定**。

## 顶层字段

| 字段 | 含义 |
|---|---|
| `format_version` | 格式版本，当前固定为 `"gmlv1"`。其它值判为 `lock malformed`。 |
| `main_module` | 主模块路径（来自 `go.mod` 的 `module` 指令）。 |
| `manifest_hash` | 归一化后依赖声明的 SHA-256（`sha256:<hex>`）。与源码排版、注释、指令书写顺序无关，只取决于声明内容。用于判定 **stale**（声明变了没重锁）。 |
| `entries[]` | 每个生效依赖一条，见下。 |
| `lock_hash` | 对除自身外所有字段的一段**规范化文本**计算的 SHA-256。与 JSON 空白/字段顺序无关。用于判定锁文件本身是否被改动（**tampered**）。 |

## `entries[]` 字段

| 字段 | 含义 |
|---|---|
| `path` | 依赖的逻辑模块路径（`go.mod` 里出现的那个名字）。 |
| `version` | 最终生效版本。模块替换时是替换目标的版本；本地目录替换时保留被替换声明里的版本。 |
| `direct` | 是否为直接依赖（无 `// indirect`）。 |
| `replaced` | 该依赖是否命中了某条 `replace` 规则。 |
| `replace_source` | 命中的替换规则标识 `"oldpath@oldversion"`；`oldversion` 为空表示是“全版本替换”规则。 |
| `replace_path` | 当 `replace` 指向**另一个模块路径**时，实际从缓存读取的生效模块路径（同路径版本钉扎时留空）。 |
| `local_dir` | 本地目录替换时的**绝对路径**；否则留空。 |
| `go_mod_digest` | 该依赖 `go.mod` 字节内容的 `sha256:<hex>`。本地替换则是该目录下 `go.mod`。 |
| `tree_digest` | 该依赖解包目录树的稳定摘要 `sha256:<hex>`（见下算法）。 |
| `introduced_by` | 引入链：从主模块开始、逐跳 `"path@version"`（本地替换节点为 `"local:<absdir>"`）。用于差异报告回答“是谁引入的”。顺序有意义，不排序。 |

## 摘要算法

- `go_mod_digest`：`sha256(go.mod 原始字节)`。
- `tree_digest`：递归遍历目录，按相对路径（POSIX 斜杠）字典序；对每个条目写入
  `相对路径 \0 <文件 sha256 或标记> \0`，最后对整段做 SHA-256。
  - 普通文件：文件内容的 `sha256:<hex>`；
  - 符号链接：标记 `symlink`（不跟随目标）；
  - 其它特殊文件：标记 `special`；目录本身不产生条目。

这两个摘要**不是** go 官方的 ziphash / go.sum 哈希；它们是本工具自定义、可离线复算
的内容指纹，只用于检测“被锁定的内容是否发生变化”。

## 完整性哈希 `lock_hash`

为避免 JSON 排版差异影响判定，哈希不直接覆盖 JSON 文本，而是覆盖一段规范化文本：
每个字符串以“长度前缀 + 值”逐行写出，条目先按 `path` 排序，最后写入条目数。
改动任意字段而不重算 `lock_hash`，`verify` 会判为 **lock tampered**。

> 注意：如果有人既改了字段又**重新正确计算**了 `lock_hash`，自哈希会保持一致。
> 此时结构字段（version/direct/replace/presence）仍会与当前声明比对并判为
> tampered；而被改动的**摘要值**与缓存实际内容不符会判为 **checksum mismatch**。
> 即：声明哈希管“声明是否漂移”，结构比对管“锁是否与声明一致”，摘要比对管
> “锁记录的内容是否与缓存一致”，三道检查分工明确。

## 示例

```json
{
  "format_version": "gmlv1",
  "main_module": "example.com/main",
  "manifest_hash": "sha256:9ce129ee…",
  "entries": [
    {
      "path": "example.com/a",
      "version": "v1.0.0",
      "direct": true,
      "replaced": false,
      "go_mod_digest": "sha256:6d6de7fe…",
      "tree_digest": "sha256:f5c53083…",
      "introduced_by": ["example.com/main"]
    },
    {
      "path": "example.com/b",
      "version": "v1.0.0",
      "direct": false,
      "replaced": false,
      "go_mod_digest": "sha256:2424c6df…",
      "tree_digest": "sha256:adccc5ce…",
      "introduced_by": ["example.com/main", "example.com/a@v1.0.0"]
    }
  ],
  "lock_hash": "sha256:ee80f938…"
}
```
