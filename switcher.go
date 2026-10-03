package sceneswitch

import "sync"

// Switcher 是直播场景切换器，由上层直播控制器调用。
// 场景、当前播出状态与切换记录均保存在内存。
type Switcher struct {
	mu      sync.Mutex
	cond    sync.Cond
	prepare PrepareFunc
	release ReleaseFunc

	cache   *materialCache
	ended   bool
	gen     uint64         // 最新已接纳请求的代际
	current *airedVersion  // 当前播出版本
	records []SwitchRecord // 按生效顺序排列的切换记录

	submits  int // 进行中的切换请求数（含阻塞在准备回调中的）
	reads    int // 未结束的读取数
	releases int // 进行中的释放回调数
}

// airedVersion 是已生效的播出版本及其持有的素材引用。
type airedVersion struct {
	scene   SceneVersion
	entries []*materialEntry
}

// releasedResource 是一份待在锁外释放的资源。
type releasedResource struct {
	id       string
	resource any
}

// New 创建切换器。prepare 与 release 均不允许为 nil。
func New(prepare PrepareFunc, release ReleaseFunc) *Switcher {
	s := &Switcher{prepare: prepare, release: release, cache: newMaterialCache()}
	s.cond.L = &s.mu
	return s
}

// Submit 提交一次切换请求。新请求一经接纳，此前尚未生效的请求即失效。
// 调用会阻塞直到本请求准备完成：成功生效返回 nil；
// 素材准备失败返回该错误；被更新请求取代或直播结束返回 ErrSuperseded；
// 直播已结束后提交返回 ErrEnded。
func (s *Switcher) Submit(scene SceneVersion) error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return ErrEnded
	}
	s.gen++
	myGen := s.gen
	s.submits++
	s.mu.Unlock()

	held := make([]*materialEntry, 0, len(scene.Slots))
	for _, id := range scene.Slots {
		entry, err := s.prepareOne(id)
		if err != nil {
			s.abandon(held)
			return err
		}
		held = append(held, entry)
	}

	s.mu.Lock()
	if s.ended || myGen != s.gen {
		// 已被更新的请求取代，或直播已结束：不能上播。
		toRelease := s.dropRefs(held)
		s.submits--
		s.cond.Broadcast()
		s.unlockAndRelease(toRelease)
		return ErrSuperseded
	}
	old := s.current
	s.current = &airedVersion{scene: scene, entries: held}
	s.records = append(s.records, SwitchRecord{
		Seq:     len(s.records) + 1,
		SceneID: scene.SceneID,
		Version: scene.Version,
	})
	var toRelease []releasedResource
	if old != nil {
		toRelease = s.dropRefs(old.entries)
	}
	s.submits--
	s.cond.Broadcast()
	s.unlockAndRelease(toRelease)
	return nil
}

// prepareOne 为调用方获取一份就绪素材的引用，必要时执行准备回调。
// 多个并发请求需要同一素材时共享一次准备结果。
func (s *Switcher) prepareOne(id string) (*materialEntry, error) {
	for {
		s.mu.Lock()
		entry, fresh := s.cache.acquire(id)
		if !fresh && !entry.ready {
			// 其他请求正在准备该素材，等待其完成或失败。
			for !entry.ready && !entry.failed {
				s.cond.Wait()
			}
			if entry.failed {
				// 准备失败：放弃该引用并重试，可能由本请求重新准备。
				entry.refs--
				s.mu.Unlock()
				continue
			}
			s.mu.Unlock()
			return entry, nil
		}
		if !fresh {
			s.mu.Unlock()
			return entry, nil
		}
		s.mu.Unlock()

		resource, err := s.prepare(id)

		s.mu.Lock()
		if err != nil {
			entry.failed = true
			delete(s.cache.entries, id)
			entry.refs--
			s.cond.Broadcast()
			s.mu.Unlock()
			return nil, err
		}
		entry.resource = resource
		entry.ready = true
		s.cond.Broadcast()
		s.mu.Unlock()
		return entry, nil
	}
}

