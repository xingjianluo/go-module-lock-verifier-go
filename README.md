# go-module-lock-verifier

一个**完全离线**的 Go 模块依赖锁定与可复现校验工具。它把 `go.mod`（或一
个多模块工作区）解析成统一的内部模型，生成带完整性保护的锁文件，并只
用本地模块缓存（`GOMODCACHE`）复算每个依赖最终生效的版本与内容哈希。

适用场景：

- 离线/隔离网络环境下的可复现构建；
- 多模块仓库：把多个子模块当成一个整体锁定与审计（见
  [docs/workspace.md](docs/workspace.md)）；
- 排查“为什么选中了这个版本”、依赖被谁引入；
- 检测依赖声明漂移、锁文件被手工改动、缓存内容被替换；
- CI 里用机器可读输出（`-json`）直接分支判断（见
  [docs/machine-report.md](docs/machine-report.md)）。

## 设计原则

1. **不静默丢弃**：版本非法、字段缺失、条目不完整都会被拒绝，并给出彼此可
   区分的拒绝原因（见下文错误语义表）。
2. **结果稳定**：归一化对条目排序，哈希只依赖内容，不依赖源文件顺序、
   mtime、inode 或读取顺序。同一份输入重复锁定 / 重复校验结果一致。
3. **绝不偷偷改锁**：`verify` / `audit` 是只读的；锁与声明对不上时直接失
   败，区分“声明变了锁没更新”（`lock_stale`）和“锁被改过 / 校验值不符”
   （`lock_tampered` / `hash_mismatch`）。
4. **并发隔离与原子落盘**：每次运行使用独立的临时工作目录，中间状态不共
   享；对缓存只读不写；`lock` 只有在全部成功后才会以“临时文件 + rename”
   的方式原子落盘，任何一步失败都不会留下半成品锁。
5. **冲突不仲裁**：多模块合并时替换规则打架会带来源逐条报出，绝不默认挑
   一边生效。

## 快速开始

```bash
# 生成/更新锁（默认读 ./go.mod，写 ./go.lock，缓存取 $GOMODCACHE）
go run ./cmd/golock lock

# 离线校验（要求缓存中已有全部所需模块）
go run ./cmd/golock verify

# 多模块工作区：一次锁定整个仓库
go run ./cmd/golock lock -work golock.work

# 只读审计：锁相对当前声明/缓存发生了什么（CI 加 -json）
go run ./cmd/golock audit -work golock.work -json
```

常用参数：

| 参数        | 默认值       | 说明                                   |
|-------------|--------------|----------------------------------------|
| `-mod PATH` | `go.mod`     | 依赖声明文件（与 `-work` 互斥）        |
| `-work PATH`| 无           | 多模块工作区文件                       |
| `-lock PATH`| `go.lock`    | 锁文件路径                             |
| `-cache DIR`| `$GOMODCACHE`| 本地模块缓存根目录                     |
| `-workdir DIR` | 临时目录   | 单次运行私有的工作目录（并发时隔离用） |
| `-json`     | 关           | 输出稳定的机器可读报告（stdout）       |

`verify` / `audit` 会先打印每个模块的差异报告（状态、锁定版本/哈希、实
际版本/哈希、`via` 引入链），再给出分类后的失败列表，退出码 0/1/2。

## 本地验证

```bash
# 全量测试，竞态检测；测试用内置的“假 GOMODCACHE”，不访问网络
go test -race ./...

# 只跑工作区合并与冲突
go test -race ./internal/workspace/ -v

# 只跑审计分类（声明漂移/锁篡改/缓存不符/只读保证）
go test -race ./internal/audit/ -v

# 端到端：工作区锁定、并发锁定、失败不落锁
go test -race ./cmd/golock/ -v
```

测试会把输入（manifest/锁内容）和判定依据（错误种类、期望值、实际值、
引入链）通过 `t.Logf` 打印出来，直接看 `-v` 输出即可追溯。

也可以用自带的离线演示脚本：

```bash
./hack/demo-offline.sh
```

该脚本在一个临时目录里构造假缓存与工程，依次演示：干净校验、篡改锁
（`lock_tampered`）、篡改缓存内容（`hash_mismatch`）、声明漂移
（`lock_stale`）。

