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
    // failure 是结果流自己报的错。它非空就说明这一门这次没读成——即使 text 里已经攒下了
    // 半句（那半句仍然有用，但没有资格算「这一门跑成了」）。
    var failure: String?

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
                // 出错时**不**把 text 清成空串：已经转出来的部分仍然是有用的信息，而
                // 长度为零的返回会让调用方以为「说的是静音」。但这件事要记下来——「这一
                // 门没读成」和「这一门听见了但没人说话」必须分得开（后者 text 空、
                // failure 也空）。
                failure = speechErrorText(error)
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

// speechErrorText 把 Speech.framework 的错误铺成一行能进日志的话。
//
// 带上域名和错误码不是啰嗦：这条链上最容易发生的一件事是「某门语言的离线识别资产不在」，
// 而它的原文是 `SFSpeechErrorDomain 4: No GeneralASR asset for language en`——只有原文
// 说得清是哪一门。把它压成一句「转写失败」，排查就得回到系统日志里去捞（那正是这一次
// 发生的事）。
private func speechErrorText(_ error: Error) -> String {
    let ns = error as NSError
    return "\(ns.domain) \(ns.code): \(ns.localizedDescription)"
}

// Module 是一个转写模块：key 是它在结果 JSON 里的名字（Go 侧的 Choose 认这两个键），
// locale 是交给 Speech 的语言标识。
private struct Module {
    var key: String
    var locale: String
}

// sttModules 是设计定的那两个模块，两门缺一门都不算完。
//
// 为什么是两个模块而不是一个：「中文句子里嵌的英文词」只有 zh-CN 能读出来（en-US 读中文
// 是垃圾），而「纯英文的句子」zh-CN 会给出音译垃圾——两边各自跑，然后选一个，比任何单
// 模块都好。判据不在这里：这里只如实报出原文与置信度，选哪一个由 Go 侧决定（它是纯逻辑，
// 在主机上就能测）。
private let sttModules = [Module(key: "zh", locale: "zh-CN"), Module(key: "en", locale: "en-US")]

// Said 是一个模块这一次说的话。
private struct Said {
    var text: String
    var conf: Double
}

// Attempt 是一次尝试的结果：跑成了的模块各自的原文（键是 Module.key），或者整次失败的
// 原因。
//
// 「跑成了」不等于「有话」：跑成的模块可以交出空串（按下了但没说话），那是正常输入。
// 没跑成的模块不出现在 said 里——这个区别就是 missing 那个字段的全部依据。
private struct Attempt {
    var said: [String: Said] = [:]
    var error: String?
}

// transcribeOnce 用一个 analyzer 跑给定的这几个模块，交回各自的原文与置信度。
//
// 喂 PCM 走流式入口，不写临时文件（探针第 6 轮验过：与文件模式逐字一致，§8 承诺了音频
// 不落盘）。分块大小随便挑——512 与 1600 个样本的结果一模一样。
@available(macOS 26.0, *)
private func transcribeOnce(_ samples: [Int16], rate: Double, modules wanted: [Module]) async -> Attempt {
    let transcribers = wanted.map { makeTranscriber($0.locale) }
    let boxes = wanted.map { _ in TranscriberBox() }
    let analyzer = SpeechAnalyzer(modules: transcribers)
    let collectors = zip(boxes, transcribers).map { $0.0.collect($0.1) }

    guard let format = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: rate,
                                     channels: 1, interleaved: false) else {
        return Attempt(error: "no Int16 format for \(rate) Hz")
    }
    let (stream, continuation) = AsyncStream<AnalyzerInput>.makeStream()
    let starter = Task { try await analyzer.start(inputSequence: stream) }

    let chunk = 1600
    var index = 0
    while index < samples.count {
        let count = min(chunk, samples.count - index)
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format,
                                            frameCapacity: AVAudioFrameCount(count)) else {
            continuation.finish()
            return Attempt(error: "no PCM buffer for \(count) samples")
        }
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
        return Attempt(error: speechErrorText(error))
    }
    for collector in collectors {
        _ = await collector.value
    }

    var said: [String: Said] = [:]
    for (box, module) in zip(boxes, wanted) where box.failure == nil {
        said[module.key] = Said(text: box.text, conf: box.confidence)
    }
    if said.isEmpty {
        // 一门都没跑成：原因来自各模块的结果流。不能因为 analyzer 说自己收尾干净就当成
        // 一次成功的转写——那正是「静默失败」最容易被放过去的地方。
        let reasons = zip(boxes, wanted).compactMap { $0.0.failure }
        return Attempt(error: reasons.isEmpty ? "no module produced a result" : reasons.joined(separator: "; "))
    }
    return Attempt(said: said)
}

