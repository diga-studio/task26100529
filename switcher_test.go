package sceneswitch

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeBackend 是可控的素材准备/释放后端：支持按素材阻塞、失败，
// 并精确统计每份素材的准备与释放次数。
type fakeBackend struct {
	mu       sync.Mutex
	gates    map[string]chan struct{} // 准备前等待的闸门
	started  map[string]chan struct{} // 准备开始时关闭
	fail     map[string]error
	prepared map[string]int
	released map[string]int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		gates:    make(map[string]chan struct{}),
		started:  make(map[string]chan struct{}),
		fail:     make(map[string]error),
		prepared: make(map[string]int),
		released: make(map[string]int),
	}
}

func (f *fakeBackend) gate(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gates[id] == nil {
		f.gates[id] = make(chan struct{})
	}
	return f.gates[id]
}

func (f *fakeBackend) startedCh(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started[id] == nil {
		f.started[id] = make(chan struct{})
	}
	return f.started[id]
}

func (f *fakeBackend) setFail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[id] = err
}

func (f *fakeBackend) prepare(id string) (any, error) {
	f.mu.Lock()
	if ch := f.started[id]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	gate := f.gates[id]
	err := f.fail[id]
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.prepared[id]++
	f.mu.Unlock()
	return id + "#resource", nil
}

func (f *fakeBackend) release(id string, _ any) {
	f.mu.Lock()
	f.released[id]++
	f.mu.Unlock()
}

func (f *fakeBackend) count(m map[string]int, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[id]
}

func (f *fakeBackend) preparedCount(id string) int { return f.count(f.prepared, id) }
func (f *fakeBackend) releasedCount(id string) int { return f.count(f.released, id) }

// 较早请求最后准备完成、最新请求失败：旧请求不得复活，记录只含成功生效项。
func TestStaleRequestDoesNotRevive(t *testing.T) {
	backend := newFakeBackend()
	sw := New(backend.prepare, backend.release)

	base := SceneVersion{SceneID: "base", Version: 1, Slots: []string{"base.a", "base.b"}}
	if err := sw.Submit(base); err != nil {
		t.Fatalf("submit base: %v", err)
	}

	// 较早请求 A：素材准备缓慢，阻塞在闸门处。
	slow := SceneVersion{SceneID: "slow", Version: 2, Slots: []string{"slow.a"}}
	backend.gate("slow.a") // 预先放置闸门，确保准备阻塞
	slowErr := make(chan error, 1)
	go func() { slowErr <- sw.Submit(slow) }()
	<-backend.startedCh("slow.a") // 确认 A 已开始准备

	// 较新请求 B：素材立即失败。
	broken := SceneVersion{SceneID: "broken", Version: 3, Slots: []string{"broken.a"}}
	backend.setFail("broken.a", errors.New("disk error"))
	if err := sw.Submit(broken); err == nil || err.Error() != "disk error" {
		t.Fatalf("submit broken: got %v, want disk error", err)
	}

	// B 失败后，A 的准备才完成：A 已被取代，不能上播。
	close(backend.gate("slow.a"))
	if err := <-slowErr; !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale submit: got %v, want ErrSuperseded", err)
	}

	// 播出场景仍是 base，记录只含 base。
	h, err := sw.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if h.Scene().SceneID != "base" {
		t.Fatalf("on air = %s, want base", h.Scene().SceneID)
	}
	h.Close()

	records := sw.Records()
	if len(records) != 1 || records[0].SceneID != "base" || records[0].Seq != 1 {
		t.Fatalf("records = %+v, want only base", records)
	}

	// 失效请求准备出的资源必须释放恰好一次。
	if got := backend.preparedCount("slow.a"); got != 1 {
		t.Fatalf("prepared slow.a = %d, want 1", got)
	}
	if got := backend.releasedCount("slow.a"); got != 1 {
		t.Fatalf("released slow.a = %d, want 1", got)
	}

	sw.End()
	// 结束后 base 的素材也被释放恰好一次，没有遗留资源。
	for _, id := range []string{"base.a", "base.b"} {
		if got := backend.releasedCount(id); got != 1 {
			t.Fatalf("released %s = %d, want 1", id, got)
		}
	}
}

