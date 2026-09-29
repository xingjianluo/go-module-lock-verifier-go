# go-module-lock-verifier

一个**纯离线、零第三方依赖**（仅用 Go 标准库）的 Go 模块依赖锁定与可复现校验工具。
它把工程里的依赖声明（`go.mod`）解析成统一内部模型，基于本地模块缓存做离线版本
选择，生成并维护一份带完整性保护的锁文件（`go.lock`），并在任何一步对不上时给出
**彼此可区分**的失败原因和一份“谁引入的 / 期望值 / 实际值”的差异报告。

工具不会修改你的 `go.mod`，`verify` 绝不会改写 `go.lock`；只有显式执行 `lock` 才会
在确有变化时原子地重写锁文件。

---

## 1. 它解决什么问题

- **离线构建 / 可复现**：所有解析与校验只读本地 `GOMODCACHE`，没有任何网络请求；
  缓存缺什么就明确报“缺依赖”，而不是尝试下载。
- **依赖漂移排查**：连续两次校验结论一致；缓存内容被动过、锁被改过、声明变了没重锁，
  是三类不同错误，不会混为一谈。
- **统一依赖模型**：直接依赖、`// indirect` 间接依赖、`replace`（含本地目录替换和
  模块替换）、`exclude` 都纳入同一份归一化模型；同一输入归一化结果顺序稳定。
- **严格拒绝畸形声明**：版本写法非法、模块路径缺失/为空、`replace`/`exclude` 不完整、
  本地替换目录不存在等条目**不会被悄悄跳过**，每条都带可区分错误码并整体失败。

## 2. 组件与数据流

```
go.mod ──parse──▶ Manifest(归一化模型) ──CanonicalHash()
                     │  requires / replaces / excludes（均稳定排序）
                     ▼
              graph.Resolver（只读本地 cache）
                     │  展开传递依赖（读缓存内各模块 go.mod）
                     │  应用 replace / exclude
                     │  最小版本选择（MVS，取最高被要求版本）
                     ▼
                 Result（每个依赖带 IntroducedBy 引入链）
                     │
            ┌────────┴────────┐
            ▼                 ▼
        lock(写锁)        verify(校验，不改锁)
            │                 │
            ▼                 ▼
         go.lock   ◀──三方对比──▶ 本地 cache 实际摘要
                  (锁 / 声明期望 / 实际缓存)
```

包划分：

| 包 | 职责 |
|---|---|
| `internal/manifest` | 解析 `go.mod` → 归一化模型；逐条目错误码；稳定 `CanonicalHash` |
| `internal/semver` | 规范化语义版本解析/比较；可区分的版本拒绝码 |
| `internal/cache` | 只读 `GOMODCACHE`；`go.mod` 与目录树稳定摘要；缺失分类 |
| `internal/graph` | 离线传递依赖展开、replace/exclude、MVS、冲突判定、引入链 |
| `internal/lockfile` | 锁模型、确定性 JSON、自哈希完整性、篡改检测 |
| `internal/verify` | 编排锁定/校验；错误归类；期望 vs 实际差异报告 |
| `cmd/golock` | 命令行入口（`lock` / `verify`） |

## 3. 快速开始

```bash
# 编译（仅标准库，无需联网拉依赖）
go build ./cmd/golock

# 生成或更新锁（写到 <工程>/go.lock；无变化时不重写）
golock lock   -C /path/to/module --cache /path/to/GOMODCACHE

# 离线校验（只读；-v 打印每个被校验模块与差异行）
golock verify -C /path/to/module --cache /path/to/GOMODCACHE -v
```

`--cache` 省略时依次取 `$GOMODCACHE`、`$GOPATH/pkg/mod`、`~/go/pkg/mod`。

### 退出码

| 退出码 | 含义 |
|---|---|
| `0` | 锁定成功 / 锁已最新；或校验通过 |
| `1` | 锁定或校验失败（分类明细打印到 stderr） |
| `2` | 命令行用法错误 |

## 4. 锁定与校验语义

### 锁定（`lock`）

1. 严格解析 `go.mod`；任一条目不合法即整体失败（不产出半截锁）。
2. 在本地缓存上离线展开依赖图并做最小版本选择。
3. 为每个生效依赖计算 `go_mod_digest`（其 `go.mod` 的 SHA-256）与
   `tree_digest`（对目录树按相对路径排序后的稳定 SHA-256）。
4. 以确定性 JSON 写出 `go.lock`，并填入 `manifest_hash` 与自校验 `lock_hash`。

**幂等**：对同一份声明重复 `lock`，产出字节完全一致；当磁盘上的锁已与期望一致时
不会发生写操作。

### 校验（`verify`）

按固定顺序判定，先到先报，避免原因混淆：

1. `go.mod` 解析失败 → **manifest malformed**（直接返回）。
2. 锁 JSON 畸形 → **lock malformed**。
3. 锁自哈希 `lock_hash` 不符 → **lock tampered**（锁文件被直接改动）。
4. `manifest_hash` 与当前声明不符 → **lock stale**（声明变了但没重锁；此时**不**再
   按新声明去解析，从而不会被“新依赖还没下载”等噪声掩盖过期这一主因）。