// transcribe 是 tachi_stt_transcribe 的实体：跑那两个模块，交回一行 JSON。
//
// 为什么要逐门兜底：两个模块**共用一个 analyzer**，而任何一门语言的离线资产不在，整条
// input loop 就带着另一门一起死——实测过一次（缺 en 的资产，于是连纯中文的那句话也一起
// 没了）。所以整对失败时逐门再试一遍：能用的那门的话仍然交得出去，缺资产那门进 missing。
@available(macOS 26.0, *)
private func transcribe(_ samples: [Int16], rate: Double) async -> String {
    var attempt = await transcribeOnce(samples, rate: rate, modules: sttModules)
    var reason = attempt.error ?? ""

    if attempt.said.isEmpty {
        // 逐门问一遍。这里拿到的是**每一门各自**的原因，比整对那句准（整对那句只报第一
        // 个倒下的语言）。
        var reasons: [String] = []
        for module in sttModules {
            let one = await transcribeOnce(samples, rate: rate, modules: [module])
            if let said = one.said[module.key] {
                attempt.said[module.key] = said
            } else if let why = one.error {
                reasons.append(why)
            }
        }
        if !reasons.isEmpty {
            reason = reasons.joined(separator: "; ")
        }
    }

    let missing = sttModules.filter { attempt.said[$0.key] == nil }
    var installing: [String] = []
    for module in missing {
        // 问一次系统「这门语言的资产要不要装」。答案要进 JSON：屏幕上那句话（和日志）得
        // 分辨「缺资产，正在下载」与「这里坏了」——这两件事对用户的意义完全不同。
        if await installNeeded(module) {
            installing.append(module.locale)
        }
    }
    if attempt.said.isEmpty && reason.isEmpty {
        reason = "no module produced a result"
    }
    return transcriptionJSON(said: attempt.said, missing: missing.map(\.locale),
                             installing: installing, error: attempt.said.isEmpty ? reason : nil)
}

// transcriptionJSON 拼出交回 Go 的那行 JSON。
//
// 形状固定（四个结果键一直在，没跑成的模块给空串和 0）：Go 侧一次 unmarshal 就能拿到全部。
// `missing` / `installing` / `error` 是这一次转写的**账**——给人看的原因，不是解码用的数据：
//   - missing：这次没跑成的模块（按 locale 报），不管是什么原因；
//   - installing：其中系统确认「缺离线资产」、已经发起安装的那些（missing 的子集）。
//     两件事必须分得开：缺资产会自己好，别的不会，而屏幕上要对用户说的是不一样的话。
//   - error：一门都没跑成时的原因原文。
@available(macOS 26.0, *)
private func transcriptionJSON(said: [String: Said], missing: [String], installing: [String],
                               error: String?) -> String {
    var out = "{"
    for module in sttModules {
        let got = said[module.key] ?? Said(text: "", conf: 0)
        out += "\"\(module.key)\":\"" + jsonEscaped(got.text) + "\","
        out += "\"\(module.key)Conf\":" + String(got.conf) + ","
    }
    out += "\"missing\":" + jsonArray(missing)
    out += ",\"installing\":" + jsonArray(installing)
    if let error = error {
        out += ",\"error\":\"" + jsonEscaped(error) + "\""
    }
    return out + "}"
}

