# go-module-lock-verifier

一个**完全离线**的 Go 模块依赖锁定与可复现校验工具。它把 `go.mod`（单模块）
或 `golock.work`（多模块工作区）解析成统一的内部模型，生成带完整性保护的
锁文件，并只用本地模块缓存（`GOMODCACHE`）复算每个依赖最终生效的版本与
内容哈希。

适用场景：

- 离线/隔离网络环境下的可复现构建；
- 多模块仓库：把若干子模块当作一个整体锁定与审计（跨子模块去重、合并，
  冲突明确报错）；
- 排查“为什么选中了这个版本”、依赖被谁（哪个子模块）引入；
- 变更审计：区分“声明变了锁没更新”“锁被外部改过”“缓存内容对不上”；
- CI 集成：结构化 JSON 输出，失败原因可机器判定。

## 设计原则

1. **不静默丢弃**：版本非法、字段缺失、条目不完整都会被拒绝，并给出彼此可
   区分的拒绝原因（见下文错误语义表）。
2. **结果稳定**：归一化对条目排序，哈希只依赖内容，不依赖源文件顺序、
   mtime、inode 或读取顺序。同一份输入重复锁定 / 重复校验结果一致。
3. **绝不偷偷改锁**：`verify` 是只读的；锁与声明对不上时直接失败，区分
   “声明变了锁没更新”（`lock_stale`）和“锁被改过 / 校验值不符”
   （`lock_tampered` / `hash_mismatch`）。
4. **并发隔离**：每次校验使用独立的临时工作目录，中间状态不共享；对缓存只
   读不写。

## 快速开始

```bash
# 生成/更新锁（默认读 ./go.mod，写 ./go.lock，缓存取 $GOMODCACHE）
go run ./cmd/golock lock

# 离线校验（要求缓存中已有全部所需模块）
go run ./cmd/golock verify

# 多模块工作区：一次锁定 / 审计整个仓库
go run ./cmd/golock lock  -workspace golock.work
go run ./cmd/golock audit -workspace golock.work

# CI 机器可读输出（JSON 写 stdout，人类可读信息写 stderr）
go run ./cmd/golock audit -workspace golock.work -json
```

常用参数：

| 参数             | 默认值       | 说明                                       |
|------------------|--------------|--------------------------------------------|
| `-mod PATH`      | `go.mod`     | 依赖声明文件（单模块模式）                 |
| `-workspace PATH`| 无           | 工作区文件（多模块模式，与 `-mod` 二选一） |
| `-lock PATH`     | `go.lock`    | 锁文件路径                                 |
| `-cache DIR`     | `$GOMODCACHE`| 本地模块缓存根目录                         |
| `-workdir DIR`   | 临时目录     | 单次运行私有的工作目录（并发时隔离用）     |
| `-json`          | 关           | 输出稳定的机器可读 JSON 结果               |

`verify` / `audit` 会先打印每个模块的差异报告（状态、锁定版本/哈希、实际
版本/哈希、`via` 引入链），再给出 `VERIFY OK` 或分类后的失败列表，退出码
0/1/2（0 通过，1 校验失败，2 用法或 I/O 错误）。`audit` 是 `verify` 的别
名，强调“只读变更审计”这一用途；两者都绝不修改锁文件或缓存。

`lock` 是原子的：先写同目录临时文件、fsync、再 rename；解析或解析依赖
任何一步失败都**不会**写锁，磁盘上不会留下半成品。

## 多模块工作区

工作区文件（默认名 `golock.work`）列出要当作整体处理的子模块：

```text
name example.com/ws        # 可选，默认 "workspace"；作为锁的 module-name
go 1.23                    # 可选，默认取各子模块 go 指令的最大值
use ./mod-a
use (
    ./mod-b
    ./mod-c
)
```

合并规则（详见 [docs/workspace.md](docs/workspace.md)）：

- 同一依赖被多个子模块以**相同版本** require：去重合并，每个声明过的子
  模块都会进入该依赖的 `via` 引入链；
- 同一依赖被以**不同版本** require：报 `requirement_conflict`，列出每个
  子模块各自的要求，绝不默认挑一个生效；
