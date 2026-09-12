# vector-bridge：Go 向量检索库桥接（运行时绑定本地动态库）

一个从零实现的最小可运行方案：

```
Go 程序 ──(cgo 跳板, 运行时)──> dlopen / LoadLibrary ──> libvecidx (C++ flat 索引, C ABI)
```

- **原生层**（`native/`）：C++17 实现的 flat 暴力向量索引，导出纯 C ABI；
  支持 L2 平方距离与 cosine 距离、批量添加、TopK（大小为 K 的堆，O(n log K)）、
  二进制落盘/加载；内部互斥锁保证并发安全。
- **桥接层**（`bridge/`）：cgo 包，但**不做链接期绑定**——编译时无需动态库存在，
  运行时才通过 `dlopen(3)` / `LoadLibraryA` 加载并解析符号。
- **示例**（`cmd/example/`）：读取小型向量文本文件，建索引、TopK 查询、
  保存/重载并打印相似结果。

## 目录结构

```
vector-bridge/
├── go.mod
├── Makefile                 # 顶层: lib / example / run / test
├── native/
│   ├── Makefile             # 产出 build/libvecidx.{so,dylib} 或 vecidx.dll
│   ├── vector_index.h       # 对外 C ABI (跨平台导出宏)
│   └── vector_index.cpp     # flat 索引实现
├── bridge/
│   ├── bridge.go            # 运行时加载 + 句柄封装 + 生命周期
│   └── bridge_test.go       # 端到端测试 (含 -race 并发测试)
├── cmd/example/main.go      # 命令行示例
└── testdata/vectors.txt     # 4 维 × 11 条示例向量
```

## 快速开始

```bash
# 需要 Go 1.22+、g++、make
make lib          # 编译动态库 -> native/build/
make example      # 编译命令行 -> build/example
make run          # 一键冒烟: 建索引 + TopK + 保存 + 重载
make test         # go test (含竞态: CGO_ENABLED=1 go test -race ./...)
```

直接运行：

```bash
./build/example \
  -file testdata/vectors.txt \
  -metric l2 -k 3 \
  -query "0.10,0.20,0.30,0.40" \
  -save build/index.bin        # 可选: 落盘后重新加载再查一次
```

预期输出（Top1 为 query 自身，距离 0）：

```
loaded 11 vectors, dim=4, metric=l2
index size: 11
top 3 nearest neighbors:
  #1  id=10   distance=0.000000
  #2  id=12   distance=0.001000
  #3  id=11   distance=0.001000
index saved to build/index.bin; reloading...
```

向量文件为简单文本格式，首行是维度，之后每行 `id v1 v2 ... vD`，支持空行与 `#` 注释。

## 代码用法

```go
import "vector-bridge/bridge"

// 1) 进程内加载一次动态库（路径解析规则见下）
if err := bridge.Load(""); err != nil { log.Fatal(err) }

// 2) 创建索引（不透明句柄）
idx, err := bridge.NewIndex(bridge.Config{Dim: 128, Metric: bridge.Cosine})
if err != nil { log.Fatal(err) }
defer idx.Close() // 必须释放

// 3) 批量添加：vectors 为行优先 []float32，长度 = len(ids)*Dim
if err := idx.Add(ids, vectors); err != nil { log.Fatal(err) }

// 4) TopK 查询，结果按距离升序（两种度量均为越小越相似）
hits, err := idx.Search(query, 10)

// 5) 持久化 / 恢复
_ = idx.Save("index.bin")
idx2, _ := bridge.LoadIndex("index.bin")
defer idx2.Close()
```

## C ABI 一览（`native/vector_index.h`）