// jsonArray 拼一个字符串数组。元素是 locale 标识或模块名，转义照旧走 jsonEscaped——
// 手写 JSON 的规矩是「每一处字符串都过同一道口子」，不给例外。
private func jsonArray(_ items: [String]) -> String {
    "[" + items.map { "\"" + jsonEscaped($0) + "\"" }.joined(separator: ",") + "]"
}

// installNeeded 回答「这门语言的离线资产，系统说要不要装」，要装就顺手在后台装上。
//
// 为什么不能等系统自己发现：这台机器上的资产是**按需**的，没有订阅者时会被系统清掉。
// 实测过一次——English 的资产不在，转写整条失败，而系统两秒之后才开始下载它。也就是说
// 用户的第一句话注定赶不上；而说话这件事本身就是「需要」的证明。
//
// 为什么不等它装完：一次下载上百 MB，把它压在「按住说话」这条路上，比「这次只用能用的
// 那门语言」更糟。请求发出去就走，下一次说话它可能就已经在了。
//
// 答案记在 installAsked 里：缺资产的失败每次说话都会复现，既没必要每次都去问系统，也
// 不能每次都给同一个资产起一个下载任务。
@available(macOS 26.0, *)
private func installNeeded(_ module: Module) async -> Bool {
    if let known = askedBefore(module.key) {
        return known
    }

    var needed = false
    do {
        if let request = try await AssetInventory.assetInstallationRequest(
            supporting: [makeTranscriber(module.locale)]) {
            needed = true
            Task.detached(priority: .background) {
                // 装不上就是装不上（没网、没磁盘）：这一次已经降级跑完了，不把这条路变成错误。
                try? await request.downloadAndInstall()
            }
        }
    } catch {
        // 问不出来就当「不用装」：这一次已经降级跑完了，这个判断只影响屏幕上那句话。
    }
    rememberAsked(module.key, needed)
    return needed
}

// installAsked 是上面那个问题的答案（键是 Module.key）。
//
// 用锁而不是 actor：这段代码从 cgo 调进来的线程上跑，没有 actor 上下文可以 await。
nonisolated(unsafe) private var installAsked: [String: Bool] = [:]
private let installAskedLock = NSLock()

private func askedBefore(_ key: String) -> Bool? {
    installAskedLock.lock()
    defer { installAskedLock.unlock() }
    return installAsked[key]
}

private func rememberAsked(_ key: String, _ needed: Bool) {
    installAskedLock.lock()
    defer { installAskedLock.unlock() }
    installAsked[key] = needed
}

// jsonEscaped 转义一个字符串，好把它塞进上面那行 JSON 里。
//
// 手写而不是用 JSONEncoder：要编码的是「两个字符串 + 两个数 + 两个数组」，为此定义一个
// Codable 结构体比自己转义更容易出错（真正的坑只有控制字符和引号，都在下面）。这段文本是
// 识别结果，里面的引号来自人说「引号」两个字，不是没有可能。
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
// 返回值是一行 JSON（见 transcriptionJSON），由 tachi_stt_free 释放。
//
// ⚠ 失败**不**用 NULL 表示：哪几门语言没跑成、为什么没跑成，都在 JSON 的 missing / error
// 里。NULL 只剩「垫片压根没跑起来」这一种意思（系统太老、参数没法用）。之前那种写法把
// 原因吞掉了——调用方只能拿到一句「转写失败」，而「缺资产」和「引擎坏了」是两件要分开
// 处理的事（前者会自己好，后者不会）。
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
    // 注意这里的类型：不再是可选——转写失败也有话要带回去（见上面那段）。
    let json = syncWait { () async -> String in
        await transcribe(pcm, rate: sampleRate)
    }
    return strdup(json)
}
