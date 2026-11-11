import Foundation
import os

/// Paths inside the app container. The Linux guest sees `root` bind-mounted at
/// /pb (read-only code, read-write data), so every path the engine stores is
/// relative to `root` and resolves the same on both sides.
enum Layout {
    static let root: URL = {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        let url = base.appendingPathComponent("pocketbox", isDirectory: true)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }()

    static var content: URL { root.appendingPathComponent("content") }
    static var images: URL { root.appendingPathComponent("images") }
    static var engine: URL { root.appendingPathComponent("engine") }
    static var logs: URL { root.appendingPathComponent("logs") }
    static var volumes: URL { root.appendingPathComponent("volumes") }
    static var tmp: URL { root.appendingPathComponent("tmp") }

    /// Where the ish fakefs lives. Kept outside `images` so a purge of images
    /// never removes the shell state the daemon runs in.
    static var rootfs: URL { root.appendingPathComponent("rootfs") }
    static var fakefs: URL { rootfs.appendingPathComponent("alpine.pbfs") }
    static var rootfsData: URL { fakefs.appendingPathComponent("data") }
    static var seededMarker: URL { rootfs.appendingPathComponent(".seeded") }

    /// Guest path of the app data root.
    static let guestRoot = "/pb"

    static func guestPath(forHostPath path: String) -> String {
        let prefix = root.path
        if path.hasPrefix(prefix) {
            return guestRoot + path.dropFirst(prefix.count)
        }
        return path
    }

    static func hostPath(forGuestPath path: String) -> String {
        if path.hasPrefix(guestRoot) {
            return root.path + path.dropFirst(guestRoot.count)
        }
        return path
    }

    static func prepare() {
        for dir in [content, images, engine, logs, volumes, tmp, rootfs] {
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        }
    }
}

/// Small ring of recent engine log lines shown in the console tab.
final class EngineLog: ObservableObject {
    static let shared = EngineLog()
    @Published private(set) var lines: [String] = []
    private let limit = 400
    private let queue = DispatchQueue(label: "pocketbox.log")

    func append(_ line: String) {
        Log.runtime.debug("\(line, privacy: .public)")
        queue.async {
            DispatchQueue.main.async {
                self.lines.append(line)
                if self.lines.count > self.limit {
                    self.lines.removeFirst(self.lines.count - self.limit)
                }
            }
        }
    }

    func clear() {
        DispatchQueue.main.async { self.lines.removeAll() }
    }
}

enum Log {
    static let runtime = os.Logger(subsystem: "dev.pocketbox.app", category: "runtime")
    static let api = os.Logger(subsystem: "dev.pocketbox.app", category: "api")
    static let ui = os.Logger(subsystem: "dev.pocketbox.app", category: "ui")
}