| 接口 | 作用 | 所有权 |
|---|---|---|
| `vi_index_create(config)` | 创建索引，返回不透明句柄 | 调用方拥有 |
| `vi_index_free(&h)` | 释放句柄并把指针置 NULL，NULL 安全 | — |
| `vi_index_add(h, ids, vectors, n)` | 批量添加 | 数组调用方拥有，返回后不持有 |
| `vi_index_search(h, q, k, out, *n)` | TopK，结果升序 | 结果缓冲调用方分配 |
| `vi_index_save / vi_index_load` | 小端二进制格式落盘/恢复 | load 出的句柄调用方释放 |
| `vi_index_size / vi_index_dim / vi_index_metric` | 元信息（条数 / 维度 / 距离度量） | — |
| `vi_last_error()` | 最近一次失败原因（线程局部） | 返回静态/线程存储，**禁止 free** |

约定：所有可失败接口返回 `int`（0 成功，非 0 失败），错误详情立刻通过
`vi_last_error()` 读取。它返回线程局部存储指针，Go 侧在同一次调用后立即
`C.GoString` 拷贝成 Go 字符串，不缓存该指针。

## 跨平台加载机制

桥接在**一个 cgo 序言内**用条件编译处理两套加载器，Go 业务代码完全平台无关：

| 平台 | 加载 | 符号解析 | 关闭 | 链接参数 |
|---|---|---|---|---|
| Linux / 其他 ELF | `dlopen(path, RTLD_NOW \| RTLD_LOCAL)` | `dlsym` | `dlclose` | `-ldl` |
| macOS | 同上（系统提供 dlopen，无需额外库也可） | `dlsym` | `dlclose` | `-ldl` |
| Windows | `LoadLibraryA` | `GetProcAddress` | `FreeLibrary` | `-lkernel32`（MinGW） |

- `RTLD_NOW`：加载时立即解析全部未定义符号，库不完整时当场失败，而不是等调用时崩溃；
- `RTLD_LOCAL`：不把库符号泄入全局命名空间，避免与其他 .so 符号冲突；
- 动态库文件名按平台约定：`libvecidx.so` / `libvecidx.dylib` / `vecidx.dll`，
  由 `runtime.GOOS` 选择；
- Go 不能把 `void*` 直接当函数指针调用，每个 ABI 入口在序言里有一个
  `static` C 跳板函数负责转型（`vi_t_add`、`vi_t_search` …）。

**库路径解析顺序**（`bridge.Load(path)`）：

1. 显式参数 `path`；
2. 环境变量 `VECIDX_LIB`；
3. 开发态相对目录：`./build`、`./native/build`、`../build`、`../native/build`；
4. 系统目录：`/usr/local/lib`、`/usr/lib`、`/usr/lib64`、`/opt/vecidx/lib`
   （Windows 为 `C:\Program Files\vecidx\bin`）；
5. 裸库名，交给系统加载器默认规则：
   - Linux：`LD_LIBRARY_PATH` → `ld.so.cache`（`ldconfig`）→ 默认目录；
   - macOS：`@rpath/@loader_path`、`DYLD_LIBRARY_PATH`、`DYLD_FALLBACK_LIBRARY_PATH`；
   - Windows：应用目录 → 系统目录 → `PATH`（可用 `AddDllDirectory`/`SetDllDirectory` 定制）。

显式路径（参数或环境变量）加载失败时**直接报错**，不做隐式回退；
自动搜索时每个候选的失败原因会聚合返回，便于排障：

```
load native library failed: vecidx: native library "libvecidx.so" not found in any candidate location:
  build/libvecidx.so: cannot open shared object file: No such file or directory
  ...
```

编译各平台动态库：

```bash
# Linux (默认)
make -C native                          # -> native/build/libvecidx.so
sudo cp native/build/libvecidx.so /usr/local/lib/ && sudo ldconfig

# macOS
g++ -std=c++17 -O2 -fPIC -DVI_BUILD_DLL -dynamiclib \
    -o native/build/libvecidx.dylib native/vector_index.cpp

# Windows / MSYS2-MinGW
g++ -std=c++17 -O2 -DVI_BUILD_DLL -shared \
    -o native/build/vecidx.dll native/vector_index.cpp \
    -Wl,--out-implib,native/build/libvecidx.a
```