// 准备和读取尚未结束时结束直播：End 阻塞等待，
// 返回后每份成功资源恰好释放一次。
func TestEndWaitsForPrepareAndRead(t *testing.T) {
	backend := newFakeBackend()
	sw := New(backend.prepare, backend.release)

	base := SceneVersion{SceneID: "base", Version: 1, Slots: []string{"base.a", "base.b"}}
	if err := sw.Submit(base); err != nil {
		t.Fatalf("submit base: %v", err)
	}

	// 渲染端持有一次未结束的读取。
	handle, err := sw.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// 一个尚未生效的请求阻塞在素材准备中。
	pending := SceneVersion{SceneID: "pending", Version: 2, Slots: []string{"pending.a"}}
	backend.gate("pending.a") // 预先放置闸门，确保准备阻塞
	submitErr := make(chan error, 1)
	go func() { submitErr <- sw.Submit(pending) }()
	<-backend.startedCh("pending.a")

	// 结束直播：在准备与读取未完成前不得返回。
	endDone := make(chan struct{})
	go func() { sw.End(); close(endDone) }()
	select {
	case <-endDone:
		t.Fatal("End returned while prepare and read were still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	// 放行准备：请求因直播结束而失效，资源仍需释放恰好一次。
	close(backend.gate("pending.a"))
	if err := <-submitErr; !errors.Is(err, ErrSuperseded) {
		t.Fatalf("pending submit: got %v, want ErrSuperseded", err)
	}

	// 读取完成，End 应当返回。
	handle.Close()
	select {
	case <-endDone:
	case <-time.After(5 * time.Second):
		t.Fatal("End did not return after prepare and read finished")
	}

	// 每份成功准备的资源都恰好释放一次，没有遗留。
	for _, id := range []string{"base.a", "base.b", "pending.a"} {
		if p, r := backend.preparedCount(id), backend.releasedCount(id); p != 1 || r != 1 {
			t.Fatalf("%s prepared=%d released=%d, want 1/1", id, p, r)
		}
	}

	// 结束后不再接纳切换或新读取，记录不再变化。
	if err := sw.Submit(base); !errors.Is(err, ErrEnded) {
		t.Fatalf("submit after end: got %v, want ErrEnded", err)
	}
	if _, err := sw.Read(); !errors.Is(err, ErrEnded) {
		t.Fatalf("read after end: got %v, want ErrEnded", err)
	}
	records := sw.Records()
	if len(records) != 1 || records[0].SceneID != "base" {
		t.Fatalf("records = %+v, want only base", records)
	}
}

// 切换后旧版本读取可继续、新读取只能取得新版本；
// 共享素材不被提前释放；已释放素材可重新准备。
func TestReadConsistencyAndReprepare(t *testing.T) {
	backend := newFakeBackend()
	sw := New(backend.prepare, backend.release)

	v1 := SceneVersion{SceneID: "scene", Version: 1, Slots: []string{"m1", "m2"}}
	if err := sw.Submit(v1); err != nil {
		t.Fatalf("submit v1: %v", err)
	}

	// 旧版本读取开始于切换之前。
	oldRead, err := sw.Read()
	if err != nil {
		t.Fatalf("read old: %v", err)
	}

	// 切换到 v2：m2 共享，m3 新增，m1 仅被旧读取持有。
	v2 := SceneVersion{SceneID: "scene", Version: 2, Slots: []string{"m2", "m3"}}
	if err := sw.Submit(v2); err != nil {
		t.Fatalf("submit v2: %v", err)
	}

	// 旧读取仍持有 m1，不得释放。
	if got := backend.releasedCount("m1"); got != 0 {
		t.Fatalf("released m1 = %d while old read open, want 0", got)
	}
	if oldRead.Scene().Version != 1 {
		t.Fatalf("old read version = %d, want 1", oldRead.Scene().Version)
	}
	if res := oldRead.Resources(); len(res) != 2 || res[0] != "m1#resource" {
		t.Fatalf("old read resources = %v, want [m1#resource m2#resource]", res)
	}

	// 新开始的读取只能取得新版本。
	newRead, err := sw.Read()
	if err != nil {
		t.Fatalf("read new: %v", err)
	}
	if newRead.Scene().Version != 2 {
		t.Fatalf("new read version = %d, want 2", newRead.Scene().Version)
	}

	// 旧读取结束后 m1 释放恰好一次；m2 仍被使用，不得释放。
	oldRead.Close()
	if got := backend.releasedCount("m1"); got != 1 {
		t.Fatalf("released m1 = %d, want 1", got)
	}
	if got := backend.releasedCount("m2"); got != 0 {
		t.Fatalf("released m2 = %d while still in use, want 0", got)
	}
	newRead.Close()

	// 已释放的 m1 再次被使用：重新准备。
	v3 := SceneVersion{SceneID: "scene", Version: 3, Slots: []string{"m1"}}
	if err := sw.Submit(v3); err != nil {
		t.Fatalf("submit v3: %v", err)
	}
	if got := backend.preparedCount("m1"); got != 2 {
		t.Fatalf("prepared m1 = %d, want 2 (re-prepared)", got)
	}

	sw.End()
	// 每份素材的释放次数等于准备次数，没有遗留资源。
	for id, p := range backend.prepared {
		if r := backend.releasedCount(id); r != p {
			t.Fatalf("%s prepared=%d released=%d, want equal", id, p, r)
		}
	}
	records := sw.Records()
	if len(records) != 3 {
		t.Fatalf("records = %+v, want 3 entries", records)
	}
	for i, want := range []uint64{1, 2, 3} {
		if records[i].Version != want || records[i].Seq != i+1 {
			t.Fatalf("records[%d] = %+v, want version %d seq %d", i, records[i], want, i+1)
		}
	}
}
