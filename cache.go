package sceneswitch

// materialEntry 跟踪一份素材的准备状态与引用计数。
// 引用持有者：当前播出版本、有效待切换请求、未结束的读取。
type materialEntry struct {
	id       string
	resource any
	refs     int
	ready    bool // 准备回调已成功返回
	failed   bool // 准备回调已失败，等待者应放弃引用并重试
}

// materialCache 按素材标识共享已准备的素材。
// 同一素材标识在一次直播内对应不变内容，因此可以安全共享。
// 所有方法都必须在 Switcher.mu 持锁状态下调用。
type materialCache struct {
	entries map[string]*materialEntry
}

func newMaterialCache() *materialCache {
	return &materialCache{entries: make(map[string]*materialEntry)}
}

// acquire 返回素材标识对应的条目并将其引用计数加一。
// fresh 为 true 表示条目是新创建的，调用方负责执行准备回调。
func (c *materialCache) acquire(id string) (entry *materialEntry, fresh bool) {
	entry, ok := c.entries[id]
	if ok {
		entry.refs++
		return entry, false
	}
	entry = &materialEntry{id: id, refs: 1}
	c.entries[id] = entry
	return entry, true
}

// releaseRef 将条目引用计数减一；归零时从缓存移除并返回
// 待释放的资源，调用方负责在锁外执行释放回调。
func (c *materialCache) releaseRef(entry *materialEntry) (resource any, release bool) {
	entry.refs--
	if entry.refs > 0 || !entry.ready {
		return nil, false
	}
	delete(c.entries, entry.id)
	return entry.resource, true
}