交叉编译 Go 侧时只需 `GOOS/GOARCH CGO_ENABLED=1 CC=<交叉gcc> go build`，
桥接层的平台分支由 cgo 构建标签自动选择。**未启用 cgo
（`CGO_ENABLED=0`）时本包无法编译**——它本质上依赖 C 互操作。

## 内存生命周期（重点）

所有权边界非常明确：**谁分配谁释放，指针不跨边界长期保存**。

1. **句柄**：`vi_index_create` / `vi_index_load` 在 C++ 堆（`new`）上创建索引，
   Go 侧仅持有一个 `unsafe.Pointer`。必须调用 `idx.Close()`（→
   `vi_index_free` → `delete`）释放；原生函数会把句柄指针置 NULL，
   `Close` 幂等且可安全重复调用。
2. **兜底 finalizer**：`NewIndex`/`LoadIndex` 会注册
   `runtime.SetFinalizer`，用户忘记 `Close` 时 GC 回收对象会顺带释放原生内存，
   防止永久泄漏。**但 GC 时机不确定，绝不能依赖它做及时回收**；并且 finalizer
   中能成功释放的前提是动态库仍处于加载状态。
3. **关闭后使用**：`Close` 后再调 `Add/Search/Save/Size` 返回
   `bridge.ErrClosed`。实现上用 `sync.RWMutex` 序列化：调用持有读锁，
   `Close` 持有写锁，保证不会在原生调用执行到一半时把句柄删掉。
4. **数据数组**：`Add` 的 `ids/vectors`、`Search` 的 `query` 都遵循 cgo
   指针规则——调用期间 Go 侧切片保活（`runtime.KeepAlive`），原生代码
   **同步使用、立即返回、不保存任何指向 Go 内存的指针**，因此调用结束后
   GC 可自由回收这些切片。结果数组 `vi_result_t*` 由 Go 分配，再拷贝成
   `[]bridge.Result` 返回。
5. **字符串**：跨边界路径用 `C.CString` 分配、调用后立刻 `C.free`；
   `vi_last_error()` 指向线程局部 C++ 字符串内部缓冲，Go 侧即时拷贝，
   不持有、不释放。
6. **动态库本身**：进程生命周期内只 `Load` 一次（重复调用报错），**刻意不提供
   Unload**。原因是所有活跃句柄、finalizer 都依赖库内代码与符号地址，
   提前 `dlclose`/`FreeLibrary` 会造成悬挂代码指针。库随进程退出由 OS 回收，
   这也是多数运行时插件系统的标准做法。
7. **并发**：原生索引内部 `std::mutex` 保护增删查（查询也取锁以与
   `Add` 互斥），`go test -race ./bridge` 包含 8 goroutine × 200 次查询的
   并发用例；但**同一个 `Index` 不要在 `Close` 之后被任何 goroutine 使用**。
8. **线程局部错误**：错误信息是 thread-local，所以“调用失败 → 立刻取
   `vi_last_error`”必须发生在同一线程的同一次 cgo 调用序列内；桥接层正是
   这样封装的，业务侧只接触 Go `error`。

## 索引文件格式

固定小端序（当前实现面向小端机器；如需大端可移植性，加载时做字节转换即可）：

```
magic u64 "VECIDX01" | version u32 | dim u32 | metric u32 | n u64
ids[n] int64 | norms[n] f32 | data[n*dim] f32   (cosine 下 data 已归一化)
```

文件损坏（魔数/版本不符、截断）时 `LoadIndex` 返回错误，不产生半成品句柄。

## 设计取舍与扩展

- 当前是 **flat 暴力检索**，定位是教学/小库（万级向量以内）。由于 Go 侧只依赖
  一组稳定 C ABI，把原生实现替换为 HNSW / IVF-PQ（如 usearch/faiss 风格）时
  **Go 代码无需改动**，只需保持 `vector_index.h` 的符号与语义。
- 为了“从零实现”，加载器没有使用第三方库（如 purego）；若允许依赖，
  `github.com/ebitengine/purego` 可在不启用 cgo 的情况下完成同样的运行时绑定。
