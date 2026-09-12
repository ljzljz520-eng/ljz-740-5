/*
 * vector_index.h - 向量检索库对外 C ABI
 *
 * 设计要点:
 *  - 全部接口使用 C 链接 (extern "C"), 保证 dlopen/dlsym 可直接解析;
 *  - 句柄不透明 (vi_index_t*), 内部状态对 Go 侧不可见;
 *  - 所有可能失败的接口返回 int 状态码: 0 成功, 非 0 失败;
 *  - 失败原因通过 vi_last_error() 获取 (线程局部, 不持有堆指针, 无需释放);
 *  - 所有权: 句柄必须通过 vi_index_free 释放; 调用方传入的数组调用方负责。
 */
#ifndef VECTOR_INDEX_H
#define VECTOR_INDEX_H

#include <stdint.h>
#include <stddef.h>

#if defined(_WIN32)
  #ifdef VI_BUILD_DLL
    #define VI_API __declspec(dllexport)
  #else
    #define VI_API __declspec(dllimport)
  #endif
#else
  #define VI_API __attribute__((visibility("default")))
#endif

#ifdef __cplusplus
extern "C" {
#endif

typedef struct vi_index vi_index_t;

/* 距离度量 */
typedef enum {
    VI_METRIC_L2     = 0, /* 平方欧氏距离, 越小越相似 */
    VI_METRIC_COSINE = 1  /* 余弦距离 = 1 - cos,  越小越相似; 内部对向量做归一化 */
} vi_metric_t;

/* 创建索引配置 */
typedef struct {
    int32_t dim;      /* 向量维度, 必须 > 0 */
    int32_t metric;   /* vi_metric_t */
} vi_config_t;

/* 单条检索结果 */
typedef struct {
    int64_t id;
    float   distance;
} vi_result_t;

/* 生命周期 ---------------------------------------------------------- */

/* 创建空索引, 失败返回 NULL (此时 vi_last_error 有效) */
VI_API vi_index_t *vi_index_create(const vi_config_t *config);

/* 释放索引句柄; ptr 指向的指针会被置空; NULL 安全。
 * 此后 Go 侧持有的裸指针绝不可再使用。 */
VI_API void vi_index_free(vi_index_t **index);

/* 数据写入 ---------------------------------------------------------- */

/* 批量添加向量:
 *  ids     : n 个 int64 id (调用方保证不重复)
 *  vectors : n * dim 个 float, 行优先连续存储
 *  返回非 0 时整个批次不写入。*/
VI_API int vi_index_add(vi_index_t *index,
                        const int64_t *ids,
                        const float *vectors,
                        size_t n);

/* 查询 TopK:
 *  query   : dim 个 float
 *  top_k   : 期望返回条数, 1..n
 *  results : 调用方分配, 容量 >= top_k
 *  out_n   : 实际写入条数 (索引为空时为 0)
 * 结果按 distance 升序排列。*/
VI_API int vi_index_search(const vi_index_t *index,
                           const float *query,
                           size_t top_k,
                           vi_result_t *results,
                           size_t *out_n);

/* 持久化 ------------------------------------------------------------ */

/* 二进制落盘 (小端字节序) */
VI_API int vi_index_save(const vi_index_t *index, const char *path);

/* 从磁盘加载, 失败返回 NULL */
VI_API vi_index_t *vi_index_load(const char *path);

/* 元信息 ------------------------------------------------------------ */

VI_API size_t vi_index_size(const vi_index_t *index);
VI_API int    vi_index_dim(const vi_index_t *index, int32_t *out_dim);

/* 读出索引的距离度量 (vi_metric_t); load 出的句柄同样适用,
 * 保证 Go 侧公开状态与持久化文件头中的 metric 一致。 */
VI_API int    vi_index_metric(const vi_index_t *index, int32_t *out_metric);

/* 取最近一次失败的错误信息 (线程局部静态存储, 不需要释放) */
VI_API const char *vi_last_error(void);

#ifdef __cplusplus
}
#endif

#endif /* VECTOR_INDEX_H */
