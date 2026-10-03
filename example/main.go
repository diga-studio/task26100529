// 示例：上层直播控制器使用 sceneswitch 库完成场景切换。
// 素材是本地目录中的文件，准备回调读取文件内容，不做音视频解码。
package main

import (
	"embed"
	"fmt"
	"log"

	"sceneswitch"
)

//go:embed assets
var assets embed.FS

// 准备素材：从本地素材目录读入文件内容（示例中以 embed 内嵌）。
func prepare(materialID string) (any, error) {
	data, err := assets.ReadFile("assets/" + materialID)
	if err != nil {
		return nil, fmt.Errorf("prepare %s: %w", materialID, err)
	}
	fmt.Printf("[prepare] %s (%d bytes)\n", materialID, len(data))
	return data, nil
}

// 释放素材：示例中仅打印日志，实际场景可归还缓冲区等资源。
func release(materialID string, _ any) {
	fmt.Printf("[release] %s\n", materialID)
}

func main() {
	sw := sceneswitch.New(prepare, release)

	// 导播依次切入两个场景；每份版本内容由调用方完整提供。
	opening := sceneswitch.SceneVersion{
		SceneID: "opening",
		Version: 1,
		Slots:   []string{"opening.bg", "opening.bgm"},
	}
	if err := sw.Submit(opening); err != nil {
		log.Fatalf("submit opening: %v", err)
	}

	// 渲染端读取当前播出场景；读取期间版本与素材保持可用。
	handle, err := sw.Read()
	if err != nil {
		log.Fatalf("read: %v", err)
	}
	fmt.Printf("[on air] scene=%s version=%d slots=%v\n",
		handle.Scene().SceneID, handle.Scene().Version, handle.Scene().Slots)
	_ = handle.Resources() // 与槽位一一对应的素材资源
	handle.Close()

	interview := sceneswitch.SceneVersion{
		SceneID: "interview",
		Version: 3,
		Slots:   []string{"interview.bg", "interview.bgm"},
	}
	if err := sw.Submit(interview); err != nil {
		log.Fatalf("submit interview: %v", err)
	}

	// 结束直播：等待所有准备、读取与释放完成，此后没有遗留资源。
	sw.End()

	fmt.Println("[records]")
	for _, r := range sw.Records() {
		fmt.Printf("  #%d scene=%s version=%d\n", r.Seq, r.SceneID, r.Version)
	}
}
