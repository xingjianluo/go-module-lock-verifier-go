# 机器可读输出（-json）

`lock` / `verify` / `audit` 加 `-json` 后，stdout 输出一份结构化 JSON；
人类可读信息（差异报告、VERIFY OK、错误列表）改走 stderr，stdout 可被
`jq` 等工具直接消费。

## 稳定性保证

- 字段顺序固定（结构体声明顺序）；`modules` 按 `path` 排序，`issues` 按
  `(category, kind, module, expected, actual)` 排序，`sources` /
  `introduced_by` 排序去重；
- 同一份输入反复运行输出字节一致；并发运行不互相影响结论；
- 失败也是结构化输出：脚本用 `category` + `kind` 分支，不需要解析文本。

## 顶层字段

| 字段              | 类型     | 含义                                             |
|-------------------|----------|--------------------------------------------------|
| `command`         | string   | 产生结果的子命令：`lock` / `verify` / `audit`    |
| `ok`              | bool     | 无任何 issue 时为 true                           |
| `subject`         | string   | 作用对象：module path 或工作区 name              |
| `modules`         | array    | 审计类命令的逐模块结果（见下）                   |
| `modules_written` | int      | `lock` 成功时锁定的模块数                        |
| `lock_path`       | string   | 被写入/被审计的锁文件路径                        |
| `issues`          | array    | 全部结构化发现；`ok=true` 时为空                 |

## modules 条目

| 字段             | 含义                                       |
|------------------|--------------------------------------------|
| `path`           | 模块路径                                   |
| `status`         | `ok` / `ok-local-replace` / `unlocked` / `extra-locked` / `version-mismatch` / `hash-mismatch` / `missing` |
| `locked_version` / `actual_version` | 锁定 / 实际解析版本          |
| `locked_hash` / `actual_hash`       | 锁定 / 实际内容哈希          |
| `introduced_by`  | 引入链（工作区下顶层条目是子模块 path）    |

## issues 条目

| 字段       | 含义                                                   |
|------------|--------------------------------------------------------|
| `category` | `manifest` / `workspace` / `lock` / `cache` / `internal` |
| `kind`     | 可判定的具体原因（见下表）                             |
| `module`   | 涉及的模块/依赖（可为空）                              |
| `expected` / `actual` | 期望值 / 实际值（可为空）                     |
| `sources`  | 引入方或冲突各方（如 `example.com/mod-a: require ...`）|
| `detail`   | 人类可读补充说明                                       |

## 错误分类语义

| category    | kind                                   | 含义与建议动作                                   |
|-------------|----------------------------------------|--------------------------------------------------|
| `manifest`  | `malformed_version`、`missing_path`、`missing_version`、`incomplete_entry`、`bad_directive`、`duplicate_replace`、`replace_conflict`、`exclude_without_version` | go.mod/工作区文件条目非法；修正声明后重跑        |
| `workspace` | `requirement_conflict`                 | 子模块对同一依赖要求不同版本；需人工对齐         |
| `workspace` | `replace_conflict`                     | 子模块 replace 目标互相打架；需人工对齐          |
| `workspace` | `duplicate_module`                     | 两个 use 条目是同一 module path                  |
| `workspace` | `module_unreadable`                    | use 指向的 go.mod 缺失或被拒                     |
| `lock`      | `lock_stale`                           | 声明变了锁没更新；重新 `lock` 即可               |
| `lock`      | `lock_tampered`                        | 锁在生成后被外部改动（integrity 不符）；不要重锁，先查改动来源 |
| `lock`      | `lock_malformed` / `lock_version_unsupported` | 锁无法解析 / 格式版本不认识               |
| `cache`     | `missing_module`                       | 本地缓存缺所需模块；先离线填充缓存               |
| `cache`     | `hash_mismatch`                        | 缓存内容与锁记录不符；内容被替换过               |
| `cache`     | `version_conflict`                     | 所有被请求版本都被 exclude                       |
| `internal`  | `internal_error`                       | 未分类错误（I/O 等），退出码 2                   |

## 退出码

- `0`：通过（`ok: true`）
- `1`：校验/处理失败，`issues` 中有结构化原因
- `2`：用法错误或在分类之前发生的 I/O 失败

## 原子性与并发

- `lock` 只在全部解析成功后才写锁：先写同目录 `.golock-*.tmp` 临时文
  件、fsync、再原子 rename；任何一步失败临时文件被清理，磁盘上不会留
  下半成品或残缺锁；
- `verify` / `audit` 对锁与缓存只读；
- 多个进程同时跑（哪怕指向同一份缓存、同一把锁）不会互相踩到中间状
  态：读到的一定是某个完整版本，结论不因并发抖动。
