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
import AVFoundation

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

// --- 转写 --------------------------------------------------------------------

// 一个模块的转写结果。置信度取各 run 的**最小值**：一句话里有一段没把握，整句就不能
// 算有把握（探针第 5 轮就是这么取的，两边的数要能对上）。
@available(macOS 26.0, *)
private final class TranscriberBox: @unchecked Sendable {
    var text = ""
    var confidence: Double = 0

    func collect(_ module: SpeechTranscriber) -> Task<Void, Never> {
        Task {
            do {
                for try await result in module.results {
                    text += String(result.text.characters)
                    let runs = result.text.runs
                        .compactMap { $0[AttributeScopes.SpeechAttributes.ConfidenceAttribute.self] }
                    if let min = runs.min() {
                        confidence = min
                    }
                }
            } catch {
                // 结果流出错时**不**改成空串：已经转出来的部分仍然是有用的信息，而
                // 长度为零的返回会让调用方以为「说的是静音」。
            }
        }
    }
}

// transcriber 造一个模块：preset.transcription 的选项本来就是空的，我们只需要额外
// 要那一个置信度属性——判据的第二层靠它（见 Go 侧 Choose）。
@available(macOS 26.0, *)
private func makeTranscriber(_ locale: String) -> SpeechTranscriber {
    SpeechTranscriber(locale: Locale(identifier: locale),
                      transcriptionOptions: [],
                      reportingOptions: [],
                      attributeOptions: [.transcriptionConfidence])
}

// transcribePair 用两个模块各自转写整段音频，交出一段 JSON。
//
// 为什么是两个模块而不是一个：「中文句子里嵌的英文词」只有 zh-CN 能读出来（en-US 读中文
// 是垃圾），而「纯英文的句子」zh-CN 会给出音译垃圾——两边各自跑，然后选一个，比任何单
// 模块都好。判据不在这里：这里只如实报出原文与置信度，选哪一个由 Go 侧决定（它是纯逻辑，
// 在主机上就能测）。
@available(macOS 26.0, *)
private func transcribePair(_ samples: [Int16], rate: Double) async -> String? {
    let zh = makeTranscriber("zh-CN")
    let en = makeTranscriber("en-US")
    let analyzer = SpeechAnalyzer(modules: [zh, en])
    let zhBox = TranscriberBox()
    let enBox = TranscriberBox()
    let zhTask = zhBox.collect(zh)
    let enTask = enBox.collect(en)

    // 喂 PCM 走流式入口，不写临时文件（探针第 6 轮验过：与文件模式逐字一致）。
    // 分块大小随便挑——512 与 1600 个样本的结果一模一样。
    guard let format = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: rate,
                                     channels: 1, interleaved: false) else {
        return nil
    }
    let (stream, continuation) = AsyncStream<AnalyzerInput>.makeStream()
    let starter = Task { try await analyzer.start(inputSequence: stream) }

    let chunk = 1600
    var index = 0
    while index < samples.count {
        let count = min(chunk, samples.count - index)
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format,
                                            frameCapacity: AVAudioFrameCount(count)) else { break }
        buffer.frameLength = AVAudioFrameCount(count)
        samples.withUnsafeBufferPointer { source in
            buffer.int16ChannelData![0].update(from: source.baseAddress! + index, count: count)
        }
        continuation.yield(AnalyzerInput(buffer: buffer))
        index += count
    }
    continuation.finish()

    do {
        try await starter.value
        try await analyzer.finalizeAndFinishThroughEndOfInput()
    } catch {
        return nil
    }
    _ = await zhTask.value
    _ = await enTask.value

    return "{\"zh\":\"" + jsonEscaped(zhBox.text) + "\",\"zhConf\":" + String(zhBox.confidence)
        + ",\"en\":\"" + jsonEscaped(enBox.text) + "\",\"enConf\":" + String(enBox.confidence) + "}"
}

// jsonEscaped 转义一个字符串，好把它塞进上面那行 JSON 里。
//
// 手写而不是用 JSONEncoder：要编码的是「两个字符串 + 两个数」，为此定义一个 Codable
// 结构体比自己转义更容易出错（真正的坑只有控制字符和引号，都在下面）。这段文本是识别
// 结果，里面的引号来自人说「引号」两个字，不是没有可能。
private func jsonEscaped(_ text: String) -> String {
    var out = ""
    for scalar in text.unicodeScalars {
        switch scalar {
        case "\"": out += "\\\""
        case "\\": out += "\\\\"
        case "\n": out += "\\n"
        case "\r": out += "\\r"
        case "\t": out += "\\t"
        default:
            if scalar.value < 0x20 {
                out += String(format: "\\u%04x", scalar.value)
            } else {
                out.unicodeScalars.append(scalar)
            }
        }
    }
    return out
}

// tachi_stt_transcribe 把一段 16 kHz 单声道 PCM 转成一句话。
//
// 入参是**裸 Int16 样本 + 采样率**：链路那头解出来的就是这个，全程不碰磁盘（设计文档 §8）。
// 返回值是一段 JSON（两个模块各自的结果），由 tachi_stt_free 释放；失败返回 NULL——
// 「做不到」必须能被调用方分辨出来，而不是给一个空串（那看起来像「人没说话」）。
//
// ⚠ 这一步需要「语音识别」权限（NSSpeechRecognitionUsageDescription）。与前几次一样，
// TCC 把授权归因给 **responsible process**，而且判定发生在第一次调用时——所以 app 的
// Info.plist 里必须有那个键，否则这里是**直接崩**，不是返回 NULL。
@_cdecl("tachi_stt_transcribe")
public func tachiSTTTranscribe(_ samples: UnsafePointer<Int16>?,
                               _ count: Int,
                               _ sampleRate: Double) -> UnsafeMutablePointer<CChar>? {
    guard #available(macOS 26.0, *) else { return nil }
    guard let samples = samples, count > 0, sampleRate > 0 else { return nil }

    let pcm = Array(UnsafeBufferPointer(start: samples, count: count))
    let json = syncWait { () async -> String? in
        await transcribePair(pcm, rate: sampleRate)
    }
    guard let json = json else { return nil }
    return strdup(json)
}
