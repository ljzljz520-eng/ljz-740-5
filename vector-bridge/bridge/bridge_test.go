package bridge

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// 没有原生库时(例如交叉编译环境)跳过, 有库时做完整往返验证。
func skipIfNoLib(t *testing.T) {
	t.Helper()
	// Load 在同一测试进程内只能成功一次; "already loaded" 是预期状态。
	if err := Load(""); err != nil {
		if lib == nil {
			t.Skipf("native library unavailable: %v", err)
		}
	}
}

func TestAddSearchL2(t *testing.T) {
	skipIfNoLib(t)

	idx, err := NewIndex(Config{Dim: 2, Metric: L2})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	ids := []int64{1, 2, 3, 4}
	vecs := []float32{
		0, 0,
		1, 0,
		0, 1,
		10, 10,
	}
	if err := idx.Add(ids, vecs); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if idx.Size() != 4 {
		t.Fatalf("Size = %d, want 4", idx.Size())
	}

	rs, err := idx.Search([]float32{0.1, 0.0}, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d results, want 2", len(rs))
	}
	if rs[0].ID != 1 || rs[1].ID != 2 {
		t.Fatalf("order = %d,%d, want 1,2", rs[0].ID, rs[1].ID)
	}
	if d := math.Abs(float64(rs[0].Distance - 0.01)); d > 1e-5 {
		t.Fatalf("nearest distance = %v, want 0.01", rs[0].Distance)
	}

	// k 大于索引规模时返回全部
	rs, _ = idx.Search([]float32{0, 0}, 10)
	if len(rs) != 4 {
		t.Fatalf("k>n returned %d, want 4", len(rs))
	}
}

func TestCosine(t *testing.T) {
	skipIfNoLib(t)
	idx, _ := NewIndex(Config{Dim: 2, Metric: Cosine})
	defer idx.Close()

	ids := []int64{1, 2}
	vecs := []float32{1, 0, -1, 0} // 同向/反向
	if err := idx.Add(ids, vecs); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rs, err := idx.Search([]float32{2, 0}, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rs[0].ID != 1 {
		t.Fatalf("nearest = %d, want 1", rs[0].ID)
	}
	if math.Abs(float64(rs[0].Distance)) > 1e-6 {
		t.Fatalf("cosine self-distance = %v, want 0", rs[0].Distance)
	}
	if math.Abs(float64(rs[1].Distance)-2.0) > 1e-6 {
		t.Fatalf("opposite distance = %v, want 2", rs[1].Distance)
	}
}

func TestSaveLoad(t *testing.T) {
	skipIfNoLib(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "idx.bin")

	idx, _ := NewIndex(Config{Dim: 3, Metric: L2})
	if err := idx.Add([]int64{7, 8}, []float32{1, 2, 3, 4, 5, 6}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index file missing: %v", err)
	}

	idx2, err := LoadIndex(path)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	defer idx2.Close()
	if idx2.Dim() != 3 || idx2.Size() != 2 {
		t.Fatalf("reloaded state = dim %d size %d", idx2.Dim(), idx2.Size())
	}
	if idx2.Metric() != L2 {
		t.Fatalf("reloaded Metric = %v, want l2", idx2.Metric())
	}
	rs, err := idx2.Search([]float32{1, 2, 3}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rs[0].ID != 7 || rs[0].Distance != 0 {
		t.Fatalf("reloaded search = %+v, want id 7 dist 0", rs[0])
	}
}

func TestSaveLoadCosineMetric(t *testing.T) {
	skipIfNoLib(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cosine.bin")

	idx, err := NewIndex(Config{Dim: 2, Metric: Cosine})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if err := idx.Add([]int64{1, 2}, []float32{1, 0, -1, 0}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := idx.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	loaded, err := LoadIndex(path)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	defer loaded.Close()

	// 回归: metric 持久化在文件头中, 加载后公开状态必须仍是 cosine,
	// 不能因 Go 侧零值回退成 l2。
	if loaded.Metric() != Cosine {
		t.Fatalf("loaded Metric = %v (%d), want cosine (%d)",
			loaded.Metric(), loaded.Metric(), Cosine)
	}
	if loaded.Dim() != 2 || loaded.Size() != 2 {
		t.Fatalf("reloaded state = dim %d size %d", loaded.Dim(), loaded.Size())
	}

	// 距离语义也必须仍是 cosine: 同向 0、反向 2;
	// 若底层被当成 L2, 反方向 (-1,0) 对 (2,0) 的平方欧氏距离会是 9 而非 2。
	rs, err := loaded.Search([]float32{2, 0}, 2)
	if err != nil {
		t.Fatalf("Search after reload: %v", err)
	}
	if len(rs) != 2 || rs[0].ID != 1 {
		t.Fatalf("reloaded search = %+v, want id 1 first", rs)
	}
	if math.Abs(float64(rs[0].Distance)) > 1e-6 {
		t.Fatalf("same-direction distance = %v, want 0 (cosine)", rs[0].Distance)
	}
	if math.Abs(float64(rs[1].Distance)-2.0) > 1e-6 {
		t.Fatalf("opposite distance = %v, want 2 (cosine); metric likely reloaded as l2",
			rs[1].Distance)
	}
}

func TestCloseIdempotentAndUseAfterClose(t *testing.T) {
	skipIfNoLib(t)
	idx, _ := NewIndex(Config{Dim: 1, Metric: L2})
	if err := idx.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("second Close should be nil, got %v", err)
	}
	if _, err := idx.Search([]float32{1}, 1); err != ErrClosed {
		t.Fatalf("Search after Close err = %v, want ErrClosed", err)
	}
	if err := idx.Add([]int64{1}, []float32{1}); err != ErrClosed {
		t.Fatalf("Add after Close err = %v, want ErrClosed", err)
	}
}

func TestValidation(t *testing.T) {
	skipIfNoLib(t)
	if _, err := NewIndex(Config{Dim: 0}); err == nil {
		t.Fatal("dim=0 should fail")
	}
	idx, _ := NewIndex(Config{Dim: 2, Metric: L2})
	defer idx.Close()
	if err := idx.Add([]int64{1}, []float32{1}); err == nil {
		t.Fatal("wrong vector width should fail")
	}
	if err := idx.Add([]int64{1}, []float32{1, float32(math.NaN())}); err == nil {
		t.Fatal("NaN should be rejected")
	}
	if _, err := idx.Search([]float32{1}, 1); err == nil {
		t.Fatal("wrong query width should fail")
	}
	if _, err := idx.Search([]float32{1, 1}, 0); err == nil {
		t.Fatal("k=0 should fail")
	}
}

func TestConcurrentSearch(t *testing.T) {
	skipIfNoLib(t)
	idx, _ := NewIndex(Config{Dim: 3, Metric: L2})
	defer idx.Close()

	ids := make([]int64, 500)
	flat := make([]float32, 500*3)
	for i := range ids {
		ids[i] = int64(i)
		flat[i*3] = float32(i)
	}
	if err := idx.Add(ids, flat); err != nil {
		t.Fatalf("Add: %v", err)
	}

	done := make(chan struct{})
	for w := 0; w < 8; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				if _, err := idx.Search([]float32{1, 2, 3}, 5); err != nil {
					t.Errorf("concurrent Search: %v", err)
					return
				}
			}
		}()
	}
	for w := 0; w < 8; w++ {
		<-done
	}
}
