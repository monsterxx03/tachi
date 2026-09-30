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

// --- 采集（麦克风）--------------------------------------------------------------

// 采集是这条链的**前一半**，而它以前不存在：转写那半吃的是「一段 16 kHz 单声道 Int16」，
// 而那段样本一直由 badge 那条路送进来（设备 8 KB/s 的 IMA ADPCM 走 BLE，desktop/badge.go
// 解成样本）。桌面自己的麦克风按钮要的就是同一句话——所以这里把它录成**同一个形状**，
// 好让 desktop 那边两个调用方喂给 Transcribe 的东西逐字相同。
//
// 为什么不用 AVAudioRecorder 写个文件再读回来：识别那半承诺过音频不落盘（设计文档 §8），
// 而这里连临时文件都不需要——采样回调交回来的就是内存里的 buffer。中间那次重采样也在内存
// 里，走 AVAudioConverter（硬件给的是 16/44.1/48 kHz 的浮点，而引擎要 16 k）。
//
// ⚠ **为什么是 AVCaptureSession 而不是 AVAudioEngine**（这一条是踩出来的，别再换回去）：
// AVAudioEngine 的 IO 周期采样率取自**输出设备**，而它的 inputNode 必须服从同一条时钟——
// 同一副蓝牙耳机输入是 16 k、输出是 44.1 k，于是 inputNode 报出来的格式（44.1 k）在输入
// 硬件上根本不存在。它的采集形状又是「读格式 → 按这个格式装 tap → start」，格式在装 tap
// 那一刻定死；而蓝牙耳机空闲时停在 A2DP（没有可用输入），按下麦克风才切到 HFP，这一切都
// 发生在 start 之后。结果是 start() 不抛错、isRunning 随后翻 false、**tap 一次都不回调**，
// 上层看到的就是「按下了但没听到内容」——一个样本都没有，而不是转写为空。
// AVCaptureSession 反过来：它管设备，startRunning 会**等** profile 切换完成（实测 ~57 ms），
// 每块的格式随后由 CMSampleBuffer 如实带来，没有任何地方需要提前猜。它不做音频图，所以也
// 没有「大家必须同一条时钟」这条约束。
//
// ⚠ 这一段**不**要求 macOS 26，所以它不关在 #available 里：AVCaptureSession 老得多，真正
// 要求 26 的是转写那半（availability 由 tachi_stt_available 一个人回答，见 stt_darwin.go）。
// ⚠ 但麦克风要**另一个** TCC 权限，和语音识别不是同一个（NSMicrophoneUsageDescription）：
// 少了那个键，第一次真的去开会崩，不是返回错误——和蓝牙那次同一个坑（docs/agents/badge.md）。

// recordSampleRate 是交回给 Go 的采样率。
//
// 16 k 不是随便挑的：转写那半的输入一直是这个数（badge 那条路的目标采样率也是它），所以
// 两个调用方在引擎眼里没有区别——判据、资产、降级那一整套都在同一种输入上被验过。
private let recordSampleRate: Double = 16000

// recordMaxSamples 是垫片自己的硬上限（120 秒），**不是**「一次录多久」的策略。
//
// 那个策略在 Go 侧（voiceMaxRecord，60 秒的看门狗）：录多久要能被上层的状态机和界面看见，
// 而这是采集层的自卫——万一没人来 Stop，这个数组不能一直涨下去把内存吃掉。
private let recordMaxSamples = Int(recordSampleRate) * 120

// recordTarget 是交给识别引擎的格式，全局只建一次（转写那半要的就是 16 k 单声道 Int16）。
private let recordTarget = AVAudioFormat(commonFormat: .pcmFormatInt16,
                                        sampleRate: recordSampleRate,
                                        channels: 1,
                                        interleaved: true)

// recordQueue 是采样回调跑的那条队列。
//
// **串行**是有意的：转换器是有状态的（重采样会跨块带尾巴），它只在这条队列上被碰，所以
// RecordSink 里那个 converter 不必再加一把锁。
private let recordQueue = DispatchQueue(label: "tachi.stt.record")

// 采集状态。用锁而不是 actor：这些函数都从 cgo 调进来的线程上跑，没有 actor 上下文可以
// await（和 installAsked 同一处境），而回调那边是另一条队列——两边都要看同一份数据。
private let recordLock = NSLock()
nonisolated(unsafe) private var recordSession: AVCaptureSession?
nonisolated(unsafe) private var recordSamples: [Int16] = []
// recordSink 必须是**强引用**：setSampleBufferDelegate 存的是 weak，这个字段一没，回调就
// 静默停止——症状和「设备格式对不上」一模一样（会话在跑，一个样本都不来）。
nonisolated(unsafe) private var recordSink: RecordSink?

