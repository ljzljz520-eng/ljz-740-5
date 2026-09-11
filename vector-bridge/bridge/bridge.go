// Package bridge 是本地向量检索动态库 (libvecidx) 的 Go 运行时桥接层。
//
// 动态库不参与 Go 链接期绑定，而是在运行时通过 dlopen(3) (Linux/macOS) 或
// LoadLibrary (Windows) 加载，再用 dlsym/GetProcAddress 解析 C ABI 符号。
// 因此编译本包不需要动态库存在，部署时也可以单独替换动态库实现。
package bridge

/*
#cgo linux LDFLAGS: -ldl
#cgo darwin LDFLAGS: -ldl
#cgo windows LDFLAGS: -lkernel32

#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>

// ---- 跨平台加载原语 --------------------------------------------------
#if defined(_WIN32)
  #include <windows.h>

  static void *vi_open_lib(const char *path) {
      // SetDllDirectory / 默认搜索目录由调用方控制; 这里只做标准加载
      return (void *)LoadLibraryA(path);
  }
  static void vi_close_lib(void *h) {
      if (h) FreeLibrary((HMODULE)h);
  }
  static void *vi_sym(void *h, const char *name) {
      return (void *)GetProcAddress((HMODULE)h, name);
  }
  static const char *vi_open_error(void) {
      static __thread char buf[96];
      snprintf(buf, sizeof(buf), "LoadLibraryA/GetProcAddress failed, win32 err=%lu",
               (unsigned long)GetLastError());
      return buf;
  }
#else
  #include <dlfcn.h>

  static void *vi_open_lib(const char *path) {
      // RTLD_NOW: 立即解析全部未定义符号, 缺符号当场报错;
      // RTLD_LOCAL: 不把库符号泄入全局作用域, 避免与其他库符号冲突。
      return dlopen(path, RTLD_NOW | RTLD_LOCAL);
  }
  static void vi_close_lib(void *h) {
      if (h) dlclose(h);
  }
  static void *vi_sym(void *h, const char *name) {
      return dlsym(h, name);
  }
  static const char *vi_open_error(void) {
      const char *e = dlerror();
      return e ? e : "unknown dynamic loader error";
  }
#endif

// ---- 与 vector_index.h 二进制一致的镜像声明 (不 #include 头文件,
//      保证本包在没有原生库/头文件的机器上也能编译) ---------------------
typedef struct vi_index vi_index_t;
typedef struct { int32_t dim; int32_t metric; } vi_config_c;
typedef struct { int64_t id; float distance; } vi_result_c;

typedef vi_index_t *(*pfn_create)(const vi_config_c *);
typedef void        (*pfn_free)(vi_index_t **);
typedef int         (*pfn_add)(vi_index_t *, const int64_t *, const float *, size_t);
typedef int         (*pfn_search)(const vi_index_t *, const float *, size_t,
                                  vi_result_c *, size_t *);
typedef int         (*pfn_save)(const vi_index_t *, const char *);
typedef vi_index_t *(*pfn_load)(const char *);
typedef size_t      (*pfn_size)(const vi_index_t *);
typedef int         (*pfn_dim)(const vi_index_t *, int32_t *);
typedef const char *(*pfn_err)(void);

// Go 无法直接把 void* 当函数指针调用, 每个 ABI 入口用一个极小的 C 跳板转型。
static vi_index_t *vi_t_create(void *p, const vi_config_c *c) { return ((pfn_create)p)(c); }
static void        vi_t_free(void *p, vi_index_t **h)         { ((pfn_free)p)(h); }
static int vi_t_add(void *p, vi_index_t *h, const int64_t *ids,
                    const float *v, size_t n) { return ((pfn_add)p)(h, ids, v, n); }
static int vi_t_search(void *p, const vi_index_t *h, const float *q, size_t k,
                       vi_result_c *r, size_t *n) {
    return ((pfn_search)p)(h, q, k, r, n);
}
static int         vi_t_save(void *p, const vi_index_t *h, const char *path) { return ((pfn_save)p)(h, path); }
static vi_index_t *vi_t_load(void *p, const char *path)                     { return ((pfn_load)p)(path); }
static size_t      vi_t_size(void *p, const vi_index_t *h)                  { return ((pfn_size)p)(h); }
static int         vi_t_dim(void *p, const vi_index_t *h, int32_t *d)       { return ((pfn_dim)p)(h, d); }
static const char *vi_t_lasterr(void *p)                                    { return ((pfn_err)p)(); }
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

// Metric 距离度量。
type Metric int32

const (
	// L2 平方欧氏距离, 值越小越相似。
	L2 Metric = 0
	// Cosine 余弦距离 (1 - cosine), 值越小越相似; 库内部会做归一化。
	Cosine Metric = 1
)

func (m Metric) String() string {
	switch m {
	case L2:
		return "l2"
	case Cosine:
		return "cosine"
	default:
		return fmt.Sprintf("metric(%d)", int(m))
	}
}

// Config 创建索引的配置。
type Config struct {
	Dim    int
	Metric Metric
}

// Result 单条 TopK 命中。
type Result struct {
	ID       int64
	Distance float32
}

// ErrClosed 句柄已释放后继续使用。
var ErrClosed = errors.New("vector index already closed")

// symbols 缓存 dlsym 解析出的函数地址, 之后每次调用零查找开销。
type symbols struct {
	create, free, add, search, save, load, size, dim, lastErr unsafe.Pointer
}

type loadedLib struct {
	handle unsafe.Pointer
	sym    *symbols
}

var (
	loadMu sync.Mutex
	lib    *loadedLib
)

// 动态库的跨平台文件名。
func libFileName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libvecidx.dylib"
	case "windows":
		return "vecidx.dll"
	default: // linux/freebsd 等 ELF 平台
		return "libvecidx.so"
	}
}

// Load 在运行时加载本地动态库。
//
// path 为空时按以下顺序查找:
//  1. 环境变量 VECIDX_LIB 指定的完整路径;
//  2. ./build、./native/build 等相对当前工作目录的位置 (开发态);
//  3. /usr/local/lib、/usr/lib (POSIX) 等系统目录;
//  4. 裸库名, 交给系统加载器按默认规则查找
//     (Linux: LD_LIBRARY_PATH/ld.so.conf; macOS: DYLD_*; Windows: DLL 搜索路径)。
//
// 显式指定 path (或设置了 VECIDX_LIB) 时, 加载失败直接返回错误, 不做隐式回退。
// 库在进程生命周期内只加载一次, 不提供卸载接口 (见包文档内存生命周期说明)。
func Load(path string) error {
	loadMu.Lock()
	defer loadMu.Unlock()
	if lib != nil {
		return errors.New("vecidx: native library already loaded")
	}

	if path == "" {
		path = os.Getenv("VECIDX_LIB")
	}
	if path != "" {
		h, err := openAndResolve(path)
		if err != nil {
			return err
		}
		lib = h
		return nil
	}

	candidates := []string{
		filepath.Join("build", libFileName()),
		filepath.Join("native", "build", libFileName()),
		filepath.Join("..", "build", libFileName()),
		filepath.Join("..", "native", "build", libFileName()), // 从 bridge 包目录跑 go test
	}
	for _, dir := range systemDirs() {
		candidates = append(candidates, filepath.Join(dir, libFileName()))
	}
	candidates = append(candidates, libFileName()) // 最后交给系统默认搜索路径

	var errs []string
	for _, cand := range candidates {
		h, err := openAndResolve(cand)
		if err == nil {
			lib = h
			return nil
		}
		errs = append(errs, fmt.Sprintf("  %s: %v", cand, err))
	}
	return fmt.Errorf("vecidx: native library %q not found in any candidate location:\n%s",
		libFileName(), joinErrs(errs))
}

func joinErrs(errs []string) string {
	out := ""
	for _, e := range errs {
		out += e + "\n"
	}
	return out
}

func systemDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\Program Files\vecidx\bin`}
	}
	return []string{"/usr/local/lib", "/usr/lib", "/usr/lib64", "/opt/vecidx/lib"}
}

func requireLib() (*loadedLib, error) {
	loadMu.Lock()
	l := lib
	loadMu.Unlock()
	if l == nil {
		return nil, errors.New("vecidx: native library not loaded; call bridge.Load() first")
	}
	return l, nil
}

// openAndResolve 打开动态库并解析全部 ABI 符号; 任一符号缺失立即关闭库,
// 避免留下半初始化状态。
func openAndResolve(path string) (*loadedLib, error) {
	cpath := C.CString(path)
	h := C.vi_open_lib(cpath)
	C.free(unsafe.Pointer(cpath))
	if h == nil {
		return nil, fmt.Errorf("%s", C.GoString(C.vi_open_error()))
	}

	s := &symbols{}
	type entry struct {
		name  string
		where *unsafe.Pointer
	}
	entries := []entry{
		{"vi_index_create", &s.create},
		{"vi_index_free", &s.free},
		{"vi_index_add", &s.add},
		{"vi_index_search", &s.search},
		{"vi_index_save", &s.save},
		{"vi_index_load", &s.load},
		{"vi_index_size", &s.size},
		{"vi_index_dim", &s.dim},
		{"vi_last_error", &s.lastErr},
	}
	for _, e := range entries {
		cn := C.CString(e.name)
		addr := C.vi_sym(h, cn)
		C.free(unsafe.Pointer(cn))
		if addr == nil {
			C.vi_close_lib(h)
			return nil, fmt.Errorf("symbol %s not found in %s (%s)",
				e.name, path, C.GoString(C.vi_open_error()))
		}
		*e.where = addr
	}
	return &loadedLib{handle: h, sym: s}, nil
}

func nativeErr(s *symbols) error {
	return errors.New("vecidx: " + C.GoString(C.vi_t_lasterr(s.lastErr)))
}

// Index 是不透明原生句柄的 Go 侧所有者。
// 必须调用 Close 释放; 未关闭时 GC finalizer 仅作兜底, 不保证及时执行。
type Index struct {
	handle unsafe.Pointer
	dim    int
	metric Metric

	// 用于序列化 "调用中" 与 "Close" 之间的 Go 侧可见性;
	// 数据并发安全本身由原生库内部的互斥锁保证。
	opMu sync.RWMutex
}

// NewIndex 创建空索引。
func NewIndex(cfg Config) (*Index, error) {
	if cfg.Dim <= 0 {
		return nil, errors.New("vecidx: Dim must be > 0")
	}
	if cfg.Metric != L2 && cfg.Metric != Cosine {
		return nil, fmt.Errorf("vecidx: invalid metric %d", cfg.Metric)
	}
	l, err := requireLib()
	if err != nil {
		return nil, err
	}

	cc := C.vi_config_c{dim: C.int32_t(cfg.Dim), metric: C.int32_t(cfg.Metric)}
	h := C.vi_t_create(l.sym.create, &cc)
	if h == nil {
		return nil, nativeErr(l.sym)
	}
	idx := &Index{handle: unsafe.Pointer(h), dim: cfg.Dim, metric: cfg.Metric}
	runtime.SetFinalizer(idx, indexFinalize)
	return idx, nil
}

// indexFinalize 安全网: 用户忘记 Close 时避免原生内存永久泄漏。
// 不能依赖它做及时回收 (GC 时机不确定)。
func indexFinalize(idx *Index) {
	if idx.handle != nil {
		if l, err := requireLib(); err == nil {
			h := idx.handle
			C.vi_t_free(l.sym.free, (**C.vi_index_t)(unsafe.Pointer(&h)))
			idx.handle = h // 原生侧已置 NULL
		}
	}
}

// Close 释放原生句柄, 幂等且可安全重复调用。
func (idx *Index) Close() error {
	idx.opMu.Lock()
	defer idx.opMu.Unlock()
	if idx.handle == nil {
		return nil
	}
	l, err := requireLib()
	if err != nil {
		return err
	}
	h := idx.handle
	C.vi_t_free(l.sym.free, (**C.vi_index_t)(unsafe.Pointer(&h)))
	idx.handle = h // 原生侧 vi_index_free 会把 *ptr 写成 NULL
	runtime.SetFinalizer(idx, nil)
	return nil
}

// Dim 返回索引向量维度。
func (idx *Index) Dim() int { return idx.dim }

// Metric 返回距离度量。
func (idx *Index) Metric() Metric { return idx.metric }

// Add 批量写入向量。ids 与 vectors 归调用方所有, 调用返回后原生侧不保留任何
// 指向 Go 内存的指针。len(vectors) 必须等于 len(ids)*Dim。
// 空批次是 no-op。
func (idx *Index) Add(ids []int64, vectors []float32) error {
	if len(ids) == 0 {
		return nil
	}
	if len(vectors) != len(ids)*idx.dim {
		return fmt.Errorf("vecidx: expect %d floats for %d ids (dim=%d), got %d",
			len(ids)*idx.dim, len(ids), idx.dim, len(vectors))
	}
	for _, v := range vectors {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return errors.New("vecidx: vectors must not contain NaN/Inf")
		}
	}
	l, err := requireLib()
	if err != nil {
		return err
	}
	idx.opMu.RLock()
	defer idx.opMu.RUnlock()
	if idx.handle == nil {
		return ErrClosed
	}

	rc := C.vi_t_add(l.sym.add,
		(*C.vi_index_t)(idx.handle),
		(*C.int64_t)(unsafe.Pointer(&ids[0])),
		(*C.float)(unsafe.Pointer(&vectors[0])),
		C.size_t(len(ids)))
	runtime.KeepAlive(ids)
	runtime.KeepAlive(vectors)
	runtime.KeepAlive(idx)
	if rc != 0 {
		return nativeErr(l.sym)
	}
	return nil
}

// Search 检索与 query 最近的 k 条记录, 按距离升序返回; 索引为空时返回空切片。
func (idx *Index) Search(query []float32, k int) ([]Result, error) {
	if k <= 0 {
		return nil, errors.New("vecidx: k must be > 0")
	}
	if len(query) != idx.dim {
		return nil, fmt.Errorf("vecidx: query dim %d != index dim %d", len(query), idx.dim)
	}
	l, err := requireLib()
	if err != nil {
		return nil, err
	}
	idx.opMu.RLock()
	defer idx.opMu.RUnlock()
	if idx.handle == nil {
		return nil, ErrClosed
	}

	buf := make([]C.vi_result_c, k)
	var outN C.size_t
	rc := C.vi_t_search(l.sym.search,
		(*C.vi_index_t)(idx.handle),
		(*C.float)(unsafe.Pointer(&query[0])),
		C.size_t(k),
		&buf[0],
		&outN)
	runtime.KeepAlive(query)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(idx)
	if rc != 0 {
		return nil, nativeErr(l.sym)
	}

	out := make([]Result, int(outN))
	for i := range out {
		out[i] = Result{ID: int64(buf[i].id), Distance: float32(buf[i].distance)}
	}
	return out, nil
}

// Save 将索引以小端二进制格式落盘。
func (idx *Index) Save(path string) error {
	if path == "" {
		return errors.New("vecidx: empty path")
	}
	l, err := requireLib()
	if err != nil {
		return err
	}
	idx.opMu.RLock()
	defer idx.opMu.RUnlock()
	if idx.handle == nil {
		return ErrClosed
	}
	cp := C.CString(path)
	rc := C.vi_t_save(l.sym.save, (*C.vi_index_t)(idx.handle), cp)
	C.free(unsafe.Pointer(cp))
	runtime.KeepAlive(idx)
	if rc != 0 {
		return nativeErr(l.sym)
	}
	return nil
}

// LoadIndex 从磁盘恢复索引。返回的句柄与 NewIndex 的句柄生命周期规则相同。
func LoadIndex(path string) (*Index, error) {
	if path == "" {
		return nil, errors.New("vecidx: empty path")
	}
	l, err := requireLib()
	if err != nil {
		return nil, err
	}
	cp := C.CString(path)
	h := C.vi_t_load(l.sym.load, cp)
	C.free(unsafe.Pointer(cp))
	if h == nil {
		return nil, nativeErr(l.sym)
	}

	var dim C.int32_t
	if rc := C.vi_t_dim(l.sym.dim, h, &dim); rc != 0 {
		C.vi_t_free(l.sym.free, (**C.vi_index_t)(unsafe.Pointer(&h)))
		return nil, nativeErr(l.sym)
	}
	idx := &Index{handle: unsafe.Pointer(h), dim: int(dim)}
	runtime.SetFinalizer(idx, indexFinalize)
	return idx, nil
}

// Size 返回索引中当前向量条数。
func (idx *Index) Size() int {
	l, err := requireLib()
	if err != nil {
		return 0
	}
	idx.opMu.RLock()
	defer idx.opMu.RUnlock()
	if idx.handle == nil {
		return 0
	}
	n := C.vi_t_size(l.sym.size, (*C.vi_index_t)(idx.handle))
	runtime.KeepAlive(idx)
	return int(n)
}
