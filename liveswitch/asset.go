package liveswitch

// assetEntry 是素材缓存中的一个条目：同一素材标识在一次直播内
// 只准备一次并被所有使用者共享，引用归零且无人使用时释放恰好一次。
type assetEntry struct {
	id    string
	res   Resource
	err   error // 准备失败原因（成功时为 nil）
	ready bool  // 准备已结束（无论成败）
	refs  int   // 当前使用者数量
	gone  bool  // 已从缓存移除（不再接纳新使用者）
	done  chan struct{}
}

// acquireLocked 返回该素材的缓存条目并增加一次引用，
// 缓存未命中时创建条目并启动后台准备。调用时需持有 s.mu。
func (s *Switcher) acquireLocked(id string) *assetEntry {
	if e, ok := s.assets[id]; ok {
		e.refs++
		return e
	}
	e := &assetEntry{id: id, refs: 1, done: make(chan struct{})}
	s.assets[id] = e
	s.inflight++
	go s.prepare(e)
	return e
}

// unrefLocked 减少一次引用；归零且准备已结束时移出缓存，
// 准备成功的资源交由后台释放恰好一次。调用时需持有 s.mu。
func (s *Switcher) unrefLocked(e *assetEntry) {
	e.refs--
	if e.refs == 0 && e.ready && !e.gone {
		delete(s.assets, e.id)
		e.gone = true
		if e.err == nil {
			s.startReleaseLocked(e)
		}
	}
}

// prepare 在后台执行调用方提供的准备回调。
func (s *Switcher) prepare(e *assetEntry) {
	res, err := s.h.Prepare(e.id)
	s.mu.Lock()
	e.res, e.err = res, err
	e.ready = true
	close(e.done)
	if err != nil || e.refs == 0 {
		// 失败条目不入缓存（之后可重新准备）；
		// 准备完成时已无使用者的成功资源立即释放。
		delete(s.assets, e.id)
		e.gone = true
		if err == nil {
			s.startReleaseLocked(e)
		}
	}
	s.inflight--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// startReleaseLocked 在后台释放资源，释放计入 inflight 以便 End 等待。
// 调用时需持有 s.mu。
func (s *Switcher) startReleaseLocked(e *assetEntry) {
	s.inflight++
	go func() {
		s.h.Release(e.id, e.res)
		s.mu.Lock()
		s.inflight--
		s.cond.Broadcast()
		s.mu.Unlock()
	}()
}