// tachi_stt_mic_status 回答麦克风的授权状态。
//
// 三个值，而它们要分成三句不同的话讲给用户听，所以不压成 bool：
//   1 = 有权限   0 = 还没问过（该弹一次窗）   2 = 被拒了/被系统策略挡住（只能去系统设置里改）
// 采集和转写是**两个**权限：有语音识别权限不等于能开麦克风，反之亦然。
@_cdecl("tachi_stt_mic_status")
public func tachiSTTMicStatus() -> Int32 {
    if #available(macOS 14.0, *) {
        switch AVAudioApplication.shared.recordPermission {
        case .granted: return 1
        case .denied: return 2
        case .undetermined: return 0
        @unknown default: return 2
        }
    }
    switch AVCaptureDevice.authorizationStatus(for: .audio) {
    case .authorized: return 1
    case .notDetermined: return 0
    default: return 2
    }
}

// tachi_stt_mic_ask 弹一次权限询问（系统弹窗）并**等用户点完**，返回拿到的状态。
//
// 为什么是「问出答案」而不是「发起一次询问就返回」：调用它的是用户点麦克风的那一下。
// 不等的话，第一次使用就变成「点一下 → 请去授权 → 再点一下」，而用户刚才明明就是在按
// 「开始说话」。等它，那次点击的直接后果就是开始录音。
//
// 为什么还要超时：弹窗没人点就永远不会有人 signal（用户可能就把它晾在那儿）。超时返回
// 0（还没问出结果）而不是一直挂着——调用方据此说一句「授权窗口还开着」，而那个还在等的
// 询问会在用户点完之后自己完成（信号量由闭包持有，超时后 signal 也不会出事）。
@_cdecl("tachi_stt_mic_ask")
public func tachiSTTMicAsk(_ timeoutSeconds: Double) -> Int32 {
    let semaphore = DispatchSemaphore(value: 0)
    if #available(macOS 14.0, *) {
        AVAudioApplication.requestRecordPermission { _ in semaphore.signal() }
    } else {
        AVCaptureDevice.requestAccess(for: .audio) { _ in semaphore.signal() }
    }
    if semaphore.wait(timeout: .now() + timeoutSeconds) == .timedOut {
        return 0
    }
    return tachiSTTMicStatus()
}

// tachi_stt_record_start 开始采集。返回 1 表示真的在录，0 表示没起来（原因进不了 C ABI
// 的正经通道，所以调用方先自己查权限/可用性，见 stt_darwin.go）。
//
// 幂等：已经在录时重复调用返回 1，不重建会话——上层那次重复点击（界面还没收到状态）不该
// 把正在录的东西冲掉。
//
// ⚠ startRunning 是**阻塞**的，而这里要的正是它阻塞：蓝牙耳机从 A2DP 切到 HFP 就发生在这
// 几百毫秒里（实测 ~57 ms），切完才可能有数据。调用它的是 cgo 出来的线程（不是主线程），
// 所以等在这里不会卡住界面。
@_cdecl("tachi_stt_record_start")
public func tachiSTTRecordStart() -> Int32 {
    recordLock.lock()
    let already = recordSession != nil
    recordLock.unlock()
    if already { return 1 }

    // 没授权就**不**碰设备：硬开会崩（少 Info.plist 键）或者立刻抛（被拒），而调用方要的是
    // 一句能讲给用户听的话，不是这里的一声闷响。
    if tachiSTTMicStatus() != 1 { return 0 }

    // 没有输入设备时 AVCaptureDevice.default 给 nil：那不是「静音」，是「录不了」。
    guard let device = AVCaptureDevice.default(for: .audio),
          let input = try? AVCaptureDeviceInput(device: device) else { return 0 }
    let session = AVCaptureSession()
    guard session.canAddInput(input) else { return 0 }
    session.addInput(input)

    let sink = RecordSink()
    let output = AVCaptureAudioDataOutput()
    output.setSampleBufferDelegate(sink, queue: recordQueue)
    guard session.canAddOutput(output) else { return 0 }
    session.addOutput(output)

    recordLock.lock()
    recordSamples = []
    recordLock.unlock()

    session.startRunning()
    guard session.isRunning else { return 0 }

    recordLock.lock()
    recordSession = session
    recordSink = sink
    recordLock.unlock()
    return 1
}

