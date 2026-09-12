// vector_index.cpp - Flat 索引实现 (适合小向量库; 接口形态与 HNSW/IVF 可互换)
#include "vector_index.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <mutex>
#include <new>
#include <queue>
#include <string>
#include <vector>

#include <memory>
namespace {

/* ---- 线程局部错误信息: 不返回堆指针, Go 侧只需在同一次调用后立刻拷贝 ---- */
thread_local std::string g_err;

void set_error(const std::string &msg) { g_err = msg; }

constexpr uint64_t kMagic   = 0x3130584449434556ULL; // "VECIDX01" 小端读取后的值
constexpr uint32_t kVersion = 1u;

bool all_finite(const float *p, size_t n) {
    for (size_t i = 0; i < n; ++i) {
        if (!std::isfinite(p[i])) return false;
    }
    return true;
}

struct HeapItem {
    float   distance;
    int64_t id;
    /* std::priority_queue 默认大顶堆: 让 distance 最大的在 top,
     * 新元素只要比 top 更近就替换, 遍历结束堆内即最小的 k 个 */
    bool operator<(const HeapItem &o) const { return distance < o.distance; }
};

} // namespace

struct vi_index {
    int32_t dim = 0;
    vi_metric_t metric = VI_METRIC_L2;

    std::vector<int64_t> ids;
    std::vector<float>   data; // n * dim, 行优先
    std::vector<float>   norms; // cosine: 原始 L2 范数; L2: 存平方范数加速
    mutable std::mutex   mu;

    float metric_distance(const float *q, size_t row) const {
        const float *v = &data[row * static_cast<size_t>(dim)];
        float d = 0.0f;
        if (metric == VI_METRIC_L2) {
            // ||q-v||^2 = ||q||^2 + ||v||^2 - 2 q.v
            float qq = 0.0f, qv = 0.0f;
            for (int32_t i = 0; i < dim; ++i) {
                qq += q[i] * q[i];
                qv += q[i] * v[i];
            }
            d = qq + norms[row] - 2.0f * qv;
            if (d < 0.0f) d = 0.0f; // 消除浮点误差导致的负值
        } else {
            // 库内向量已归一化存储: dist = 1 - q_unit . v_unit
            float qn = 0.0f, dot = 0.0f;
            for (int32_t i = 0; i < dim; ++i) {
                qn += q[i] * q[i];
                dot += q[i] * v[i];
            }
            qn = std::sqrt(qn);
            if (qn == 0.0f) return 1.0f;
            float cosine = dot / qn; // v 已是单位向量
            if (cosine > 1.0f) cosine = 1.0f;
            if (cosine < -1.0f) cosine = -1.0f;
            d = 1.0f - cosine;
        }
        return d;
    }
};

