# 多模块工作区（golock.work）

一个仓库下挂多个各自带 `go.mod` 的子模块时，用一个工作区文件把它们
声明成一个整体，`golock` 会对整个工作区做一次合并、锁定与审计。

## 工作区文件格式

按行组织，UTF-8，`#` 之后为注释：

```text
name example.com/my-workspace   # 可选,默认 "workspace"
use ./services/api
use ./services/worker
use ./libs/common
```

- `name`：工作区身份，作为合并后“主模块”的 module path 写进锁的
  `module-name`。
- `use <dir>`：一个子模块目录（相对工作区文件所在目录），目录下必须有
  可解析的 `go.mod`。同一个目录重复 `use` 会被拒绝
  （`workspace_malformed`）。
- 子模块处理顺序按 `use` 目录名排序确定，与书写顺序无关——同一工作区
  文件无论怎么调换 `use` 行顺序，合并结果完全一致。

## 合并规则

合并产出一份统一的声明集（喂给离线解析器），并记录每条声明的来源
（哪个子模块引入的）：

1. **require 取并集去重**：同一 `path@version` 只保留一条；版本选择遵循
   与单模块一致的 MVS 口径——同一依赖被多个子模块要求不同版本时，选最
   高版本，这不是“默认挑一个”，而是统一的版本序（预发布低于正式版、
   伪版本按预发布处理、`+incompatible` 与构建元数据按规范 semver 比
   较）。`indirect` 标记：只要有一个子模块直接require，合并后即为直接
   依赖。
2. **exclude 取并集**：所有子模块的 exclude 全部生效。
3. **replace 全局统一生效**：规则按 `old[@version]` 作为键去重。
   - 多个子模块写下**完全相同**的规则：静默去重，来源全部记录。
   - 同一个 `old[@version]` 被映射到**不同目标**：硬冲突
     （`workspace_replace_conflict`），报错逐条列出每个子模块的目录、
     模块路径、规则原文与行号。**不会**默认挑任何一边生效；冲突存在
     时锁定与审计都不会继续解析。
4. **go 指令**：取各子模块中最高的 go 版本。

## 边界与限制

- 两个子模块声明了**相同的 module path**：拒绝
  （`workspace_duplicate_module`），合并结果无法归属。
- 子模块 `go.mod` 里的解析拒绝（非法版本等）会带上子模块身份
  （模块路径 + 目录）原样上报，不会被吞掉。
- 子模块 `go.mod` 缺失/不可读：`workspace_missing_module`。
- 传递依赖仍来自本地缓存中各模块自带的 `go.mod`；工作区不改变离线前提。
- 本地路径 replace（`=> ../local`）的传递依赖从该目录的 `go.mod` 读取，
  与单模块行为一致。

## 增删子模块与可复现性

- 工作区文件增删 `use` 行后重新 `lock`：合并声明集如实变化，锁的
  `manifest-hash` 随之变化，不会残留已移除子模块的依赖，也不会漏掉新
  增子模块的依赖。
- 同一份输入（工作区文件 + 各子模块 `go.mod` + 缓存内容）重复
  `lock`/`audit`，结果逐字节一致；`lock` 幂等，内容不变时不重写文件。

## 示例

```bash
# 锁定整个工作区(生成一个统一的 go.lock)
golock lock -work golock.work -cache "$GOMODCACHE"

# 只读审计:锁相对当前工作区声明发生了什么
golock audit -work golock.work -cache "$GOMODCACHE" -json
```
