package liveswitch

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// rig 是可编程的素材回调：按素材标识控制准备阻塞/失败与释放阻塞，并统计次数。
type rig struct {
	mu           sync.Mutex
	prepareGate  map[string]chan struct{}
	prepareErr   map[string]error
	releaseGate  map[string]chan struct{}
	prepareCount map[string]int
	releaseCount map[string]int
}

func newRig() *rig {
	return &rig{
		prepareGate:  make(map[string]chan struct{}),
		prepareErr:   make(map[string]error),
		releaseGate:  make(map[string]chan struct{}),
		prepareCount: make(map[string]int),
		releaseCount: make(map[string]int),
	}
}

func (g *rig) hooks() Hooks {
	return Hooks{
		Prepare: func(id string) (Resource, error) {
			g.mu.Lock()
			g.prepareCount[id]++
			gate, err := g.prepareGate[id], g.prepareErr[id]
			g.mu.Unlock()
			if gate != nil {
				<-gate
			}
			if err != nil {
				return nil, err
			}
			return id + "-res", nil
		},
		Release: func(id string, _ Resource) {
			g.mu.Lock()
			gate := g.releaseGate[id]
			g.mu.Unlock()
			if gate != nil {
				<-gate
			}
			g.mu.Lock()
			g.releaseCount[id]++
			g.mu.Unlock()
		},
	}
}

func (g *rig) open(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.prepareGate[id])
	g.prepareGate[id] = nil
}

func (g *rig) counts(id string) (prep, rel int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prepareCount[id], g.releaseCount[id]
}

// waitFor 轮询直到条件满足，避免测试依赖固定睡眠。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// 交错：较早请求最后准备完成、最新请求失败。
// 旧请求不得复活，记录只含成功生效项。
func TestInterleavedSupersedeAndFailure(t *testing.T) {
	g := newRig()
	g.prepareGate["a1"] = make(chan struct{}) // A 的素材一直阻塞，最后才完成
	g.prepareErr["c1"] = errors.New("boom")   // C 的素材准备失败
	sw := New(g.hooks())

	reqA := sw.Submit(SceneVersion{SceneID: "A", Version: 1, Slots: []string{"a1"}})
	reqB := sw.Submit(SceneVersion{SceneID: "B", Version: 1, Slots: []string{"b1"}})
	if err := reqB.Err(); err != nil {
		t.Fatalf("B should take effect: %v", err)
	}
	reqC := sw.Submit(SceneVersion{SceneID: "C", Version: 1, Slots: []string{"c1"}})
	if err := reqC.Err(); err == nil {
		t.Fatal("C should fail")
	}

	// C 失败后，B 仍在播出。
	h, err := sw.Read()
	if err != nil {
		t.Fatal(err)
	}
	if h.Version.SceneID != "B" {
		t.Fatalf("on air = %s, want B", h.Version.SceneID)
	}
	h.Close()

	// 此刻才放行最早请求的素材：A 不得复活。
	g.open("a1")
	if err := reqA.Err(); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("A err = %v, want ErrSuperseded", err)
	}
	waitFor(t, "a1 released", func() bool {
		p, r := g.counts("a1")
		return p == 1 && r == 1 // 失效请求准备出的资源也必须释放恰好一次
	})

	h, err = sw.Read()
	if err != nil {
		t.Fatal(err)
	}
	if h.Version.SceneID != "B" {
		t.Fatalf("A revived! on air = %s, want B", h.Version.SceneID)
	}
	h.Close()

	sw.End()
	recs := sw.Records()
	if len(recs) != 1 || recs[0].SceneID != "B" || recs[0].Seq != 1 {
		t.Fatalf("records = %+v, want only B", recs)
	}
}

