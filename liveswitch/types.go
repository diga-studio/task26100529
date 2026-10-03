package liveswitch

import "errors"

// SceneVersion 是一份完整的场景版本：场景标识、版本号与有序素材槽位。
// 版本内容由调用方完整提供且保持不变；同一素材标识在一次直播内对应不变内容。
type SceneVersion struct {
	SceneID string
	Version uint64
	Slots   []string // 有序素材标识列表
}

// Record 是一次成功生效的切换记录。
type Record struct {
	SceneID string
	Version uint64
	Seq     int // 生效顺序，从 1 开始
}

// Resource 是素材准备回调返回的不透明句柄，读取与释放时原样回传。
type Resource = any

// Hooks 由调用方提供，负责本地素材的准备与释放。两者都可能阻塞。
type Hooks struct {
	Prepare func(assetID string) (Resource, error)
	Release func(assetID string, res Resource)
}

var (
	// ErrNoScene 表示当前没有播出场景。
	ErrNoScene = errors.New("liveswitch: no scene on air")
	// ErrEnded 表示直播已结束，不再接纳切换或新读取。
	ErrEnded = errors.New("liveswitch: stream ended")
	// ErrSuperseded 表示请求已被更新的请求取代而失效。
	ErrSuperseded = errors.New("liveswitch: request superseded")
)
