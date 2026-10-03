// 调用示例：模拟导播连续点击场景，素材从本地目录“准备”。
package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"liveswitch"
)

func main() {
	// 素材目录：本示例用本地文件充当素材，库本身不做音视频解码。
	dir, err := os.MkdirTemp("", "assets")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, id := range []string{"cam1", "cam2", "logo", "subtitle"} {
		if err := os.WriteFile(filepath.Join(dir, id+".bin"), []byte(id), 0o644); err != nil {
			log.Fatal(err)
		}
	}

	sw := liveswitch.New(liveswitch.Hooks{
		Prepare: func(assetID string) (liveswitch.Resource, error) {
			data, err := os.ReadFile(filepath.Join(dir, assetID+".bin"))
			if err != nil {
				return nil, err
			}
			time.Sleep(time.Duration(rand.Intn(80)) * time.Millisecond) // 模拟准备耗时
			fmt.Printf("prepared  %s\n", assetID)
			return data, nil
		},
		Release: func(assetID string, _ liveswitch.Resource) {
			fmt.Printf("released  %s\n", assetID)
		},
	})

	// 导播连续点击：前一个尚未生效就被新的取代。
	r1 := sw.Submit(liveswitch.SceneVersion{SceneID: "opening", Version: 1, Slots: []string{"cam1", "logo"}})
	r2 := sw.Submit(liveswitch.SceneVersion{SceneID: "interview", Version: 1, Slots: []string{"cam2", "logo", "subtitle"}})
	fmt.Printf("opening: %v\n", r1.Err()) // 已被取代
	if err := r2.Err(); err != nil {
		log.Fatalf("interview: %v", err)
	}

	// 渲染端读取当前播出场景。
	h, err := sw.Read()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("on air: %s v%d, %d slots\n", h.Version.SceneID, h.Version.Version, len(h.Resources))
	h.Close()

	sw.End()
	for _, rec := range sw.Records() {
		fmt.Printf("record #%d: %s v%d\n", rec.Seq, rec.SceneID, rec.Version)
	}
}