5. 离线解析失败 → 按 **missing dependency** / **version conflict** 分类。
6. 三方对比（锁 / 由声明解析出的结构期望 / 缓存实际摘要）：
   - 锁的结构字段（version、direct、replace、是否存在）与声明不符 → **lock tampered**；
   - 锁记录的摘要与缓存实际内容不符 → **checksum mismatch**；
   - 依赖在本地缓存缺失 → **missing dependency**。

`verify` 永远不写 `go.lock`，不会“替你把锁悄悄改掉”。

## 5. 错误分类（可区分，可用 `errors.Is` 判定）

| 错误码 | 触发条件 | 典型修复 |
|---|---|---|
| `ErrManifestMalformed` | `go.mod` 有条目不合法/不完整（见下“声明级错误码”） | 修正 `go.mod` |
| `ErrLockMalformed` | `go.lock` 不是合法 JSON、字段缺失、格式版本不符、重复条目 | 删除后重新 `lock` |
| `ErrLockStale` | `manifest_hash` 不匹配：声明已变更，锁未重新生成 | 重新 `golock lock` |
| `ErrLockTampered` | `lock_hash` 不符；或锁的结构字段与当前声明矛盾 | 用可信来源重新 `lock` |
| `ErrMissingDependency` | 选中的依赖（或 replace 目标）本地缓存中不存在 | 预先准备离线缓存 |
| `ErrChecksumMismatch` | 锁记录的 go.mod/树摘要 与缓存实际内容不一致 | 排查缓存/制品是否被改动 |
| `ErrVersionConflict` | 最小版本选择选中的版本正好被 `exclude` 排除 | 调整 require/exclude/replace |

声明级（`manifest.EntryErrorCode`）与解析级（`semver.VersionError.Code`）还有更细的
可区分码：空路径、空版本、版本非法、指令格式错误、重复 replace、重复 exclude、
非法 replace、本地替换目录缺失、非法路径；版本侧有空串、缺 `v` 前缀、数字段非法、
后缀非法等。

### 差异报告（Diffs）

每个失败都可能携带若干 `DiffEntry`：

- `Path`：出问题的模块；
- `IntroducedBy`：引入链（主模块开始，逐跳 `path@version`），说明**是谁引入的**；
- `Field`：`manifest_hash` / `version` / `direct` / `replace` /
  `go_mod_digest` / `tree_digest` / `present`；
- `Expected` / `Actual`：期望值与实际值（摘要或版本）。

## 6. 离线缓存布局要求

缓存根目录按标准 `GOMODCACHE` 布局：

```
<root>/<encoded-path>@<version>/...        # 解包后的模块树（含 go.mod）
<root>/cache/download/<encoded-path>/@v/<version>.mod   # go.mod 下载件（兜底）
```

`<encoded-path>` 与 go 命令一致：大写字母 `X` 编码为 `!x`。优先读模块树内的
`go.mod`，缺失时回退到下载件 `.mod`。本地目录替换（`=> ./path` / 绝对路径）直接对
该目录计算同样的树摘要，并会继续读取其 `go.mod` 展开传递依赖。

## 7. 兼容性与边界（请务必阅读）

本项目刻意只使用标准库、不调用 `go` 命令，因此它是 go 模块语义的一个**受控子集**：

- **支持**：`module`、`go`、`require`（含 `// indirect`）、`replace`
  （`path [v] => path v` 与 `=> ./local`、`=> /abs/dir`）、`exclude`；
  块式 `require/replace/exclude (...)` 与单行指令。
- **版本形式**：要求规范化语义版本 `vMAJOR.MINOR.PATCH[-pre][+incompatible]`
  （支持 go 的伪版本时间戳/数字形式，因为它本身符合数字段规则）。go.mod 中不支持
  非规范的查询写法（如 `latest`、分支名、`>v1.0` 区间）。
- **版本选择**：简化版 MVS——取所有被要求版本中的最高语义版本；被 `exclude` 正好
  排除的“最高选中版本”判为硬冲突（不自动升级到下一个未排除版本）。
- **忽略但不报错**：`toolchain`、`retract` 等本工具范围外的指令（会跳过）。
- **不做**：网络下载、`go.sum` 数据库校验、构建列表裁剪（pruning）、工作区
  （`go.work`）、Lazy Module Loading 的完整语义。摘要算法是本工具自定义的稳定
  SHA-256，**不等同于** go 官方 ziphash，但足以检测任何被锁定内容的变化。
- 大版本模块路径后缀（`/v2`）按普通路径处理，不额外推断。

## 8. 本地如何验证

```bash
# 全部单测（含错误分类、稳定归一化、重复锁定、锁篡改、缓存漂移、并发）
go test ./...

# 带竞态检测（并发校验互不共享中间状态/缓存）
go test -race ./...

# 查看测试打印的“输入 + 判定依据”
go test ./internal/manifest/ -v
go test ./internal/verify/   -v

# 静态检查
go vet ./...
```

锁文件字段的逐字段含义见 [docs/lock-format.md](docs/lock-format.md)。
