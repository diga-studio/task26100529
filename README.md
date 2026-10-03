# sceneswitch

直播场景切换库，由上层直播控制器调用。场景、当前播出状态和切换记录保存在内存，
素材位于本地目录，不做音视频解码。

## 核心概念

- `SceneVersion`：一份场景版本，含场景标识、版本号和有序素材槽位，
  由调用方完整提供且保持不变；同一素材标识在一次直播内对应不变内容。
- `PrepareFunc` / `ReleaseFunc`：调用方提供的本地素材准备/释放回调，准备可能阻塞或失败。
- `Switcher`：切换器，提供 `Submit`（提交切换）、`Read`（读取当前场景）、
  `End`（结束直播）三类操作，初始没有播出场景。

## 行为保证

- 新请求一经接纳，此前尚未生效的请求即失效；失效请求即使稍后准备完成也不能上播。
- 场景所有素材准备成功后整体生效并产生切换记录；任一素材失败则保留原播出场景。
- 每次读取获得完整的播出版本及其素材，`Close` 前版本与素材保持可用；
  切换后旧版本读取可继续，新读取只能取得新版本。
- 当前场景、有效待切换请求、未结束读取使用的资源不会释放；
  无使用者的成功资源恰好释放一次（含失效请求准备出的资源）；已释放素材可重新准备。
- `End` 后不再接纳切换与新读取，等待所有准备、读取与释放完成后返回，无遗留资源。

## 使用

```go
sw := sceneswitch.New(prepare, release)
err := sw.Submit(sceneswitch.SceneVersion{SceneID: "opening", Version: 1, Slots: []string{"a.bg"}})
h, err := sw.Read()   // 渲染端读取当前播出场景
_ = h.Resources()     // 与槽位一一对应的素材资源
h.Close()
sw.End()              // 结束直播，等待全部收尾
records := sw.Records()
```

完整示例见 `example/main.go`，运行：`go run ./example`

## 测试

```sh
go test -race ./...
```
