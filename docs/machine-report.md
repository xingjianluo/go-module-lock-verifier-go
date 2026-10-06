# 机器可读输出（-json）

`lock` / `verify` / `audit` 加 `-json` 后，在 stdout 输出一份结构化
JSON 报告。字段顺序与条目顺序稳定：同一份输入反复运行输出逐字节一致，
CI 脚本可以直接 `diff` 或按字段分支。

## 顶层字段

| 字段             | 类型     | 含义                                                   |
|------------------|----------|--------------------------------------------------------|
| `schema_version` | string   | 报告模式版本，当前固定 `"1"`                           |
| `command`        | string   | `lock` / `verify` / `audit`                            |
| `ok`             | bool     | 无任何 finding 时为 `true`                             |
| `subject`        | string   | 主模块路径或工作区 `name`                              |
| `modules`        | string[] | 工作区子模块路径（排序确定）；单模块运行省略           |
| `locked_modules` | int      | 锁定/观察到的模块数                                    |
| `findings`       | array    | 结构化问题列表；无问题时为 `[]`（不是 `null`）         |

## finding 字段

| 字段            | 类型     | 含义                                                       |
|-----------------|----------|------------------------------------------------------------|
| `category`      | string   | `manifest` / `lock` / `cache` / `workspace` / `error`      |
| `kind`          | string   | 可判定的具体原因（见下表）                                 |
| `module`        | string   | 涉及的依赖模块路径（如适用）                               |
| `subject`       | string   | 工作区层面的对象（如冲突的 replace 源 `old@version`）      |
| `expected`      | string   | 期望值（锁记录的哈希/版本、合法条目等）                    |
| `actual`        | string   | 实际观察到的值                                             |
| `introduced_by` | string[] | 引入者：谁require了该模块 / 哪些子模块贡献了冲突规则       |
| `detail`        | string   | 人读补充说明                                               |

findings 按 `(category, kind, module, subject, expected, actual)` 排序，
顺序稳定。

## category 与 kind 对照

### `manifest` —— 声明条目无法受理

| kind                     | 含义                                       |
|--------------------------|--------------------------------------------|
| `malformed_version`      | 版本字符串不合法                           |
| `missing_path` / `missing_version` | 关键字段缺失                       |
| `incomplete_entry`       | 条目参数不足、缺 `=>`、块未闭合            |
| `bad_directive`          | 未知指令或无法分词                         |
| `duplicate_replace` / `replace_conflict` | 单文件内替换规则重复/冲突    |
| `exclude_without_version`| exclude 未带版本                           |

### `lock` —— 锁与声明的关系

| kind                       | 含义                                                         |
|----------------------------|--------------------------------------------------------------|
| `lock_stale`               | 声明变了锁没更新（manifest-hash 不符或模块集漂移）：重新 `lock` 即可 |
| `lock_tampered`            | 锁内容与内嵌 `integrity` 不符：锁在生成后被外部改动          |
| `lock_hash_mismatch`       | 锁内单模块哈希与同输入推导值不符（API 层分类）               |
| `lock_malformed` / `lock_version_unsupported` | 锁无法解析 / 格式版本不认识            |

`lock_stale` 与 `lock_tampered` 严格区分：前者锁文件完好但过期；后者锁
文件本身被改过。

### `cache` —— 本地缓存与锁记录的关系

| kind               | 含义                                             |
|--------------------|--------------------------------------------------|
| `missing_module`   | 缓存中缺少所需模块（`.mod` 或内容）              |
| `hash_mismatch`    | 缓存内容哈希与锁记录不一致（缓存内容被动过）     |
| `version_conflict` | 某模块所有被请求版本都被 exclude，选不出可用版本 |

### `workspace` —— 多模块合并

| kind                         | 含义                                             |
|------------------------------|--------------------------------------------------|
| `workspace_malformed`        | 工作区文件本身无法解析                           |
| `workspace_missing_module`   | `use` 目录下没有可读的 `go.mod`                  |
| `workspace_duplicate_module` | 两个子模块声明了同一个 module path               |
| `workspace_replace_conflict` | 子模块把同一 replace 源映射到不同目标；`introduced_by` 列出全部贡献者 |

## 退出码

| 退出码 | 含义                                       |
|--------|--------------------------------------------|
| 0      | 成功，无 finding                           |
| 1      | 运行完成但有 finding（报告已完整输出）     |
| 2      | 用法错误 / I/O 硬错误（可能无结构化输出）  |

## 原子性与并发保证

- `lock` 只有在解析、合并、离线解析全部成功后才写锁；写入采用
  “同目录临时文件 + fsync + rename”，任何一步失败都不会在磁盘上留下
  半成品或残缺锁，也不会留下临时文件。
- `verify` / `audit` 严格只读：不写锁、不写 manifest、不写缓存。
- 多个进程同时对同一缓存运行（哪怕指向同一把锁）互不影响：缓存只读，
  锁的替换是原子 rename，结论不会因并发而抖动。

## 示例：CI 分支判断

```bash
if golock audit -work golock.work -json > report.json; then
  echo "lock is in sync"
else
  kind=$(jq -r '.findings[0].category + "/" + .findings[0].kind' report.json)
  case "$kind" in
    lock/lock_stale)    echo "run: golock lock -work golock.work" ;;
    lock/lock_tampered) echo "lock file was edited externally" ;;
    cache/*)            echo "local cache inconsistent with lock" ;;
    workspace/*)        echo "workspace declarations conflict" ;;
  esac
fi
```