// abandon 在准备失败后放弃已持有的引用并结束本请求。
func (s *Switcher) abandon(held []*materialEntry) {
	s.mu.Lock()
	toRelease := s.dropRefs(held)
	s.submits--
	s.cond.Broadcast()
	s.unlockAndRelease(toRelease)
}

// dropRefs 放弃一组素材引用，返回需要在锁外释放的资源。调用方须持锁。
func (s *Switcher) dropRefs(entries []*materialEntry) []releasedResource {
	var out []releasedResource
	for _, e := range entries {
		if resource, ok := s.cache.releaseRef(e); ok {
			out = append(out, releasedResource{id: e.id, resource: resource})
		}
	}
	return out
}

// unlockAndRelease 在持锁状态下登记释放任务，解锁后执行释放回调。
// 登记与解锁必须原子完成，否则 End 可能在回调登记前误判已排空。
func (s *Switcher) unlockAndRelease(toRelease []releasedResource) {
	if len(toRelease) > 0 {
		s.releases++
	}
	s.mu.Unlock()
	if len(toRelease) == 0 {
		return
	}
	for _, r := range toRelease {
		s.release(r.id, r.resource)
	}
	s.mu.Lock()
	s.releases--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Read 读取当前播出版本，返回的句柄在 Close 前保证版本与素材可用。
// 初始无播出场景返回 ErrNoScene，直播结束后返回 ErrEnded。
func (s *Switcher) Read() (*ReadHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, ErrEnded
	}
	if s.current == nil {
		return nil, ErrNoScene
	}
	for _, e := range s.current.entries {
		e.refs++
	}
	s.reads++
	return &ReadHandle{sw: s, scene: s.current.scene, entries: s.current.entries}, nil
}

// Records 返回按生效顺序排列的切换记录。
func (s *Switcher) Records() []SwitchRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SwitchRecord(nil), s.records...)
}

// End 结束直播：不再接纳切换与新读取，未生效请求全部失效，
// 等待所有准备、读取与释放完成后返回，此后没有遗留资源，
// 也不会再产生切换记录。重复调用是安全的。
func (s *Switcher) End() {
	s.mu.Lock()
	if !s.ended {
		s.ended = true
		s.gen++ // 使尚未生效的请求全部失效
	}
	var toRelease []releasedResource
	if s.current != nil {
		toRelease = s.dropRefs(s.current.entries)
		s.current = nil
	}
	s.unlockAndRelease(toRelease)

	s.mu.Lock()
	for s.submits > 0 || s.reads > 0 || s.releases > 0 {
		s.cond.Wait()
	}
	s.mu.Unlock()
}

// ReadHandle 是一次读取的句柄，持有播出版本与其素材的引用。
type ReadHandle struct {
	sw      *Switcher
	scene   SceneVersion
	entries []*materialEntry
	closed  bool
}

// Scene 返回本次读取锁定的播出版本。
func (h *ReadHandle) Scene() SceneVersion {
	return h.scene
}

// Resources 返回与场景槽位一一对应的素材资源句柄。
// 仅在 Close 之前调用才能保证资源有效。
func (h *ReadHandle) Resources() []any {
	h.sw.mu.Lock()
	defer h.sw.mu.Unlock()
	out := make([]any, len(h.entries))
	for i, e := range h.entries {
		out[i] = e.resource
	}
	return out
}

// Close 结束本次读取，释放对版本与素材的引用。重复调用是安全的。
func (h *ReadHandle) Close() {
	h.sw.mu.Lock()
	if h.closed {
		h.sw.mu.Unlock()
		return
	}
	h.closed = true
	toRelease := h.sw.dropRefs(h.entries)
	h.sw.reads--
	h.sw.cond.Broadcast()
	h.sw.unlockAndRelease(toRelease)
}
