// Package sceneswitch 提供直播场景切换能力：场景版本管理、
// 素材准备/释放的引用计数、切换记录与直播结束收尾。
package sceneswitch

import "errors"

// SceneVersion 是一份场景版本，内容由调用方完整提供且保持不变。
type SceneVersion struct {
	SceneID string   // 场景标识
	Version uint64   // 版本号
	Slots   []string // 有序素材槽位，元素为素材标识
}

// SwitchRecord 是一次成功生效的切换记录。
type SwitchRecord struct {
	Seq     int    // 生效顺序号，从 1 开始
	SceneID string // 场景标识
	Version uint64 // 版本号
}

// PrepareFunc 由调用方提供，准备本地素材，可能阻塞或失败。
// 返回的资源句柄原样透传给读取方与 ReleaseFunc。
type PrepareFunc func(materialID string) (any, error)

// ReleaseFunc 由调用方提供，释放 PrepareFunc 成功返回的资源。
// 每份成功准备的资源保证被释放恰好一次。
type ReleaseFunc func(materialID string, resource any)

var (
	// ErrEnded 表示直播已结束，不再接纳切换或新读取。
	ErrEnded = errors.New("sceneswitch: live ended")
	// ErrNoScene 表示初始状态下还没有播出场景。
	ErrNoScene = errors.New("sceneswitch: no scene on air")
	// ErrSuperseded 表示请求已被更新的请求取代，不能上播。
	ErrSuperseded = errors.New("sceneswitch: request superseded")
)
