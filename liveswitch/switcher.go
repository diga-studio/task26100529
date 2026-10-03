package liveswitch

import "sync"

// Switcher 是直播场景切换器，由上层直播控制器调用。
// 场景、当前播出状态与切换记录全部保存在内存。
type Switcher struct {
	h Hooks

	mu   sync.Mutex
	cond sync.Cond

	assets      map[string]*assetEntry // 素材缓存（不含失败条目）
	current     *onAir                 // 当前播出版本，初始为 nil
	pending     *pendingReq            // 尚未生效的最新请求
	records     []Record               // 按生效顺序排列
	ended       bool
	activeReads int // 尚未结束的读取数
	inflight    int // 进行中的准备与释放回调数
}

// onAir 是播出版本及其持有的素材条目（按素材标识去重）。
type onAir struct {
	version SceneVersion
	entries []*assetEntry
}

// pendingReq 是尚未生效的切换请求。
type pendingReq struct {
	version SceneVersion
	entries []*assetEntry // 已引用的素材（按标识去重）
	result  *Request
	dead    bool // 已被更新的请求取代或被 End 失效
}

// New 创建一个切换器。初始没有播出场景。
func New(h Hooks) *Switcher {
	s := &Switcher{h: h, assets: make(map[string]*assetEntry)}
	s.cond.L = &s.mu
	return s
}

// Request 是一次已接纳切换请求的句柄，用于等待其最终结局。
type Request struct {
	done chan struct{}
	err  error
}

// Done 在请求尘埃落定（生效、失败或失效）后关闭。
func (r *Request) Done() <-chan struct{} { return r.done }

// Err 返回请求失败原因；成功生效时返回 nil。
func (r *Request) Err() error {
	<-r.done
	return r.err
}

// Submit 提交一次切换。新请求一经接纳，便使此前尚未生效的请求失效。
// 素材准备在后台进行；返回的 Request 可用于等待结果。
func (s *Switcher) Submit(v SceneVersion) *Request {
	r := &Request{done: make(chan struct{})}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		r.err = ErrEnded
		close(r.done)
		return r
	}
	if s.pending != nil {
		s.killPendingLocked(s.pending)
	}
	p := &pendingReq{version: v, result: r}
	seen := make(map[string]bool, len(v.Slots))
	for _, id := range v.Slots {
		if seen[id] {
			continue
		}
		seen[id] = true
		p.entries = append(p.entries, s.acquireLocked(id))
	}
	s.pending = p
	s.mu.Unlock()
	go s.settle(p)
	return r
}

// killPendingLocked 使尚未生效的请求失效；其素材引用由 settle 统一回收。
// 调用时需持有 s.mu。
func (s *Switcher) killPendingLocked(p *pendingReq) {
	p.dead = true
}

// settle 等待请求的全部素材准备结束，然后整体生效或失败收场。
// 素材引用统一在此回收：失效请求稍后准备出的资源也在此释放。
func (s *Switcher) settle(p *pendingReq) {
	var prepErr error
	for _, e := range p.entries {
		<-e.done
		if e.err != nil && prepErr == nil {
			prepErr = e.err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == p {
		s.pending = nil
	}
	switch {
	case p.dead:
		// 已被更新的请求取代或直播已结束：不能上播。
		p.result.err = ErrSuperseded
	case prepErr != nil:
		// 任一素材失败则请求失败，保留原播出场景，不产生记录。
		p.result.err = prepErr
	default:
		// 整体生效：旧播出场景的引用移交前不能释放其仍在读取的资源，
		// 这里仅减少切换器对它的持有，活跃读取仍各自持有引用。
		if s.current != nil {
			for _, e := range s.current.entries {
				s.unrefLocked(e)
			}
		}
		s.current = &onAir{version: p.version, entries: p.entries}
		p.entries = nil // 引用所有权移交给播出版本
		s.records = append(s.records, Record{
			SceneID: p.version.SceneID,
			Version: p.version.Version,
			Seq:     len(s.records) + 1,
		})
	}
	for _, e := range p.entries {
		s.unrefLocked(e)
	}
	close(p.result.done)
	s.cond.Broadcast()
}

// Read 读取当前播出的完整版本及其素材。
// 返回的句柄在 Close 前保证版本与素材均保持可用。
// 直播结束后或尚无播出场景时返回错误。
func (s *Switcher) Read() (*ReadHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, ErrEnded
	}
	if s.current == nil {
		return nil, ErrNoScene
	}
	res := make([]Resource, len(s.current.version.Slots))
	byID := make(map[string]*assetEntry, len(s.current.entries))
	for _, e := range s.current.entries {
		byID[e.id] = e
		e.refs++ // 读取对去重后的每个素材各持有一次引用
	}
	for i, id := range s.current.version.Slots {
		res[i] = byID[id].res
	}
	s.activeReads++
	return &ReadHandle{
		Version:   s.current.version,
		Resources: res,
		s:         s,
		entries:   s.current.entries,
	}, nil
}

// ReadHandle 是一次进行中的读取。
type ReadHandle struct {
	Version   SceneVersion
	Resources []Resource // 与 Version.Slots 一一对应

	s       *Switcher
	entries []*assetEntry
	closed  bool
}

// Close 结束本次读取，释放对版本与素材的占用。
// 重复调用安全，仅首次生效。
func (rh *ReadHandle) Close() {
	s := rh.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if rh.closed {
		return
	}
	rh.closed = true
	for _, e := range rh.entries {
		s.unrefLocked(e)
	}
	s.activeReads--
	s.cond.Broadcast()
}

// Records 返回按生效顺序排列的切换记录副本。
func (s *Switcher) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// End 结束直播：不再接纳切换或新读取，尚未生效的请求全部失效，
// 等待所有准备、读取与释放完成后返回，此时没有遗留资源。
func (s *Switcher) End() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if s.pending != nil {
		s.killPendingLocked(s.pending)
		s.pending = nil
	}
	if s.current != nil {
		for _, e := range s.current.entries {
			s.unrefLocked(e)
		}
		s.current = nil
	}
	// 等待所有准备、读取与释放完成；此后没有遗留资源。
	for s.inflight > 0 || s.activeReads > 0 {
		s.cond.Wait()
	}
	s.mu.Unlock()
}