- `replace` 规则目标完全一致：去重；目标不一致：报 `replace_conflict`；
- `exclude` 规则取并集去重，统一生效；
- 预发布、构建元数据、伪版本、`+incompatible` 等合法版本写法按统一口径
  处理，不会被误判为非法；
- 合并结果排序确定：同一份工作区重复处理得到字节级一致的锁；增删子模
  块后重新处理，结果如实反映、不重不漏。

## 机器可读输出（CI）

加 `-json` 后，`lock` / `verify` / `audit` 在 stdout 输出一份结构化
JSON：字段顺序与条目顺序稳定，同一份输入反复运行字节一致。失败时
`issues` 数组给出可判定的结构化原因：

```json
{
  "command": "audit",
  "ok": false,
  "subject": "example.com/ws",
  "modules": [ {"path": "...", "status": "...", "introduced_by": ["..."]} ],
  "issues": [
    {
      "category": "workspace",
      "kind": "requirement_conflict",
      "module": "example.com/dep",
      "expected": "v1.2.0",
      "actual": "v1.3.0",
      "sources": ["example.com/mod-a: require example.com/dep v1.2.0", "..."]
    }
  ]
}
```

`category` 取值：`manifest` / `workspace` / `lock` / `cache` /
`internal`，脚本按 `category` + `kind` 直接分支即可。字段与错误分类的
完整语义见 [docs/json-output.md](docs/json-output.md)。

## 本地验证

```bash
# 全量测试，竞态检测；测试用内置的“假 GOMODCACHE”，不访问网络
go test -race ./...

# 只跑端到端校验链路（含并发校验、重复锁定、工作区审计）
go test -race ./internal/verify/ -v

# 只跑工作区合并与冲突
go test -race ./internal/workspace/ -v
```

测试会把输入（manifest/锁内容）和判定依据（错误种类、期望值、实际值、
引入链）通过 `t.Logf` 打印出来，直接看 `-v` 输出即可追溯。

也可以用自带的离线演示脚本：

```bash
./hack/demo-offline.sh     # 单模块：干净校验、篡改锁、篡改缓存、声明漂移
./hack/demo-workspace.sh   # 工作区：整体锁定、JSON 审计、冲突报错、原子性
```

`demo-offline.sh` 依次演示：干净校验、篡改锁（`lock_tampered`）、篡改缓
存内容（`hash_mismatch`）、声明漂移（`lock_stale`）。
`demo-workspace.sh` 依次演示：工作区整体锁定、`-json` 机器可读审计、
子模块版本冲突（`requirement_conflict`，且失败运行不改动已有锁、不留
临时文件）、声明漂移（`lock_stale`）。

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

错误分三层，每层都有可区分的 kind，CLI 输出会加前缀标签：

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
| `[workspace]`| `requirement_conflict`            | 子模块对同一依赖要求了不同版本（列出各自要求）             |
| `[workspace]`| `replace_conflict`                | 子模块对同一 replace 目标给出不同替换                      |
| `[workspace]`| `duplicate_module`                | 两个 use 条目声明了同一个 module path                      |
| `[workspace]`| `module_unreadable`               | use 指向的 go.mod 读不到或解析被拒                         |

`lock_stale` 与 `lock_tampered` 的关键区别：前者锁文件本身完好但已过期，
重新锁定即可；后者锁文件在生成后被改动，`integrity` 对不上。

锁字段与格式细节见 [docs/lock-format.md](docs/lock-format.md)。

## 包结构

```
cmd/golock          CLI：lock / verify / audit，支持 -workspace 与 -json
internal/model      统一内部模型与分类错误类型
internal/manifest   go.mod 词法/语法解析、归一化与内容哈希
internal/workspace  多模块工作区加载、跨子模块合并与冲突检测
internal/lockfile   锁的生成、序列化、解析、完整性校验、幂等比较、原子写入
internal/resolver   离线 MVS：替换/排除、选版、传递依赖、内容哈希
internal/verify     串联解析-锁校验-离线解析-差异报告（只读审计核心）
internal/report     机器可读 JSON 输出（稳定字段/条目顺序、错误分类）
internal/testutil   构造离线假 GOMODCACHE 的测试辅助
```