// tachi_stt_record_stop 结束采集，把攒下的样本交出去。
//
// 返回 1 表示这一次采集结束了（count 可能是 0——那是「按下了但没说话」，正常输入）；
// 返回 0 表示当前没有在采（重复点击、或者本来就没起来）。两种都不是错误，调用方分得开
// 就够——所以没有第三态。
//
// ⚠ 交出去的样本由调用方用 tachi_stt_free_samples 释放。
@_cdecl("tachi_stt_record_stop")
public func tachiSTTRecordStop(_ samples: UnsafeMutablePointer<UnsafeMutablePointer<Int16>?>?,
                               _ count: UnsafeMutablePointer<Int>?,
                               _ rate: UnsafeMutablePointer<Double>?) -> Int32 {
    recordLock.lock()
    let session = recordSession
    let sink = recordSink
    recordSession = nil
    recordLock.unlock()
    guard let session = session else { return 0 }

    // 会话先停，再等回调把队排空：停会话不等于已经在跑的那一块回调结束了，不等它，最后
    // 几十毫秒的语音会被丢掉——而那正好是用户松手前说的最后半句。
    session.stopRunning()
    recordQueue.sync {}

    recordLock.lock()
    let got = recordSamples
    recordSamples = []
    recordLock.unlock()
    _ = sink // 会话停之前一直持有（回调正是靠它活着），到这里就不再有回调了

    count?.pointee = got.count
    rate?.pointee = recordSampleRate
    guard !got.isEmpty else {
        samples?.pointee = nil
        return 1
    }
    let buffer = UnsafeMutablePointer<Int16>.allocate(capacity: got.count)
    buffer.initialize(from: got, count: got.count)
    samples?.pointee = buffer
    return 1
}

// tachi_stt_free_samples 释放 tachi_stt_record_stop 交出去的样本。
//
// 单独一个函数而不是复用 tachi_stt_free：那两个 buffer 是不同方式分配的（strdup 出来的
// 字符串 vs Int16 的 allocate），配对错了就是堆损坏——而这种错**不会**在这里报，它会
// 在后面某次分配时炸掉。
@_cdecl("tachi_stt_free_samples")
public func tachiSTTFreeSamples(_ samples: UnsafeMutablePointer<Int16>?) {
    samples?.deallocate()
}

// RecordSink 把会话交来的每一块音频转成 16 k 单声道 Int16，接在记录后面。
//
// ⚠ 这个类的**格式不再提前假定**：采样率从每块自己的格式描述里读（内置麦 48 k、蓝牙 HFP
// 16 k 都走同一份代码），转换器因此按源格式建、格式变了就重建。采集那一半过去栽在
// 「start 之前先问一次格式、然后把它定死」上——那时问到的其实是**输出设备**的采样率。
//
// 它只跑在 recordQueue 上（串行），所以 converter 不需要锁；取不到数据就**安静地丢掉这
// 一块**——识别引擎要的是连续样本，往里塞缺口只会得到更差的文本，而真的什么都录不到时，
// 上层看到的是 count 为 0 的那一下。
private final class RecordSink: NSObject, AVCaptureAudioDataOutputSampleBufferDelegate {
    private var converter: AVAudioConverter?
    private var converterRate: Double = 0

    func captureOutput(_ output: AVCaptureOutput,
                       didOutput sampleBuffer: CMSampleBuffer,
                       from connection: AVCaptureConnection) {
        guard let target = recordTarget else { return }
        let frames = CMSampleBufferGetNumSamples(sampleBuffer)
        guard frames > 0,
              let description = CMSampleBufferGetFormatDescription(sampleBuffer),
              let asbd = CMAudioFormatDescriptionGetStreamBasicDescription(description),
              let source = AVAudioFormat(streamDescription: asbd),
              let input = AVAudioPCMBuffer(pcmFormat: source, frameCapacity: AVAudioFrameCount(frames))
        else { return }
        input.frameLength = AVAudioFrameCount(frames)
        guard CMSampleBufferCopyPCMDataIntoAudioBufferList(sampleBuffer, at: 0,
                                                          frameCount: Int32(frames),
                                                          into: input.mutableAudioBufferList) == noErr else { return }

        if converter == nil || converterRate != source.sampleRate {
            converter = AVAudioConverter(from: source, to: target)
            converterRate = source.sampleRate
        }
        guard let converter = converter else { return }

        let ratio = recordSampleRate / source.sampleRate
        // 容量留一点余量：降采样的输出帧数会因块的边界有±几帧的出入，紧贴着算会丢掉尾巴。
        let capacity = AVAudioFrameCount(Double(frames) * ratio) + 32
        guard let out = AVAudioPCMBuffer(pcmFormat: target, frameCapacity: capacity) else { return }

        // 转换器要求「按需供料」：给它一块，然后告诉它没有了（noDataNow）——它会把这一块
        // 转出来的帧数如实写在 out.frameLength 里（可能少于容量，也可能跨块带尾巴）。
        var supplied = false
        var conversionError: NSError?
        converter.convert(to: out, error: &conversionError) { _, status in
            if supplied {
                status.pointee = .noDataNow
                return nil
            }
            supplied = true
            status.pointee = .haveData
            return input
        }
        guard conversionError == nil, out.frameLength > 0, let channel = out.int16ChannelData else { return }

        recordLock.lock()
        defer { recordLock.unlock() }
        // 单声道 interleaved：第 0 个通道就是全部样本。
        let converted = Int(out.frameLength)
        if recordSamples.count >= recordMaxSamples { return }
        let room = min(converted, recordMaxSamples - recordSamples.count)
        recordSamples.append(contentsOf: UnsafeBufferPointer(start: channel[0], count: room))
    }
}