## 支持的 go.mod 范围

- 指令：`module`、`go`、`require`（单行与括号块、`// indirect`）、
  `replace`（版本替换与本地路径替换）、`exclude`；`toolchain` 被容忍。
- 版本：规范 semver（`vMAJOR.MINOR.PATCH`）、预发布、构建元数据、
  伪版本（如 `v0.0.0-20210101000000-abcdef123456`）、`+incompatible`。
- 注释：`//` 行注释与 `/* ... */` 块注释；双引号 / 反引号字符串。
- 不在范围内的指令会作为 `bad_directive` 被明确拒绝，而不是被忽略。

解析得到的传递依赖关系来自缓存中各模块自带的 `go.mod`，因此离线校验前，
构建所需的模块 `.mod` 与 zip（或解压目录）必须已在本地缓存中。

## 错误语义

错误分四层，每层都有可区分的 kind，CLI 输出会加前缀标签（`-json` 时即
为 finding 的 `category`/`kind`）：

| 标签         | kind                              | 含义                                                       |
|--------------|-----------------------------------|------------------------------------------------------------|
| `[manifest]` | `malformed_version`               | 版本字符串不合法                                           |
| `[manifest]` | `missing_path` / `missing_version`| 路径或版本关键字段缺失                                     |
| `[manifest]` | `incomplete_entry`                | 条目缺少参数、缺少 `=>`、块未闭合等                        |
| `[manifest]` | `bad_directive`                   | 未知指令、无法分词（如未闭合引号）                         |
| `[manifest]` | `duplicate_replace` / `replace_conflict` | 替换规则重复、两侧参数冲突                          |
| `[manifest]` | `exclude_without_version`         | exclude 未带版本                                           |
| `[lock]`     | `lock_stale`                      | 声明变了但锁没更新（`manifest-hash` 不符或模块集差异）     |
| `[lock]`     | `lock_tampered`                   | 锁内容与其内嵌 `integrity` 不符——锁被改过                 |
| `[lock]`     | `lock_hash_mismatch`              | 锁内单模块哈希与同输入推导值不符（API 层分类，见锁文档）   |
| `[lock]`     | `lock_malformed` / `lock_version_unsupported` | 锁无法解析 / 格式版本不认识                     |
| `[cache]`    | `missing_module`                  | 缓存中缺少所需模块（`.mod` 或内容）                       |
| `[cache]`    | `hash_mismatch`                   | 缓存内容哈希与锁记录不一致（内容被动过）                   |
| `[cache]`    | `version_conflict`                | 某模块所有被请求版本都被 exclude，选不出可用版本           |
| `[workspace]`| `workspace_malformed`             | 工作区文件无法解析                                         |
| `[workspace]`| `workspace_missing_module`        | `use` 目录下没有可读的 `go.mod`                            |
| `[workspace]`| `workspace_duplicate_module`      | 两个子模块声明了同一个 module path                         |
| `[workspace]`| `workspace_replace_conflict`      | 子模块把同一 replace 源映射到不同目标（列出全部来源）      |

`lock_stale` 与 `lock_tampered` 的关键区别：前者锁文件本身完好但已过期，
重新锁定即可；后者锁文件在生成后被改动，`integrity` 对不上。

锁字段与格式细节见 [docs/lock-format.md](docs/lock-format.md)；工作区合
并规则见 [docs/workspace.md](docs/workspace.md)；机器可读输出字段见
[docs/machine-report.md](docs/machine-report.md)。

## 包结构

```
cmd/golock          CLI：lock / verify / audit，-work 与 -json
internal/model      统一内部模型与分类错误类型
internal/manifest   go.mod 词法/语法解析、归一化与内容哈希
internal/workspace  多模块工作区加载、合并与冲突检测
internal/lockfile   锁的生成、序列化、解析、完整性校验、幂等比较、原子写
internal/resolver   离线 MVS：替换/排除、选版、传递依赖、内容哈希
internal/verify     串联解析-锁校验-离线解析-差异报告（只读核心）
internal/audit      只读变更审计：声明漂移/锁篡改/缓存不符分类
internal/report     机器可读（JSON）结果渲染，字段与条目顺序稳定
internal/testutil   构造离线假 GOMODCACHE 的测试辅助
```
