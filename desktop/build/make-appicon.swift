// make-appicon.swift — 从插画源图生成 macOS 应用图标。
//
//   swift build/make-appicon.swift            # 默认读 build/appicon-source.jpg
//
// 输出两个文件（都必须提交）：
//   build/appicon.png                     ← wails3 generate icons 的输入（→ icons.icns / icon.ico）
//   build/appicon.icon/Assets/tachi_art.png ← Icon Composer 源（macOS 26 的 Liquid Glass 图标，
//                                             需要 Xcode 的 icontool 才会编译进 Assets.car）
//
// 为什么要这一步：macOS 不会给应用图标自动裁形状。直接把矩形插画塞进去，Dock 里就是一块
// 带白边的方块。规范做法是自己在画布上画出圆角方形（外框透明），把插画填进去：
//   - 画布 1024×1024，内容区 824×824（Apple 图标网格，四周留 100pt）
//   - 圆角半径 = 内容边长 × 22.5%
// 源图是 1536×1024 的白底插画，内容本身近似正方形（936×948，居中），所以先按下面的
// CROP 裁一个正方形再填进内容区，不会切掉任何内容；换源图时改 CROP 即可。

import AppKit

// 源图裁切（像素，原点左上）：当前源图的插画内容居中的最大正方形。
let CROP_X: CGFloat = 254
let CROP_Y: CGFloat = 0
let CROP_SIZE: CGFloat = 1024

// Apple 图标网格
let CANVAS: CGFloat = 1024
let INSET: CGFloat = 100
let CORNER_RATIO: CGFloat = 0.225

let args = CommandLine.arguments
let root = FileManager.default.currentDirectoryPath
let srcPath = args.count > 1 ? args[1] : "\(root)/build/appicon-source.jpg"
let outputs = args.count > 2
    ? [args[2]]
    : ["\(root)/build/appicon.png", "\(root)/build/appicon.icon/Assets/tachi_art.png"]

guard let src = NSImage(contentsOfFile: srcPath),
      let cg = src.cgImage(forProposedRect: nil, context: nil, hints: nil) else {
    FileHandle.standardError.write("cannot load \(srcPath)\n".data(using: .utf8)!)
    exit(1)
}

let side = CANVAS - INSET * 2
let radius = side * CORNER_RATIO

guard let ctx = CGContext(data: nil, width: Int(CANVAS), height: Int(CANVAS),
                          bitsPerComponent: 8, bytesPerRow: 0,
                          space: CGColorSpaceCreateDeviceRGB(),
                          bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue),
      let square = cg.cropping(to: CGRect(x: CROP_X, y: CROP_Y, width: CROP_SIZE, height: CROP_SIZE)) else {
    exit(1)
}

// 画布默认透明；只画圆角方形内的插画 → 圆角外保持透明，Dock 里不会出现方块。
let rect = CGRect(x: INSET, y: INSET, width: side, height: side)
ctx.saveGState()
ctx.addPath(CGPath(roundedRect: rect, cornerWidth: radius, cornerHeight: radius, transform: nil))
ctx.clip()
ctx.interpolationQuality = .high
ctx.draw(square, in: rect)
ctx.restoreGState()

guard let image = ctx.makeImage() else { exit(1) }
let data = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:])!
for out in outputs {
    try! FileManager.default.createDirectory(atPath: (out as NSString).deletingLastPathComponent,
                                             withIntermediateDirectories: true)
    try! data.write(to: URL(fileURLWithPath: out))
    print("wrote \(out)")
}
