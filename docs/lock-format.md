# 锁文件格式（go.lock）

- 格式版本：`1`
- 编码：UTF-8、纯文本、按行组织
- 默认文件名：`go.lock`

## 示例

```text
# golock v1
integrity sha256:9f1c2d6b...
format-version 1
module-name example.com/main
manifest-hash sha256:e19dd204...
module example.com/a v1.2.3 hash sha256:aaa... via example.com/main
module example.com/b v0.1.0 indirect hash sha256:bbb... via example.com/a example.com/main
module example.com/c v1.0.0 indirect replace-local ../vendor/c via example.com/b
module example.com/d v1.0.0 indirect replace-by example.com/fork v2.0.0 hash sha256:ddd... via example.com/b
```

## 字段含义

### 头

| 行                | 含义                                                                 |
|-------------------|----------------------------------------------------------------------|
| `# golock v1`     | 固定头，标识文件类型与主版本。缺失或不一致 => `lock_malformed`。     |
| `integrity sha256:<hex>` | 对除本行外其余所有行（保持行序、以换行连接）计算的 SHA-256。任何行被改动都会导致它对不上 => `lock_tampered`。 |
| `format-version 1`| 锁格式版本。非 `1` => `lock_version_unsupported`。                   |
| `module-name`     | 生成锁时主模块的 module path；与当前 manifest 不符 => `lock_stale`。 |
| `manifest-hash`   | 归一化后 `go.mod` 的 `sha256:` 哈希。声明变了它就对不上 => `lock_stale`，与 `integrity` 失败（`lock_tampered`）严格区分。 |

归一化只保留语义：指令内容排序后连接，忽略源文件中的指令书写顺序与空白。
因此等价的两份 `go.mod` 一定得到相同的 `manifest-hash`。

### module 行

```text
module <path> <version> [indirect] [hash sha256:<hex>]
       [replace-by <new-path> <new-version> | replace-local <dir>]
       [via <p1> <p2> ...]
```

| 字段            | 含义                                                                 |
|-----------------|----------------------------------------------------------------------|
| `path/version`  | 该依赖最终被选中的模块路径与版本（MVS 最大可满足版本）。             |
| `indirect`      | 主模块没有直接 require 它（仅传递引入）。                            |
| `hash`          | 模块**内容哈希**，`sha256:<hex>`。来源优先是缓存里的规范 zip；没有 zip 时回退到解压目录的确定性哈希。本地替换无此字段。 |
| `replace-by`    | 生效的版本替换：实际取的是 `<new-path>@<new-version>`，哈希也按它算。|
| `replace-local` | 生效的本地路径替换；不计算内容哈希（机器相关），状态显示为 `ok-local-replace`。 |
| `via`           | 引入链：哪些模块（或主模块）的 require 导致它进入构建列表，去重排序。用于回答“是谁引入的”。 |

锁中模块按 path 排序；`via` 列表去重排序。因此同一份输入重复生成得到字节级
一致的文件（幂等）。

## 内容哈希的确定性

- zip：按文件名排序，逐文件喂入 `file <name> mode <mode>\n` + 文件内容 +
  换行；不包含 zip 元数据或 mtime。
- 解压目录回退：按相对路径（正斜杠）排序，喂入同样的头 + 内容；不依赖
  inode、mtime、读取顺序。
- 两种模式对同一份文件树内容给出一致口径：哈希只随文件路径、权限模式、
  文件内容变化。

注意：这是本工具自洽的内容哈希（用于复现与漂移检测），并非 Go 官方
`go.sum` 的 `h1:` 哈希；二者不互通，但都能达到“内容变了必然发现”的目的。

## 兼容范围

- 只承诺解析 `format-version 1`；将来出现新版本时，旧工具返回
  `lock_version_unsupported` 而不是误读。
- 解析器对未知关键字、module 行重复、字段缺参都返回 `lock_malformed`。
- 未知的**额外整行**目前不被忽略：会导致解析失败（保守策略，避免漏审）。
- 本地路径替换随机器不同不可移植，锁中不携带其内容哈希；校验只确认替换
  规则本身与版本选择未漂移。
- 行内空格分隔；含空格的本地路径在序列化时用双引号包裹（校验器按 Go 引
  号规则解码）。

## 失败判定流程

1. 解析 manifest：任何条目被拒绝 => `*model.RejectError`，中止，不动锁。
2. 解析锁文件：
   - 头/结构损坏 => `lock_malformed`；
   - `integrity` 对不上 => `lock_tampered`（锁被动过）；
   - 格式版本未知 => `lock_version_unsupported`。
3. 比对 `manifest-hash` / module-name：不符 => `lock_stale`（声明变了，
   需要重新 `lock`）。
4. 仅用本地缓存离线解析：
   - 缺 `.mod` 或缺内容 => `missing_module`；
   - 所有被请求版本都被 exclude => `version_conflict`。
5. 逐项比对锁与解析结果：
   - 模块集合 / 版本差异 => `lock_stale`；
   - 内容哈希差异 => `hash_mismatch`（缓存内容与锁不一致）。
6. 输出差异报告：每个模块的 `status`、锁定值、实际值、`via` 引入链。

整个流程对锁文件与缓存只读；不存在任何“校验时顺手更新锁”的路径。

## 状态（报告中的 status 列）

| status              | 含义                                              |
|---------------------|---------------------------------------------------|
| `ok`                | 版本与内容哈希都一致                              |
| `ok-local-replace`  | 本地路径替换，规则一致（不校验内容哈希）          |
| `unlocked`          | 解析得到但锁里没有 => `lock_stale`                |
| `extra-locked`      | 锁里有但当前解析不需要 => `lock_stale`            |
| `version-mismatch`  | 锁版本与解析版本不同 => `lock_stale`              |
| `hash-mismatch`     | 版本相同但内容哈希不同 => `hash_mismatch`         |
| `missing`           | 缓存中找不到内容 => `missing_module`              |