extern "C" {

vi_index_t *vi_index_create(const vi_config_t *config) {
    if (!config) { set_error("config is null"); return nullptr; }
    if (config->dim <= 0) { set_error("dim must be > 0"); return nullptr; }
    if (config->metric != VI_METRIC_L2 && config->metric != VI_METRIC_COSINE) {
        set_error("unknown metric"); return nullptr;
    }
    auto *idx = new (std::nothrow) vi_index_t();
    if (!idx) { set_error("out of memory"); return nullptr; }
    idx->dim = config->dim;
    idx->metric = static_cast<vi_metric_t>(config->metric);
    return idx;
}

void vi_index_free(vi_index_t **index) {
    if (!index || !*index) return;
    delete *index;
    *index = nullptr; // 悬垂指针防护
}

int vi_index_add(vi_index_t *index, const int64_t *ids,
                 const float *vectors, size_t n) {
    if (!index)  { set_error("index is null"); return 1; }
    if (n == 0)  return 0;
    if (!ids || !vectors) { set_error("ids/vectors is null"); return 1; }
    if (!all_finite(vectors, n * static_cast<size_t>(index->dim))) {
        set_error("vector contains NaN/Inf"); return 1;
    }

    std::lock_guard<std::mutex> lk(index->mu);
    const size_t old = index->ids.size();
    try {
        index->ids.reserve(old + n);
        index->data.reserve((old + n) * static_cast<size_t>(index->dim));
        index->norms.reserve(old + n);

        for (size_t r = 0; r < n; ++r) {
            const float *src = vectors + r * static_cast<size_t>(index->dim);
            float norm2 = 0.0f;
            for (int32_t i = 0; i < index->dim; ++i) norm2 += src[i] * src[i];

            index->ids.push_back(ids[r]);
            if (index->metric == VI_METRIC_L2) {
                index->norms.push_back(norm2);
                index->data.insert(index->data.end(), src, src + index->dim);
            } else {
                float nrm = std::sqrt(norm2);
                index->norms.push_back(nrm);
                if (nrm == 0.0f) {
                    set_error("zero vector cannot be added to cosine index");
                    // 回滚本批次
                    index->ids.resize(old);
                    index->data.resize(old * static_cast<size_t>(index->dim));
                    index->norms.resize(old);
                    return 1;
                }
                for (int32_t i = 0; i < index->dim; ++i)
                    index->data.push_back(src[i] / nrm);
            }
        }
    } catch (const std::bad_alloc &) {
        set_error("out of memory");
        return 1;
    }
    return 0;
}

int vi_index_search(const vi_index_t *index, const float *query,
                    size_t top_k, vi_result_t *results, size_t *out_n) {
    if (!index) { set_error("index is null"); return 1; }
    if (!query || !results || !out_n) { set_error("null argument"); return 1; }
    if (top_k == 0) { set_error("top_k must be > 0"); return 1; }

    std::priority_queue<HeapItem> heap;

    {
        std::lock_guard<std::mutex> lk(index->mu);
        const size_t total = index->ids.size();
        for (size_t row = 0; row < total; ++row) {
            float d = index->metric_distance(query, row);
            if (heap.size() < top_k) {
                heap.push({d, index->ids[row]});
            } else if (d < heap.top().distance) {
                heap.pop();
                heap.push({d, index->ids[row]});
            }
        }

        const size_t hit = heap.size();
        *out_n = hit;
        for (size_t i = hit; i-- > 0; ) {       // 堆顶是最远的, 从尾部向前填
            results[i].distance = heap.top().distance;
            results[i].id       = heap.top().id;
            heap.pop();
        }
    }
    return 0;
}

int vi_index_save(const vi_index_t *index, const char *path) {
    if (!index || !path) { set_error("null argument"); return 1; }
    std::lock_guard<std::mutex> lk(index->mu);

    std::ofstream f(path, std::ios::binary | std::ios::trunc);
    if (!f) { set_error(std::string("cannot open for write: ") + path); return 1; }

    const uint32_t dim = static_cast<uint32_t>(index->dim);
    const uint32_t metric = static_cast<uint32_t>(index->metric);
    const uint64_t n = index->ids.size();

    /* 固定小端布局: magic | version | dim | metric | n | ids | norms | data */
    auto wr = [&](const void *p, size_t bytes) {
        f.write(static_cast<const char *>(p), static_cast<std::streamsize>(bytes));
    };
    uint64_t magic = kMagic;
    uint32_t version = kVersion;
    wr(&magic, sizeof magic);
    wr(&version, sizeof version);
    wr(&dim, sizeof dim);
    wr(&metric, sizeof metric);
    wr(&n, sizeof n);
    if (n) {
        wr(index->ids.data(),   n * sizeof(int64_t));
        wr(index->norms.data(), n * sizeof(float));
        wr(index->data.data(),  n * dim * sizeof(float));
    }
    if (!f) { set_error("write failed (disk full?)"); return 1; }
    return 0;
}

vi_index_t *vi_index_load(const char *path) {
    if (!path) { set_error("path is null"); return nullptr; }
    std::ifstream f(path, std::ios::binary);
    if (!f) { set_error(std::string("cannot open: ") + path); return nullptr; }

    uint64_t magic = 0, n = 0;
    uint32_t version = 0, dim = 0, metric = 0;
    auto rd = [&](void *p, size_t bytes) -> bool {
        f.read(static_cast<char *>(p), static_cast<std::streamsize>(bytes));
        return static_cast<bool>(f);
    };
    if (!rd(&magic, sizeof magic) || magic != kMagic) {
        set_error("bad magic: not a vector index file"); return nullptr;
    }
    if (!rd(&version, sizeof version) || version != kVersion) {
        set_error("unsupported file version"); return nullptr;
    }
    if (!rd(&dim, sizeof dim) || !rd(&metric, sizeof metric) || !rd(&n, sizeof n)) {
        set_error("truncated header"); return nullptr;
    }
    if (dim == 0 || (metric != VI_METRIC_L2 && metric != VI_METRIC_COSINE)) {
        set_error("corrupt header fields"); return nullptr;
    }

    auto idx = std::unique_ptr<vi_index_t>(new vi_index_t());
    idx->dim = static_cast<int32_t>(dim);
    idx->metric = static_cast<vi_metric_t>(metric);
    if (n > 0) {
        idx->ids.resize(n);
        idx->norms.resize(n);
        idx->data.resize(n * dim);
        if (!rd(idx->ids.data(), n * sizeof(int64_t)) ||
            !rd(idx->norms.data(), n * sizeof(float)) ||
            !rd(idx->data.data(), n * dim * sizeof(float))) {
            set_error("truncated body"); return nullptr;
        }
    }
    return idx.release();
}

size_t vi_index_size(const vi_index_t *index) {
    if (!index) return 0;
    std::lock_guard<std::mutex> lk(index->mu);
    return index->ids.size();
}

int vi_index_dim(const vi_index_t *index, int32_t *out_dim) {
    if (!index || !out_dim) { set_error("null argument"); return 1; }
    std::lock_guard<std::mutex> lk(index->mu);
    *out_dim = index->dim;
    return 0;
}

int vi_index_metric(const vi_index_t *index, int32_t *out_metric) {
    if (!index || !out_metric) { set_error("null argument"); return 1; }
    std::lock_guard<std::mutex> lk(index->mu);
    *out_metric = static_cast<int32_t>(index->metric);
    return 0;
}

const char *vi_last_error(void) { return g_err.c_str(); }

} // extern "C"
