// tachi 的语音转写垫片（Swift 侧）。
//
// 为什么必须是 Swift：macOS 26 那套新的转写 API——SpeechAnalyzer / SpeechTranscriber /
// AssetInventory——**只有 Swift 有**。ObjC 头文件里只剩老的 SFSpeechRecognizer，而那个
// 已经不支持本机识别的新路线（见 docs/2026-09-29-badge-voice-answers-design.md §7）。
// 所以 desktop/ 里那条「内联 ObjC」的老路（webview_awake_darwin.go）在这里走不通，
// 只能让 swiftc 编出产物再链进 cgo——这是这件事引入的唯一新构建环节。
//
// ⚠ 编译目标是 **macOS 12.0**，和 app 的其余部分一致（build/darwin/Taskfile.yml），
// 而不是 26.0。这条不是洁癖：定了 26.0 的话，整个 app 在 macOS 15 上**连启动都不行**
// （二进制直接要 26 的符号），而不是「语音这一项不可用」。所以下面每一处 26 才有的
// 东西都关在 #available 里——它让 swiftc 把这些符号**弱链接**，老系统上它们不存在，
// 而我们也永远不会去调。这是这套东西「没能力不能装死」那条规矩在编译期的样子。
import Foundation
import Speech

// 同步等待一个 async 工作的结果。
//
// 这个桥是这层存在的理由：Swift 那边的 API 全是 async，而 Go 通过 cgo 只能调同步函数。
// 做法是信号量 + 一个分离的 Task——Task 跑在全局并发执行器上（不是调用线程），所以
// 阻塞调用线程不会把它一起卡住。
//
// ⚠ 这个「阻塞调用线程等 Task」的形状有个前提：调用方**不能是主线程**。Go 的 cgo 调用
// 跑在 Go 自己的线程上，所以成立；但哪天有人从 Swift 的主线程调进来，这里就是死锁。
private func syncWait<T>(_ work: @escaping () async -> T) -> T {
    let semaphore = DispatchSemaphore(value: 0)
    // nonisolated(unsafe)：只被 Task 写一次、且写入后才 signal，signal 之后才读。
    // 用锁包起来只是为了让编译器满意，代价比这层本来要做的事还大。
    nonisolated(unsafe) var result: T?
    Task {
        result = await work()
        semaphore.signal()
    }
    semaphore.wait()
    return result!
}

// tachi_stt_available 回答「这台机器上能不能做本机转写」。
//
// 返回 1/0 而不是 bool：C ABI 里 bool 的大小各编译器不一定一致，int32 没有歧义。
// 系统早于 macOS 26 时返回 0——不是「不知道」，是明确的「做不了」。
@_cdecl("tachi_stt_available")
public func tachiSTTAvailable() -> Int32 {
    guard #available(macOS 26.0, *) else { return 0 }
    return SpeechTranscriber.isAvailable ? 1 : 0
}

// tachi_stt_engine_note 返回一句「引擎现状」（已装资产的语言等）。
//
// 它存在的理由有两个，都是 P0 要证的东西：一是 Swift → Go 的**字符串**能过 C ABI，
// 二是上面的同步桥在真实数据上真的能通（`installedLocales` 是 async 的）。
//
// 返回值由 tachi_stt_free 释放；失败返回 NULL。
@_cdecl("tachi_stt_engine_note")
public func tachiSTTEngineNote() -> UnsafeMutablePointer<CChar>? {
    if #available(macOS 26.0, *) {
        let note = syncWait { () -> String in
            let installed = await SpeechTranscriber.installedLocales
                .map(\.identifier).sorted()
            let supported = await SpeechTranscriber.supportedLocales.count
            return "available=\(SpeechTranscriber.isAvailable ? 1 : 0) "
                + "installed=\(installed.joined(separator: ",")) "
                + "supported=\(supported)"
        }
        return strdup(note)
    }
    // 老系统上仍然回一句非空的话：调用方要能从「为什么不行」里分辨出这一种，
    // 而不是把「系统太老」和「垫片没链上」看成同一件事。
    return strdup("available=0 reason=macos-too-old")
}

// tachi_stt_free 释放上面那些字符串。
//
// 所有权刻意归 Go：Swift 这边 malloc 的缓冲由 Go 在拿到之后释放，而不是让 Go 拿着一个
// 指向 Swift 内部的指针——那种指针什么时候失效只有 Swift 知道，而这里没有任何东西
// 保证 Go 会在那之前放手。
@_cdecl("tachi_stt_free")
public func tachiSTTFree(_ pointer: UnsafeMutablePointer<CChar>?) {
    free(pointer)
}
