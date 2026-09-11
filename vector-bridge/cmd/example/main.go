// 命令行示例: 读取小型向量文件 -> 创建索引 -> 添加向量 -> 查询 TopK
//
//	-> 保存/重载验证持久化 -> 打印相似结果。
//
// 向量文件格式 (UTF-8 文本, 支持空行和 # 注释):
//
//	# 第一行:  维度
//	4
//	# 之后每行: id v1 v2 v3 v4 ...
//	10  0.10 0.20 0.30 0.40
//	11  0.11 0.21 0.29 0.41
//
// 用法:
//
//	go run ./cmd/example -file testdata/vectors.txt -k 3 \
//	    -metric l2 -query "0.10,0.20,0.30,0.40" -save build/index.bin
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"vector-bridge/bridge"
)

type row struct {
	id  int64
	vec []float32
}

func loadVectors(path string) (dim int, rows []row, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if dim == 0 {
			if len(fields) != 1 {
				return 0, nil, fmt.Errorf("line %d: header must contain only dim", lineNo)
			}
			dim, err = strconv.Atoi(fields[0])
			if err != nil || dim <= 0 {
				return 0, nil, fmt.Errorf("line %d: bad dim %q", lineNo, fields[0])
			}
			continue
		}
		if len(fields) != dim+1 {
			return 0, nil, fmt.Errorf("line %d: expect id + %d floats, got %d fields",
				lineNo, dim, len(fields))
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("line %d: bad id: %w", lineNo, err)
		}
		v := make([]float32, dim)
		for i := 0; i < dim; i++ {
			x, err := strconv.ParseFloat(fields[i+1], 32)
			if err != nil {
				return 0, nil, fmt.Errorf("line %d col %d: %w", lineNo, i+2, err)
			}
			v[i] = float32(x)
		}
		rows = append(rows, row{id: id, vec: v})
	}
	if err := sc.Err(); err != nil {
		return 0, nil, err
	}
	if dim == 0 {
		return 0, nil, fmt.Errorf("empty vector file: %s", path)
	}
	if len(rows) == 0 {
		return 0, nil, fmt.Errorf("no vector rows in %s", path)
	}
	return dim, rows, nil
}

func parseQuery(s string, dim int) ([]float32, error) {
	parts := strings.Split(s, ",")
	if len(parts) != dim {
		return nil, fmt.Errorf("query needs %d comma-separated floats, got %d", dim, len(parts))
	}
	q := make([]float32, dim)
	for i, p := range parts {
		x, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("query element %d: %w", i, err)
		}
		q[i] = float32(x)
	}
	return q, nil
}

func printResults(rs []bridge.Result) {
	if len(rs) == 0 {
		fmt.Println("(index empty, no results)")
		return
	}
	for i, r := range rs {
		fmt.Printf("  #%d  id=%-4d distance=%.6f\n", i+1, r.ID, r.Distance)
	}
}

func main() {
	var (
		file   = flag.String("file", "testdata/vectors.txt", "vector text file")
		metric = flag.String("metric", "l2", "distance metric: l2|cosine")
		k      = flag.Int("k", 3, "top K")
		query  = flag.String("query", "", "comma-separated query vector")
		lib    = flag.String("lib", "", "path to native dynamic library (default: auto-detect / VECIDX_LIB)")
		save   = flag.String("save", "", "optional path to persist the index")
	)
	flag.Parse()

	m := bridge.L2
	if *metric == "cosine" {
		m = bridge.Cosine
	} else if *metric != "l2" {
		fmt.Fprintf(os.Stderr, "unknown metric: %s\n", *metric)
		os.Exit(2)
	}

	// 1) 运行时加载本地动态库。
	if err := bridge.Load(*lib); err != nil {
		fmt.Fprintf(os.Stderr, "load native library failed: %v\n", err)
		os.Exit(1)
	}

	// 2) 读取向量文件。
	dim, rows, err := loadVectors(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read vectors failed: %v\n", err)
		os.Exit(1)
	}
	ids := make([]int64, len(rows))
	flat := make([]float32, 0, len(rows)*dim)
	for i, r := range rows {
		ids[i] = r.id
		flat = append(flat, r.vec...)
	}
	fmt.Printf("loaded %d vectors, dim=%d, metric=%s\n", len(rows), dim, m)

	// 3) 创建索引, 并确保函数退出时释放原生句柄。
	idx, err := bridge.NewIndex(bridge.Config{Dim: dim, Metric: m})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create index failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := idx.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close index failed: %v\n", err)
		}
	}()

	// 4) 批量添加向量。
	if err := idx.Add(ids, flat); err != nil {
		fmt.Fprintf(os.Stderr, "add vectors failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("index size: %d\n", idx.Size())

	// 5) 查询向量。
	var q []float32
	if *query != "" {
		q, err = parseQuery(*query, dim)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	} else {
		// 默认拿文件里第一条向量当 query, 最近邻必然包含它自己 (距离 0)。
		q = rows[0].vec
		fmt.Printf("(no -query given, using first vector id=%d)\n", rows[0].id)
	}

	rs, err := idx.Search(q, *k)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("top %d nearest neighbors:\n", *k)
	printResults(rs)

	// 6) 保存索引并重新加载, 验证持久化与新句柄的生命周期。
	if *save != "" {
		if err := idx.Save(*save); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("index saved to %s; reloading...\n", *save)

		idx2, err := bridge.LoadIndex(*save)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reload failed: %v\n", err)
			os.Exit(1)
		}
		defer idx2.Close()
		fmt.Printf("reloaded index size: %d\n", idx2.Size())

		rs2, err := idx2.Search(q, *k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "search after reload failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("top K after reload:")
		printResults(rs2)
	}
}
