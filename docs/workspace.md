# 多模块工作区（golock.work）

一个仓库下挂多个各自带 `go.mod` 的子模块时，用工作区文件把它们当作一个
整体锁定与审计。默认文件名 `golock.work`，通过 `-workspace PATH` 指定。

## 文件格式

行式文本，词法规则与 `go.mod` 相同（`//` 行注释、`/* */` 块注释、
双引号/反引号字符串）：

```text
name example.com/ws        # 可选；合并后声明集的标识，默认 "workspace"
go 1.23                    # 可选；缺省时取各子模块 go 指令的最大值（按数值比较）
use ./mod-a                # 单行形式
use (                      # 块形式
    ./mod-b
    ./mod-c
)
```

- `use` 的路径相对于工作区文件所在目录；每个条目必须含可解析的 `go.mod`，
  否则报 `module_unreadable` 并指明是哪个 `use` 目录。
- 重复的 `use` 条目幂等去重；两个条目声明同一个 module path 报
  `duplicate_module`。
- 未知指令、缺参数、块未闭合等按 `bad_directive` / `incomplete_entry`
  明确拒绝，不静默跳过。

## 合并规则

合并是**纯函数**：只读各子模块 `go.mod`，输出一份排序确定的合并声明集
（作为锁的 `manifest-hash` 输入）。同一份工作区反复合并结果字节一致。

| 声明      | 一致时                         | 冲突时                                                     |
|-----------|--------------------------------|------------------------------------------------------------|
| `require` | 相同 path+version 去重合并；每个声明过的子模块都记入 `via` 引入链 | 同 path 不同 version ⇒ `requirement_conflict`，列出每个子模块各自的要求，**不默认挑一个** |
| `replace` | 同一 old path@version 且目标（新路径/版本/是否本地）完全一致 ⇒ 去重 | 目标不一致 ⇒ `replace_conflict`，列出各方目标              |
| `exclude` | 并集去重，统一生效             | （exclude 无冲突形态，只有覆盖范围叠加）                   |

冲突错误（`*model.WorkspaceError`，JSON 中 `category: "workspace"`）都带
`sources` 字段，逐条写明“哪个子模块声明了什么”，例如：

```text
requirement_conflict: example.com/dep
  sources:
    example.com/mod-a: require example.com/dep v1.2.0
    example.com/mod-b: require example.com/dep v1.3.0
```

## 版本口径

合并与选版使用与单模块一致的规范版本口径：`vMAJOR.MINOR.PATCH`、预发布
（`-rc.1`）、构建元数据（`+build.5`）、伪版本
（`v0.0.0-20210101000000-abcdef123456`）、`+incompatible`，以及预发布与
构建元数据的组合。这些都是合法写法，不会被误判为 `malformed_version`。

## 边界与语义

- 合并后的声明集交给与单模块相同的离线 MVS 解析器：传递依赖来自缓存中
  各模块自带的 `go.mod`，`replace`/`exclude` 统一生效。
- 锁的 `module-name` 是工作区 `name`，`manifest-hash` 覆盖合并后的声明
  集；任一子模块声明变化都会使哈希变化 ⇒ 审计报 `lock_stale`。
- 增删子模块后重新 `lock`：新子模块的依赖并入、被删子模块独有的依赖消
  失，不重不漏；若被删子模块的依赖仍被其他子模块声明，则锁不变（声明
  集语义等价）。
- 传递依赖的 `via` 引入链以**子模块 module path** 为顶层归因，能直接看
  出“是哪个子模块引入的”。
- 本地路径 replace（`=> ../local/dep`）不计算内容哈希，与单模块行为一致；
  不同子模块对同一路径的本地 replace 目标不同同样算 `replace_conflict`。