// 结束直播时准备与读取尚未结束：End 必须等待，
// 返回后每份成功资源恰好释放一次，且没有遗留。
func TestEndWaitsForPrepareReadAndRelease(t *testing.T) {
	g := newRig()
	g.prepareGate["x1"] = make(chan struct{}) // V2 的准备被卡住
	g.releaseGate["p1"] = make(chan struct{}) // p1 的释放被卡住
	sw := New(g.hooks())

	// V1 上播并持有一个未结束的读取。
	if err := sw.Submit(SceneVersion{SceneID: "V1", Version: 1, Slots: []string{"p1", "p2"}}).Err(); err != nil {
		t.Fatal(err)
	}
	reader, err := sw.Read()
	if err != nil {
		t.Fatal(err)
	}

	// V2 提交后准备被卡住；随后 End 使其失效。
	reqV2 := sw.Submit(SceneVersion{SceneID: "V2", Version: 1, Slots: []string{"x1"}})

	ended := make(chan struct{})
	go func() {
		sw.End()
		close(ended)
	}()

	assertBlocked := func(what string) {
		t.Helper()
		select {
		case <-ended:
			t.Fatalf("End returned while %s", what)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// 准备未结束 → End 不返回；放行准备后，失效的 V2 资源要释放。
	assertBlocked("prepare in flight")
	g.open("x1")
	if err := reqV2.Err(); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("V2 err = %v, want ErrSuperseded", err)
	}

	// 读取未结束 → End 不返回；p1 的释放还被卡住。
	assertBlocked("read still open")
	reader.Close()
	assertBlocked("release of p1 blocked")
	close(g.releaseGate["p1"])

	<-ended

	// End 返回后：每份成功准备（含失效请求准备出的）都恰好释放一次。
	for _, id := range []string{"p1", "p2", "x1"} {
		p, r := g.counts(id)
		if p != 1 || r != 1 {
			t.Fatalf("asset %s: prepared %d, released %d, want 1/1", id, p, r)
		}
	}

	// 结束后不再接纳切换或新读取。
	if err := sw.Submit(SceneVersion{SceneID: "V3", Version: 1, Slots: []string{"z1"}}).Err(); !errors.Is(err, ErrEnded) {
		t.Fatalf("submit after End: %v, want ErrEnded", err)
	}
	if _, err := sw.Read(); !errors.Is(err, ErrEnded) {
		t.Fatalf("read after End: %v, want ErrEnded", err)
	}
	if recs := sw.Records(); len(recs) != 1 || recs[0].SceneID != "V1" {
		t.Fatalf("records = %+v, want only V1", recs)
	}
}

// 切换后已经开始的旧版本读取可以继续，其素材在读取结束前不得释放；
// 新开始的读取只能取得新版本。
func TestReadPinsOldVersion(t *testing.T) {
	g := newRig()
	sw := New(g.hooks())

	if err := sw.Submit(SceneVersion{SceneID: "V1", Version: 1, Slots: []string{"old", "shared"}}).Err(); err != nil {
		t.Fatal(err)
	}
	old, err := sw.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Submit(SceneVersion{SceneID: "V2", Version: 2, Slots: []string{"new", "shared"}}).Err(); err != nil {
		t.Fatal(err)
	}

	// 旧读取仍持有 V1 的素材：old 不得释放，shared 不得重复准备。
	if p, r := g.counts("old"); p != 1 || r != 0 {
		t.Fatalf("old: prepared %d released %d, want 1/0 while read open", p, r)
	}
	if p, _ := g.counts("shared"); p != 1 {
		t.Fatalf("shared prepared %d times, want 1 (reused)", p)
	}
	if old.Resources[0] != "old-res" {
		t.Fatalf("old read resource = %v", old.Resources[0])
	}

	// 新读取只能取得 V2。
	fresh, err := sw.Read()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version.SceneID != "V2" || fresh.Resources[0] != "new-res" {
		t.Fatalf("fresh read = %+v, want V2", fresh.Version)
	}
	fresh.Close()

	// 旧读取结束后 old 才释放，且恰好一次。
	old.Close()
	waitFor(t, "old released", func() bool {
		_, r := g.counts("old")
		return r == 1
	})

	sw.End()
	if p, r := g.counts("new"); p != 1 || r != 1 {
		t.Fatalf("new: prepared %d released %d, want 1/1", p, r)
	}
	if p, r := g.counts("shared"); p != 1 || r != 1 {
		t.Fatalf("shared: prepared %d released %d, want 1/1", p, r)
	}
}

// 已释放的素材之后再被使用，可以重新准备。
func TestReleasedAssetCanBeReprepared(t *testing.T) {
	g := newRig()
	sw := New(g.hooks())

	v1 := SceneVersion{SceneID: "S", Version: 1, Slots: []string{"m"}}
	if err := sw.Submit(v1).Err(); err != nil {
		t.Fatal(err)
	}
	// 切到不含 m 的场景，m 被释放。
	if err := sw.Submit(SceneVersion{SceneID: "S", Version: 2, Slots: []string{"n"}}).Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "m released", func() bool {
		_, r := g.counts("m")
		return r == 1
	})
	// 再次使用 m：重新准备。
	if err := sw.Submit(SceneVersion{SceneID: "S", Version: 3, Slots: []string{"m"}}).Err(); err != nil {
		t.Fatal(err)
	}
	if p, _ := g.counts("m"); p != 2 {
		t.Fatalf("m prepared %d times, want 2", p)
	}
	sw.End()
	if _, r := g.counts("m"); r != 2 {
		t.Fatalf("m released %d times, want 2", r)
	}
}
