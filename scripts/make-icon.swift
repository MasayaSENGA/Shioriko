// Renders Shioriko's app icon (a phone with a bookmark ribbon — "shiori" is
// Japanese for bookmark) and writes packaging/AppIcon.icns.
//
//   swift scripts/make-icon.swift [output.icns]
import AppKit

func color(_ hex: UInt32, _ alpha: CGFloat = 1) -> NSColor {
    NSColor(srgbRed: CGFloat((hex >> 16) & 0xFF) / 255,
            green: CGFloat((hex >> 8) & 0xFF) / 255,
            blue: CGFloat(hex & 0xFF) / 255, alpha: alpha)
}

// draw paints the icon on a 1024x1024 canvas (y grows upwards).
func draw() {
    // Background tile on the standard macOS icon grid.
    let tile = NSRect(x: 100, y: 100, width: 824, height: 824)
    let tilePath = NSBezierPath(roundedRect: tile, xRadius: 185, yRadius: 185)
    NSGraphicsContext.saveGraphicsState()
    let shadow = NSShadow()
    shadow.shadowColor = NSColor.black.withAlphaComponent(0.28)
    shadow.shadowOffset = NSSize(width: 0, height: -12)
    shadow.shadowBlurRadius = 28
    shadow.set()
    color(0x12A36F).setFill()
    tilePath.fill()
    NSGraphicsContext.restoreGraphicsState()
    NSGradient(starting: color(0x6EE7B0), ending: color(0x0B8F63))!.draw(in: tilePath, angle: -90)

    // Phone body.
    let body = NSRect(x: 322, y: 196, width: 380, height: 632)
    NSGraphicsContext.saveGraphicsState()
    let phoneShadow = NSShadow()
    phoneShadow.shadowColor = NSColor.black.withAlphaComponent(0.25)
    phoneShadow.shadowOffset = NSSize(width: 0, height: -10)
    phoneShadow.shadowBlurRadius = 24
    phoneShadow.set()
    color(0xFFFFFF).setFill()
    NSBezierPath(roundedRect: body, xRadius: 70, yRadius: 70).fill()
    NSGraphicsContext.restoreGraphicsState()

    // Screen.
    let screen = body.insetBy(dx: 26, dy: 26)
    color(0xE7F7EF).setFill()
    NSBezierPath(roundedRect: screen, xRadius: 46, yRadius: 46).fill()

    // Camera pill.
    color(0x1F2937).setFill()
    NSBezierPath(roundedRect: NSRect(x: 472, y: screen.maxY - 44, width: 80, height: 22), xRadius: 11, yRadius: 11).fill()

    // File rows: a folder-ish square and a text line each.
    for i in 0..<4 {
        let y = screen.maxY - 150 - CGFloat(i) * 92
        color(0x34C28A).setFill()
        NSBezierPath(roundedRect: NSRect(x: screen.minX + 34, y: y, width: 56, height: 56), xRadius: 14, yRadius: 14).fill()
        color(0xB5E3CD).setFill()
        let w: CGFloat = [190, 150, 176, 120][i]
        NSBezierPath(roundedRect: NSRect(x: screen.minX + 110, y: y + 30, width: w, height: 16), xRadius: 8, yRadius: 8).fill()
        NSBezierPath(roundedRect: NSRect(x: screen.minX + 110, y: y + 6, width: w * 0.6, height: 12), xRadius: 6, yRadius: 6).fill()
    }

    // Bookmark ribbon tucked into the top of the phone.
    let rx: CGFloat = 588, rw: CGFloat = 70, top: CGFloat = 868, bottom: CGFloat = 560, notch: CGFloat = 34
    let ribbon = NSBezierPath()
    ribbon.move(to: NSPoint(x: rx, y: top))
    ribbon.line(to: NSPoint(x: rx + rw, y: top))
    ribbon.line(to: NSPoint(x: rx + rw, y: bottom))
    ribbon.line(to: NSPoint(x: rx + rw / 2, y: bottom + notch))
    ribbon.line(to: NSPoint(x: rx, y: bottom))
    ribbon.close()
    NSGraphicsContext.saveGraphicsState()
    let ribbonShadow = NSShadow()
    ribbonShadow.shadowColor = NSColor.black.withAlphaComponent(0.22)
    ribbonShadow.shadowOffset = NSSize(width: 0, height: -4)
    ribbonShadow.shadowBlurRadius = 8
    ribbonShadow.set()
    color(0xF2555A).setFill()
    ribbon.fill()
    NSGraphicsContext.restoreGraphicsState()
    NSGradient(starting: color(0xFF7A70), ending: color(0xE2444C))!.draw(in: ribbon, angle: -90)
}

func png(size: Int) -> Data {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    rep.size = NSSize(width: size, height: size)
    NSGraphicsContext.saveGraphicsState()
    let ctx = NSGraphicsContext(bitmapImageRep: rep)!
    NSGraphicsContext.current = ctx
    ctx.imageInterpolation = .high
    let scale = CGFloat(size) / 1024
    ctx.cgContext.scaleBy(x: scale, y: scale)
    draw()
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

let out = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "packaging/AppIcon.icns"
let iconset = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("Shioriko.iconset")
try? FileManager.default.removeItem(at: iconset)
try! FileManager.default.createDirectory(at: iconset, withIntermediateDirectories: true)
for base in [16, 32, 128, 256, 512] {
    try! png(size: base).write(to: iconset.appendingPathComponent("icon_\(base)x\(base).png"))
    try! png(size: base * 2).write(to: iconset.appendingPathComponent("icon_\(base)x\(base)@2x.png"))
}
let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset.path, "-o", out]
try! p.run()
p.waitUntilExit()
if p.terminationStatus != 0 { exit(1) }
// Also leave a preview next to the icns for reviewing the artwork.
if CommandLine.arguments.count > 2 {
    try! png(size: 1024).write(to: URL(fileURLWithPath: CommandLine.arguments[2]))
}
print("wrote \(out)")
